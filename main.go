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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const abiVersion uint32 = 1

// host callback methods used by this plugin.
const (
	methodHostStreamEmit  = "host.stream.emit"
	methodHostStreamClose = "host.stream.close"
	methodHostAuthList    = "host.auth.list"
	methodHostAuthGet     = "host.auth.get"
	methodHostLog         = "host.log"
)

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

// pluginError carries the HTTP status the host should return to the client.
type pluginError struct {
	code    string
	status  int
	message string
}

func (e *pluginError) Error() string { return e.message }

func failf(code string, status int, format string, args ...any) *pluginError {
	return &pluginError{code: code, status: status, message: fmt.Sprintf(format, args...)}
}

var (
	streamWG     sync.WaitGroup
	shutdownOnce sync.Once
	// streams holds the cancel func of every live relay so shutdown can abort
	// them. The host dlcloses this shared library as soon as shutdown returns,
	// so returning with a pump still inside it would segfault the process.
	streamsMu sync.Mutex
	streams   = make(map[string]context.CancelFunc)
)

func trackStream(id string, cancel context.CancelFunc) {
	streamsMu.Lock()
	streams[id] = cancel
	streamsMu.Unlock()
}

func untrackStream(id string) {
	streamsMu.Lock()
	delete(streams, id)
	streamsMu.Unlock()
}

func cancelAllStreams() {
	streamsMu.Lock()
	pending := make([]context.CancelFunc, 0, len(streams))
	for _, cancel := range streams {
		pending = append(pending, cancel)
	}
	streamsMu.Unlock()
	for _, cancel := range pending {
		cancel()
	}
}

func main() {}

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

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelopeJSON("invalid_method", "method is required", 0))
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := dispatch(C.GoString(method), payload)
	if err != nil {
		code, status := "plugin_error", 0
		var pluginErr *pluginError
		if errors.As(err, &pluginErr) {
			code, status = pluginErr.code, pluginErr.status
		}
		writeResponse(response, errorEnvelopeJSON(code, err.Error(), status))
		return 1
	}
	raw, errEncode := okEnvelopeJSON(result)
	if errEncode != nil {
		writeResponse(response, errorEnvelopeJSON("encode_failed", errEncode.Error(), 0))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	shutdownOnce.Do(func() {
		cancelAllStreams()
		done := make(chan struct{})
		go func() {
			streamWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
}

func dispatch(method string, request []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registerPlugin(request)
	case "plugin.shutdown":
		cliproxyPluginShutdown()
		return nil, nil
	case "model.static", "model.for_auth":
		return staticModels()
	case "auth.login.start", "auth.login.poll":
		return nil, failf("unsupported", 0, "zcode 使用长期 API Key，没有交互式登录流程；把凭据文件放进 auth-dir 即可")
	case "auth.identifier", "executor.identifier":
		return identifierResponse{Identifier: providerID}, nil
	case "auth.parse":
		return parseAuth(request)
	case "auth.refresh":
		return refreshAuth(request)
	case "executor.execute":
		return execute(request)
	case "executor.execute_stream":
		return executeStream(request)
	case "executor.count_tokens":
		return countTokens(request)
	case "executor.http_request":
		return nil, failf("unsupported", 0, "zcode plugin does not serve executor.http_request")
	case "management.register":
		return registerManagement()
	case "management.handle":
		return handleManagement(request)
	default:
		return nil, failf("unknown_method", 0, "unknown method: %s", method)
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func okEnvelopeJSON(result any) ([]byte, error) {
	raw := json.RawMessage("{}")
	if result != nil {
		encoded, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			return nil, errMarshal
		}
		raw = encoded
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelopeJSON(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code:       code,
		Message:    message,
		HTTPStatus: status,
	}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// hostCall invokes a host callback and decodes its result envelope into out.
func hostCall(method string, payload any, out any) error {
	var body []byte
	if payload != nil {
		encoded, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return errMarshal
		}
		body = encoded
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cBody *C.uint8_t
	if len(body) > 0 {
		cBody = (*C.uint8_t)(C.CBytes(body))
		defer C.free(unsafe.Pointer(cBody))
	}
	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, cBody, C.size_t(len(body)), &response)
	var raw []byte
	if response.ptr != nil && response.len > 0 {
		raw = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return fmt.Errorf("host call %s failed: %s", method, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	var env envelope
	if errDecode := json.Unmarshal(raw, &env); errDecode != nil {
		return fmt.Errorf("decode host response %s: %w", method, errDecode)
	}
	if !env.OK {
		message := "host call failed"
		if env.Error != nil && strings.TrimSpace(env.Error.Message) != "" {
			message = env.Error.Message
		}
		return fmt.Errorf("%s: %s", method, message)
	}
	if len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// hostLog writes one plugin log line into the host log stream.
func hostLog(message string) {
	_ = hostCall(methodHostLog, map[string]any{"level": "info", "message": message}, nil)
}
