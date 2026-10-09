package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestReportedReasoningUsageUsesOnlyExplicitCounters(t *testing.T) {
	for _, counter := range []string{"", `,"output_tokens_details":{"reasoning_tokens":0}`, `,"output_tokens_details":{"thinking_tokens":2}`} {
		var measured reportedReasoningUsage
		measured.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":3` + counter + `}}`))
		wire := []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10,\"output_tokens_details\":{\"reasoning_tokens\":999}}}}\r\n\r\n")
		fixed := measured.apply(wire, sdktranslator.FormatOpenAIResponse)
		for _, line := range bytes.Split(fixed, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			count := gjson.GetBytes(payload, "response.usage.output_tokens_details.reasoning_tokens")
			if count.Exists() != measured.reported || measured.reported && count.Int() != measured.tokens {
				t.Fatalf("fabricated reasoning usage: %s", fixed)
			}
			if gjson.GetBytes(payload, "response.usage.output_tokens").Int() != 3 || gjson.GetBytes(payload, "response.usage.total_tokens").Int() != 10 {
				t.Fatalf("authoritative counters changed: %s", fixed)
			}
		}
		if !bytes.HasSuffix(fixed, []byte("\r\n\r\n")) {
			t.Fatalf("SSE framing changed: %q", fixed)
		}
	}
}

func TestMessagesTranslationDoesNotEstimateReasoningFromText(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
		for _, counter := range []string{"", `,"output_tokens_details":{"reasoning_tokens":1}`} {
			message := []byte(`{"id":"msg_usage","type":"message","role":"assistant","model":"kimi-k3","content":[{"type":"thinking","thinking":"` + strings.Repeat("thought ", 100) + `"},{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3` + counter + `}}`)
			sse := claudeMessageAsSSE(message)
			nonstream, err := translateNonStream(context.Background(), sdktranslator.FormatClaude, format, "kimi-k3", []byte(`{}`), []byte(`{}`), sse)
			if err != nil {
				t.Fatal(err)
			}
			field := "usage.output_tokens_details.reasoning_tokens"
			if format == sdktranslator.FormatOpenAI {
				field = "usage.completion_tokens_details.reasoning_tokens"
			}
			count := gjson.GetBytes(nonstream, field)
			if count.Exists() != (counter != "") || counter != "" && count.Int() != 1 {
				t.Fatalf("nonstream fabricated count for %s: %s", format, nonstream)
			}
			stream, failures := collectProtocolStream(context.Background(), sdktranslator.FormatClaude, format,
				pluginapi.HTTPStreamChunk{Payload: sse[:len(sse)/2]}, pluginapi.HTTPStreamChunk{Payload: sse[len(sse)/2:]})
			if len(failures) != 0 {
				t.Fatalf("stream failures: %v", failures)
			}
			for _, line := range bytes.Split(stream, []byte("\n")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
				parent, countPath := "usage", field
				if gjson.GetBytes(payload, "response").IsObject() {
					parent, countPath = "response.usage", "response."+field
				}
				if !gjson.GetBytes(payload, parent).IsObject() {
					continue
				}
				count := gjson.GetBytes(payload, countPath)
				if count.Exists() != (counter != "") || counter != "" && count.Int() != 1 {
					t.Fatalf("stream fabricated reasoning for %s: %s", format, payload)
				}
			}
		}
	}
}

func TestNativeProtocolReasoningUsageIsNotRewritten(t *testing.T) {
	payload := []byte(`{"usage":{"output_tokens_details":{"reasoning_tokens":17},"output_tokens":20}}`)
	var unknown reportedReasoningUsage
	if got := unknown.apply(payload, sdktranslator.FormatClaude); !bytes.Equal(got, payload) {
		t.Fatalf("native Messages changed: %s", got)
	}
}
