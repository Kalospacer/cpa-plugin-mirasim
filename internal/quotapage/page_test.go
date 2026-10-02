package quotapage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const testBasePath = "/v0/resource/plugins/mirasim"

type fakeFetcher struct {
	responses map[string]pluginapi.QuotaFetchResponse
	errs      map[string]error
	requests  []pluginapi.QuotaFetchRequest
}

func (f *fakeFetcher) FetchQuota(_ context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	f.requests = append(f.requests, req)
	if errFetch := f.errs[req.AuthIndex]; errFetch != nil {
		return pluginapi.QuotaFetchResponse{}, errFetch
	}
	return f.responses[req.AuthIndex], nil
}

type fakeHTTPClient struct{}

func (fakeHTTPClient) Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return pluginapi.HTTPResponse{}, nil
}

func (fakeHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, nil
}

type fakeHost struct {
	entries   []pluginapi.HostAuthFileEntry
	auths     map[string]pluginapi.HostAuthGetResponse
	listErr   error
	getErrs   map[string]error
	client    pluginapi.HostHTTPClient
	listCalls int
	getCalls  []string
}

func (h *fakeHost) ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	h.listCalls++
	if h.listErr != nil {
		return nil, h.listErr
	}
	return h.entries, nil
}

func (h *fakeHost) GetAuth(_ context.Context, authIndex string) (pluginapi.HostAuthGetResponse, error) {
	h.getCalls = append(h.getCalls, authIndex)
	if errGet := h.getErrs[authIndex]; errGet != nil {
		return pluginapi.HostAuthGetResponse{}, errGet
	}
	auth, ok := h.auths[authIndex]
	if !ok {
		return pluginapi.HostAuthGetResponse{}, errors.New("no such auth")
	}
	return auth, nil
}

func (h *fakeHost) HTTPClient() pluginapi.HostHTTPClient { return h.client }

func quotaPageURL(page *Page) string { return testBasePath + page.Resource().Path }

func quotaRequest(page *Page) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: http.MethodGet, Path: quotaPageURL(page)}
}

func mustServe(t *testing.T, page *Page, req pluginapi.ManagementRequest, host HostServices) pluginapi.ManagementResponse {
	t.Helper()
	resp, errServe := page.Serve(context.Background(), req, host)
	if errServe != nil {
		t.Fatalf("Serve error = %v", errServe)
	}
	return resp
}

func TestResourceIsAMenuRouteWithAStableUnpredictableSegment(t *testing.T) {
	first := New(&fakeFetcher{})
	second := New(&fakeFetcher{})

	route := first.Resource()
	if route.Menu != menuLabel {
		t.Fatalf("menu = %q, want %q", route.Menu, menuLabel)
	}
	if route.Description == "" {
		t.Fatal("resource route has no description")
	}
	if !strings.HasPrefix(route.Path, routePrefix) {
		t.Fatalf("path = %q, want prefix %q", route.Path, routePrefix)
	}
	segment := strings.TrimPrefix(route.Path, routePrefix)
	// rand.Text carries 128 bits, so anything this short would mean the segment
	// stopped being unguessable.
	if len(segment) < 16 {
		t.Fatalf("segment = %q, want at least 16 characters", segment)
	}
	// A config apply re-runs plugin.Build; the URL a panel may already have
	// open must not move, so every page in the process shares one segment.
	if second.Resource().Path != route.Path {
		t.Fatalf("pages in one process disagree on the segment: %q vs %q", route.Path, second.Resource().Path)
	}
}

func TestOwnsMatchesOnlyItsOwnSegment(t *testing.T) {
	page := New(&fakeFetcher{})
	path := quotaPageURL(page)
	secret := strings.TrimPrefix(page.Resource().Path, routePrefix)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"own path", path, true},
		{"login resource", testBasePath + "/oauth/start", false},
		{"other segment", testBasePath + "/quota/other", false},
		{"secret behind another prefix", testBasePath + "/oauth/" + secret, false},
		{"trailing slash", path + "/", false},
		{"extra segment after the secret", path + "/extra", false},
		{"bare segment without prefix", secret, false},
		{"empty", "", false},
		{"root", "/", false},
	}
	for _, testCase := range cases {
		if got := page.Owns(testCase.path); got != testCase.want {
			t.Fatalf("%s: Owns(%q) = %v, want %v", testCase.name, testCase.path, got, testCase.want)
		}
	}
}

