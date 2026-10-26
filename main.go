// Package main implements the oc2cpa plugin for CLIProxyAPI.
//
// The plugin wraps the OpenCode Zen free model endpoint (opencode.ai/inference/v1)
// as a CLIProxyAPI provider: it speaks OpenAI chat-completions both ways, injects the
// reserved "read" gate tool the free tier requires, renames client tools into oc2cpa_*
// aliases, restores them in responses, and aggregates the stream-only upstream into a
// non-streaming payload when the client asked for one.
//
// It is a Go port of the original Node.js server (zhuweiyou/oc2api, v2.9.0)
// adapted to the
// CLIProxyAPI c-shared plugin ABI (version 1, RPC schema 6).
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
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const abiVersion uint32 = pluginabi.ABIVersion

// envelope mirrors pluginabi.Envelope for the RPC bytes crossing the C boundary.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func main() {}

// cliproxy_plugin_init is the only symbol the host looks up by name. It must not
// start goroutines or timers: on darwin/amd64 the Go runtime parks the first
// goroutine on the dlopen thread and corrupts the host memory allocator.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall is the single entry point for every host-to-plugin RPC.
// Request bytes always live in host memory, so they are copied before use; the
// response buffer is allocated here and released by the host through
// cliproxyPluginFree.
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	defer func() {
		if recover() != nil {
			writeResponseBytes(response, marshalErrorEnvelope("plugin_panic", "plugin request failed safely", 500))
			rc = 1
		}
	}()
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponseBytes(response, marshalErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	if response == nil || uint64(requestLen) > maxRPCBytes || (request == nil && requestLen > 0) {
		writeResponseBytes(response, marshalErrorEnvelope("invalid_request", "invalid or oversized plugin request", 413))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		status := 0
		type statusCoder interface{ StatusCode() int }
		var sc statusCoder
		if errors.As(errHandle, &sc) && sc != nil {
			status = sc.StatusCode()
		}
		code := pluginErrorCode
		type codeCoder interface{ ErrorCode() string }
		var cc codeCoder
		if errors.As(errHandle, &cc) && cc != nil {
			code = cc.ErrorCode()
		}
		writeResponseBytes(response, marshalErrorEnvelope(code, errHandle.Error(), status))
		return 1
	}
	writeResponseBytes(response, raw)
	return 0
}

// cliproxyPluginFree releases response buffers the host no longer needs.
// num is informational only; C.CBytes is not indexed by length.
//
//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

// cliproxyPluginShutdown is a deliberate no-op: the host calls it right before
// the library is dropped. Touching Go runtime state here (locks, channels,
// goroutines) has been observed to SIGSEGV under the host's teardown path, so
// cleanup is left to the operating system reclaiming the process.
//
//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponseBytes(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	if len(raw) > maxRPCBytes {
		raw = marshalErrorEnvelope("response_too_large", "plugin response exceeds size limit", 502)
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func marshalErrorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: message, HTTPStatus: status},
	})
	return raw
}

func marshalOKEnvelope(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return marshalErrorEnvelope(pluginErrorCode, err.Error())
	}
	// Doubly nested marshal keeps the envelope shape {"ok":true,"result":<result>}.
	out, err := json.Marshal(envelope{OK: true, Result: json.RawMessage(raw)})
	if err != nil {
		return marshalErrorEnvelope(pluginErrorCode, err.Error())
	}
	return out
}

