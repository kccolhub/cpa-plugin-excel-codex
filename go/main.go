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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginID = "excel-codex"

// pluginVersion is overridden by the release workflow so the binary metadata
// and the CPA release asset always use the same version.
var pluginVersion = "0.4.0"

var currentConfig atomic.Value
var homeBridgeCounter atomic.Uint64

var bridgeRuntime = struct {
	sync.Mutex
	bySource map[string]*bridgeRuntimeEntry
}{
	bySource: make(map[string]*bridgeRuntimeEntry),
}

type bridgeRuntimeEntry struct {
	Requests      uint64
	Successes     uint64
	Failures      uint64
	LastStatus    int
	LastError     string
	LastRequestAt time.Time
	LastSuccessAt time.Time
	LastFailureAt time.Time
}

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
	Enabled      bool   `yaml:"enabled"`
	BaseURL      string `yaml:"base_url"`
	AuthMode     string `yaml:"auth_mode"`
	AuthProvider string `yaml:"auth_provider"`
	AuthIndex    string `yaml:"auth_index"`
	AuthStrategy string `yaml:"auth_strategy"`
	APIKey       string `yaml:"api_key"`
	APIKeyEnv    string `yaml:"api_key_env"`
	AccountID    string `yaml:"account_id"`
}

const (
	authModeAuto    = "auto"
	authModeHome    = "home"
	authModeSidecar = "sidecar"

	authStrategySticky     = "sticky"
	authStrategyRoundRobin = "round_robin"
	authStrategyFirst      = "first"
)

