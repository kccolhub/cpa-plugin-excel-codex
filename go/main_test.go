package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestEndpointURL(t *testing.T) {
	tests := map[string]string{
		"http://bridge":                          "http://bridge/v1/responses",
		"http://bridge/":                         "http://bridge/v1/responses",
		"http://bridge/v1":                       "http://bridge/v1/responses",
		"http://bridge/v1/responses":             "http://bridge/v1/responses",
		"https://bps.openai.com/basispoints/api": "https://bps.openai.com/basispoints/api/responses",
	}
	for input, want := range tests {
		if got := endpointURL(input); got != want {
			t.Fatalf("endpointURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRequestBodyAddsModelAndStream(t *testing.T) {
	body, err := requestBody(pluginapi.ExecutorRequest{
		Model:           "gpt-5.6-sol-excel",
		Payload:         []byte(`{"input":"hello"}`),
		OriginalRequest: []byte(`{"model":"wrong"}`),
	}, true)
	if err != nil {
		t.Fatalf("requestBody() error = %v", err)
	}
	text := string(body)
	for _, want := range []string{`"model":"gpt-5.6-sol-excel"`, `"stream":true`, `"input":"hello"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("request body %s does not contain %s", text, want)
		}
	}
}

func TestRequestHeadersDoNotForwardInboundAuthorization(t *testing.T) {
	inbound := http.Header{"Authorization": []string{"Bearer client-key"}}
	headers := requestHeaders(upstreamCredentials{Token: "bridge-key", AccountID: "acct"}, inbound, true)
	if got := headers.Get("Authorization"); got != "Bearer bridge-key" {
		t.Fatalf("authorization = %q, want bridge key", got)
	}
	if got := headers.Get("chatgpt-account-id"); got != "acct" {
		t.Fatalf("chatgpt-account-id = %q, want acct", got)
	}
	if got := headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("accept = %q, want event stream", got)
	}
}

func TestRequestHeadersIncludeHomeAccountUserID(t *testing.T) {
	headers := requestHeaders(upstreamCredentials{
		Token:         "home-token",
		AccountID:     "acct",
		AccountUserID: "user",
	}, nil, false)
	if got := headers.Get("Authorization"); got != "Bearer home-token" {
		t.Fatalf("authorization = %q, want home token", got)
	}
	if got := headers.Get("x-openai-account-user-id"); got != "user" {
		t.Fatalf("x-openai-account-user-id = %q, want user", got)
	}
}

func TestExtractHomeCredentialsTopLevel(t *testing.T) {
	got, err := extractHomeCredentials([]byte(`{"type":"codex","access_token":"home-token","account_id":"acct"}`))
	if err != nil {
		t.Fatalf("extractHomeCredentials() error = %v", err)
	}
	if got.Token != "home-token" || got.AccountID != "acct" {
		t.Fatalf("credentials = %#v, want top-level token and account", got)
	}
}

func TestExtractHomeCredentialsFromNestedJWTClaims(t *testing.T) {
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-jwt","chatgpt_account_user_id":"user-jwt"}}`
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	got, err := extractHomeCredentials([]byte(`{"type":"codex","access_token":"` + token + `"}`))
	if err != nil {
		t.Fatalf("extractHomeCredentials() error = %v", err)
	}
	if got.AccountID != "acct-jwt" || got.AccountUserID != "user-jwt" {
		t.Fatalf("credentials = %#v, want JWT claims", got)
	}
}

func TestHomeModeOnlyAllowsOfficialBPSEndpoint(t *testing.T) {
	if !isTrustedBPSBaseURL("https://bps.openai.com/basispoints/api") {
		t.Fatal("official BPS endpoint was rejected")
	}
	for _, baseURL := range []string{
		"http://bps.openai.com/basispoints/api",
		"https://bps.openai.com.evil.example/basispoints/api",
		"https://excel-sub2api:8000",
		"https://bps.openai.com/other",
	} {
		if isTrustedBPSBaseURL(baseURL) {
			t.Fatalf("untrusted Home endpoint accepted: %s", baseURL)
		}
	}
}

func TestExcelModelsAdvertiseBothContextWindows(t *testing.T) {
	models := excelModels()
	if len(models) != 12 {
		t.Fatalf("model count = %d, want 12", len(models))
	}
	seenLong := false
	for _, model := range models {
		if strings.HasSuffix(model.ID, "-1m-excel") {
			seenLong = true
			if model.ContextLength != 918000 {
				t.Fatalf("long model %s context = %d, want 918000", model.ID, model.ContextLength)
			}
		}
	}
	if !seenLong {
		t.Fatal("no long-context Excel model advertised")
	}
}

func TestSelectHomeBridgeStickyUsesSessionAffinity(t *testing.T) {
	candidates := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "account-a"},
		{AuthIndex: "account-b"},
		{AuthIndex: "account-c"},
	}
	cfg := pluginConfig{AuthStrategy: authStrategySticky, AuthProvider: "codex"}
	req := pluginapi.ExecutorRequest{
		Headers: http.Header{"X-Codex-Session-ID": []string{"session-123"}},
	}
	first, err := selectHomeBridge(cfg, candidates, req)
	if err != nil {
		t.Fatalf("selectHomeBridge() error = %v", err)
	}
	second, err := selectHomeBridge(cfg, candidates, req)
	if err != nil {
		t.Fatalf("selectHomeBridge() second error = %v", err)
	}
	if first.AuthIndex == "" || first.AuthIndex != second.AuthIndex {
		t.Fatalf("sticky bridge indexes = %q and %q, want the same account", first.AuthIndex, second.AuthIndex)
	}
}

func TestSelectHomeBridgeRoundRobinCyclesAccounts(t *testing.T) {
	candidates := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "account-a"},
		{AuthIndex: "account-b"},
	}
	cfg := pluginConfig{AuthStrategy: authStrategyRoundRobin, AuthProvider: "codex"}
	homeBridgeCounter.Store(0)
	first, err := selectHomeBridge(cfg, candidates, pluginapi.ExecutorRequest{})
	if err != nil {
		t.Fatalf("selectHomeBridge() first error = %v", err)
	}
	second, err := selectHomeBridge(cfg, candidates, pluginapi.ExecutorRequest{})
	if err != nil {
		t.Fatalf("selectHomeBridge() second error = %v", err)
	}
	third, err := selectHomeBridge(cfg, candidates, pluginapi.ExecutorRequest{})
	if err != nil {
		t.Fatalf("selectHomeBridge() third error = %v", err)
	}
	if first.AuthIndex != "account-a" || second.AuthIndex != "account-b" || third.AuthIndex != "account-a" {
		t.Fatalf("round robin indexes = %q, %q, %q", first.AuthIndex, second.AuthIndex, third.AuthIndex)
	}
}