func TestPageRendersAccountAndModelGroupsWithoutCredentials(t *testing.T) {
	planExp := time.Now().Add(29*24*time.Hour + time.Hour).Unix()
	authJSON := fmt.Sprintf(`{"type":"mirasim","access_token":"access-secret","refresh_token":"refresh-secret",`+
		`"device_private_key":"device-secret","email":"operator@example.com","plan":"pro","plan_exp":%d}`, planExp)
	// Resets are relative to now so the calendar segments stay inside the
	// visible two weeks no matter when the test runs.
	reset5h := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	reset7d := time.Now().UTC().Add(2*24*time.Hour + 30*time.Minute).Format(time.RFC3339)
	fetcher := &fakeFetcher{responses: map[string]pluginapi.QuotaFetchResponse{
		"a1": {
			Subscription: &pluginapi.QuotaSubscription{Plan: "pro", TierName: "paid"},
			Groups: []pluginapi.QuotaGroup{
				{DisplayName: "Account limits", Buckets: []pluginapi.QuotaBucket{
					{Window: "5h", RemainingFraction: 0.42, ResetTime: reset5h, Description: "57.5% used · ok"},
					{Window: "7d", RemainingFraction: 0.86, ResetTime: reset7d},
				}},
				{DisplayName: "Model limits", Buckets: []pluginapi.QuotaBucket{
					{Window: "7d_claude", RemainingFraction: 0, ResetTime: reset7d},
				}},
			},
		},
	}}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{{
			ID: "id-1", AuthIndex: "a1", Provider: "mirasim", Type: "mirasim",
			Success: 1760, Failed: 149,
			RecentRequests: []pluginapi.HostRecentRequestEntry{
				{Time: "t1", Success: 1}, {Time: "t2", Success: 1}, {Time: "t3", Failed: 1},
			},
		}},
		auths:  map[string]pluginapi.HostAuthGetResponse{"a1": {AuthIndex: "a1", JSON: []byte(authJSON)}},
		client: fakeHTTPClient{},
	}
	page := New(fetcher)

	resp := mustServe(t, page, quotaRequest(page), host)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	body := string(resp.Body)
	for _, want := range []string{
		"operator@example.com", `class="card-name"`, `class="plan-value">pro<`, "续期时间", "天后",
		"个凭证", "个已加载", "5小时限额", "剩余 42%", `width:42.0%`, `bar mid`,
		"周限额", "剩余 86%", `width:86.0%`, "MODEL 限额", "7d_claude", "已用尽", `bar hot`, `width:0.0%`,
		"此分组包含：7d_claude", "配额窗口", "两周", `class="gh-top">日<`,
		`class="seg cur"`, `class="seg next"`, `class="seg past"`, `class="track"`, `class="lane-head"`,
		`data-view="week"`, `data-view="hour"`, "按周", "5小时", "nowline",
		"data-until", "小时后刷新", "分钟后刷新", "Date.now()", "data-refresh", "location.reload()",
		`id="toast"`, "sessionStorage", "mq-refreshed", "data-nav", "window-fill",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page does not contain %q:\n%s", want, body)
		}
	}
	// Nothing from the credential JSON's secrets may reach the browser. The
	// email is the card's identity and is intentionally shown, the way the
	// panel's own credential card shows it; tokens and keys stay out.
	for _, secret := range []string{
		"access-secret", "refresh-secret", "device-secret",
		"access_token", "refresh_token", "device_private_key",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("page leaked %q:\n%s", secret, body)
		}
	}
	if contentType := resp.Headers.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("content type = %q", contentType)
	}
	if policy := resp.Headers.Get("Content-Security-Policy"); !strings.Contains(policy, "frame-ancestors 'self'") {
		t.Fatalf("CSP does not allow the panel iframe: %q", policy)
	}
	// The page needs a script for browser-local time and a live countdown;
	// allow only this script, using the per-response nonce in the CSP.
	scriptNonce := regexp.MustCompile(`<script nonce="([A-Za-z0-9]+)">`).FindStringSubmatch(body)
	if len(scriptNonce) != 2 {
		t.Fatalf("page has no nonced script:\n%s", body)
	}
	if policy := resp.Headers.Get("Content-Security-Policy"); !strings.Contains(policy, "script-src 'nonce-"+scriptNonce[1]+"'") || strings.Contains(policy, "script-src 'unsafe-inline'") {
		t.Fatalf("CSP does not allow exactly the page script: %q", policy)
	}
	if cache := resp.Headers.Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("cache control = %q", cache)
	}

	if len(fetcher.requests) != 1 {
		t.Fatalf("fetcher calls = %d, want 1", len(fetcher.requests))
	}
	request := fetcher.requests[0]
	if request.AuthIndex != "a1" || request.AuthID != "id-1" || request.Provider != "mirasim" {
		t.Fatalf("fetch request = %#v", request)
	}
	if string(request.StorageJSON) != authJSON {
		t.Fatalf("fetch request did not carry the stored JSON: %s", request.StorageJSON)
	}
	if request.HTTPClient == nil {
		t.Fatal("fetch request did not carry the host HTTP client")
	}
}

