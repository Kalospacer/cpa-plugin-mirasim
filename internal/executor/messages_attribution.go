package executor

import (
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 非 Claude 模型会把每轮变化的 Claude Code 计费指纹当作提示词，破坏前缀复用。
func normalizeMessagesAttribution(body []byte, model string) []byte {
	model = strings.ToLower(model)
	if !strings.HasPrefix(model, "gemini-") && !strings.HasPrefix(model, "deepseek-") && !strings.HasPrefix(model, "glm-") && !strings.HasPrefix(model, "kimi-") {
		return body
	}
	system := gjson.GetBytes(body, "system")
	var updated []byte
	var err error
	switch {
	case system.Type == gjson.String:
		text, changed := withoutClaudeBillingLine(system.String())
		if !changed {
			return body
		}
		if strings.TrimSpace(text) == "" {
			updated, err = sjson.DeleteBytes(body, "system")
		} else {
			updated, err = sjson.SetBytes(body, "system", text)
		}
	case system.IsArray():
		blocks := system.Array()
		kept := make([]string, 0, len(blocks))
		changed := false
		for _, block := range blocks {
			raw := block.Raw
			if block.Get("type").String() == "text" {
				if text, stripped := withoutClaudeBillingLine(block.Get("text").String()); stripped {
					changed = true
					if strings.TrimSpace(text) == "" {
						continue
					}
					value, errSet := sjson.SetBytes([]byte(raw), "text", text)
					if errSet != nil {
						return body
					}
					raw = string(value)
				}
			}
			kept = append(kept, raw)
		}
		if !changed {
			return body
		}
		if len(kept) == 0 {
			updated, err = sjson.DeleteBytes(body, "system")
		} else {
			updated, err = sjson.SetRawBytes(body, "system", []byte("["+strings.Join(kept, ",")+"]"))
		}
	default:
		return body
	}
	if err != nil {
		return body
	}
	return updated
}

func withoutClaudeBillingLine(text string) (string, bool) {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, claudeAttributionPrefix) {
		return text, false
	}
	_, rest, _ := strings.Cut(trimmed, "\n")
	return rest, true
}
