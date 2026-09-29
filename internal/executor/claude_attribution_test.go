package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The relay's Claude mount refuses a request whose system prompt carries no
// Claude Code attribution block, so every Claude body this plugin sends has to
// carry one. The other families on the same Messages wire read system text as
// prompt content and must not receive it.
func TestClaudeAttributionIsAddedOnlyForClaudeModels(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		payload  string
		wantText bool
	}{
		{
			name:     "claude without system",
			model:    "claude-opus-5-5",
			payload:  `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`,
			wantText: true,
		},
		{
			name:     "claude with existing system string",
			model:    "claude-sonnet-5",
			payload:  `{"model":"claude-sonnet-5","system":"You are helpful.","messages":[{"role":"user","content":"hello"}]}`,
			wantText: true,
		},
		{
			name:     "claude with existing system blocks",
			model:    "claude-haiku-4-5",
			payload:  `{"model":"claude-haiku-4-5","system":[{"type":"text","text":"You are helpful."}],"messages":[{"role":"user","content":"hello"}]}`,
			wantText: true,
		},
		{
			name:     "deepseek keeps its own thinking controls",
			model:    "deepseek-flash",
			payload:  `{"model":"deepseek-flash","messages":[{"role":"user","content":"hello"}]}`,
			wantText: false,
		},
		{
			name:     "glm is not a Claude mount request",
			model:    "glm-5.3",
			payload:  `{"model":"glm-5.3","messages":[{"role":"user","content":"hello"}]}`,
			wantText: false,
		},
		{
			name:     "gpt keeps the Responses wire",
			model:    "gpt-5.6-sol",
			payload:  `{"model":"gpt-5.6-sol","input":"hello"}`,
			wantText: false,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body := ensureClaudeAttributionSystem([]byte(test.payload), test.model, "0.0.354")
			// A body the relay cannot parse is refused outright, so validity is
			// part of the contract rather than an implementation detail.
			if !gjson.ValidBytes(body) {
				t.Fatalf("body is not valid JSON: %s", body)
			}
			text := gjson.GetBytes(body, "system.0.text").String()
			hasAttribution := strings.HasPrefix(text, claudeAttributionPrefix)
			if hasAttribution != test.wantText {
				t.Fatalf("attribution present = %v, want %v; body = %s", hasAttribution, test.wantText, body)
			}
			if !test.wantText {
				return
			}
			// The relay reads cc_version and cc_entrypoint out of the block, so
			// both have to be present and the version has to be the real one.
			if !strings.Contains(text, "cc_version=0.0.354.") {
				t.Fatalf("cc_version missing from %q", text)
			}
			if !strings.Contains(text, "cc_entrypoint=cli;") {
				t.Fatalf("cc_entrypoint missing from %q", text)
			}
		})
	}
}

func TestClaudeAttributionKeepsCallerSystemContent(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"Keep me.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`)
	body := ensureClaudeAttributionSystem(payload, "claude-opus-5-5", "0.0.354")

	if got := gjson.GetBytes(body, "system.#").Int(); got != 2 {
		t.Fatalf("system blocks = %d, want 2; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "system.1.text").String(); got != "Keep me." {
		t.Fatalf("caller system block = %q, body = %s", got, body)
	}
	if !gjson.GetBytes(body, "system.1.cache_control").Exists() {
		t.Fatalf("caller cache_control was dropped: %s", body)
	}
}

func TestClaudeAttributionLeavesAnExistingBlockAlone(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=9.9.9.zzz; cc_entrypoint=cli;"}],"messages":[{"role":"user","content":"hello"}]}`)
	body := ensureClaudeAttributionSystem(payload, "claude-opus-5-5", "0.0.354")

	if got := gjson.GetBytes(body, "system.#").Int(); got != 1 {
		t.Fatalf("system blocks = %d, want 1; body = %s", got, body)
	}
	if got := gjson.GetBytes(body, "system.0.text").String(); !strings.Contains(got, "cc_version=9.9.9.zzz") {
		t.Fatalf("caller attribution was replaced: %s", body)
	}
}

// The fingerprint is deterministic for one conversation and changes with the
// version, which is what lets the relay correlate a caller across turns.
func TestClaudeAttributionFingerprintIsStable(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"Reply with exactly OK."}]}`)
	first := ensureClaudeAttributionSystem(payload, "claude-opus-5-5", "0.0.354")
	second := ensureClaudeAttributionSystem(payload, "claude-opus-5-5", "0.0.354")
	if string(first) != string(second) {
		t.Fatalf("same request produced different bodies:\n%s\n%s", first, second)
	}
	other := ensureClaudeAttributionSystem(payload, "claude-opus-5-5", "0.0.355")
	if string(first) == string(other) {
		t.Fatalf("version change did not change the body: %s", first)
	}
}