func TestPageLeavesMissingOrInvalidResetWithoutCountdown(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]pluginapi.QuotaFetchResponse{
		"a1": {Groups: []pluginapi.QuotaGroup{{DisplayName: "Account limits", Buckets: []pluginapi.QuotaBucket{
			{Window: "offset", ResetTime: "2026-09-20T16:00:00.123456789+08:00"},
			{Window: "missing"},
			{Window: "invalid", ResetTime: "unknown"},
		}}}},
	}}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", Provider: "mirasim"}},
		auths:   map[string]pluginapi.HostAuthGetResponse{"a1": {AuthIndex: "a1", JSON: []byte(`{"type":"mirasim"}`)}},
		client:  fakeHTTPClient{},
	}
	page := New(fetcher)
	resp := mustServe(t, page, quotaRequest(page), host)
	body := string(resp.Body)
	if !strings.Contains(body, `datetime="2026-09-20T08:00:00.123Z"`) {
		t.Fatalf("offset reset was not normalized to one instant:\n%s", body)
	}
	if count := strings.Count(body, `<time `); count != 1 {
		t.Fatalf("time element count = %d, want only the valid reset:\n%s", count, body)
	}
	// Progress-bar rows for missing/invalid resets keep their name and show the
	// reset value as unavailable; they get no <time> element and no live
	// countdown.
	for _, fragment := range []string{
		`<span class="limit-name" title="missing">missing</span>`,
		`<span class="limit-name" title="invalid">invalid</span>`,
		`<span class="limit-until">—</span>`,
		`<span class="limit-until">unknown</span>`,
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("progress bar for a reset-less window is missing %q:\n%s", fragment, body)
		}
	}
}

// One unreadable credential must not take the whole page down: the other
// accounts still render, and the failing one is marked unavailable.
func TestPageKeepsAnUnreadableCredentialAsUnavailable(t *testing.T) {
	fetcher := &fakeFetcher{
		responses: map[string]pluginapi.QuotaFetchResponse{
			"a2": {Groups: []pluginapi.QuotaGroup{{DisplayName: "Account limits", Buckets: []pluginapi.QuotaBucket{
				{Window: "1d", RemainingFraction: 1},
			}}}},
		},
		errs: map[string]error{"a1": errors.New("upstream token secret boom")},
	}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			{AuthIndex: "a1", Provider: "mirasim"},
			{AuthIndex: "a2", Provider: "mirasim"},
		},
		auths: map[string]pluginapi.HostAuthGetResponse{
			"a1": {AuthIndex: "a1", JSON: []byte(`{"type":"mirasim"}`)},
			"a2": {AuthIndex: "a2", JSON: []byte(`{"type":"mirasim"}`)},
		},
		client: fakeHTTPClient{},
	}
	page := New(fetcher)

	resp := mustServe(t, page, quotaRequest(page), host)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "账户 1") || !strings.Contains(body, "账户 2") {
		t.Fatalf("both accounts should render:\n%s", body)
	}
	if !strings.Contains(body, unavailableAccount) {
		t.Fatalf("unavailable account is not marked:\n%s", body)
	}
	if !strings.Contains(body, "1d") {
		t.Fatalf("readable account is missing its window:\n%s", body)
	}
	if strings.Contains(body, "boom") {
		t.Fatalf("page leaked the fetch error:\n%s", body)
	}
	if len(fetcher.requests) != 2 {
		t.Fatalf("fetcher calls = %d, want 2", len(fetcher.requests))
	}
}

