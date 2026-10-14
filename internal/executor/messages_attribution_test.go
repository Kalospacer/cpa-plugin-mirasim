package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
	thinkingpkg "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestMessagesRequestDropsChangingClaudeAttribution(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "mirasim/gemini-4-argon(high)", "gemini-3.1-pro-preview", "deepseek-flash", "mirasim/deepseek-flash(high)", "glm-5.3-flash", "mirasim/glm-5.3-flash(high)", "kimi-k3", "mirasim/kimi-code/k3(high)"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				var previous []byte
				for _, fingerprint := range []string{"11111", "22222"} {
					payload := []byte(fmt.Sprintf(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.abc; cch=%s;"},{"type":"text","text":"Keep this instruction.","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","description":"Keep this tool.","input_schema":{"type":"object","properties":{}}}],"max_tokens":65536}`, fingerprint))
					body, route, errBuild := buildProviderRequest(pluginapi.ExecutorRequest{
						Model: model, SourceFormat: "claude", Format: "claude", Payload: payload,
					}, stream, thinkingpkg.ShapeUnknown, executorTestClientVersion)
					if errBuild != nil || route.Path != "/v1/messages" {
						t.Fatalf("route=%+v err=%v", route, errBuild)
					}
					if got := gjson.GetBytes(body, "system.0.text").String(); got != "Keep this instruction." {
						t.Fatalf("system prefix contains billing attribution: %s", body)
					}
					if gjson.GetBytes(body, "system.0.cache_control.ttl").String() != "1h" || gjson.GetBytes(body, "tools.0.description").String() != "Keep this tool." || gjson.GetBytes(body, "messages.0.content").String() != "hello" {
						t.Fatalf("caller instructions or cache controls changed: %s", body)
					}
					if previous != nil && !bytes.Equal(previous, body) {
						t.Fatalf("per-request billing fingerprint changed the model prefix:\n%s\n%s", previous, body)
					}
					previous = body
				}
			})
		}
	}
}

func TestMessagesSystemShapesAcrossRequestPaths(t *testing.T) {
	cases := []struct {
		name, system, want string
	}{
		{"blocks", `[{"type":"text","text":"x-anthropic-billing-header: cch=11111;"},{"type":"text","text":"first","cache_control":{"type":"ephemeral"}},{"type":"text","text":"second"}]`, `[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}},{"type":"text","text":"second"}]`},
		{"only attribution", `[{"type":"text","text":"x-anthropic-billing-header: cch=11111;"}]`, ""},
		{"string attribution", `"x-anthropic-billing-header: cch=11111;"`, ""},
		{"string with instructions", `"x-anthropic-billing-header: cch=11111;\nKeep the next line."`, `"Keep the next line."`},
		{"block with instructions", `[{"type":"text","text":"x-anthropic-billing-header: cch=11111;\r\nKeep the next line.","cache_control":{"type":"ephemeral"}}]`, `[{"type":"text","text":"Keep the next line.","cache_control":{"type":"ephemeral"}}]`},
		{"leading whitespace", `[{"type":"text","text":" \tx-anthropic-billing-header: cch=11111;"},{"type":"text","text":"keep"}]`, `[{"type":"text","text":"keep"}]`},
		{"ordinary string", `"Explain x-anthropic-billing-header: without changing this instruction."`, `"Explain x-anthropic-billing-header: without changing this instruction."`},
		{"no attribution", `[{"type":"text","text":"keep","cache_control":{"type":"ephemeral"}}]`, `[{"type":"text","text":"keep","cache_control":{"type":"ephemeral"}}]`},
	}
	for _, model := range []string{"gemini-4-argon", "deepseek-flash", "glm-5.3-flash", "kimi-k3"} {
		for _, test := range cases {
			for _, httpRequest := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/http=%t", model, test.name, httpRequest), func(t *testing.T) {
					payload := []byte(`{"system":` + test.system + `,"messages":[{"role":"user","content":"Keep literal x-anthropic-billing-header: cch=11111; in this message."}],"max_tokens":65536}`)
					normalize := func(input []byte) ([]byte, error) {
						if httpRequest {
							return normalizeHTTPRequestBody(input, model, sdktranslator.FormatClaude, thinkingpkg.ShapeUnknown)
						}
						return normalizeBody(input, model, false, sdktranslator.FormatClaude)
					}
					body, errNormalize := normalize(payload)
					if errNormalize != nil {
						t.Fatal(errNormalize)
					}
					if got := gjson.GetBytes(body, "system").Raw; !equalAttributionJSON(got, test.want) {
						t.Fatalf("system=%s, want %s", got, test.want)
					}
					if got := gjson.GetBytes(body, "messages.0.content").String(); got != gjson.GetBytes(payload, "messages.0.content").String() {
						t.Fatalf("ordinary message text changed: %s", body)
					}
					again, errAgain := normalize(body)
					if errAgain != nil || !bytes.Equal(again, body) {
						t.Fatalf("normalization is not idempotent: %s, err=%v", again, errAgain)
					}
				})
			}
		}
	}
}

