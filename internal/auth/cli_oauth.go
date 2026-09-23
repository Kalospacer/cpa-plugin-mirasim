package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
)

// cliManualPromptDelay is a variable only so tests can shorten the wait before
// the manual paste prompt appears.
var cliManualPromptDelay = 15 * time.Second

const manualPastePrompt = "Paste the Mirasim callback URL (or press Enter to keep waiting): "

// These are constant errors on purpose. The pasted value is operator-supplied
// and may carry a credential, so nothing derived from it may reach an error
// string, a log line or stderr — url.Parse in particular quotes the offending
// input in its message.
var (
	errInvalidCallbackURL   = errors.New("invalid Mirasim callback URL")
	errForeignCallbackURL   = errors.New("pasted URL is not this login's Mirasim callback address")
	errMissingCallbackToken = errors.New("Mirasim callback URL is missing access_token")
)

func (p *Provider) runLocalLogin(ctx context.Context, settings pluginconfig.Settings, provider string, proxyURL string, noBrowser bool) (pluginapi.AuthData, []byte, error) {
	loginProvider, errProvider := resolveLoginProvider(provider, settings.OAuthLoginProvider)
	if errProvider != nil {
		return pluginapi.AuthData{}, nil, errProvider
	}
	offered, errDiscovery := discoverLoginProviders(ctx, settings.AdminURL, proxyURL)
	if errDiscovery != nil {
		return pluginapi.AuthData{}, nil, errDiscovery
	}
	if !providerOffered(offered, loginProvider) {
		return pluginapi.AuthData{}, nil, unsupportedLoginProviderError(loginProvider, offered)
	}
	state, errState := randomOAuthValue(32)
	if errState != nil {
		return pluginapi.AuthData{}, nil, errState
	}
	// The same loopback capture the Management Center flow uses, so both paths
	// share one listener, one single-use random callback path, and one set of
	// credential checks.
	capture, errCapture := startLoopbackCapture(loopbackCallbackPort(settings.OAuthCallbackPort), state)
	if errCapture != nil {
		return pluginapi.AuthData{}, nil, errCapture
	}
	defer capture.Close()
	authURL, errURL := buildMirasimOAuthURL(settings.AdminURL, loginProvider, capture.CallbackURL(), state)
	if errURL != nil {
		return pluginapi.AuthData{}, nil, errURL
	}

	prompt := []byte("Open this URL to authenticate Mirasim:\n\n" + authURL + "\n\n")
	_, _ = os.Stdout.Write(prompt)
	if !noBrowser {
		_ = openBrowser(authURL)
	}
	timer := time.NewTimer(cliLoginTTL)
	defer timer.Stop()
	manualTimer := time.NewTimer(cliManualPromptDelay)
	defer manualTimer.Stop()
	// The prompt is served by the process's single stdin reader rather than by a
	// reader of this login's own: os.Stdin cannot be read with cancellation, so a
	// per-login reader would outlive every login that times out.
	var promptRequests chan<- stdinRequest
	var pasted chan stdinReply
	// abandoned tells the shared reader this login has stopped waiting, so a line
	// typed after a cancellation or a timeout is kept for the next prompt instead
	// of being dropped into a channel nobody is reading.
	abandoned := make(chan struct{})
	defer close(abandoned)
	for {
		select {
		case <-ctx.Done():
			return pluginapi.AuthData{}, nil, ctx.Err()
		case <-timer.C:
			return pluginapi.AuthData{}, nil, fmt.Errorf("Mirasim OAuth login timed out")
		case result := <-capture.Results():
			return p.finishLocalLogin(ctx, settings, proxyURL, state, result)
		case <-manualTimer.C:
			pasted = make(chan stdinReply)
			promptRequests = p.promptRequests()
		case promptRequests <- stdinRequest{kind: promptOAuthCallbackURL, reply: pasted, done: abandoned}:
			// Only ask once the reader is actually on this login's line.
			promptRequests = nil
			_, _ = os.Stdout.Write([]byte(manualPastePrompt))
		case reply := <-pasted:
			pasted = nil
			if reply.err != nil && reply.line == "" {
				return pluginapi.AuthData{}, nil, reply.err
			}
			result, okResult, errParse := parseManualOAuthResult(reply.line, capture.CallbackURL())
			if errParse != nil {
				return pluginapi.AuthData{}, nil, errParse
			}
			if okResult {
				// Mirasim 0.0.272 omits state from its token callback. The paste has
				// already been checked against this login's own callback address, so
				// bind a state-less result to this invocation before the
				// constant-time check in finishLocalLogin.
				bindMissingOAuthState(&result, state)
				return p.finishLocalLogin(ctx, settings, proxyURL, state, result)
			}
		}
	}
}

