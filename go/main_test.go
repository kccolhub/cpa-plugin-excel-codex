package main

import (
	"net/http"
	"strings"
	"testing"

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
	headers := requestHeaders(pluginConfig{APIKey: "bridge-key", AccountID: "acct"}, inbound, true)
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
