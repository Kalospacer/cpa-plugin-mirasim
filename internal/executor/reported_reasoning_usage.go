package executor

import (
	"bytes"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// reportedReasoningUsage tracks measurements, never estimates from text length.
// The pinned Messages-to-Responses translator estimates reasoning tokens even
// though Messages providers do not necessarily report this counter separately.
type reportedReasoningUsage struct {
	tokens   int64
	reported bool
}

func (u *reportedReasoningUsage) observe(payload []byte) {
	for _, raw := range bytes.Split(payload, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(line) {
			continue
		}
		usage := gjson.GetBytes(line, "usage")
		if !usage.Exists() {
			usage = gjson.GetBytes(line, "message.usage")
		}
		for _, path := range []string{"output_tokens_details.thinking_tokens", "output_tokens_details.reasoning_tokens", "thinking_tokens"} {
			count := usage.Get(path)
			if count.Type == gjson.Number && count.Int() >= 0 && count.Float() == float64(count.Int()) {
				u.tokens, u.reported = count.Int(), true
				break
			}
		}
	}
}

func (u reportedReasoningUsage) apply(payload []byte, format sdktranslator.Format) []byte {
	var path, usagePath string
	switch format {
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		usagePath, path = "usage", "usage.output_tokens_details.reasoning_tokens"
	case sdktranslator.FormatOpenAI:
		usagePath, path = "usage", "usage.completion_tokens_details.reasoning_tokens"
	default:
		return payload
	}
	lines := bytes.SplitAfter(payload, []byte("\n"))
	var result bytes.Buffer
	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(line) {
			result.Write(raw)
			continue
		}
		field, parent := path, usagePath
		if gjson.GetBytes(line, "response").IsObject() {
			field, parent = "response."+path, "response."+usagePath
		}
		if !gjson.GetBytes(line, parent).IsObject() {
			result.Write(raw)
			continue
		}
		var updated []byte
		var err error
		if u.reported {
			updated, err = sjson.SetBytes(line, field, u.tokens)
		} else {
			updated, err = sjson.DeleteBytes(line, field)
		}
		if err != nil {
			result.Write(raw)
			continue
		}
		start := bytes.Index(raw, line)
		result.Write(raw[:start])
		result.Write(updated)
		result.Write(raw[start+len(line):])
	}
	return result.Bytes()
}