func (p *Provider) finishLocalLogin(ctx context.Context, settings pluginconfig.Settings, proxyURL, state string, result localOAuthResult) (pluginapi.AuthData, []byte, error) {
	if result.errorMessage != "" {
		return pluginapi.AuthData{}, nil, fmt.Errorf("Mirasim OAuth login failed: %s", result.errorMessage)
	}
	if !constantTimeEqual(state, strings.TrimSpace(result.state)) {
		return pluginapi.AuthData{}, nil, fmt.Errorf("Mirasim OAuth state mismatch")
	}
	storage, errStorage := p.finalizeOAuthStorage(ctx, settings, result.accessToken, result.refreshToken, proxyURL, nil)
	if errStorage != nil {
		return pluginapi.AuthData{}, nil, errStorage
	}
	result.accessToken, result.refreshToken = "", ""
	client := p.pool.Client(storage)
	fileName := storage.DefaultAuthFileName()
	auth := storage.AuthData(fileName, fileName, client.NextRefreshAfter(time.Now()))
	return auth, []byte("Mirasim authentication successful.\n"), nil
}

// parseManualOAuthResult reads a callback URL the operator pasted at the prompt,
// and accepts it only if it names this login's own callback address.
//
// That check is the paste's channel binding. Mirasim 0.0.272 omits the state it
// was handed, so a state-less paste carries nothing else tying it to the login in
// progress, and any URL an operator could be talked into pasting would otherwise
// install someone else's credentials as their own. The legitimate paste always
// passes: it is copied from the browser's address bar, which holds the
// redirect_uri this login just advertised, down to its 144-bit random path.
func parseManualOAuthResult(input, callbackURL string) (localOAuthResult, bool, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return localOAuthResult{}, false, nil
	}
	if !strings.Contains(value, "://") {
		// An address bar that hides the scheme still yields host, path and query.
		value = "http://" + value
	}
	parsed, errParse := url.Parse(value)
	if errParse != nil {
		return localOAuthResult{}, false, errInvalidCallbackURL
	}
	expected, errExpected := url.Parse(callbackURL)
	if errExpected != nil {
		return localOAuthResult{}, false, errInvalidCallbackURL
	}
	// Userinfo is not part of the authority, so a URL carrying it is refused
	// outright rather than compared: the address bar this paste is copied from
	// never holds one, and it keeps the comparison below exact.
	if parsed.User != nil || !strings.EqualFold(parsed.Host, expected.Host) || parsed.Path != expected.Path {
		return localOAuthResult{}, false, errForeignCallbackURL
	}
	values := parsed.Query()
	if parsed.Fragment != "" {
		if fragment, errFragment := url.ParseQuery(parsed.Fragment); errFragment == nil {
			for key, entries := range fragment {
				if values.Get(key) == "" && len(entries) > 0 {
					values.Set(key, entries[0])
				}
			}
		}
	}
	result := oauthResultFromValues(values)
	if result.accessToken == "" && result.errorMessage == "" {
		return localOAuthResult{}, false, errMissingCallbackToken
	}
	return result, true, nil
}

// stdinReply is one line read from standard input, or the error that ended the
// read.
type stdinReply struct {
	line string
	err  error
}

// stdinPromptKind names the question a prompt is asking. A retained line is
// only ever handed to a prompt asking the same question again: the operator
// typed a callback URL or a sign-in code, not "the next line", and the two are
// not interchangeable.
type stdinPromptKind int

const (
	promptOAuthCallbackURL stdinPromptKind = iota
	promptEmailCode
)

