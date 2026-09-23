package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

func TestStartLoginSendsTheBrowserToMirasimWithALoopbackCallback(t *testing.T) {
	provider, adminURL := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	accessToken := identityJWT("account-123", "user@example.com", time.Now().Add(time.Hour))

	started, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Provider: "mirasim"})
	if errStart != nil {
		t.Fatalf("StartLogin() error = %v", errStart)
	}
	if started.Provider != credentials.Provider || started.State == "" {
		t.Fatalf("start response = %#v", started)
	}
	// The browser goes straight to Mirasim; CPA is not asked to proxy anything.
	authorize := mustParseURL(t, started.URL)
	if authorize.Host != mustParseURL(t, adminURL).Host || authorize.Path != "/auth/oauth/github/login" {
		t.Fatalf("authorize URL = %s", authorize)
	}
	if authorize.Query().Get("state") != started.State {
		t.Fatalf("authorize state = %q, want %q", authorize.Query().Get("state"), started.State)
	}
	callbackURL := mustParseURL(t, authorize.Query().Get("redirect_uri"))
	if callbackURL.Scheme != "http" || callbackURL.Hostname() != "127.0.0.1" || !strings.HasPrefix(callbackURL.Path, "/callback/") {
		t.Fatalf("redirect_uri = %s, want a loopback callback", callbackURL)
	}
	if raw := []byte(toText(started.Metadata)); bytes.Contains(raw, []byte(accessToken)) || bytes.Contains(raw, []byte("refresh-secret")) {
		t.Fatalf("start metadata contains a token: %s", raw)
	}

	status, body := getCallback(t, callbackURL.String(), url.Values{
		"state":         []string{started.State},
		"access_token":  []string{accessToken},
		"refresh_token": []string{"refresh-secret"},
	})
	if status != http.StatusOK || !strings.Contains(body, "sign-in complete") {
		t.Fatalf("callback status = %d, body = %s", status, body)
	}
	if strings.Contains(body, accessToken) || strings.Contains(body, "refresh-secret") {
		t.Fatal("callback page reflected a credential")
	}

	polled := awaitLoginResult(t, provider, pluginapi.AuthLoginPollRequest{Provider: "mirasim", State: started.State, Host: pluginapi.HostConfigSummary{ProxyURL: "direct"}, HTTPClient: oauthValidationClient{}})
	if polled.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("PollLogin() = %#v", polled)
	}
	var payload map[string]any
	if errJSON := json.Unmarshal(polled.Auth.StorageJSON, &payload); errJSON != nil {
		t.Fatal(errJSON)
	}
	if payload["access_token"] != accessToken || payload["refresh_token"] != "refresh-secret" || payload["device_private_key"] == "" || payload["auth_kind"] != "oauth" {
		t.Fatal("OAuth auth JSON is missing self-contained credential fields")
	}
	if payload["account_id"] != "account-123" || payload["email"] != "user@example.com" {
		t.Fatalf("OAuth identity = account_id:%v email:%v", payload["account_id"], payload["email"])
	}
	if polled.Auth.FileName != "mirasim-account-123.json" || polled.Auth.Label != "Mirasim (user@example.com)" {
		t.Fatalf("OAuth auth identity = file:%q label:%q", polled.Auth.FileName, polled.Auth.Label)
	}
	if _, present := payload["credential_dir"]; present {
		t.Fatal("OAuth auth JSON contains a legacy credential path")
	}
	if _, errParseAuth := credentials.Parse(polled.Auth.StorageJSON, provider.settings); errParseAuth != nil {
		t.Fatalf("parse OAuth auth JSON error = %v", errParseAuth)
	}
	// The one-shot listener must not outlive the login it served.
	awaitListenerClosed(t, callbackURL.String())
}

// Mirasim 0.0.272 sometimes drops the state it was handed. The 144-bit callback
// path on a loopback-only listener is the channel binding in that case.
func TestCallbackWithoutStateStillBindsToTheLoginThatOpenedThePort(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	started, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
	callbackURL := callbackURLOf(t, started.URL)
	status, _ := getCallback(t, callbackURL, url.Values{
		"access_token":  []string{identityJWT("account-9", "user@example.com", time.Now().Add(time.Hour))},
		"refresh_token": []string{"refresh-secret"},
	})
	if status != http.StatusOK {
		t.Fatalf("state-less callback status = %d", status)
	}
	polled := awaitLoginResult(t, provider, pluginapi.AuthLoginPollRequest{State: started.State, HTTPClient: oauthValidationClient{}})
	if polled.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("PollLogin() = %#v", polled)
	}
}

