package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf16"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The relay's Claude mount refuses an inference request whose system prompt
// carries neither Claude Code's billing attribution block nor its client
// identity block, and answers 400 invalid_request_error before any model is
// reached. The official client always sends one; a plain API caller and an agent
// harness that speaks the same wire do not, which is why both are rejected on
// every turn while the account itself stays healthy.
//
// CLIProxyAPI's built-in Claude executor generates the same block for its own
// requests. This plugin reaches the relay through its own executor, so nothing
// upstream supplies one and it has to be added here.
const (
	claudeAttributionPrefix = "x-anthropic-billing-header:"
	// claudeAttributionSalt is the salt Claude Code mixes into the three-character
	// build fingerprint it embeds after cc_version.
	claudeAttributionSalt = "59cf53e54c78"
	// claudeAttributionEntrypoint is the client entry point the official CLI
	// reports for the CLI profile this plugin presents.
	claudeAttributionEntrypoint = "cli"
)

// claudeAttributionSampleAt holds the prompt offsets Claude Code samples for its
// build fingerprint. It is JavaScript indexing, so the offsets are UTF-16 code
// units rather than runes.
var claudeAttributionSampleAt = [3]int{4, 7, 20}

// claudeAttributionModel reports whether a model is served by the relay's Claude
// mount. Only those requests need the attribution block: the other families this
// plugin speaks for take the same Messages wire but read system text as prompt
// content, so a billing block there would be sent to the model as instructions.
func claudeAttributionModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
}

// ensureClaudeAttributionSystem prepends the billing attribution block to a
// Claude Messages body that lacks one, and returns every other body unchanged.
// Blocks the caller already sent keep their order, text and cache_control, and a
// body that already carries attribution is left alone.
func ensureClaudeAttributionSystem(body []byte, model, clientVersion string) []byte {
	if !claudeAttributionModel(model) || !gjson.ValidBytes(body) {
		return body
	}
	version := strings.TrimSpace(clientVersion)
	if version == "" {
		return body
	}
	system := gjson.GetBytes(body, "system")
	if systemHasAttribution(system) {
		return body
	}
	block, errBlock := claudeAttributionBlock(version, body)
	if errBlock != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "system", claudeSystemWithAttribution(system, block))
	if errSet != nil {
		return body
	}
	return updated
}

// systemHasAttribution reports whether a system field already carries the
// billing attribution block, so a caller that sends its own is left untouched.
func systemHasAttribution(system gjson.Result) bool {
	switch {
	case !system.Exists():
		return false
	case system.Type == gjson.String:
		return strings.HasPrefix(system.String(), claudeAttributionPrefix)
	case system.IsArray():
		found := false
		system.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" && strings.HasPrefix(block.Get("text").String(), claudeAttributionPrefix) {
				found = true
				return false
			}
			return true
		})
		return found
	default:
		return false
	}
}

// claudeSystemWithAttribution renders the system field with the attribution block
// first. An existing system value keeps its own bytes: a string becomes one text
// block and an array keeps every block it had, in order.
func claudeSystemWithAttribution(system gjson.Result, block []byte) []byte {
	rendered := make([]byte, 0, len(block)+64)
	rendered = append(rendered, '[')
	rendered = append(rendered, block...)
	switch {
	case !system.Exists():
	case system.Type == gjson.String:
		existing, errMarshal := claudeTextBlock(system.String())
		if errMarshal != nil {
			return []byte("[" + string(block) + "]")
		}
		rendered = append(rendered, ',')
		rendered = append(rendered, existing...)
	case system.IsArray():
		// Drop the array's own brackets and keep every block it held, so the
		// rendered field stays one valid array.
		if raw := strings.TrimSpace(system.Raw); raw != "[]" && len(raw) >= 2 {
			rendered = append(rendered, ',')
			rendered = append(rendered, raw[1:len(raw)-1]...)
		}
	default:
		return []byte("[" + string(block) + "]")
	}
	rendered = append(rendered, ']')
	return rendered
}

// claudeAttributionBlock builds the attribution text block, mirroring the block
// Claude Code prepends to its own system prompt.
func claudeAttributionBlock(version string, body []byte) ([]byte, error) {
	return claudeTextBlock(claudeAttributionText(version, body))
}

func claudeAttributionText(version string, body []byte) string {
	var builder strings.Builder
	builder.WriteString(claudeAttributionPrefix)
	builder.WriteString(" cc_version=")
	builder.WriteString(version)
	builder.WriteByte('.')
	builder.WriteString(claudeAttributionFingerprint(version, body))
	builder.WriteString("; cc_entrypoint=")
	builder.WriteString(claudeAttributionEntrypoint)
	builder.WriteByte(';')
	return builder.String()
}

// claudeAttributionFingerprint reproduces the three-character build fingerprint
// Claude Code embeds after cc_version: SHA-256 over a fixed salt, three sampled
// code units of the prompt text, and the version.
func claudeAttributionFingerprint(version string, body []byte) string {
	units := utf16.Encode([]rune(claudePromptText(body)))
	sampled := make([]uint16, 0, len(claudeAttributionSampleAt))
	for _, index := range claudeAttributionSampleAt {
		character := uint16('0')
		if index < len(units) {
			character = units[index]
		}
		sampled = append(sampled, character)
	}
	digest := sha256.Sum256([]byte(claudeAttributionSalt + string(utf16.Decode(sampled)) + version))
	return hex.EncodeToString(digest[:])[:3]
}

// claudePromptText collects the text the fingerprint samples from: the user turns
// in order, so one conversation always yields the same fingerprint.
func claudePromptText(body []byte) string {
	var builder strings.Builder
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		if message.Get("role").String() != "user" {
			return true
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			builder.WriteString(content.String())
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				builder.WriteString(part.Get("text").String())
			}
			return true
		})
		return true
	})
	return builder.String()
}

// claudeTextBlock renders one Claude text block carrying the given text.
func claudeTextBlock(text string) ([]byte, error) {
	block := []byte(`{"type":"text","text":""}`)
	updated, errSet := sjson.SetBytes(block, "text", text)
	if errSet != nil {
		return nil, errSet
	}
	return updated, nil
}