func equalAttributionJSON(left, right string) bool {
	if left == right {
		return true
	}
	var a, b any
	return json.Unmarshal([]byte(left), &a) == nil && json.Unmarshal([]byte(right), &b) == nil && reflect.DeepEqual(a, b)
}

func TestMessagesAttributionCleanupLeavesOtherModelsUntouched(t *testing.T) {
	const payload = `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.abc; cch=11111;","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Keep this instruction."}],"messages":[{"role":"user","content":"hello"}]}`
	for _, model := range []string{"claude-sonnet-5-5", "gpt-6-sol", "custom-model"} {
		for _, httpRequest := range []bool{false, true} {
			var body []byte
			var errNormalize error
			if httpRequest {
				body, errNormalize = normalizeHTTPRequestBody([]byte(payload), model, sdktranslator.FormatClaude, thinkingpkg.ShapeUnknown)
			} else {
				body, errNormalize = normalizeBody([]byte(payload), model, false, sdktranslator.FormatClaude)
			}
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			if got := gjson.GetBytes(body, "system").Raw; !equalAttributionJSON(got, gjson.Get(payload, "system").Raw) {
				t.Fatalf("%s system changed: %s", model, body)
			}
		}
	}
}

func TestMessagesCrossProtocolRequestsDropBillingAttribution(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "deepseek-flash", "glm-5.3-flash", "kimi-k3"} {
		for format, payload := range map[sdktranslator.Format]string{
			sdktranslator.FormatOpenAI:         `{"messages":[{"role":"system","content":"x-anthropic-billing-header: cch=11111;"},{"role":"system","content":"Keep this instruction."},{"role":"user","content":"hello"}]}`,
			sdktranslator.FormatOpenAIResponse: `{"instructions":"x-anthropic-billing-header: cch=11111;\nKeep this instruction.","input":"hello"}`,
		} {
			body, _, errBuild := buildProviderRequest(pluginapi.ExecutorRequest{
				Model: model, SourceFormat: format.String(), Format: format.String(), Payload: []byte(payload),
			}, true, thinkingpkg.ShapeUnknown, executorTestClientVersion)
			if errBuild != nil {
				t.Fatal(errBuild)
			}
			system := gjson.GetBytes(body, "system").Raw
			if strings.Contains(system, claudeAttributionPrefix) || !strings.Contains(system, "Keep this instruction.") {
				t.Fatalf("%s/%s attribution cleanup lost instructions: %s", model, format, body)
			}
		}
	}
}

func TestMessagesAttributionCleanupReachesEveryExecutorEntry(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "deepseek-flash", "glm-5.3-flash", "kimi-k3"} {
		for _, entry := range []string{"execute", "stream", "count", "http"} {
			t.Run(model+"/"+entry, func(t *testing.T) {
				storage := executorTestStorage(t)
				var captured []byte
				host := attributionHostClient{executorHostClient: executorHostClient{do: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					parsed, _ := url.Parse(request.URL)
					if parsed.Path == "/v1/device/session" {
						return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"ticket":"ticket","expiresIn":900}`)}, nil
					}
					captured = bytes.Clone(request.Body)
					body := []byte(`{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`)
					if parsed.Path == "/v1/messages/count_tokens" {
						body = []byte(`{"input_tokens":7}`)
					}
					return pluginapi.HTTPResponse{StatusCode: 200, Body: body}, nil
				}}, capture: &captured}
				executor := New(pluginconfig.Defaults(), mirasim.NewPool())
				payload := []byte(fmt.Sprintf(`{"model":%q,"system":[{"type":"text","text":"x-anthropic-billing-header: cch=11111;"},{"type":"text","text":"Keep this instruction.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}],"max_tokens":65536}`, model))
				request := pluginapi.ExecutorRequest{Model: model, SourceFormat: "claude", Format: "claude", Payload: payload, OriginalRequest: payload, StorageJSON: storage.JSON(), HTTPClient: host}
				ctx := context.Background()
				var err error
				switch entry {
				case "execute":
					_, err = executor.Execute(ctx, request)
				case "count":
					_, err = executor.CountTokens(ctx, request)
				case "http":
					_, err = executor.HttpRequest(ctx, pluginapi.ExecutorHTTPRequest{Method: http.MethodPost, URL: "https://relay.example/v1/messages", Body: payload, StorageJSON: storage.JSON(), HTTPClient: host})
				case "stream":
					var response pluginapi.ExecutorStreamResponse
					response, err = executor.ExecuteStream(ctx, request)
					if err == nil {
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(captured, "system.0.text").String(); got != "Keep this instruction." || gjson.GetBytes(captured, "system.#").Int() != 1 {
					t.Fatalf("%s kept billing attribution or lost instructions: %s", entry, captured)
				}
			})
		}
	}
}

type attributionHostClient struct {
	executorHostClient
	capture *[]byte
}

func (c attributionHostClient) DoStream(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	*c.capture = bytes.Clone(request.Body)
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"message_stop\"}\n\n")}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: 200, Chunks: chunks}, nil
}
