package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// The host API table is owned by CPA and freed after shutdown, so its fields
// are copied here instead of keeping the pointer. Go cannot call C function
// pointers directly; these bridges do.
static void* hu_host_ctx;
static cliproxy_host_call_fn hu_host_call;
static cliproxy_host_free_fn hu_host_free;

static void hu_store_host(const cliproxy_host_api* host) {
	hu_host_ctx = host->host_ctx;
	hu_host_call = host->call;
	hu_host_free = host->free_buffer;
}

static void hu_clear_host(void) {
	hu_host_call = NULL;
	hu_host_free = NULL;
	hu_host_ctx = NULL;
}

static int hu_call_host(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* resp) {
	if (hu_host_call == NULL) {
		return 1;
	}
	return hu_host_call(hu_host_ctx, method, req, len, resp);
}

// Host buffers must be released by the host allocator (LocalAlloc on Windows).
static void hu_free_host(void* ptr, size_t len) {
	if (ptr != NULL && hu_host_free != NULL) {
		hu_host_free(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"cpa-harness-usage/internal/plugin"
)

const maxRequestBytes = 4 << 20

var (
	coreMu sync.RWMutex
	core   = plugin.New()
)

// hostMu keeps host calls from racing shutdown: CPA cancels in-flight host
// operations before calling shutdown, then frees host_ctx afterwards.
var (
	hostMu    sync.RWMutex
	hostReady bool
)

var errHostCall = errors.New("host call failed")

func main() {}

func getCore() *plugin.Plugin {
	coreMu.RLock()
	defer coreMu.RUnlock()
	return core
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil || host == nil || host.abi_version != 1 {
		return 1
	}
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)

	newCore := plugin.New()

	hasHost := host.call != nil && host.free_buffer != nil
	hostMu.Lock()
	if hasHost {
		C.hu_store_host(host)
		hostReady = true
	} else {
		C.hu_clear_host()
		hostReady = false
	}
	hostMu.Unlock()

	if hasHost {
		newCore.SetHost(hostCall)
	}

	coreMu.Lock()
	oldCore := core
	core = newCore
	coreMu.Unlock()

	if oldCore != nil {
		oldCore.Close()
	}

	return 0
}

// hostCall invokes one CPA host callback and returns its response envelope.
func hostCall(method string, request []byte) ([]byte, error) {
	hostMu.RLock()
	defer hostMu.RUnlock()
	if !hostReady {
		return nil, errHostCall
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest unsafe.Pointer
	if len(request) > 0 {
		cRequest = C.CBytes(request)
		defer C.free(cRequest)
	}
	var response C.cliproxy_buffer
	rc := C.hu_call_host(cMethod, (*C.uint8_t)(cRequest), C.size_t(len(request)), &response)
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.hu_free_host(response.ptr, response.len)
	}
	if rc != 0 || len(out) == 0 {
		return nil, errHostCall
	}
	return out, nil
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, req *C.uint8_t, size C.size_t, resp *C.cliproxy_buffer) C.int {
	if resp == nil {
		return 1
	}
	resp.ptr = nil
	resp.len = 0
	if method == nil || size > maxRequestBytes || (size > 0 && req == nil) {
		writeEnvelope(resp, cpaError("invalid_request", "request is invalid"))
		return 1
	}
	var raw []byte
	if size > 0 {
		raw = C.GoBytes(unsafe.Pointer(req), C.int(size))
	}
	p := getCore()
	if p == nil {
		writeEnvelope(resp, cpaError("unavailable", "plugin is not initialized"))
		return 0
	}
	writeEnvelope(resp, p.Handle(C.GoString(method), raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	coreMu.Lock()
	p := core
	core = nil
	coreMu.Unlock()

	if p != nil {
		p.Close()
	}

	hostMu.Lock()
	hostReady = false
	C.hu_clear_host()
	hostMu.Unlock()
}

func cpaError(code, message string) []byte {
	raw, err := json.Marshal(map[string]any{
		"ok":    false,
		"error": map[string]string{"code": code, "message": message},
	})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"internal_error","message":"internal_error"}}`)
	}
	return raw
}

func writeEnvelope(resp *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	resp.ptr = ptr
	resp.len = C.size_t(len(raw))
}
