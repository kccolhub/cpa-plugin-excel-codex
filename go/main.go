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
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginID = "excel-codex"

// pluginVersion is overridden by the release workflow so the binary metadata
// and the CPA release asset always use the same version.
var pluginVersion = "0.1.0"

var currentConfig atomic.Value

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type statusError struct {
	code       string
	message    string
	statusCode int
	retryable  bool
}

func (e *statusError) Error() string   { return e.message }
func (e *statusError) StatusCode() int { return e.statusCode }

type pluginConfig struct {
	Enabled   bool   `yaml:"enabled"`
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"`
	APIKeyEnv string `yaml:"api_key_env"`
	AccountID string `yaml:"account_id"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool     `json:"model_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`
}

type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type hostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type hostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
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
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 400, false))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		code := "plugin_error"
		status := 0
		retryable := false
		var sc interface{ StatusCode() int }
		if errors.As(errHandle, &sc) && sc != nil {
			status = sc.StatusCode()
		}
		if se, ok := errHandle.(*statusError); ok {
			if se.code != "" {
				code = se.code
			}
			retryable = se.retryable
		}
		writeResponse(response, errorEnvelope(code, errHandle.Error(), status, retryable))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: excelModels()})
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0, false), nil
	}
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:   true,
		BaseURL:   "http://127.0.0.1:8000",
		APIKeyEnv: "EXCEL_CODEX_API_KEY",
	}
}

func configure(raw []byte) error {
	req := lifecycleRequest{}
	if len(raw) > 0 {
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return fmt.Errorf("decode lifecycle request: %w", errDecode)
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
			return fmt.Errorf("decode plugin config: %w", errDecode)
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.APIKeyEnv = strings.TrimSpace(cfg.APIKeyEnv)
	cfg.AccountID = strings.TrimSpace(cfg.AccountID)
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultPluginConfig().BaseURL
	}
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = defaultPluginConfig().APIKeyEnv
	}
	currentConfig.Store(cfg)
	return nil
}

func loadedConfig() pluginConfig {
	if raw := currentConfig.Load(); raw != nil {
		if cfg, ok := raw.(pluginConfig); ok {
			return cfg
		}
	}
	return defaultPluginConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Excel Codex",
			Version:          pluginVersion,
			Author:           "kccolhub",
			GitHubRepository: "https://github.com/kccolhub/cpa-plugin-excel-codex",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable Excel Codex routing."},
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Excel bridge base URL, for example http://excel-sub2api:8000 or https://bps.openai.com/basispoints/api."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Bearer key for the configured Excel bridge endpoint. Leave empty to read api_key_env."},
				{Name: "api_key_env", Type: pluginapi.ConfigFieldTypeString, Description: "Environment variable containing the bridge API key."},
				{Name: "account_id", Type: pluginapi.ConfigFieldTypeString, Description: "Optional ChatGPT account ID forwarded as chatgpt-account-id."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai-response"},
			ExecutorOutputFormats: []string{"openai-response"},
		},
	}
}

func excelModels() []pluginapi.ModelInfo {
	base := []struct {
		id      string
		name    string
		context int64
	}{
		{"gpt-5.6-luna-excel", "5.6-Luna Excel", 500000},
		{"gpt-5.6-terra-excel", "5.6-Terra Excel", 500000},
		{"gpt-5.6-sol-excel", "5.6-Sol Excel", 500000},
		{"gpt-6-sol-excel", "6-Sol Excel", 500000},
		{"gpt-6-luna-excel", "6-Luna Excel", 500000},
		{"gpt-6-astra-excel", "6-Astra Excel", 500000},
	}
	models := make([]pluginapi.ModelInfo, 0, len(base)*2)
	for _, item := range base {
		models = append(models, modelInfo(item.id, item.name, item.context))
		models = append(models, modelInfo(strings.TrimSuffix(item.id, "-excel")+"-1m-excel", item.name+" 1M", 918000))
	}
	return models
}

func modelInfo(id, displayName string, context int64) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    "openai-excel",
		DisplayName:                displayName,
		Description:                "OpenAI Excel add-in backend through the CPA Excel Codex plugin.",
		InputTokenLimit:            context,
		OutputTokenLimit:           32768,
		ContextLength:              context,
		MaxCompletionTokens:        32768,
		SupportedGenerationMethods: []string{"responses", "chat"},
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		UserDefined:                true,
	}
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
		return nil, fmt.Errorf("decode executor request: %w", errDecode)
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, &statusError{code: "plugin_disabled", message: "Excel Codex plugin is disabled", statusCode: 503, retryable: false}
	}
	body, errBody := requestBody(req.ExecutorRequest, false)
	if errBody != nil {
		return nil, errBody
	}
	resp, errHTTP := doHTTP(req.HostCallbackID, cfg, req.ExecutorRequest, body, false)
	if errHTTP != nil {
		return nil, errHTTP
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamStatusError(resp.StatusCode, resp.Body)
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
		return nil, fmt.Errorf("decode executor stream request: %w", errDecode)
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, &statusError{code: "plugin_disabled", message: "Excel Codex plugin is disabled", statusCode: 503, retryable: false}
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, fmt.Errorf("stream_id is required")
	}
	body, errBody := requestBody(req.ExecutorRequest, true)
	if errBody != nil {
		return nil, errBody
	}
	resp, errHTTP := doHTTPStream(req.HostCallbackID, cfg, req.ExecutorRequest, body)
	if errHTTP != nil {
		return nil, errHTTP
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, hostHTTPStreamCloseRequest{StreamID: resp.StreamID})
		return nil, upstreamStatusError(resp.StatusCode, nil)
	}
	go pumpStream(req.StreamID, resp.StreamID)
	return okEnvelope(rpcExecutorStreamResponse{Headers: resp.Headers})
}

