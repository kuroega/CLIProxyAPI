package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; host_call_fn call; host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*plugin_free_fn)(void*, size_t);
typedef void (*plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; plugin_call_fn call; plugin_free_fn free_buffer; plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int workbuddyCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void workbuddyFree(void*, size_t);
extern void workbuddyShutdown(void);
static const cliproxy_host_api* host_api;
static void set_host_api(const cliproxy_host_api* h) { host_api = h; }
static int call_host(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* resp) {
    if (!host_api || !host_api->call) return 1;
    return host_api->call(host_api->host_ctx, method, req, len, resp);
}
static void free_host(void* ptr, size_t len) {
    if (host_api && host_api->free_buffer && ptr) host_api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  struct {
		ModelRegistrar        bool     `json:"model_registrar"`
		ModelProvider         bool     `json:"model_provider"`
		AuthProvider          bool     `json:"auth_provider"`
		Executor              bool     `json:"executor"`
		ExecutorModelScope    string   `json:"executor_model_scope"`
		ExecutorInputFormats  []string `json:"executor_input_formats"`
		ExecutorOutputFormats []string `json:"executor_output_formats"`
	} `json:"capabilities"`
}
type rpcRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id"`
}
type hostRequest struct {
	pluginapi.HTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type hostStream struct {
	StatusCode int    `json:"status_code"`
	StreamID   string `json:"stream_id"`
}
type hostChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	C.set_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.plugin_call_fn(C.workbuddyCall)
	plugin.free_buffer = C.plugin_free_fn(C.workbuddyFree)
	plugin.shutdown = C.plugin_shutdown_fn(C.workbuddyShutdown)
	return 0
}

//export workbuddyCall
func workbuddyCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	if method == nil {
		writeResponse(response, fail(errors.New("method is required")))
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := dispatch(C.GoString(method), raw, hostCall)
	if err != nil {
		result = fail(err)
	}
	writeResponse(response, result)
	return 0
}

//export workbuddyFree
func workbuddyFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export workbuddyShutdown
func workbuddyShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}
func success(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: data})
}
func fail(err error) []byte {
	status := 0
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		status = upstream.status
	}
	data, _ := json.Marshal(envelope{Error: &envelopeError{Code: "workbuddy_error", Message: err.Error(), HTTPStatus: status}})
	return data
}
func hostCall(method string, request any, result any) error {
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode callback: %w", err)
	}
	cm := C.CString(method)
	defer C.free(unsafe.Pointer(cm))
	var resp C.cliproxy_buffer
	var ptr *C.uint8_t
	if len(data) > 0 {
		p := C.CBytes(data)
		defer C.free(p)
		ptr = (*C.uint8_t)(p)
	}
	code := C.call_host(cm, ptr, C.size_t(len(data)), &resp)
	var raw []byte
	if resp.ptr != nil && resp.len > 0 {
		raw = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil {
		C.free_host(resp.ptr, resp.len)
	}
	var env envelope
	if len(raw) == 0 || json.Unmarshal(raw, &env) != nil {
		return fmt.Errorf("host callback %s failed (code %d)", method, int(code))
	}
	if !env.OK || code != 0 {
		if env.Error != nil {
			return fmt.Errorf("host callback %s: %s", method, env.Error.Message)
		}
		return fmt.Errorf("host callback %s failed", method)
	}
	if result != nil {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("decode %s: %w", method, err)
		}
	}
	return nil
}
func dispatch(method string, raw []byte, call callback) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		reg := registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: "WorkBuddy", Version: "0.1.0", Author: "CLIProxyAPI", GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI", ConfigFields: []pluginapi.ConfigField{}}}
		reg.Capabilities.ModelRegistrar, reg.Capabilities.ModelProvider, reg.Capabilities.AuthProvider, reg.Capabilities.Executor = true, true, true, true
		reg.Capabilities.ExecutorModelScope = "both"
		reg.Capabilities.ExecutorInputFormats = []string{"chat-completions"}
		reg.Capabilities.ExecutorOutputFormats = []string{"chat-completions"}
		return success(reg)
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return success(map[string]string{"identifier": provider})
	case pluginabi.MethodModelRegister, pluginabi.MethodModelStatic:
		return success(pluginapi.ModelRegistrationResponse{Provider: provider, Models: catalog("all")})
	case pluginabi.MethodModelForAuth:
		return modelsForAuth(raw, call)
	case pluginabi.MethodAuthParse:
		return parseAuth(raw)
	case pluginabi.MethodAuthLoginStart:
		return startLogin(raw, call)
	case pluginabi.MethodAuthLoginPoll:
		return pollLogin(raw, call)
	case pluginabi.MethodAuthRefresh:
		return refreshAuth(raw, call)
	case pluginabi.MethodExecutorExecute:
		return execute(raw, call, false)
	case pluginabi.MethodExecutorExecuteStream:
		return execute(raw, call, true)
	case pluginabi.MethodExecutorCountTokens:
		return success(pluginapi.ExecutorResponse{Payload: []byte(`{"total_tokens":0}`)})
	case pluginabi.MethodExecutorHTTPRequest:
		return nil, errors.New("arbitrary HTTP forwarding is not supported")
	default:
		return nil, fmt.Errorf("unsupported method %s", strings.TrimSpace(method))
	}
}
func httpHeaders(pairs map[string]string) http.Header {
	h := make(http.Header)
	for k, v := range pairs {
		h.Set(k, v)
	}
	return h
}
