package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
)

const (
	// oauthLoginTTL matches CPA's own OAuth session store, which expires at 30
	// minutes. A Management Center browser login therefore stays valid for exactly
	// as long as the host will keep polling it.
	oauthLoginTTL = 30 * time.Minute
	// cliLoginTTL bounds the blocking --mirasim-login wait, which is interactive
	// and additionally offers a manual paste prompt after a few seconds.
	cliLoginTTL = 3 * time.Minute
	// maxOAuthSessions is small because every pending browser login holds a
	// loopback port open until it completes or expires.
	maxOAuthSessions = 4
	// fallbackLoginProvider is the last resort when neither the caller nor the
	// configuration names a Mirasim sign-in provider.
	fallbackLoginProvider = "github"
)

type oauthSession struct {
	state        string
	provider     string
	capture      *loopbackCapture
	teardown     *time.Timer
	expiresAt    time.Time
	accessToken  string
	refreshToken string
	callbackDone bool
	finalizing   bool
	errorMessage string
	auth         *pluginapi.AuthData
}

type oauthCoordinator struct {
	mu       sync.Mutex
	sessions map[string]*oauthSession
	now      func() time.Time
}

func newOAuthCoordinator() *oauthCoordinator {
	return &oauthCoordinator{sessions: make(map[string]*oauthSession), now: time.Now}
}

// StartLogin drives CPA's native plugin login abstraction: the host registers the
// returned State, the browser follows the returned Mirasim authorize URL, and the
// callback lands on a loopback listener this plugin owns. No HTTP route is
// registered with the host on any prefix.
func (p *Provider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if provider := strings.TrimSpace(req.Provider); provider != "" && !strings.EqualFold(provider, credentials.Provider) {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	// req.BaseURL is deliberately ignored. It points at CPA's
	// /v0/management/oauth-callback, which hard-rejects any callback without an
	// OAuth `code` and persists only {code,state,error}; Mirasim returns
	// access_token and refresh_token instead. PollLogin likewise only ever sees
	// the static metadata registered here, never callback query data.
	loginProvider, errProvider := resolveLoginProvider(metadataString(req.Metadata, "provider"), p.settings.OAuthLoginProvider)
	if errProvider != nil {
		return pluginapi.AuthLoginStartResponse{}, errProvider
	}
	offered, errDiscovery := discoverLoginProviders(ctx, p.settings.AdminURL, req.Host.ProxyURL)
	if errDiscovery != nil {
		return pluginapi.AuthLoginStartResponse{}, errDiscovery
	}
	if !providerOffered(offered, loginProvider) {
		return pluginapi.AuthLoginStartResponse{}, unsupportedLoginProviderError(loginProvider, offered)
	}
	state, errState := randomOAuthValue(32)
	if errState != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("generate Mirasim OAuth state: %w", errState)
	}
	port := loopbackCallbackPort(p.settings.OAuthCallbackPort)

	now := p.oauth.now()
	expiresAt := now.Add(oauthLoginTTL)
	// The session slot is taken under the same lock that checks the cap. Binding
	// the listener afterwards takes long enough that concurrent StartLogin calls
	// would otherwise all pass the check before any of them inserted, and the cap
	// exists because every pending login holds a loopback port open.
	p.oauth.mu.Lock()
	stale := p.oauth.purgeLocked(now)
	if port != 0 {
		// A pinned port can host exactly one listener, so the newest login wins and
		// the previous one is closed rather than left to fail the bind below.
		stale = append(stale, p.oauth.drainLocked()...)
	} else if len(p.oauth.sessions) >= maxOAuthSessions {
		p.oauth.mu.Unlock()
		closeLoopbackCaptures(stale)
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("too many pending Mirasim OAuth sessions")
	}
	p.oauth.sessions[state] = &oauthSession{state: state, provider: loginProvider, expiresAt: expiresAt}
	p.oauth.mu.Unlock()
	closeLoopbackCaptures(stale)

	capture, errCapture := startLoopbackCapture(port, state)
	if errCapture != nil {
		p.oauth.expire(state)
		return pluginapi.AuthLoginStartResponse{}, errCapture
	}
	authURL, errURL := buildMirasimOAuthURL(p.settings.AdminURL, loginProvider, capture.CallbackURL(), state)
	if errURL != nil {
		capture.Close()
		p.oauth.expire(state)
		return pluginapi.AuthLoginStartResponse{}, errURL
	}

	p.oauth.mu.Lock()
	session := p.oauth.sessions[state]
	if session == nil {
		// Another login claimed the pinned port, or the slot expired, while this
		// listener was coming up.
		p.oauth.mu.Unlock()
		capture.Close()
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("Mirasim OAuth session was replaced before it could start")
	}
	session.capture = capture
	// An abandoned login must release its port on its own rather than squatting
	// until some later call happens to purge it.
	session.teardown = time.AfterFunc(oauthLoginTTL, func() { p.oauth.expire(state) })
	p.oauth.mu.Unlock()

	go p.awaitOAuthCallback(state, capture)

	return pluginapi.AuthLoginStartResponse{
		Provider:  credentials.Provider,
		URL:       authURL,
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"flow":           "browser_oauth",
			"login_provider": loginProvider,
			"expires_at":     expiresAt.UTC().Format(time.RFC3339),
		},
	}, nil
}