type upstreamCredentials struct {
	Token         string
	AccountID     string
	AccountUserID string
	Source        string
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
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
	ManagementAPI         bool     `json:"management_api"`
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

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementResource `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementResource struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

type managementStatusPageData struct {
	GeneratedAt    time.Time
	Config         pluginConfig
	HomeAvailable  bool
	HomeError      string
	Accounts       []managementAccountRow
	Bridges        []managementBridgeRow
	SidecarSource  string
	TotalRequests  uint64
	TotalSuccesses uint64
	TotalFailures  uint64
	LastBridge     string
}

type managementBridgeRow struct {
	Source        string
	Requests      uint64
	Successes     uint64
	Failures      uint64
	LastStatus    int
	LastError     string
	LastRequestAt time.Time
}

type managementAccountRow struct {
	AuthIndex     string
	Name          string
	Label         string
	Email         string
	Status        string
	Disabled      bool
	Unavailable   bool
	RuntimeOnly   bool
	Selected      bool
	Requests      uint64
	Successes     uint64
	Failures      uint64
	LastStatus    int
	LastError     string
	LastRequestAt time.Time
	LastSuccessAt time.Time
	LastFailureAt time.Time
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
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistrationResponse{
			Resources: []managementResource{{
				Path:        "/status",
				Menu:        "Excel Codex Bridge",
				Description: "Shows the Excel Codex bridge runtime, Home account pool, and request counters.",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0, false), nil
	}
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:      true,
		BaseURL:      "http://127.0.0.1:8000",
		AuthMode:     authModeAuto,
		AuthProvider: "codex",
		AuthStrategy: authStrategySticky,
		APIKeyEnv:    "EXCEL_CODEX_API_KEY",
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
	cfg.AuthMode = strings.ToLower(strings.TrimSpace(cfg.AuthMode))
	cfg.AuthProvider = strings.ToLower(strings.TrimSpace(cfg.AuthProvider))
	cfg.AuthIndex = strings.TrimSpace(cfg.AuthIndex)
	cfg.AuthStrategy = strings.ToLower(strings.TrimSpace(cfg.AuthStrategy))
	cfg.AuthStrategy = strings.ReplaceAll(cfg.AuthStrategy, "-", "_")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.APIKeyEnv = strings.TrimSpace(cfg.APIKeyEnv)
	cfg.AccountID = strings.TrimSpace(cfg.AccountID)
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultPluginConfig().BaseURL
	}
	if cfg.AuthMode == "" {
		cfg.AuthMode = defaultPluginConfig().AuthMode
	}
	if cfg.AuthProvider == "" {
		cfg.AuthProvider = defaultPluginConfig().AuthProvider
	}
	switch cfg.AuthMode {
	case authModeAuto, authModeHome, authModeSidecar:
	default:
		return fmt.Errorf("auth_mode must be one of %q, %q, or %q", authModeAuto, authModeHome, authModeSidecar)
	}
	if cfg.AuthStrategy == "" {
		cfg.AuthStrategy = defaultPluginConfig().AuthStrategy
	}
	switch cfg.AuthStrategy {
	case authStrategySticky, authStrategyRoundRobin, authStrategyFirst:
	default:
		return fmt.Errorf("auth_strategy must be one of %q, %q, or %q", authStrategySticky, authStrategyRoundRobin, authStrategyFirst)
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
				{Name: "auth_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{authModeAuto, authModeHome, authModeSidecar}, Description: "Credential source: home reuses a CPA Home Codex credential, sidecar uses the bridge API key, and auto selects based on the endpoint."},
				{Name: "auth_provider", Type: pluginapi.ConfigFieldTypeString, Description: "Home credential provider to read when auth_mode is home or auto, normally codex."},
				{Name: "auth_index", Type: pluginapi.ConfigFieldTypeString, Description: "Optional exact Home auth_index. Set this when Home stores more than one Codex credential."},
				{Name: "auth_strategy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{authStrategySticky, authStrategyRoundRobin, authStrategyFirst}, Description: "Home multi-account bridge selection: sticky keeps a session on one account, round_robin distributes requests, and first keeps legacy single-account behavior."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Bearer key for the configured Excel bridge endpoint. Leave empty to read api_key_env."},
				{Name: "api_key_env", Type: pluginapi.ConfigFieldTypeString, Description: "Environment variable containing the bridge API key."},
				{Name: "account_id", Type: pluginapi.ConfigFieldTypeString, Description: "Optional account ID override forwarded as chatgpt-account-id."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai-response"},
			ExecutorOutputFormats: []string{"openai-response"},
			ManagementAPI:         true,
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
	credentials, errCredentials := credentialsForRequest(cfg, req.ExecutorRequest)
	if errCredentials != nil {
		return nil, &statusError{code: "credentials_unavailable", message: errCredentials.Error(), statusCode: 503, retryable: true}
	}
	body, errBody := requestBody(req.ExecutorRequest, false)
	if errBody != nil {
		return nil, errBody
	}
	resp, errHTTP := doHTTP(req.HostCallbackID, cfg, credentials, req.ExecutorRequest, body, false)
	if errHTTP != nil {
		recordBridgeResult(credentials.Source, false, statusCodeFromError(errHTTP), errHTTP.Error())
		return nil, errHTTP
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		recordBridgeResult(credentials.Source, false, resp.StatusCode, errorTextFromBody(resp.Body))
		return nil, upstreamStatusError(resp.StatusCode, resp.Body)
	}
	recordBridgeResult(credentials.Source, true, resp.StatusCode, "")
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
	credentials, errCredentials := credentialsForRequest(cfg, req.ExecutorRequest)
	if errCredentials != nil {
		return nil, &statusError{code: "credentials_unavailable", message: errCredentials.Error(), statusCode: 503, retryable: true}
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, fmt.Errorf("stream_id is required")
	}
	body, errBody := requestBody(req.ExecutorRequest, true)
	if errBody != nil {
		return nil, errBody
	}
	resp, errHTTP := doHTTPStream(req.HostCallbackID, cfg, credentials, req.ExecutorRequest, body)
	if errHTTP != nil {
		recordBridgeResult(credentials.Source, false, statusCodeFromError(errHTTP), errHTTP.Error())
		return nil, errHTTP
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, hostHTTPStreamCloseRequest{StreamID: resp.StreamID})
		recordBridgeResult(credentials.Source, false, resp.StatusCode, fmt.Sprintf("upstream stream returned HTTP %d", resp.StatusCode))
		return nil, upstreamStatusError(resp.StatusCode, nil)
	}
	go pumpStream(req.StreamID, resp.StreamID, credentials.Source, resp.StatusCode)
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

func doHTTP(callbackID string, cfg pluginConfig, credentials upstreamCredentials, req pluginapi.ExecutorRequest, body []byte, stream bool) (hostHTTPResponse, error) {
	var resp hostHTTPResponse
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            endpointURL(cfg.BaseURL),
		Headers:        requestHeaders(credentials, req.Headers, stream),
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

func doHTTPStream(callbackID string, cfg pluginConfig, credentials upstreamCredentials, req pluginapi.ExecutorRequest, body []byte) (hostHTTPStreamResponse, error) {
	var resp hostHTTPStreamResponse
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            endpointURL(cfg.BaseURL),
		Headers:        requestHeaders(credentials, req.Headers, true),
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

func pumpStream(outputID, upstreamID, source string, status int) {
	defer func() {
		_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, hostHTTPStreamCloseRequest{StreamID: upstreamID})
		_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID})
	}()
	for {
		raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, hostHTTPStreamReadRequest{StreamID: upstreamID})
		if errCall != nil {
			recordBridgeResult(source, false, statusCodeFromError(errCall), errCall.Error())
			_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID, Error: errCall.Error()})
			return
		}
		var chunk hostHTTPStreamReadResponse
		if errDecode := json.Unmarshal(raw, &chunk); errDecode != nil {
			recordBridgeResult(source, false, status, errDecode.Error())
			_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID, Error: errDecode.Error()})
			return
		}
		if len(chunk.Payload) > 0 {
			if _, errEmit := callHost(pluginabi.MethodHostStreamEmit, hostStreamEmitRequest{StreamID: outputID, Payload: chunk.Payload}); errEmit != nil {
				recordBridgeResult(source, false, statusCodeFromError(errEmit), errEmit.Error())
				return
			}
		}
		if chunk.Error != "" {
			recordBridgeResult(source, false, status, chunk.Error)
			_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: outputID, Error: chunk.Error})
			return
		}
		if chunk.Done {
			recordBridgeResult(source, true, status, "")
			return
		}
	}
}

func requestHeaders(credentials upstreamCredentials, inbound http.Header, stream bool) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	if credentials.Token != "" {
		headers.Set("Authorization", "Bearer "+credentials.Token)
	}
	if credentials.AccountID != "" {
		headers.Set("chatgpt-account-id", credentials.AccountID)
		headers.Set("x-openai-account-id", credentials.AccountID)
	}
	if credentials.AccountUserID != "" {
		headers.Set("x-openai-account-user-id", credentials.AccountUserID)
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

func credentialsForRequest(cfg pluginConfig, req pluginapi.ExecutorRequest) (upstreamCredentials, error) {
	switch cfg.AuthMode {
	case authModeHome:
		return loadHomeCredentials(cfg, req)
	case authModeSidecar:
		return sidecarCredentials(cfg)
	case authModeAuto:
		if isTrustedBPSBaseURL(cfg.BaseURL) {
			if credentials, errHome := loadHomeCredentials(cfg, req); errHome == nil {
				return credentials, nil
			}
		}
		if key := apiKey(cfg); key != "" {
			return upstreamCredentials{Token: key, AccountID: cfg.AccountID, Source: authModeSidecar}, nil
		}
		if isTrustedBPSBaseURL(cfg.BaseURL) {
			return loadHomeCredentials(cfg, req)
		}
		return upstreamCredentials{}, fmt.Errorf("auth_mode=auto needs a sidecar API key for %s, or set auth_mode=home with https://bps.openai.com/basispoints/api", cfg.BaseURL)
	default:
		return upstreamCredentials{}, fmt.Errorf("unsupported auth_mode %q", cfg.AuthMode)
	}
}

func sidecarCredentials(cfg pluginConfig) (upstreamCredentials, error) {
	key := apiKey(cfg)
	if key == "" {
		return upstreamCredentials{}, fmt.Errorf("sidecar API key is empty; configure api_key or api_key_env")
	}
	return upstreamCredentials{Token: key, AccountID: cfg.AccountID, Source: authModeSidecar}, nil
}

func loadHomeCredentials(cfg pluginConfig, req pluginapi.ExecutorRequest) (upstreamCredentials, error) {
	if !isTrustedBPSBaseURL(cfg.BaseURL) {
		return upstreamCredentials{}, fmt.Errorf("auth_mode=home only permits https://bps.openai.com/basispoints/api; use auth_mode=sidecar for excel-sub2api")
	}
	rawList, errList := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if errList != nil {
		return upstreamCredentials{}, fmt.Errorf("Home auth list unavailable: %w", errList)
	}
	var list hostAuthListResponse
	if errDecode := json.Unmarshal(rawList, &list); errDecode != nil {
		return upstreamCredentials{}, fmt.Errorf("decode Home auth list: %w", errDecode)
	}

	candidates := make([]pluginapi.HostAuthFileEntry, 0, len(list.Files))
	for index := range list.Files {
		entry := list.Files[index]
		provider := strings.TrimSpace(entry.Provider)
		if provider == "" {
			provider = strings.TrimSpace(entry.Type)
		}
		if cfg.AuthIndex != "" && entry.AuthIndex != cfg.AuthIndex {
			continue
		}
		if cfg.AuthProvider != "" && !strings.EqualFold(provider, cfg.AuthProvider) {
			continue
		}
		if entry.Disabled || entry.Unavailable || entry.RuntimeOnly || entry.AuthIndex == "" {
			continue
		}
		candidates = append(candidates, entry)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].AuthIndex < candidates[j].AuthIndex
	})
	selected, errSelect := selectHomeBridge(cfg, candidates, req)
	if errSelect != nil {
		return upstreamCredentials{}, errSelect
	}

	rawAuth, errGet := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: selected.AuthIndex})
	if errGet != nil {
		return upstreamCredentials{}, fmt.Errorf("read Home credential %s: %w", selected.AuthIndex, errGet)
	}
	var authFile pluginapi.HostAuthGetResponse
	if errDecode := json.Unmarshal(rawAuth, &authFile); errDecode != nil {
		return upstreamCredentials{}, fmt.Errorf("decode Home credential %s: %w", selected.AuthIndex, errDecode)
	}
	credentials, errExtract := extractHomeCredentials(authFile.JSON)
	if errExtract != nil {
		return upstreamCredentials{}, fmt.Errorf("Home credential %s: %w", selected.AuthIndex, errExtract)
	}
	if cfg.AccountID != "" {
		credentials.AccountID = cfg.AccountID
	}
	credentials.Source = authModeHome + ":" + selected.AuthIndex
	return credentials, nil
}

func selectHomeBridge(cfg pluginConfig, candidates []pluginapi.HostAuthFileEntry, req pluginapi.ExecutorRequest) (*pluginapi.HostAuthFileEntry, error) {
	if len(candidates) == 0 {
		if cfg.AuthIndex != "" {
			return nil, fmt.Errorf("Home auth_index %q was not found or is unavailable", cfg.AuthIndex)
		}
		return nil, fmt.Errorf("Home has no available %s credential", cfg.AuthProvider)
	}
	if cfg.AuthIndex != "" {
		for index := range candidates {
			if candidates[index].AuthIndex == cfg.AuthIndex {
				return &candidates[index], nil
			}
		}
		return nil, fmt.Errorf("Home auth_index %q was not found or is unavailable", cfg.AuthIndex)
	}
	if len(candidates) == 1 {
		return &candidates[0], nil
	}

	switch cfg.AuthStrategy {
	case authStrategyFirst:
		return &candidates[0], nil
	case authStrategyRoundRobin:
		return &candidates[nextHomeBridgeIndex(len(candidates))], nil
	case authStrategySticky:
		if key := requestAffinityKey(req); key != "" {
			return &candidates[bridgeIndexFromKey(key, len(candidates))], nil
		}
		return &candidates[nextHomeBridgeIndex(len(candidates))], nil
	default:
		return nil, fmt.Errorf("unsupported auth_strategy %q", cfg.AuthStrategy)
	}
}

func nextHomeBridgeIndex(count int) int {
	if count <= 1 {
		return 0
	}
	return int(homeBridgeCounter.Add(1)-1) % count
}

func bridgeIndexFromKey(key string, count int) int {
	if count <= 1 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(key))
	return int(hash.Sum64() % uint64(count))
}

func requestAffinityKey(req pluginapi.ExecutorRequest) string {
	for _, name := range []string{
		"x-codex-session-id",
		"x-codex-thread-id",
		"x-codex-parent-thread-id",
		"x-cliproxy-session-id",
		"x-session-id",
	} {
		if value := headerString(req.Headers, name); value != "" {
			return value
		}
	}
	for _, name := range []string{
		"session_id",
		"thread_id",
		"parent_thread_id",
		"canonical_session_id",
		"codex_session_id",
		"codex_thread_id",
		"lcp_session_id",
	} {
		if value := metadataString(req.Metadata, name); value != "" {
			return value
		}
	}
	for _, name := range []string{"session_id", "thread_id"} {
		if value := strings.TrimSpace(req.Query.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func headerString(headers http.Header, key string) string {
	for name, values := range headers {
		if !strings.EqualFold(name, key) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func metadataString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func extractHomeCredentials(raw []byte) (upstreamCredentials, error) {
	var document map[string]any
	if errDecode := json.Unmarshal(raw, &document); errDecode != nil {
		return upstreamCredentials{}, fmt.Errorf("invalid auth JSON: %w", errDecode)
	}
	token := firstString(document, "access_token")
	if token == "" {
		token = nestedString(document, "tokens", "access_token")
	}
	if token == "" {
		token = nestedString(document, "token", "access_token")
	}
	if token == "" {
		return upstreamCredentials{}, fmt.Errorf("no ChatGPT access_token")
	}
	accountID := firstString(document, "account_id")
	if accountID == "" {
		accountID = nestedString(document, "tokens", "account_id")
	}
	accountUserID := firstString(document, "account_user_id", "chatgpt_account_user_id")
	if accountUserID == "" {
		accountUserID = jwtClaimString(token, "chatgpt_account_user_id")
	}
	if accountID == "" {
		accountID = jwtClaimString(token, "chatgpt_account_id", "account_id")
	}
	if accountID == "" {
		accountID = jwtClaimString(firstString(document, "id_token"), "chatgpt_account_id", "account_id")
	}
	if accountID == "" {
		return upstreamCredentials{}, fmt.Errorf("no ChatGPT account_id")
	}
	return upstreamCredentials{
		Token:         token,
		AccountID:     accountID,
		AccountUserID: accountUserID,
	}, nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func nestedString(values map[string]any, objectKey, valueKey string) string {
	nested, ok := values[objectKey].(map[string]any)
	if !ok {
		return ""
	}
	return firstString(nested, valueKey)
}

func jwtClaimString(token string, keys ...string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		payload, errDecode = base64.URLEncoding.DecodeString(parts[1])
	}
	if errDecode != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if value := firstString(claims, keys...); value != "" {
		return value
	}
	if authClaims, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		return firstString(authClaims, keys...)
	}
	return ""
}

func isTrustedBPSBaseURL(base string) bool {
	parsed, errParse := url.Parse(strings.TrimSpace(base))
	if errParse != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "bps.openai.com") {
		return false
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	return path == "/basispoints/api"
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

func statusCodeFromError(err error) int {
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func errorTextFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if strings.TrimSpace(payload.Error.Message) != "" {
			return strings.TrimSpace(payload.Error.Message)
		}
		if strings.TrimSpace(payload.Message) != "" {
			return strings.TrimSpace(payload.Message)
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 240 {
		text = text[:240] + "…"
	}
	return text
}

func recordBridgeResult(source string, success bool, status int, errText string) {
	source = normalizeBridgeSource(source)
	now := time.Now().UTC()

	bridgeRuntime.Lock()
	defer bridgeRuntime.Unlock()
	entry := bridgeRuntime.bySource[source]
	if entry == nil {
		entry = &bridgeRuntimeEntry{}
		bridgeRuntime.bySource[source] = entry
	}
	entry.Requests++
	entry.LastStatus = status
	entry.LastRequestAt = now
	if success {
		entry.Successes++
		entry.LastError = ""
		entry.LastSuccessAt = now
		return
	}
	entry.Failures++
	entry.LastError = strings.TrimSpace(errText)
	entry.LastFailureAt = now
}

func normalizeBridgeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "unknown"
	}
	return source
}

func bridgeRuntimeSnapshot() map[string]bridgeRuntimeEntry {
	bridgeRuntime.Lock()
	defer bridgeRuntime.Unlock()
	out := make(map[string]bridgeRuntimeEntry, len(bridgeRuntime.bySource))
	for source, entry := range bridgeRuntime.bySource {
		if entry == nil {
			continue
		}
		out[source] = *entry
	}
	return out
}

func handleManagement(raw []byte) ([]byte, error) {
	var req rpcManagementRequest
	if len(raw) > 0 {
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, fmt.Errorf("decode management request: %w", errDecode)
		}
	}
	cfg := loadedConfig()
	data := collectManagementStatus(cfg)
	page := renderManagementStatusPage(data)
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       page,
	})
}

func collectManagementStatus(cfg pluginConfig) managementStatusPageData {
	data := managementStatusPageData{
		GeneratedAt:   time.Now().UTC(),
		Config:        cfg,
		SidecarSource: authModeSidecar,
	}
	stats := bridgeRuntimeSnapshot()
	for source, item := range stats {
		data.TotalRequests += item.Requests
		data.TotalSuccesses += item.Successes
		data.TotalFailures += item.Failures
		data.Bridges = append(data.Bridges, managementBridgeRow{
			Source:        source,
			Requests:      item.Requests,
			Successes:     item.Successes,
			Failures:      item.Failures,
			LastStatus:    item.LastStatus,
			LastError:     item.LastError,
			LastRequestAt: item.LastRequestAt,
		})
		if data.LastBridge == "" || item.LastRequestAt.After(stats[data.LastBridge].LastRequestAt) {
			data.LastBridge = source
		}
	}
	sort.SliceStable(data.Bridges, func(i, j int) bool {
		return data.Bridges[i].Source < data.Bridges[j].Source
	})

	rawList, errList := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if errList != nil {
		data.HomeError = errList.Error()
		return data
	}
	data.HomeAvailable = true
	var list hostAuthListResponse
	if errDecode := json.Unmarshal(rawList, &list); errDecode != nil {
		data.HomeError = fmt.Sprintf("decode Home auth list: %v", errDecode)
		return data
	}
	for _, entry := range availableHomeEntries(cfg, list.Files) {
		row := managementAccountRow{
			AuthIndex:   entry.AuthIndex,
			Name:        entry.Name,
			Label:       entry.Label,
			Email:       entry.Email,
			Status:      entry.Status,
			Disabled:    entry.Disabled,
			Unavailable: entry.Unavailable,
			RuntimeOnly: entry.RuntimeOnly,
			Selected:    cfg.AuthIndex != "" && entry.AuthIndex == cfg.AuthIndex,
		}
		if row.Status == "" {
			row.Status = "available"
		}
		if stat, ok := stats[authModeHome+":"+entry.AuthIndex]; ok {
			row.Requests = stat.Requests
			row.Successes = stat.Successes
			row.Failures = stat.Failures
			row.LastStatus = stat.LastStatus
			row.LastError = stat.LastError
			row.LastRequestAt = stat.LastRequestAt
			row.LastSuccessAt = stat.LastSuccessAt
			row.LastFailureAt = stat.LastFailureAt
		}
		data.Accounts = append(data.Accounts, row)
	}
	sort.SliceStable(data.Accounts, func(i, j int) bool {
		return data.Accounts[i].AuthIndex < data.Accounts[j].AuthIndex
	})
	return data
}

func availableHomeEntries(cfg pluginConfig, entries []pluginapi.HostAuthFileEntry) []pluginapi.HostAuthFileEntry {
	out := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		if provider == "" {
			provider = strings.TrimSpace(entry.Type)
		}
		if cfg.AuthProvider != "" && !strings.EqualFold(provider, cfg.AuthProvider) {
			continue
		}
		if cfg.AuthIndex != "" && entry.AuthIndex != cfg.AuthIndex {
			continue
		}
		if entry.AuthIndex == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func renderManagementStatusPage(data managementStatusPageData) []byte {
	var builder strings.Builder
	builder.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`)
	builder.WriteString(`<title>Excel Codex Bridge</title>`)
	builder.WriteString(`<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:24px;color:#dce6f0;background:#0d1824;line-height:1.45}a{color:#8ec5ff}.card{background:#132334;border:1px solid #294057;border-radius:14px;padding:18px;margin:0 0 16px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px}.metric{background:#0f1c2a;border:1px solid #24384d;border-radius:12px;padding:14px}.metric b{display:block;font-size:24px;color:#fff}.muted{color:#9db0c4}.ok{color:#73e2a7}.warn{color:#ffd166}.bad{color:#ff8b8b}table{width:100%;border-collapse:collapse;background:#0f1c2a;border-radius:12px;overflow:hidden}th,td{text-align:left;border-bottom:1px solid #24384d;padding:10px 12px;vertical-align:top}th{color:#b9c8d8;background:#16293c}code{background:#0b1420;border:1px solid #24384d;border-radius:6px;padding:1px 5px}.pill{display:inline-block;border-radius:999px;padding:2px 8px;background:#21374e;color:#dce6f0;font-size:12px}.error{white-space:pre-wrap;color:#ffb4b4}</style>`)
	builder.WriteString(`</head><body><main>`)
	builder.WriteString(`<h1>Excel Codex Bridge</h1>`)
	builder.WriteString(`<p class="muted">Generated at ` + escape(data.GeneratedAt.Format(time.RFC3339)) + ` · <a href="?">Refresh</a></p>`)

	builder.WriteString(`<section class="card"><h2>Runtime</h2><div class="grid">`)
	writeMetric(&builder, "Requests", data.TotalRequests)
	writeMetric(&builder, "Successes", data.TotalSuccesses)
	writeMetric(&builder, "Failures", data.TotalFailures)
	builder.WriteString(`<div class="metric"><span class="muted">Recently used</span><b>`)
	builder.WriteString(escape(displayBridgeSource(data.LastBridge)))
	builder.WriteString(`</b></div>`)
	builder.WriteString(`</div></section>`)

	if len(data.Bridges) > 0 {
		builder.WriteString(`<section class="card"><h2>Bridge sources</h2><table><thead><tr><th>Source</th><th>Requests</th><th>Last result</th><th>Last error</th></tr></thead><tbody>`)
		for _, row := range data.Bridges {
			builder.WriteString(`<tr><td><code>`)
			builder.WriteString(escape(displayBridgeSource(row.Source)))
			builder.WriteString(`</code></td><td>`)
			builder.WriteString(fmt.Sprintf(`%d total<br><span class="ok">%d ok</span> · <span class="bad">%d fail</span>`, row.Requests, row.Successes, row.Failures))
			builder.WriteString(`</td><td>`)
			builder.WriteString(escape(lastBridgeResultText(row)))
			builder.WriteString(`</td><td>`)
			builder.WriteString(escape(row.LastError))
			builder.WriteString(`</td></tr>`)
		}
		builder.WriteString(`</tbody></table></section>`)
	}

	builder.WriteString(`<section class="card"><h2>Config</h2><table><tbody>`)
	writeKV(&builder, "enabled", fmt.Sprint(data.Config.Enabled))
	writeKV(&builder, "auth_mode", data.Config.AuthMode)
	writeKV(&builder, "base_url", data.Config.BaseURL)
	writeKV(&builder, "auth_provider", data.Config.AuthProvider)
	writeKV(&builder, "auth_strategy", data.Config.AuthStrategy)
	writeKV(&builder, "auth_index", data.Config.AuthIndex)
	builder.WriteString(`</tbody></table></section>`)

	builder.WriteString(`<section class="card"><h2>Home bridge accounts</h2>`)
	if data.HomeError != "" {
		builder.WriteString(`<p class="error">Home auth list unavailable: ` + escape(data.HomeError) + `</p>`)
	} else if len(data.Accounts) == 0 {
		builder.WriteString(`<p class="muted">No matching Home Codex account is currently available for this plugin config.</p>`)
	} else {
		builder.WriteString(`<table><thead><tr><th>Account</th><th>Status</th><th>Requests</th><th>Last result</th><th>Last error</th></tr></thead><tbody>`)
		for _, row := range data.Accounts {
			builder.WriteString(`<tr><td>`)
			builder.WriteString(escape(accountLabel(row)))
			builder.WriteString(`<br><code>`)
			builder.WriteString(escape(row.AuthIndex))
			builder.WriteString(`</code>`)
			if row.Selected {
				builder.WriteString(` <span class="pill">pinned</span>`)
			}
			builder.WriteString(`</td><td>`)
			builder.WriteString(statusHTML(row))
			builder.WriteString(`</td><td>`)
			builder.WriteString(fmt.Sprintf("%d total<br><span class=\"ok\">%d ok</span> · <span class=\"bad\">%d fail</span>", row.Requests, row.Successes, row.Failures))
			builder.WriteString(`</td><td>`)
			builder.WriteString(escape(lastResultText(row)))
			builder.WriteString(`</td><td>`)
			builder.WriteString(escape(row.LastError))
			builder.WriteString(`</td></tr>`)
		}
		builder.WriteString(`</tbody></table>`)
	}
	builder.WriteString(`</section>`)

	builder.WriteString(`<section class="card"><h2>What this page means</h2><p class="muted">Home mode treats each available Codex credential as one internal Excel bridge. <code>sticky</code> keeps the same session on the same account; requests without session metadata fall back to round-robin. This page never reads or displays tokens.</p></section>`)
	builder.WriteString(`</main></body></html>`)
	return []byte(builder.String())
}