func handleMethod(method string, requestJSON []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(requestJSON); err != nil {
			return marshalErrorEnvelope("invalid_configuration", err.Error(), 400), nil
		}
		bootstrapAuthOnce()
		return marshalOKEnvelope(registration()), nil
	case pluginabi.MethodPluginQuiesce:
		return marshalOKEnvelope(struct{}{}), nil
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return marshalOKEnvelope(identifierResponse{Identifier: providerName}), nil
	case pluginabi.MethodAuthParse:
		return marshalOKEnvelope(handleParseAuth(requestJSON)), nil
	case pluginabi.MethodAuthRefresh:
		return marshalOKEnvelope(handleRefreshAuth(requestJSON)), nil
	case pluginabi.MethodModelStatic:
		return marshalOKEnvelope(handleStaticModels(requestJSON)), nil
	case pluginabi.MethodModelForAuth:
		return marshalOKEnvelope(handleModelsForAuth(requestJSON)), nil
	case pluginabi.MethodExecutorExecute:
		result, execErr := handleExecute(requestJSON)
		if execErr != nil {
			return marshalErrorEnvelopeFrom(execErr), nil
		}
		return marshalOKEnvelope(result), nil
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecuteStream(requestJSON)
	case pluginabi.MethodExecutorCountTokens:
		return marshalErrorEnvelope("unsupported_method", "count_tokens is not supported by oc2cpa"), nil

	case pluginabi.MethodUsageHandle:
		if err := handleUsage(requestJSON); err != nil {
			return marshalErrorEnvelope("usage_log_failed", err.Error()), nil
		}
		return marshalOKEnvelope(struct{}{}), nil
	case pluginabi.MethodManagementRegister:
		payload, mgmtErr := handleManagementRoutes(requestJSON)
		if mgmtErr != nil {
			return marshalErrorEnvelope("management_register_failed", mgmtErr.Error()), nil
		}
		return marshalOKEnvelope(payload), nil
	case pluginabi.MethodManagementHandle:
		resp, mgmtErr := handleManagement(requestJSON)
		if mgmtErr != nil {
			return marshalErrorEnvelope("management_handle_failed", mgmtErr.Error()), nil
		}
		return marshalOKEnvelope(resp), nil
	default:
		return marshalErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

const (
	pluginErrorCode = "plugin_error"
)

// goHostCall is the cgo bridge a plugin uses to call any host.* method. It
// returns the raw envelope bytes; callers unwrap {"ok":true,"result":...} on
// their own. The request is copied into C memory once per call, matching the
// plugin ABI ownership contract.
func goHostCall(method string, requestJSON []byte) ([]byte, error) {
	if len(requestJSON) > maxRPCBytes {
		return nil, fmt.Errorf("host callback request exceeds size limit")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest *C.uint8_t
	if len(requestJSON) > 0 {
		cRequest = (*C.uint8_t)(C.CBytes(requestJSON))
		defer C.free(unsafe.Pointer(cRequest))
	}
	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, cRequest, C.size_t(len(requestJSON)), &response)
	var responseBytes []byte
	if response.ptr != nil {
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if uint64(response.len) > maxRPCBytes || (response.ptr == nil && response.len > 0) {
		return nil, fmt.Errorf("invalid or oversized host callback response")
	}
	if response.ptr != nil && response.len > 0 {
		responseBytes = C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
	}
	if rc != 0 && responseBytes == nil {
		return nil, &ocError{Code: "host_call_failed", Message: fmt.Sprintf("host %s returned %d", method, rc)}
	}
	if responseBytes != nil {
		var env envelope
		if err := json.Unmarshal(responseBytes, &env); err == nil && env.Error != nil {
			return responseBytes, &ocError{Code: env.Error.Code, Message: env.Error.Message, HTTPStatus: env.Error.HTTPStatus}
		}
	}
	return responseBytes, nil
}

// goHostCallJson marshals a typed request and unmarshals the envelope result.
func goHostCallJson(method string, request any) (json.RawMessage, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	responseBytes, err := goHostCall(method, payload)
	if err != nil {
		return nil, err
	}
	return decodeHostResult(responseBytes)
}

func decodeHostResult(responseBytes []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(responseBytes, &env); err != nil {
		return nil, err
	}
	if !env.OK || env.Error != nil || len(env.Result) == 0 {
		return nil, fmt.Errorf("host callback did not return a successful result")
	}
	return env.Result, nil
}