func requestBody(req pluginapi.ExecutorRequest, stream bool) ([]byte, error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	if len(body) == 0 {
		return nil, &statusError{code: "invalid_request", message: "executor payload is empty", statusCode: 400, retryable: false}
	}
	var object map[string]any
	if errDecode := json.Unmarshal(body, &object); errDecode != nil {
		return nil, &statusError{code: "invalid_request", message: "executor payload is not valid JSON", statusCode: 400, retryable: false}
	}
	if _, ok := object["model"]; !ok && strings.TrimSpace(req.Model) != "" {
		object["model"] = req.Model
	}
	object["stream"] = stream
	return json.Marshal(object)
}

func doHTTP(callbackID string, cfg pluginConfig, req pluginapi.ExecutorRequest, body []byte, stream bool) (hostHTTPResponse, error) {
	var resp hostHTTPResponse
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            endpointURL(cfg.BaseURL),
		Headers:        requestHeaders(cfg, req.Headers, stream),
		Body:           body,
	})
	if errCall != nil {
		return resp, &statusError{code: "upstream_request_failed", message: errCall.Error(), statusCode: 502, retryable: true}
	}
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return resp, fmt.Errorf("decode host HTTP response: %w", errDecode)
	}
	return resp, nil
}

func doHTTPStream(callbackID string, cfg pluginConfig, req pluginapi.ExecutorRequest, body []byte) (hostHTTPStreamResponse, error) {
	var resp hostHTTPStreamResponse
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            endpointURL(cfg.BaseURL),
		Headers:        requestHeaders(cfg, req.Headers, true),
		Body:           body,
	})
	if errCall != nil {
		return resp, &statusError{code: "upstream_request_failed", message: errCall.Error(), statusCode: 502, retryable: true}
	}
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return resp, fmt.Errorf("decode host HTTP stream response: %w", errDecode)
	}
	if resp.StreamID == "" {
		return resp, fmt.Errorf("host HTTP stream returned no stream ID")
	}
	return resp, nil
}

func pumpStream(outputID, upstreamID string) {
	defer func() {
		_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, hostHTTPStreamCloseRequest{StreamID: upstreamID})
		_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID})
	}()
	for {
		raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, hostHTTPStreamReadRequest{StreamID: upstreamID})
		if errCall != nil {
			_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID, Error: errCall.Error()})
			return
		}
		var chunk hostHTTPStreamReadResponse
		if errDecode := json.Unmarshal(raw, &chunk); errDecode != nil {
			_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID, Error: errDecode.Error()})
			return
		}
		if len(chunk.Payload) > 0 {
			if _, errEmit := callHost(pluginabi.MethodHostStreamEmit, hostStreamEmitRequest{StreamID: outputID, Payload: chunk.Payload}); errEmit != nil {
				return
			}
		}
		if chunk.Error != "" {
			_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID, Error: chunk.Error})
			return
		}
		if chunk.Done {
			return
		}
	}
}

func requestHeaders(cfg pluginConfig, inbound http.Header, stream bool) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	if key := apiKey(cfg); key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	if cfg.AccountID != "" {
		headers.Set("chatgpt-account-id", cfg.AccountID)
		headers.Set("x-openai-account-id", cfg.AccountID)
	}
	// These headers select the same backend profile used by the reference bridge.
	defaults := map[string]string{
		"x-basispoints-auth-mode":                            "chatgpt",
		"x-openai-internal-basispoints-client-agent-profile": "excel",
		"x-openai-internal-basispoints-client-editor":        "excel",
		"x-openai-internal-basispoints-client-host":          "office",
		"x-openai-internal-basispoints-client-platform":      "excel",
		"x-openai-internal-basispoints-client-product":       "basispoints-excel-plugin",
		"x-openai-internal-basispoints-client-runtime":       "desktop",
		"x-openai-internal-basispoints-office-host":          "Excel",
		"x-openai-internal-basispoints-office-platform":      "PC",
	}
	for key, value := range defaults {
		headers.Set(key, value)
	}
	_ = inbound // The CPA client API key must never be forwarded to the bridge.
	return headers
}

func apiKey(cfg pluginConfig) string {
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	if cfg.APIKeyEnv != "" {
		return strings.TrimSpace(os.Getenv(cfg.APIKeyEnv))
	}
	return ""
}

func endpointURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/responses"
	}
	if strings.HasSuffix(base, "/basispoints/api") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

func upstreamStatusError(status int, body []byte) error {
	code := "upstream_error"
	message := fmt.Sprintf("Excel bridge returned HTTP %d", status)
	if len(body) > 0 {
		var payload struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &payload) == nil {
			if payload.Error.Code != "" {
				code = payload.Error.Code
			}
			if payload.Error.Message != "" {
				message = payload.Error.Message
			} else if payload.Message != "" {
				message = payload.Message
			}
		}
	}
	return &statusError{code: code, message: message, statusCode: status, retryable: status == 429 || status >= 500}
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if errDecode := json.Unmarshal(rawResponse, &env); errDecode != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errDecode)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func okEnvelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code: code, Message: message, HTTPStatus: status, Retryable: retryable,
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