// retainedLineTTL bounds how long the reader holds a line whose prompt walked
// away. Keeping it is worth doing — the operator typed it once — but not worth
// doing forever: the line may be a callback URL carrying an access token, and a
// line handed over long after it was typed answers a question the operator no
// longer remembers asking. A variable only so tests can shorten it.
var retainedLineTTL = 2 * time.Minute

// stdinRequest is one prompt's claim on the shared reader. reply is unbuffered
// and done is closed once the prompt's owner has stopped waiting, which is what
// lets the reader tell a line that will never be taken from one that has simply
// not been typed yet. kind says what the prompt is asking for, so a line kept
// back for one question is never spent on another.
type stdinRequest struct {
	kind  stdinPromptKind
	reply chan<- stdinReply
	done  <-chan struct{}
}

// stdinPrompter serves every interactive prompt in the process from one
// goroutine. A blocking read of os.Stdin cannot be cancelled, so a prompt that
// starts a reader of its own leaves that reader — and its buffered reader —
// behind whenever the login it belongs to times out or is cancelled, one per
// login. This reader is created once, parks while no prompt is outstanding, and
// hands each line to the login that asked for it — or, if that login has given
// up by the time the line arrives, to the next one that prompts.
type stdinPrompter struct {
	requests chan stdinRequest
}

func newStdinPrompter(source io.Reader) *stdinPrompter {
	prompter := &stdinPrompter{requests: make(chan stdinRequest)}
	go prompter.serve(source)
	return prompter
}

// serve reads on demand only, so a plugin that never prompts never consumes a
// byte of the host's stdin.
func (s *stdinPrompter) serve(source io.Reader) {
	reader := bufio.NewReader(source)
	var pending *stdinReply
	var pendingKind stdinPromptKind
	var retention *time.Timer
	var retired <-chan time.Time
	var retainedUntil time.Time
	// forget drops a retained line and the bound that came with it. The line is
	// operator input and may carry a credential, so it is released rather than
	// left in the reader for a prompt that may never arrive.
	forget := func() {
		pending = nil
		retainedUntil = time.Time{}
		if retention != nil {
			retention.Stop()
			retention, retired = nil, nil
		}
	}
	for {
		var request stdinRequest
		var open bool
		select {
		case request, open = <-s.requests:
			if !open {
				forget()
				return
			}
		case <-retired:
			// Nothing asked for the retained line inside its lifetime.
			forget()
			continue
		}
		// A line kept for one question cannot answer another, and one kept too
		// long should not answer anything. Either way this prompt reads fresh:
		// losing a line costs the operator a retype, while spending it on the
		// wrong question loses it just the same and fails a login as well.
		if pending != nil && (request.kind != pendingKind || !time.Now().Before(retainedUntil)) {
			forget()
		}
		if pending == nil {
			line, errRead := reader.ReadString('\n')
			pending = &stdinReply{line: line, err: errRead}
			pendingKind = request.kind
		}
		select {
		case request.reply <- *pending:
			forget()
		case <-request.done:
			// The login that asked for this line gave up while the read was
			// blocked on it. Hold the line for the next prompt of the same kind
			// rather than discarding it: the operator typed it once, and
			// discarding it here left the next prompt waiting for a second line
			// it never asked for.
			if retention == nil {
				retention = time.NewTimer(retainedLineTTL)
				retired = retention.C
				retainedUntil = time.Now().Add(retainedLineTTL)
			}
		}
	}
}

var (
	processStdinOnce   sync.Once
	processStdinReader *stdinPrompter
)

// processStdinPrompts is created on first prompt, not at init, so a plugin that
// never runs an interactive login starts no goroutine at all.
func processStdinPrompts() *stdinPrompter {
	processStdinOnce.Do(func() { processStdinReader = newStdinPrompter(os.Stdin) })
	return processStdinReader
}

// promptRequests is the channel a waiting login hands its prompt request to.
// Tests substitute a prompter over a scripted reader; every real login shares
// the one reader over os.Stdin.
func (p *Provider) promptRequests() chan<- stdinRequest {
	if p != nil && p.prompter != nil {
		return p.prompter.requests
	}
	return processStdinPrompts().requests
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}

func flagBoolValue(flags map[string]pluginapi.CommandLineFlagValue, name string) bool {
	value, ok := flags[name]
	return ok && strings.EqualFold(strings.TrimSpace(value.Value), "true")
}
