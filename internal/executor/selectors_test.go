package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	thinkingpkg "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/thinking"
	"github.com/tidwall/gjson"
	"net/http"
	"strings"
	"testing"
)

func TestLongContextSelectorWithThinking(t *testing.T) {
	req := pluginapi.ExecutorRequest{Model: "mirasim/claude-sonnet-5[1m](high)", SourceFormat: "claude", Payload: []byte(`{"model":"claude-sonnet-5[1m]","max_tokens":4096,"messages":[]}`), Headers: http.Header{"Anthropic-Beta": []string{"other-beta"}}}
	body, route, err := buildProviderRequest(req, false, thinkingpkg.ShapeUnknown, executorTestClientVersion)
	if err != nil || gjson.GetBytes(body, "model").String() != "claude-sonnet-5" || gjson.GetBytes(body, "output_config.effort").String() != "high" {
		t.Fatalf("body=%s err=%v", body, err)
	}
	headers := requestHeaders(req, route.Format)
	addLongContextBeta(headers)
	if strings.Count(headers.Get("Anthropic-Beta"), "context-1m-2025-08-07") != 1 || !strings.Contains(headers.Get("Anthropic-Beta"), "other-beta") {
		t.Fatalf("headers=%v", headers)
	}
	if req.Headers.Get("Anthropic-Beta") != "other-beta" {
		t.Fatal("caller headers mutated")
	}
	raw, err := normalizeHTTPRequestBody(req.Payload, "claude-sonnet-5[1m]", route.Format, thinkingpkg.ShapeUnknown)
	if err != nil || gjson.GetBytes(raw, "model").String() != "claude-sonnet-5" {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
}