func TestCallbackWithoutRefreshTokenIsRefusedAndSavesNothing(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	started, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
	accessToken := identityJWT("account-9", "user@example.com", time.Now().Add(time.Hour))
	callbackURL := callbackURLOf(t, started.URL)
	status, body := getCallback(t, callbackURL, url.Values{
		"state":        []string{started.State},
		"access_token": []string{accessToken},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("incomplete callback status = %d", status)
	}
	if strings.Contains(body, accessToken) {
		t.Fatal("refusal page reflected the access token")
	}
	polled := awaitLoginResult(t, provider, pluginapi.AuthLoginPollRequest{State: started.State, HTTPClient: oauthValidationClient{}})
	if polled.Status != pluginapi.AuthLoginStatusError || polled.Auth.FileName != "" {
		t.Fatalf("PollLogin() = %#v", polled)
	}
	if strings.Contains(polled.Message, accessToken) || !strings.Contains(polled.Message, "renewable credentials") {
		t.Fatalf("poll message = %q", polled.Message)
	}
}

func TestSecondCallbackOnTheSamePathIsRejected(t *testing.T) {
	results := make(chan localOAuthResult, 1)
	handler := localOAuthHandler("/callback/only-once", "expected-state", results)
	query := "?state=expected-state&access_token=access&refresh_token=refresh"

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/callback/only-once"+query, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first callback status = %d", first.Code)
	}
	if result := <-results; result.accessToken != "access" || result.refreshToken != "refresh" {
		t.Fatalf("captured result = %#v", result)
	}

	// Draining the channel must not re-open the single use.
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/callback/only-once"+query, nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("second callback status = %d, want 409", second.Code)
	}
	select {
	case result := <-results:
		t.Fatalf("replayed callback produced a second result: %#v", result)
	default:
	}
}

func TestOversizedCallbackCredentialsAreRefusedBeforeCapture(t *testing.T) {
	results := make(chan localOAuthResult, 1)
	handler := localOAuthHandler("/callback/bounded", "expected-state", results)
	oversized := strings.Repeat("a", maxOAuthCredentialLen+1)
	request := httptest.NewRequest(http.MethodGet, "/callback/bounded?state=expected-state&access_token="+oversized+"&refresh_token=refresh", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized callback status = %d", recorder.Code)
	}
	result := <-results
	if result.accessToken != "" || result.refreshToken != "" || result.errorMessage == "" {
		t.Fatalf("oversized result = %#v", result)
	}
	if strings.Contains(result.errorMessage, "aaa") {
		t.Fatalf("rejection message quoted the credential: %q", result.errorMessage)
	}
}

func TestLoginProviderResolutionPrefersRequestThenConfigThenGithub(t *testing.T) {
	settings := pluginconfig.Defaults()
	settings.OAuthLoginProvider = "google"
	provider, _ := newLoopbackLoginProvider(t, settings)

	requested, errRequested := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"provider": "github"}})
	if errRequested != nil {
		t.Fatal(errRequested)
	}
	if path := mustParseURL(t, requested.URL).Path; path != "/auth/oauth/github/login" {
		t.Fatalf("requested provider path = %q", path)
	}

	configured, errConfigured := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errConfigured != nil {
		t.Fatal(errConfigured)
	}
	if path := mustParseURL(t, configured.URL).Path; path != "/auth/oauth/google/login" {
		t.Fatalf("configured provider path = %q", path)
	}

	provider.settings.OAuthLoginProvider = ""
	fallback, errFallback := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errFallback != nil {
		t.Fatal(errFallback)
	}
	if path := mustParseURL(t, fallback.URL).Path; path != "/auth/oauth/github/login" {
		t.Fatalf("fallback provider path = %q", path)
	}
}

func TestStartLoginNamesTheOfferedProvidersWhenTheRequestedOneIsNot(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	_, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"provider": "gitlab"}})
	if errStart == nil {
		t.Fatal("unsupported provider was accepted")
	}
	if !strings.Contains(errStart.Error(), "gitlab") || !strings.Contains(errStart.Error(), "github, google") {
		t.Fatalf("error = %v, want it to name the offered providers", errStart)
	}
}

func TestStartLoginRejectsAMalformedRequestedProvider(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	if _, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"provider": "../etc"}}); errStart == nil {
		t.Fatal("malformed provider was accepted")
	}
}