func TestSelectHomeBridgeHonorsExplicitAuthIndex(t *testing.T) {
	candidates := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "account-a"},
		{AuthIndex: "account-b"},
	}
	cfg := pluginConfig{AuthIndex: "account-b", AuthStrategy: authStrategyFirst, AuthProvider: "codex"}
	selected, err := selectHomeBridge(cfg, candidates, pluginapi.ExecutorRequest{})
	if err != nil {
		t.Fatalf("selectHomeBridge() error = %v", err)
	}
	if selected.AuthIndex != "account-b" {
		t.Fatalf("selected auth index = %q, want account-b", selected.AuthIndex)
	}
}

func TestRequestAffinityKeyPrefersSessionHeadersThenMetadataThenQuery(t *testing.T) {
	req := pluginapi.ExecutorRequest{
		Headers: http.Header{"X-Codex-Session-ID": []string{"header-session"}},
		Query:   url.Values{"session_id": []string{"query-session"}},
		Metadata: map[string]any{
			"session_id": "metadata-session",
		},
	}
	if got := requestAffinityKey(req); got != "header-session" {
		t.Fatalf("requestAffinityKey() = %q, want header-session", got)
	}
	req.Headers = nil
	if got := requestAffinityKey(req); got != "metadata-session" {
		t.Fatalf("requestAffinityKey() = %q, want metadata-session", got)
	}
	req.Metadata = nil
	if got := requestAffinityKey(req); got != "query-session" {
		t.Fatalf("requestAffinityKey() = %q, want query-session", got)
	}
}

