// Command soonest-reset is a CLIProxyAPI scheduler and usage plugin, built with
// -buildmode=c-shared. This file is the cgo glue only; the behaviour lives in
// internal/plugin.
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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"

	"cpa-soonest-reset/internal/plugin"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// tickEvery is how often the plugin checks whether a retest is due.
const tickEvery = time.Minute

var instance atomic.Pointer[plugin.Plugin]

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	C.store_host_api(host)
	instance.Store(plugin.New(plugin.NewHost(callHost), time.Now, tickEvery))
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if r := recover(); r != nil {
			raw, _ := pluginabi.NewErrorEnvelope("plugin_panic", fmt.Sprint(r))
			writeResponse(response, raw)
			rc = 0
		}
	}()
	p := instance.Load()
	if method == nil || p == nil {
		raw, _ := pluginabi.NewErrorEnvelope("invalid_call", "plugin not initialised or method missing")
		writeResponse(response, raw)
		return 0
	}
	var body []byte
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, p.Handle(C.GoString(method), body))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	if p := instance.Load(); p != nil {
		p.Shutdown()
	}
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var request *C.uint8_t
	if len(payload) > 0 {
		cPayload := C.CBytes(payload)
		defer C.free(cPayload)
		request = (*C.uint8_t)(cPayload)
	}
	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, request, C.size_t(len(payload)), &response)
	var raw []byte
	if response.ptr != nil {
		if response.len > 0 {
			raw = C.GoBytes(response.ptr, C.int(response.len))
		}
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(raw) == 0 {
		if rc != 0 {
			return nil, fmt.Errorf("host call returned %d with no response", int(rc))
		}
		return nil, errors.New("host call returned no response")
	}
	return raw, nil
}