// A pinned port exists so a remote CPA can be reached over an SSH tunnel, which
// means only one login can hold it: the newest must replace the previous one.
func TestPinnedCallbackPortKeepsExactlyOneListener(t *testing.T) {
	settings := pluginconfig.Defaults()
	port := freeLoopbackPort(t)
	settings.OAuthCallbackPort = port
	provider, _ := newLoopbackLoginProvider(t, settings)

	first, errFirst := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, errSecond := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errSecond != nil {
		t.Fatalf("second login could not reuse the pinned port: %v", errSecond)
	}
	for _, started := range []pluginapi.AuthLoginStartResponse{first, second} {
		if got := mustParseURL(t, callbackURLOf(t, started.URL)).Port(); got != port {
			t.Fatalf("callback port = %q, want %q", got, port)
		}
	}
	provider.oauth.mu.Lock()
	sessions := len(provider.oauth.sessions)
	provider.oauth.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("pending sessions = %d, want 1", sessions)
	}
	if polled, _ := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: first.State}); polled.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("replaced login poll = %#v", polled)
	}
}

// The cap exists because every pending login holds a loopback port open, so it
// has to hold when the calls arrive together and not just one at a time.
func TestConcurrentStartLoginNeverExceedsTheSessionCap(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	const contenders = 4 * maxOAuthSessions
	start := make(chan struct{})
	outcomes := make(chan error, contenders)
	var running sync.WaitGroup
	for contender := 0; contender < contenders; contender++ {
		running.Add(1)
		go func() {
			defer running.Done()
			<-start
			_, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
			outcomes <- errStart
		}()
	}
	close(start)
	running.Wait()
	close(outcomes)

	accepted := 0
	for errStart := range outcomes {
		if errStart == nil {
			accepted++
			continue
		}
		if !strings.Contains(errStart.Error(), "too many pending") {
			t.Fatalf("refused login error = %v, want the session cap", errStart)
		}
	}
	if accepted > maxOAuthSessions {
		t.Fatalf("concurrent StartLogin accepted %d logins, cap is %d", accepted, maxOAuthSessions)
	}
	if accepted == 0 {
		t.Fatal("concurrent StartLogin accepted no login at all")
	}
	provider.oauth.mu.Lock()
	sessions := len(provider.oauth.sessions)
	provider.oauth.mu.Unlock()
	if sessions != accepted {
		t.Fatalf("pending sessions = %d, want the %d accepted logins", sessions, accepted)
	}
}

func TestPendingLoginExpiresAndReleasesItsPort(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	provider.oauth.now = func() time.Time { return now }

	started, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
	callbackURL := callbackURLOf(t, started.URL)
	pending, _ := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: started.State})
	if pending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("pending poll = %#v", pending)
	}

	now = now.Add(oauthLoginTTL + time.Second)
	expired, _ := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: started.State})
	if expired.Status != pluginapi.AuthLoginStatusError || !strings.Contains(expired.Message, "expired") {
		t.Fatalf("expired poll = %#v", expired)
	}
	awaitListenerClosed(t, callbackURL)
}

func TestCancelledCallbackFailsTheLoginWithoutReflectingProviderDetail(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	started, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
	status, body := getCallback(t, callbackURLOf(t, started.URL), url.Values{
		"state":             []string{started.State},
		"error":             []string{"access_denied"},
		"error_description": []string{"do-not-reflect"},
	})
	if status != http.StatusBadRequest || strings.Contains(body, "do-not-reflect") {
		t.Fatalf("denied callback status = %d, body = %s", status, body)
	}
	polled := awaitLoginResult(t, provider, pluginapi.AuthLoginPollRequest{State: started.State})
	if polled.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("error poll = %#v", polled)
	}
}

func TestPollLoginRejectsCredentialsThatFailRemoteValidation(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	started, errStart := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
	if status, _ := getCallback(t, callbackURLOf(t, started.URL), url.Values{
		"state":         []string{started.State},
		"access_token":  []string{identityJWT("rejected", "", time.Now().Add(time.Hour))},
		"refresh_token": []string{"refresh-secret"},
	}); status != http.StatusOK {
		t.Fatalf("callback status = %d", status)
	}
	failedClient := oauthValidationClient{status: http.StatusUnauthorized, body: []byte(`{"error":"PRIVATE_UPSTREAM_DETAIL"}`)}
	polled := awaitLoginResult(t, provider, pluginapi.AuthLoginPollRequest{State: started.State, HTTPClient: failedClient})
	if polled.Status != pluginapi.AuthLoginStatusError || polled.Auth.FileName != "" {
		t.Fatalf("PollLogin() = %#v", polled)
	}
	if strings.Contains(polled.Message, "PRIVATE_UPSTREAM_DETAIL") || !strings.Contains(polled.Message, "HTTP 401") {
		t.Fatalf("unsafe or incomplete validation error = %q", polled.Message)
	}
}

