package main

/*
#cgo linux LDFLAGS: -ldl
#include <stdint.h>
#include <stdlib.h>
#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

typedef struct { void* ptr; size_t len; } smoke_buffer;
typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    void* call;
    void* free_buffer;
} smoke_host;
typedef struct {
    uint32_t abi_version;
    int (*call)(char*, uint8_t*, size_t, smoke_buffer*);
    void (*free_buffer)(void*, size_t);
    void (*shutdown)(void);
} smoke_plugin;
typedef int (*smoke_init_fn)(smoke_host*, smoke_plugin*);

static int smoke_open(char* path, smoke_plugin* plugin) {
#ifdef _WIN32
    HMODULE library = LoadLibraryA(path);
    if (!library) return 1;
    smoke_init_fn init = (smoke_init_fn)GetProcAddress(library, "cliproxy_plugin_init");
#else
    void* library = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (!library) return 1;
    smoke_init_fn init = (smoke_init_fn)dlsym(library, "cliproxy_plugin_init");
#endif
    if (!init) return 2;
    smoke_host host = {1, NULL, NULL, NULL};
    if (init(&host, plugin) != 0) return 3;
    if (plugin->abi_version != 1 || !plugin->call || !plugin->free_buffer || !plugin->shutdown) return 4;
    return 0;
}

static int smoke_call(smoke_plugin* plugin, char* method, uint8_t* request, size_t length, smoke_buffer* response) {
    return plugin->call(method, request, length, response);
}

static void smoke_free(smoke_plugin* plugin, smoke_buffer response) {
    plugin->free_buffer(response.ptr, response.len);
}

static void smoke_shutdown(smoke_plugin* plugin) {
    plugin->shutdown();
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"
)

func main() {
	library := flag.String("library", "", "plugin library to load")
	binary := flag.String("binary", "", "synthetic collector executable or bundled")
	isolated := flag.Bool("isolated", false, "run inside the prepared smoke environment")
	flag.Parse()
	var err error
	if *isolated {
		err = run(*library, *binary)
	} else {
		err = launchIsolated(*library, *binary)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin smoke failed:", err)
		os.Exit(1)
	}
	if *isolated {
		fmt.Println("Go host plugin ABI, lifecycle and isolated collection passed")
	}
}

func launchIsolated(library, binary string) error {
	if library == "" || binary == "" {
		return errors.New("library and collector binary are required")
	}
	library, err := filepath.Abs(library)
	if err != nil {
		return errors.New("invalid library argument")
	}
	if binary != "bundled" {
		binary, err = filepath.Abs(binary)
		if err != nil {
			return errors.New("invalid binary argument")
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("smoke executable lookup failed")
	}
	cleanup, err := isolateEnvironment()
	if err != nil {
		return err
	}
	defer cleanup()
	if binary != "bundled" {
		for _, source := range []string{"CLAUDE", "CODEX", "OPENCODE"} {
			if err := os.Setenv("FAKE_"+source+"_BODY", `{"daily":[]}`); err != nil {
				return errors.New("synthetic environment failed")
			}
			if err := os.Setenv("FAKE_"+source+"_MODE", "empty"); err != nil {
				return errors.New("synthetic environment failed")
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-isolated", "-library", library, "-binary", binary)
	command.WaitDelay = time.Second
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return errors.New("isolated smoke process failed")
	}
	return nil
}

func run(library, binary string) error {
	path := C.CString(library)
	defer C.free(unsafe.Pointer(path))
	var api C.smoke_plugin
	if status := C.smoke_open(path, &api); status != 0 {
		return fmt.Errorf("native initialization failed (stage %d)", int(status))
	}
	defer C.smoke_shutdown(&api)
	config, _ := json.Marshal(binary)
	request, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("binary: " + string(config) + "\n")})
	if _, err := call(&api, "plugin.register", request); err != nil {
		return err
	}
	if _, err := call(&api, "management.register", []byte(`{}`)); err != nil {
		return err
	}
	body, err := call(&api, "management.handle", []byte(`{"Method":"GET","Path":"/v0/management/harness-usage/daily"}`))
	if err != nil {
		return err
	}
	var response struct {
		StatusCode int
		Body       []byte
	}
	if json.Unmarshal(body, &response) != nil || response.StatusCode != 200 {
		return errors.New("management response failed")
	}
	var report struct {
		Sources []struct{ Source, Status, Error string }
	}
	if json.Unmarshal(response.Body, &report) != nil || len(report.Sources) != 3 {
		return errors.New("synthetic report failed")
	}
	for _, source := range report.Sources {
		if source.Status != "empty" {
			return fmt.Errorf("isolated collector failed (%s: %s)", source.Source, source.Error)
		}
	}
	if _, err := call(&api, "plugin.quiesce", []byte(`{}`)); err != nil {
		return err
	}
	_, err = call(&api, "plugin.shutdown", []byte(`{}`))
	return err
}

func isolateEnvironment() (func(), error) {
	originalDirectory, err := os.Getwd()
	if err != nil {
		return nil, errors.New("working directory lookup failed")
	}
	root, err := os.MkdirTemp("", "harness-usage-smoke-")
	if err != nil {
		return nil, errors.New("isolated environment creation failed")
	}
	cleanup := func() {
		_ = os.Chdir(originalDirectory)
		_ = os.RemoveAll(root)
	}
	environment := map[string]string{"TZ": "UTC", "NO_COLOR": "1"}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"SystemRoot", "WINDIR"} {
			if value := os.Getenv(name); value != "" {
				environment[name] = value
			}
		}
	}
	for name, directory := range map[string]string{
		"HOME": "home", "USERPROFILE": "home", "APPDATA": "appdata", "LOCALAPPDATA": "localappdata",
		"CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude", "OPENCODE_DATA_DIR": "opencode",
		"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state",
		"TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp",
	} {
		path := filepath.Join(root, directory)
		if err := os.MkdirAll(path, 0700); err != nil {
			cleanup()
			return nil, errors.New("isolated directory creation failed")
		}
		environment[name] = path
	}
	if err := os.MkdirAll(filepath.Join(root, "claude", "projects"), 0700); err != nil {
		cleanup()
		return nil, errors.New("isolated Claude directory creation failed")
	}
	os.Clearenv()
	for name, value := range environment {
		if err := os.Setenv(name, value); err != nil {
			cleanup()
			return nil, errors.New("isolated environment setup failed")
		}
	}
	if err := os.Chdir(root); err != nil {
		cleanup()
		return nil, errors.New("isolated working directory setup failed")
	}
	return cleanup, nil
}

func call(api *C.smoke_plugin, method string, request []byte) (json.RawMessage, error) {
	methodName := C.CString(method)
	defer C.free(unsafe.Pointer(methodName))
	data := C.CBytes(request)
	defer C.free(data)
	var response C.smoke_buffer
	status := C.smoke_call(api, methodName, (*C.uint8_t)(data), C.size_t(len(request)), &response)
	defer C.smoke_free(api, response)
	if status != 0 || response.ptr == nil || response.len > 32<<20 {
		return nil, errors.New("ABI call failed")
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &envelope) != nil || !envelope.OK {
		return nil, errors.New("RPC response failed")
	}
	return envelope.Result, nil
}
