package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

type parallelToolCallsHost struct {
	executorHostClient
}

func (h parallelToolCallsHost) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	response, errDo := h.Do(ctx, req)
	if errDo != nil {
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: response.Body}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: response.StatusCode, Headers: response.Headers, Chunks: chunks}, nil
}

// The Responses-to-Codex translation is written for the first-party Codex
// backend, which requires parallel_tool_calls to be true, so it rewrites
// whatever the caller sent. Mirasim's relay refuses a request that carries the
// Codex Responses-Lite marker or an input-level additional_tools item unless
// that field is explicitly false, and Codex sends exactly that request stating
// false. The caller's own value therefore has to survive the translation, and a
// caller that says nothing must not have a value invented for it.
func TestCodexTranslationKeepsCallerParallelToolCalls(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		present bool
		want    bool
	}{
		{name: "false", field: `"parallel_tool_calls":false,`, present: true, want: false},
		{name: "true", field: `"parallel_tool_calls":true,`, present: true, want: true},
		{name: "absent", present: false},
	}
	for _, entry := range []string{"execute", "stream"} {
		for _, testCase := range cases {
			t.Run(entry+"/"+testCase.name, func(t *testing.T) {
				payload := []byte(`{` + testCase.field +
					`"model":"gpt-6-luna","store":false,"stream":true,` +
					`"include":["reasoning.encrypted_content"],"input":[` +
					`{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"alpha",` +
					`"tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}]}]},` +
					`{"role":"user","content":"hi"}]}`)
				var sent map[string]any
				host := parallelToolCallsHost{executorHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					parsed, errParse := url.Parse(req.URL)
					if errParse != nil {
						return pluginapi.HTTPResponse{}, errParse
					}
					switch parsed.Path {
					case "/v1/device/session":
						return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"ticket":"ticket","expiresIn":900}`)}, nil
					case "/v1/responses":
						if errDecode := json.Unmarshal(req.Body, &sent); errDecode != nil {
							return pluginapi.HTTPResponse{}, errDecode
						}
						return pluginapi.HTTPResponse{
							StatusCode: 200,
							Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
							Body:       []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"),
						}, nil
					default:
						return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected path %s", parsed.Path)
					}
				}}}
				storage := executorTestStorage(t)
				exec := New(pluginconfig.Defaults(), mirasim.NewPool())
				req := pluginapi.ExecutorRequest{
					Model:           "gpt-6-luna",
					SourceFormat:    sdktranslator.FormatOpenAIResponse.String(),
					Format:          sdktranslator.FormatCodex.String(),
					Payload:         payload,
					OriginalRequest: payload,
					StorageJSON:     storage.JSON(),
					HTTPClient:      host,
				}
				switch entry {
				case "execute":
					if _, errExecute := exec.Execute(context.Background(), req); errExecute != nil {
						t.Fatal(errExecute)
					}
				case "stream":
					response, errStream := exec.ExecuteStream(context.Background(), req)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				}
				value, present := sent["parallel_tool_calls"]
				if present != testCase.present {
					t.Fatalf("parallel_tool_calls present = %v, want %v", present, testCase.present)
				}
				if testCase.present && value != testCase.want {
					t.Fatalf("parallel_tool_calls = %v, want %v", value, testCase.want)
				}
				input, ok := sent["input"].([]any)
				if !ok || len(input) != 2 {
					t.Fatalf("input was rewritten: %v", sent["input"])
				}
				if first, _ := input[0].(map[string]any); first["type"] != "additional_tools" {
					t.Fatalf("additional_tools item was rewritten: %v", input[0])
				}
			})
		}
	}
}