func writeMetric(builder *strings.Builder, label string, value uint64) {
	builder.WriteString(`<div class="metric"><span class="muted">`)
	builder.WriteString(escape(label))
	builder.WriteString(`</span><b>`)
	builder.WriteString(fmt.Sprint(value))
	builder.WriteString(`</b></div>`)
}

func writeKV(builder *strings.Builder, key, value string) {
	if strings.TrimSpace(value) == "" {
		value = "—"
	}
	builder.WriteString(`<tr><th>`)
	builder.WriteString(escape(key))
	builder.WriteString(`</th><td><code>`)
	builder.WriteString(escape(value))
	builder.WriteString(`</code></td></tr>`)
}

func accountLabel(row managementAccountRow) string {
	for _, value := range []string{row.Label, row.Email, row.Name} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return row.AuthIndex
}

func statusHTML(row managementAccountRow) string {
	className := "ok"
	text := row.Status
	if row.Disabled {
		className = "bad"
		text = "disabled"
	} else if row.Unavailable {
		className = "bad"
		text = "unavailable"
	} else if row.RuntimeOnly {
		className = "warn"
		text = "runtime-only"
	}
	return `<span class="` + className + `">` + escape(text) + `</span>`
}

func lastResultText(row managementAccountRow) string {
	if row.LastRequestAt.IsZero() {
		return "No requests yet"
	}
	parts := []string{row.LastRequestAt.Format(time.RFC3339)}
	if row.LastStatus > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", row.LastStatus))
	}
	return strings.Join(parts, " · ")
}

func lastBridgeResultText(row managementBridgeRow) string {
	if row.LastRequestAt.IsZero() {
		return "No requests yet"
	}
	parts := []string{row.LastRequestAt.Format(time.RFC3339)}
	if row.LastStatus > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", row.LastStatus))
	}
	return strings.Join(parts, " · ")
}

func displayBridgeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "—"
	}
	if strings.HasPrefix(source, authModeHome+":") {
		return "home " + strings.TrimPrefix(source, authModeHome+":")
	}
	return source
}

func escape(value string) string {
	return html.EscapeString(value)
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