func TestPageReadsOnlyMirasimCredentials(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]pluginapi.QuotaFetchResponse{
		"a1": {Groups: []pluginapi.QuotaGroup{{DisplayName: "Account limits", Buckets: []pluginapi.QuotaBucket{
			{Window: "5h", RemainingFraction: 1},
		}}}},
	}}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			{AuthIndex: "x1", Provider: "openai", Type: "openai"},
			{AuthIndex: "a1", Provider: "mirasim", Type: "mirasim"},
		},
		auths:  map[string]pluginapi.HostAuthGetResponse{"a1": {AuthIndex: "a1", JSON: []byte(`{"type":"mirasim"}`)}},
		client: fakeHTTPClient{},
	}
	page := New(fetcher)

	resp := mustServe(t, page, quotaRequest(page), host)
	body := string(resp.Body)
	if !strings.Contains(body, "账户 1") || strings.Contains(body, "账户 2") {
		t.Fatalf("only the Mirasim credential should render:\n%s", body)
	}
	if len(host.getCalls) != 1 || host.getCalls[0] != "a1" {
		t.Fatalf("GetAuth calls = %#v, want only a1", host.getCalls)
	}
	if len(fetcher.requests) != 1 || fetcher.requests[0].AuthIndex != "a1" {
		t.Fatalf("fetch requests = %#v", fetcher.requests)
	}
}

func TestPageReportsAnUnreadableCredentialList(t *testing.T) {
	host := &fakeHost{listErr: errors.New("host auth list down"), client: fakeHTTPClient{}}
	page := New(&fakeFetcher{})

	resp := mustServe(t, page, quotaRequest(page), host)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body := string(resp.Body); !strings.Contains(body, problemCredentialList) || strings.Contains(body, "账户 1") {
		t.Fatalf("page should state the list failure and render no account:\n%s", body)
	}
}

func TestPageShowsAnAccountWithoutPublishedWindows(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]pluginapi.QuotaFetchResponse{"a1": {}}}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", Provider: "mirasim"}},
		auths:   map[string]pluginapi.HostAuthGetResponse{"a1": {AuthIndex: "a1", JSON: []byte(`{"type":"mirasim"}`)}},
		client:  fakeHTTPClient{},
	}
	page := New(fetcher)

	resp := mustServe(t, page, quotaRequest(page), host)
	if body := string(resp.Body); !strings.Contains(body, emptyAccount) {
		t.Fatalf("page should say the account publishes no windows:\n%s", body)
	}
}

func TestPageShowsNoCredentialMessage(t *testing.T) {
	host := &fakeHost{client: fakeHTTPClient{}}
	page := New(&fakeFetcher{})

	resp := mustServe(t, page, quotaRequest(page), host)
	if body := string(resp.Body); !strings.Contains(body, emptyAccounts) {
		t.Fatalf("page should say no credential is installed:\n%s", body)
	}
}

func TestPageIsUnavailableWithoutHostCallbacks(t *testing.T) {
	cases := map[string]HostServices{
		"no host":           nil,
		"no HTTP client":    &fakeHost{},
		"empty HTTP client": &fakeHost{client: nil},
	}
	for name, host := range cases {
		page := New(&fakeFetcher{})
		resp := mustServe(t, page, quotaRequest(page), host)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, resp.StatusCode)
		}
		if body := string(resp.Body); !strings.Contains(body, problemNoCallbacks) {
			t.Fatalf("%s: page does not name the missing callbacks:\n%s", name, body)
		}
	}
}

func TestPageAnswersOnlyItsOwnGet(t *testing.T) {
	fetcher := &fakeFetcher{}
	host := &fakeHost{client: fakeHTTPClient{}}
	page := New(fetcher)

	requests := []pluginapi.ManagementRequest{
		{Method: http.MethodPost, Path: quotaPageURL(page)},
		{Method: http.MethodHead, Path: quotaPageURL(page)},
		{Method: ""},
	}
	for _, req := range requests {
		resp := mustServe(t, page, req, host)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %q: status = %d, want 404", req.Method, req.Path, resp.StatusCode)
		}
		if body := string(resp.Body); !strings.Contains(body, problemNotFound) {
			t.Fatalf("%s %q: page = %s", req.Method, req.Path, body)
		}
	}
	if len(fetcher.requests) != 0 {
		t.Fatalf("refused requests reached the fetcher: %#v", fetcher.requests)
	}
}

// A missing client must stop the page before it reads any credential: the
// callbacks it would otherwise use are the same ones the client came from.
func TestPageDoesNotReadCredentialsWithoutAClient(t *testing.T) {
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", Provider: "mirasim"}},
		auths:   map[string]pluginapi.HostAuthGetResponse{"a1": {AuthIndex: "a1", JSON: []byte(`{"type":"mirasim"}`)}},
	}
	page := New(&fakeFetcher{})

	resp := mustServe(t, page, quotaRequest(page), host)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if host.listCalls != 0 || len(host.getCalls) != 0 {
		t.Fatalf("host reads = %d list, %#v get; want none", host.listCalls, host.getCalls)
	}
}