func TestPollLoginRefusesAnUnknownState(t *testing.T) {
	provider, _ := newLoopbackLoginProvider(t, pluginconfig.Defaults())
	polled, errPoll := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: "not-a-session"})
	if errPoll != nil || polled.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("PollLogin() = %#v, error = %v", polled, errPoll)
	}
}

// newLoopbackLoginProvider wires a provider to a fake Mirasim authentication
// service and guarantees every listener a test opens is released.
func newLoopbackLoginProvider(t *testing.T, settings pluginconfig.Settings) (*Provider, string) {
	t.Helper()
	server := newOAuthProfileServer(t)
	t.Cleanup(server.Close)
	settings.AdminURL = server.URL
	provider := New(settings, mirasim.NewPool())
	t.Cleanup(func() { releaseLoginSessions(provider) })
	return provider, server.URL
}

// releaseLoginSessions closes every listener a test left pending, so no test can
// leak a loopback port into the next one.
func releaseLoginSessions(provider *Provider) {
	provider.oauth.mu.Lock()
	stale := provider.oauth.drainLocked()
	provider.oauth.mu.Unlock()
	closeLoopbackCaptures(stale)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		t.Fatalf("parse %q error = %v", raw, errParse)
	}
	return parsed
}

func callbackURLOf(t *testing.T, authorizeURL string) string {
	t.Helper()
	callback := mustParseURL(t, authorizeURL).Query().Get("redirect_uri")
	if callback == "" {
		t.Fatalf("authorize URL %q carries no redirect_uri", authorizeURL)
	}
	return callback
}

func getCallback(t *testing.T, callbackURL string, query url.Values) (int, string) {
	t.Helper()
	target := callbackURL
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	resp, errGet := http.Get(target)
	if errGet != nil {
		t.Fatalf("callback GET error = %v", errGet)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		t.Fatalf("read callback body error = %v", errRead)
	}
	return resp.StatusCode, string(body)
}

// awaitLoginResult polls until the captured callback has been latched, because
// the listener hands its result to the coordinator on its own goroutine.
func awaitLoginResult(t *testing.T, provider *Provider, req pluginapi.AuthLoginPollRequest) pluginapi.AuthLoginPollResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		polled, errPoll := provider.PollLogin(context.Background(), req)
		if errPoll != nil {
			t.Fatalf("PollLogin() error = %v", errPoll)
		}
		if polled.Status != pluginapi.AuthLoginStatusPending {
			return polled
		}
		if time.Now().After(deadline) {
			t.Fatal("login never left the pending state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitListenerClosed(t *testing.T, callbackURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, errGet := http.Get(callbackURL)
		if errGet != nil {
			return
		}
		_ = resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatalf("callback listener at %s is still accepting requests", callbackURL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve loopback port error = %v", errListen)
	}
	_, port, errSplit := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	if errSplit != nil {
		t.Fatalf("split reserved address error = %v", errSplit)
	}
	return port
}

func newOAuthProfileServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/oauth/providers" {
			_, _ = w.Write([]byte(`{"providers":["github","google"]}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/auth/me" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("profile request = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "user@example.com"})
	}))
}

type oauthValidationClient struct {
	status int
	body   []byte
}

func (c oauthValidationClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	parsed, _ := url.Parse(req.URL)
	if c.status != 0 {
		return pluginapi.HTTPResponse{StatusCode: c.status, Headers: make(http.Header), Body: append([]byte(nil), c.body...)}, nil
	}
	switch parsed.Path {
	case "/v1/device/session":
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"ticket":"device-ticket","expiresIn":900}`)}, nil
	case "/v1/models":
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"data":[{"id":"claude-sonnet-5"}]}`)}, nil
	default:
		return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected validation path %s", parsed.Path)
	}
}

func (oauthValidationClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, fmt.Errorf("unexpected validation stream")
}

func identityJWT(accountID, email string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": accountID, "email": email, "exp": expiry.Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func toText(value any) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(fmt.Sprint(value)), "\n", " "), "\r", " "))
}