func (p *Provider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	state := strings.TrimSpace(req.State)
	now := p.oauth.now()
	var stale []*loopbackCapture
	defer func() { closeLoopbackCaptures(stale) }()

	p.oauth.mu.Lock()
	stale = p.oauth.purgeLocked(now)
	session := p.oauth.sessions[state]
	if session == nil || !constantTimeEqual(session.state, state) {
		p.oauth.mu.Unlock()
		return oauthPollError("unknown or expired Mirasim OAuth state"), nil
	}
	if session.auth != nil {
		auth := cloneAuthData(*session.auth)
		p.oauth.mu.Unlock()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "Mirasim OAuth login completed", Auth: auth, Auths: []pluginapi.AuthData{auth}}, nil
	}
	if session.errorMessage != "" {
		message := session.errorMessage
		p.oauth.mu.Unlock()
		return oauthPollError(message), nil
	}
	if !session.callbackDone || session.finalizing {
		p.oauth.mu.Unlock()
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "Waiting for Mirasim OAuth callback"}, nil
	}
	accessToken, refreshToken := session.accessToken, session.refreshToken
	session.accessToken = ""
	session.refreshToken = ""
	session.finalizing = true
	p.oauth.mu.Unlock()

	storage, errStorage := p.finalizeOAuthStorage(ctx, p.settings, accessToken, refreshToken, req.Host.ProxyURL, req.HTTPClient)
	accessToken, refreshToken = "", ""

	p.oauth.mu.Lock()
	defer p.oauth.mu.Unlock()
	session = p.oauth.sessions[state]
	if session == nil {
		return oauthPollError("Mirasim OAuth session expired while credentials were being installed"), nil
	}
	session.finalizing = false
	if errStorage != nil {
		session.errorMessage = errStorage.Error()
		return oauthPollError(session.errorMessage), nil
	}
	fileName := storage.DefaultAuthFileName()
	auth := storage.AuthData(fileName, fileName, p.pool.Client(storage).NextRefreshAfter(now))
	session.auth = &auth
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "Mirasim OAuth login completed", Auth: auth, Auths: []pluginapi.AuthData{auth}}, nil
}

// awaitOAuthCallback moves the one captured callback into session state and then
// releases the port immediately, instead of holding it until the next poll.
func (p *Provider) awaitOAuthCallback(state string, capture *loopbackCapture) {
	select {
	case result := <-capture.Results():
		p.oauth.recordCallback(state, result)
	case <-capture.Done():
	}
	capture.Close()
}

// recordCallback latches the single callback outcome. Rejection messages describe
// only the shape of the failure, never any captured value.
func (c *oauthCoordinator) recordCallback(state string, result localOAuthResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session := c.sessions[state]
	if session == nil || session.callbackDone || session.auth != nil {
		return
	}
	session.callbackDone = true
	switch {
	case !constantTimeEqual(session.state, strings.TrimSpace(result.state)):
		session.errorMessage = "Mirasim OAuth callback state did not match"
	case result.errorMessage != "":
		// The listener only ever produces fixed messages describing the shape of the
		// failure, never a captured value, so this is safe to surface verbatim.
		session.errorMessage = "Mirasim OAuth login failed: " + result.errorMessage
	case result.accessToken == "" || result.refreshToken == "":
		session.errorMessage = "Mirasim OAuth callback did not include renewable credentials"
	default:
		session.accessToken = result.accessToken
		session.refreshToken = result.refreshToken
	}
}

// expire drops an abandoned login and frees its loopback port.
func (c *oauthCoordinator) expire(state string) {
	c.mu.Lock()
	stale := c.removeLocked(state)
	c.mu.Unlock()
	closeLoopbackCaptures(stale)
}

// purgeLocked removes every expired session and returns the listeners its callers
// must close once they have released the mutex.
func (c *oauthCoordinator) purgeLocked(now time.Time) []*loopbackCapture {
	var stale []*loopbackCapture
	for state, session := range c.sessions {
		if session == nil || !now.Before(session.expiresAt) {
			stale = append(stale, c.removeLocked(state)...)
		}
	}
	return stale
}