func TestRenderManagementStatusPageShowsRuntimeAndDoesNotExposeTokens(t *testing.T) {
	token := "secret-token-should-not-appear"
	page := renderManagementStatusPage(managementStatusPageData{
		GeneratedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		Config: pluginConfig{
			Enabled:      true,
			BaseURL:      "https://bps.openai.com/basispoints/api",
			AuthMode:     authModeHome,
			AuthProvider: "codex",
			AuthStrategy: authStrategySticky,
		},
		HomeAvailable: true,
		Accounts: []managementAccountRow{{
			AuthIndex:     "account-a",
			Label:         "A@example.com",
			Status:        "active",
			Requests:      3,
			Successes:     2,
			Failures:      1,
			LastStatus:    429,
			LastError:     "quota exceeded",
			LastRequestAt: time.Date(2026, 9, 30, 11, 59, 0, 0, time.UTC),
		}},
		Bridges: []managementBridgeRow{{
			Source:        "sidecar",
			Requests:      1,
			Successes:     1,
			LastStatus:    200,
			LastRequestAt: time.Date(2026, 9, 30, 11, 58, 0, 0, time.UTC),
		}},
		TotalRequests:  3,
		TotalSuccesses: 2,
		TotalFailures:  1,
		LastBridge:     "home:account-a",
	})
	body := string(page)
	for _, want := range []string{"Excel Codex Bridge", "sticky", "A@example.com", "quota exceeded", "429", "Bridge sources", "sidecar"} {
		if !strings.Contains(body, want) {
			t.Fatalf("status page does not contain %q", want)
		}
	}
	if strings.Contains(body, token) {
		t.Fatal("status page leaked a credential token")
	}
}

func TestRecordBridgeResultTracksCountersByHomeAuthIndex(t *testing.T) {
	bridgeRuntime.Lock()
	bridgeRuntime.bySource = make(map[string]*bridgeRuntimeEntry)
	bridgeRuntime.Unlock()
	recordBridgeResult("home:account-a", true, 200, "")
	recordBridgeResult("home:account-a", false, 429, "quota exceeded")
	snapshot := bridgeRuntimeSnapshot()
	entry, ok := snapshot["home:account-a"]
	if !ok {
		t.Fatal("missing home bridge runtime entry")
	}
	if entry.Requests != 2 || entry.Successes != 1 || entry.Failures != 1 {
		t.Fatalf("runtime counters = %#v, want 2/1/1", entry)
	}
	if entry.LastStatus != 429 || entry.LastError != "quota exceeded" {
		t.Fatalf("runtime last result = %#v, want HTTP 429 quota exceeded", entry)
	}
}

func TestManagementRegistrationDeclaresStatusResource(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("management registration error = %v", err)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode management registration envelope: %v", err)
	}
	if !envelope.OK {
		t.Fatal("management registration returned a failed envelope")
	}
	var registration struct {
		Resources []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode management registration: %v", err)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/status" {
		t.Fatalf("resources = %#v, want /status", registration.Resources)
	}
	if registration.Resources[0].Menu == "" || registration.Resources[0].Description == "" {
		t.Fatalf("status resource metadata is incomplete: %#v", registration.Resources[0])
	}
	var hostRegistration struct {
		Resources []pluginapi.ResourceRoute `json:"resources"`
	}
	if err := json.Unmarshal(envelope.Result, &hostRegistration); err != nil {
		t.Fatalf("decode host resource schema: %v", err)
	}
	if len(hostRegistration.Resources) != 1 || hostRegistration.Resources[0].Path != "/status" {
		t.Fatalf("host resources = %#v, want /status", hostRegistration.Resources)
	}
}
