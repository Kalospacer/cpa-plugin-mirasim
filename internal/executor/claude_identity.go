package executor

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// Mirasim 中转的 Claude 入口自 2026-10-04 起校验客户端身份：User-Agent 不是
// claude-cli/<版本号> 形式的请求一律返回 403 permission_error
// "this client is not supported; use the mirasim client or Claude Code"。
// 实测只换 User-Agent 即可从 403 变为 200，版本号本身不被校验。
//
// 插件以独立 c-shared 库运行，无法调用 CLIProxyAPI 内部（internal/）的 Claude
// 指纹实现，因此这里移植其中与中转相关的部分，取值与规则保持一致：
//   - 默认设备画像：internal/runtime/executor/helps/claude_device_profile.go
//   - 身份请求头规则：internal/runtime/executor/claude_executor_request.go
//     （applyClaudeHeadersWithNativeProfile 中的 identityHeader）
//
// 移植基准为 CLIProxyAPI upstream main a4acc9f7（Claude Code 2.1.280 /
// @anthropic-ai/sdk 0.112.1）。CPA 升级默认画像时，按同一来源同步下列常量。
//
// 未移植 anthropic-beta 管理与设备画像缓存：前者服务于直连 Anthropic 的 beta
// 门控，后者服务于多实例共享画像，中转均不需要。
const (
	claudeCodeUserAgent      = "claude-cli/2.1.280 (external, cli)"
	claudeCodePackageVersion = "0.112.1"
	claudeCodeRuntimeVersion = "v26.3.0"
	claudeCodeOS             = "MacOS"
	claudeCodeArch           = "arm64"
	claudeCodeTimeout        = "600"

	claudeCodeSessionHeader = "X-Claude-Code-Session-Id"
)

var (
	// 与 CPA claudeCodeNativeUserAgentPattern 一致，用于识别真实的 Claude Code 调用方。
	claudeCodeNativeUserAgentPattern = regexp.MustCompile(`(?i)^claude-cli/[0-9]+\.[0-9]+\.[0-9]+\s+\(external,\s*[^,)]+(?:,\s*agent-sdk/[0-9]+\.[0-9]+\.[0-9]+)?\)$`)
	claudePackageVersionPattern      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	claudeRuntimeVersionPattern      = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

	// 每个 Mirasim 账号在本进程内使用一个固定的会话 ID，对应 CPA 按凭证缓存的会话 ID。
	claudeCodeSessionIDs sync.Map
)

// applyClaudeCodeIdentity 为发往中转 Claude 入口的请求补齐 Claude Code 身份头。
// 只处理请求体模型为 claude-* 的请求；其他模型族共用 Messages 线路但不经过该校验，保持原样。
//
// 调用方本身就是 Claude Code（User-Agent 符合原生格式）时保留其自带的值，仅补齐缺失项；
// 否则清掉调用方的 SDK 指纹头，统一写入默认画像，避免拼出真实客户端不会发送的组合。
func applyClaudeCodeIdentity(headers http.Header, body []byte, countTokens bool, accountID string) {
	if headers == nil || !claudeAttributionModel(modelFromJSON(body)) {
		return
	}
	confirmed := claudeCodeNativeUserAgentPattern.MatchString(strings.TrimSpace(headers.Get("User-Agent")))
	if !confirmed {
		for name := range headers {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-stainless-") || strings.HasPrefix(lower, "x-claude-code-") {
				delete(headers, name)
			}
		}
	}

	identity := func(name, fallback string) {
		if confirmed && strings.TrimSpace(headers.Get(name)) != "" {
			return
		}
		headers.Set(name, fallback)
	}
	// 设备画像：与 CPA extractClaudeDeviceProfile 一致，调用方的版本号格式不合法时回落到默认值。
	identity("User-Agent", claudeCodeUserAgent)
	if !claudePackageVersionPattern.MatchString(strings.TrimSpace(headers.Get("X-Stainless-Package-Version"))) {
		headers.Set("X-Stainless-Package-Version", claudeCodePackageVersion)
	}
	if !claudeRuntimeVersionPattern.MatchString(strings.TrimSpace(headers.Get("X-Stainless-Runtime-Version"))) {
		headers.Set("X-Stainless-Runtime-Version", claudeCodeRuntimeVersion)
	}
	identity("X-Stainless-Os", claudeCodeOS)
	identity("X-Stainless-Arch", claudeCodeArch)

	identity("Anthropic-Version", "2023-06-01")
	identity("Anthropic-Dangerous-Direct-Browser-Access", "true")
	identity("X-App", "cli")
	identity("X-Stainless-Retry-Count", "0")
	identity("X-Stainless-Runtime", "node")
	identity("X-Stainless-Lang", "js")
	// Claude Code 的 count_tokens 请求不带超时头，只有原生调用方自己带了才保留。
	if !countTokens {
		identity("X-Stainless-Timeout", claudeCodeTimeout)
	}
	identity(claudeCodeSessionHeader, claudeCodeSessionID(accountID))
}

// claudeCodeSessionID 返回账号在本进程内固定的会话 ID（UUID v4 格式）。
func claudeCodeSessionID(accountID string) string {
	if existing, ok := claudeCodeSessionIDs.Load(accountID); ok {
		return existing.(string)
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw)
	id := encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	actual, _ := claudeCodeSessionIDs.LoadOrStore(accountID, id)
	return actual.(string)
}
