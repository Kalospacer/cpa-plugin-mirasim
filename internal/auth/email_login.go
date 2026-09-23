package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
)

const (
	emailCodeResource   = "/auth/code"
	emailVerifyResource = "/auth/verify"
	emailLoginTimeout   = 20 * time.Second
	emailCodeEntryTTL   = 10 * time.Minute
	maxAdminResponse    = 64 << 10
	maxAdminDetail      = 512
)

var emailAddress = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

// runEmailLogin signs in with a mailed code. Mirasim issues these for accounts
// with no OAuth provider bound to them, which the browser and provider flows
// cannot reach at all.
func (p *Provider) runEmailLogin(ctx context.Context, settings pluginconfig.Settings, email, code, proxyURL string) (pluginapi.AuthData, []byte, error) {
	email, errEmail := normalizeLoginEmail(email)
	if errEmail != nil {
		return pluginapi.AuthData{}, nil, errEmail
	}
	stdout := make([]byte, 0)
	if strings.TrimSpace(code) == "" {
		if errRequest := requestEmailCode(ctx, settings.AdminURL, proxyURL, email); errRequest != nil {
			return pluginapi.AuthData{}, nil, errRequest
		}
		notice := []byte("Mirasim sent a sign-in code to " + email + ".\n")
		_, _ = os.Stdout.Write(notice)
		stdout = append(stdout, notice...)
		entered, errPrompt := p.promptForEmailCode(ctx)
		if errPrompt != nil {
			return pluginapi.AuthData{}, stdout, errPrompt
		}
		code = entered
	}
	code, errCode := normalizeLoginCode(code)
	if errCode != nil {
		return pluginapi.AuthData{}, stdout, errCode
	}
	accessToken, refreshToken, errVerify := verifyEmailCode(ctx, settings.AdminURL, proxyURL, email, code)
	if errVerify != nil {
		return pluginapi.AuthData{}, stdout, errVerify
	}
	storage, errStorage := p.finalizeOAuthStorage(ctx, settings, accessToken, refreshToken, proxyURL, nil)
	accessToken, refreshToken = "", ""
	if errStorage != nil {
		return pluginapi.AuthData{}, stdout, errStorage
	}
	fileName := storage.DefaultAuthFileName()
	auth := storage.AuthData(fileName, fileName, p.pool.Client(storage).NextRefreshAfter(time.Now()))
	return auth, append(stdout, []byte("Mirasim authentication successful.\n")...), nil
}

// requestEmailCode asks Mirasim to mail a sign-in code. A development build of
// the service echoes the code back; that is deliberately not surfaced here.
func requestEmailCode(ctx context.Context, adminURL, proxyURL, email string) error {
	_, errPost := postAdminJSON(ctx, adminURL, proxyURL, emailCodeResource, map[string]string{"email": email})
	return errPost
}

// verifyEmailCode exchanges a mailed code for renewable credentials. A response
// without a refresh token is refused rather than saved: CPA cannot keep such a
// credential alive past the access token's own expiry.
func verifyEmailCode(ctx context.Context, adminURL, proxyURL, email, code string) (string, string, error) {
	raw, errPost := postAdminJSON(ctx, adminURL, proxyURL, emailVerifyResource, map[string]string{"email": email, "code": code})
	if errPost != nil {
		return "", "", errPost
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if errDecode := json.Unmarshal(raw, &payload); errDecode != nil {
		return "", "", fmt.Errorf("invalid Mirasim email sign-in response")
	}
	accessToken := strings.TrimSpace(payload.AccessToken)
	refreshToken := strings.TrimSpace(payload.RefreshToken)
	if accessToken == "" {
		return "", "", fmt.Errorf("Mirasim email sign-in returned no access token")
	}
	if refreshToken == "" {
		return "", "", fmt.Errorf("Mirasim email sign-in returned no renewable credential")
	}
	return accessToken, refreshToken, nil
}

// postAdminJSON talks to the authentication service through a private client.
// The request body carries a sign-in secret, so it never uses the host request
// logger, and an upstream message is bounded before it reaches an error.
func postAdminJSON(ctx context.Context, adminURL, proxyURL, resource string, body map[string]string) ([]byte, error) {
	endpoint, errEndpoint := adminEndpoint(adminURL, resource)
	if errEndpoint != nil {
		return nil, errEndpoint
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, errEncode
	}
	transport, _, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
	if errTransport != nil {
		return nil, fmt.Errorf("configure Mirasim email sign-in proxy")
	}
	client := &http.Client{Timeout: emailLoginTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if transport != nil {
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if errRequest != nil {
		return nil, fmt.Errorf("create Mirasim email sign-in request")
	}
	request.Header.Set("Content-Type", "application/json")
	resp, errDo := client.Do(request)
	if errDo != nil {
		return nil, fmt.Errorf("Mirasim authentication service is unreachable; retry sign-in")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, maxAdminResponse+1))
	if errRead != nil || len(raw) > maxAdminResponse {
		return nil, fmt.Errorf("invalid Mirasim email sign-in response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if detail := adminErrorDetail(raw); detail != "" {
			return nil, fmt.Errorf("Mirasim email sign-in failed: %s", detail)
		}
		return nil, fmt.Errorf("Mirasim email sign-in returned HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

// adminErrorDetail mirrors the official client, which shows the service's own
// detail string when it has one.
func adminErrorDetail(raw []byte) string {
	var payload struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	detail := strings.TrimSpace(payload.Detail)
	if len(detail) > maxAdminDetail {
		detail = detail[:maxAdminDetail]
	}
	for _, char := range detail {
		if char < 0x20 || char == 0x7f {
			return ""
		}
	}
	return detail
}

// promptForEmailCode shares the process's single stdin reader with the OAuth
// paste prompt: two readers over os.Stdin would each buffer, and whichever read
// first would swallow input meant for the other.
func (p *Provider) promptForEmailCode(ctx context.Context) (string, error) {
	requests := p.promptRequests()
	reply := make(chan stdinReply)
	// abandoned tells the shared reader this prompt has stopped waiting, so a code
	// typed after a cancellation or a timeout is kept for the next prompt instead
	// of being dropped into a channel nobody is reading.
	abandoned := make(chan struct{})
	defer close(abandoned)
	timer := time.NewTimer(emailCodeEntryTTL)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", fmt.Errorf("Mirasim email sign-in timed out waiting for the code")
		case requests <- stdinRequest{kind: promptEmailCode, reply: reply, done: abandoned}:
			requests = nil
			_, _ = os.Stdout.Write([]byte("Enter the Mirasim sign-in code: "))
		case value := <-reply:
			if value.err != nil && value.line == "" {
				return "", value.err
			}
			return value.line, nil
		}
	}
}

func normalizeLoginEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 254 || !emailAddress.MatchString(value) {
		return "", fmt.Errorf("invalid Mirasim account email address")
	}
	return value, nil
}

func normalizeLoginCode(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("invalid Mirasim sign-in code")
	}
	return value, nil
}
