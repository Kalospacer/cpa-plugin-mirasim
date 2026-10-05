package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

const claudeIdentityBody = `{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// 非 Claude Code 调用方：清掉调用方 SDK 指纹，整体换成 CPA 默认画像。
func TestClaudeCodeIdentityReplacesAForeignClient(t *testing.T) {
	headers := http.Header{
		"User-Agent":         {"Go-http-client/1.1"},
		"X-Stainless-Lang":   {"python"},
		"X-Stainless-Async":  {"async:asyncio"},
		"X-Claude-Code-Fake": {"spoofed"},
		"Anthropic-Beta":     {"tools-2024"},
	}
	applyClaudeCodeIdentity(headers, []byte(claudeIdentityBody), false, "acct-foreign")

	want := map[string]string{
		"User-Agent":                                claudeCodeUserAgent,
		"X-Stainless-Package-Version":               claudeCodePackageVersion,
		"X-Stainless-Runtime-Version":               claudeCodeRuntimeVersion,
		"X-Stainless-Os":                            claudeCodeOS,
		"X-Stainless-Arch":                          claudeCodeArch,
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Timeout":                       claudeCodeTimeout,
		"X-App":                                     "cli",
		"Anthropic-Version":                         "2023-06-01",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"Anthropic-Beta":                            "tools-2024",
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{"X-Stainless-Async", "X-Claude-Code-Fake"} {
		if headers.Get(name) != "" {
			t.Errorf("%s from a non-Claude Code caller must be dropped", name)
		}
	}
	session := headers.Get(claudeCodeSessionHeader)
	if len(session) != 36 {
		t.Fatalf("session id = %q", session)
	}
	again := http.Header{}
	applyClaudeCodeIdentity(again, []byte(claudeIdentityBody), false, "acct-foreign")
	if again.Get(claudeCodeSessionHeader) != session {
		t.Fatal("one account must keep one session id within the process")
	}
	other := http.Header{}
	applyClaudeCodeIdentity(other, []byte(claudeIdentityBody), false, "acct-other")
	if other.Get(claudeCodeSessionHeader) == session {
		t.Fatal("different accounts must not share a session id")
	}
}

// 真实 Claude Code 调用方：保留其自带的身份值，只补缺失项。
func TestClaudeCodeIdentityKeepsANativeClaudeCodeCaller(t *testing.T) {
	headers := http.Header{
		"User-Agent":                  {"claude-cli/2.1.284 (external, cli)"},
		"X-Stainless-Os":              {"Windows"},
		"X-Stainless-Arch":            {"x64"},
		"X-Stainless-Package-Version": {"0.113.0"},
		"X-Stainless-Runtime-Version": {"not-a-version"},
		"X-Stainless-Async":           {"async"},
		"X-Claude-Code-Session-Id":    {"caller-session"},
	}
	applyClaudeCodeIdentity(headers, []byte(claudeIdentityBody), false, "acct-native")

	want := map[string]string{
		"User-Agent":                  "claude-cli/2.1.284 (external, cli)",
		"X-Stainless-Os":              "Windows",
		"X-Stainless-Arch":            "x64",
		"X-Stainless-Package-Version": "0.113.0",
		"X-Stainless-Runtime-Version": claudeCodeRuntimeVersion,
		"X-Stainless-Async":           "async",
		"X-Claude-Code-Session-Id":    "caller-session",
		"X-App":                       "cli",
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestClaudeCodeIdentityLeavesOtherModelsAlone(t *testing.T) {
	for _, body := range []string{
		`{"model":"glm-5.2","messages":[]}`,
		`{"model":"deepseek-v4","messages":[]}`,
		`{"model":"gpt-6-luna","input":"hi"}`,
	} {
		headers := http.Header{"User-Agent": {"Go-http-client/1.1"}}
		applyClaudeCodeIdentity(headers, []byte(body), false, "acct")
		if len(headers) != 1 || headers.Get("User-Agent") != "Go-http-client/1.1" {
			t.Fatalf("%s: headers = %#v", body, headers)
		}
	}
}

// 计 token 请求走真实调用链：发出的请求带 Claude Code 身份，但不带超时头。
func TestCountTokensSendsClaudeCodeIdentity(t *testing.T) {
	storage := executorTestStorage(t)
	var sent http.Header
	host := executorHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		parsed, errParse := url.Parse(req.URL)
		if errParse != nil {
			return pluginapi.HTTPResponse{}, errParse
		}
		switch parsed.Path {
		case "/v1/device/session":
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ticket":"ticket","expiresIn":900}`)}, nil
		case "/v1/messages/count_tokens":
			sent = req.Headers.Clone()
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"input_tokens":5}`)}, nil
		default:
			return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected path %s", parsed.Path)
		}
	}}
	_, errCount := New(pluginconfig.Defaults(), mirasim.NewPool()).CountTokens(context.Background(), pluginapi.ExecutorRequest{
		Model:        "claude-sonnet-5-5",
		Format:       sdktranslator.FormatClaude.String(),
		SourceFormat: sdktranslator.FormatClaude.String(),
		Payload:      []byte(claudeIdentityBody),
		Headers:      http.Header{"User-Agent": {"Go-http-client/1.1"}},
		StorageJSON:  storage.JSON(),
		HTTPClient:   host,
	})
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if sent.Get("User-Agent") != claudeCodeUserAgent || sent.Get("X-App") != "cli" {
		t.Fatalf("count_tokens headers = %#v", sent)
	}
	if sent.Get("X-Stainless-Timeout") != "" {
		t.Fatalf("Claude Code sends no timeout on count_tokens, got %q", sent.Get("X-Stainless-Timeout"))
	}
}

// 直通请求同样覆盖 Messages 路径。
func TestHttpRequestSendsClaudeCodeIdentityOnMessages(t *testing.T) {
	storage := executorTestStorage(t)
	var sent http.Header
	host := executorHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		parsed, errParse := url.Parse(req.URL)
		if errParse != nil {
			return pluginapi.HTTPResponse{}, errParse
		}
		switch parsed.Path {
		case "/v1/device/session":
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ticket":"ticket","expiresIn":900}`)}, nil
		case "/v1/messages":
			sent = req.Headers.Clone()
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
		default:
			return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected path %s", parsed.Path)
		}
	}}
	_, errRequest := New(pluginconfig.Defaults(), mirasim.NewPool()).HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
		Method:      http.MethodPost,
		URL:         "https://relay.example/v1/messages",
		Headers:     http.Header{"User-Agent": {"curl/8.0"}},
		Body:        []byte(claudeIdentityBody),
		StorageJSON: storage.JSON(),
		HTTPClient:  host,
	})
	if errRequest != nil {
		t.Fatalf("HttpRequest() error = %v", errRequest)
	}
	if sent.Get("User-Agent") != claudeCodeUserAgent || sent.Get("X-Stainless-Timeout") != claudeCodeTimeout {
		t.Fatalf("messages headers = %#v", sent)
	}
}