func (c *oauthCoordinator) drainLocked() []*loopbackCapture {
	var stale []*loopbackCapture
	for state := range c.sessions {
		stale = append(stale, c.removeLocked(state)...)
	}
	return stale
}

func (c *oauthCoordinator) removeLocked(state string) []*loopbackCapture {
	session := c.sessions[state]
	delete(c.sessions, state)
	if session == nil {
		return nil
	}
	session.accessToken = ""
	session.refreshToken = ""
	if session.teardown != nil {
		session.teardown.Stop()
	}
	if session.capture == nil {
		return nil
	}
	return []*loopbackCapture{session.capture}
}

// resolveLoginProvider takes the first named candidate in precedence order and
// falls back to github. An explicitly requested but malformed provider is an
// error rather than something silently replaced by the default.
func resolveLoginProvider(candidates ...string) (string, error) {
	for _, candidate := range append(candidates, fallbackLoginProvider) {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		if !providerSlug.MatchString(candidate) {
			return "", fmt.Errorf("invalid Mirasim sign-in provider")
		}
		return candidate, nil
	}
	return "", fmt.Errorf("no Mirasim sign-in provider configured")
}

// metadataString reads one login metadata value. CPA forwards the
// /v0/management/mirasim-auth-url query string as StartLogin metadata, so a
// repeated query parameter arrives as a slice.
func metadataString(metadata map[string]any, key string) string {
	switch value := metadata[key].(type) {
	case string:
		return value
	case []string:
		if len(value) > 0 {
			return value[0]
		}
	}
	return ""
}

// unsupportedLoginProviderError names what Mirasim is actually offering, so an
// operator does not have to guess. requested has already passed providerSlug.
func unsupportedLoginProviderError(requested string, offered []loginProvider) error {
	ids := make([]string, 0, len(offered))
	for _, provider := range offered {
		ids = append(ids, provider.ID)
	}
	if len(ids) == 0 {
		return fmt.Errorf("Mirasim is not offering any sign-in provider right now")
	}
	sort.Strings(ids)
	return fmt.Errorf("Mirasim sign-in provider %q is not offered; available: %s", requested, strings.Join(ids, ", "))
}

// loopbackCallbackPort reads the pinned callback port. Anything outside 1-65535,
// including the unset default, means "take an ephemeral port".
func loopbackCallbackPort(value string) int {
	port, errParse := strconv.Atoi(strings.TrimSpace(value))
	if errParse != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

func randomOAuthValue(size int) (string, error) {
	raw := make([]byte, size)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func constantTimeEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminBaseURL validates the configured authentication service origin. Every
// route built against it, not just OAuth login, passes through here.
func adminBaseURL(adminURL string) (*url.URL, error) {
	base, errParse := url.Parse(strings.TrimRight(strings.TrimSpace(adminURL), "/"))
	if errParse != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.Opaque != "" || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid Mirasim authentication service URL")
	}
	base.Scheme = strings.ToLower(base.Scheme)
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("invalid Mirasim authentication service URL")
	}
	if base.Scheme == "http" && !isLoopbackHost(base.Hostname()) {
		return nil, fmt.Errorf("Mirasim authentication service URL must use HTTPS unless it is loopback")
	}
	return base, nil
}

func adminEndpoint(adminURL, resource string) (string, error) {
	base, errBase := adminBaseURL(adminURL)
	if errBase != nil {
		return "", errBase
	}
	base.Path = strings.TrimRight(base.Path, "/") + resource
	base.RawPath = ""
	return base.String(), nil
}

func buildMirasimOAuthURL(adminURL, provider, callbackURL, state string) (string, error) {
	base, errBase := adminBaseURL(adminURL)
	if errBase != nil {
		return "", errBase
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/auth/oauth/" + url.PathEscape(provider) + "/login"
	base.RawPath = ""
	query := base.Query()
	query.Set("redirect_uri", callbackURL)
	query.Set("state", state)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func oauthPollError(message string) pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}
}

func cloneAuthData(source pluginapi.AuthData) pluginapi.AuthData {
	out := source
	out.StorageJSON = append([]byte(nil), source.StorageJSON...)
	out.Metadata = make(map[string]any, len(source.Metadata))
	for key, value := range source.Metadata {
		out.Metadata[key] = value
	}
	out.Attributes = make(map[string]string, len(source.Attributes))
	for key, value := range source.Attributes {
		out.Attributes[key] = value
	}
	return out
}
