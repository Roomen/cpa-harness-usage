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

*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"cpa-harness-usage/internal/plugin"
)

const maxRequestBytes = 4 << 20

var core = plugin.New()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil || host == nil || host.abi_version != 1 {
		return 1
	}
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
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
	writeEnvelope(resp, core.Handle(C.GoString(method), raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { core.Close() }

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
