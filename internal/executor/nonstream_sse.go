package executor

import (
	"bytes"
	"encoding/json"
	"strings"
)

// claudeMessageAsSSE rewrites a non-streaming Claude Messages document
// ({"type":"message","content":[...]}) into the SSE event sequence CLIProxyAPI's
// Claude -> OpenAI non-streaming transformer expects.
//
// That transformer only reads lines prefixed with "data:", so handing it the
// plain JSON document the relay answers a non-streaming request with yields an
// empty completion: every line fails the prefix check and the accumulated
// content stays empty. Emitting the event sequence here keeps the wire shape
// the transformer was written against.
//
// A payload that is already SSE, or that is not a Claude message object, is
// returned unchanged.
func claudeMessageAsSSE(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return payload
	}
	var message struct {
		ID      string            `json:"id"`
		Type    string            `json:"type"`
		Role    string            `json:"role"`
		Model   string            `json:"model"`
		Content []json.RawMessage `json:"content"`
		Stop    string            `json:"stop_reason"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(trimmed, &message); errUnmarshal != nil || message.Type != "message" {
		return payload
	}

	role := message.Role
	if role == "" {
		role = "assistant"
	}
	usage := message.Usage
	if len(usage) == 0 || string(bytes.TrimSpace(usage)) == "null" {
		usage = json.RawMessage(`{"input_tokens":0,"output_tokens":0}`)
	}

	var out bytes.Buffer
	write := func(event map[string]any) {
		encoded, errMarshal := json.Marshal(event)
		if errMarshal != nil {
			return
		}
		out.WriteString("data: ")
		out.Write(encoded)
		out.WriteString("\n\n")
	}

	write(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": message.ID, "type": "message", "role": role,
			"model": message.Model, "content": []any{}, "usage": usage,
		},
	})

	for index, raw := range message.Content {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		}
		if errUnmarshal := json.Unmarshal(raw, &block); errUnmarshal != nil {
			continue
		}
		switch block.Type {
		case "thinking":
			start := map[string]any{"type": "thinking", "thinking": ""}
			if block.Signature != "" {
				start["signature"] = block.Signature
			}
			write(map[string]any{"type": "content_block_start", "index": index, "content_block": start})
			write(map[string]any{"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": block.Thinking}})
			if block.Signature != "" {
				write(map[string]any{"type": "content_block_delta", "index": index,
					"delta": map[string]any{"type": "signature_delta", "signature": block.Signature}})
			}
			write(map[string]any{"type": "content_block_stop", "index": index})
		case "text":
			write(map[string]any{"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "text", "text": ""}})
			write(map[string]any{"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": block.Text}})
			write(map[string]any{"type": "content_block_stop", "index": index})
		case "tool_use":
			write(map[string]any{"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": map[string]any{}}})
			arguments := strings.TrimSpace(string(block.Input))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			write(map[string]any{"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}})
			write(map[string]any{"type": "content_block_stop", "index": index})
		}
	}

	write(map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": message.Stop, "stop_sequence": nil},
		"usage": usage})
	write(map[string]any{"type": "message_stop"})
	return out.Bytes()
}
