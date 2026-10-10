package quotapage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const statusFixture = `{
	"schema": 2,
	"generatedAt": "2026-10-10T04:48:01.880Z",
	"cohorts": [
		{
			"id": "free", "state": "ok",
			"agents": [{
				"id": "claude-code", "name": "Claude",
				"summary": {"now": {"status": "down", "availability": 32.5, "window": "15m"}},
				"reasons": [{"class": "throttle", "share": 99.4}, {"class": "capacity", "share": 0.6}],
				"models": [{
					"id": "claude-opus-5-5", "name": "opus 5.5",
					"now": {"status": "down", "availability": 32.2, "window": "15m"},
					"availability": {"h24": 45.8, "d7": 74.4},
					"latency": {"p50": 30.8},
					"sameModel": 99.9
				}]
			}]
		},
		{
			"id": "paid", "state": "ok",
			"agents": [{
				"id": "kimi-code", "name": "Kimi",
				"summary": {"now": {"status": "ok", "availability": 100, "window": "15m"}},
				"models": [{
					"id": "kimi-k3", "name": "k3",
					"now": {"status": "ok", "availability": 100, "window": "15m"},
					"availability": {"h24": null, "d7": null},
					"latency": {"p50": null},
					"sameModel": null
				}]
			}]
		},
		{
			"id": "enterprise", "state": "ok",
			"agents": [{
				"id": "codex", "name": "GPT",
				"summary": {"now": {"status": "sparse", "availability": null, "window": "15m"}},
				"models": []
			}]
		}
	]
}`

type statusHTTPClient struct {
	calls int
	resp  pluginapi.HTTPResponse
	err   error
}

func (c *statusHTTPClient) Do(_ context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	c.calls++
	return c.resp, c.err
}

func (c *statusHTTPClient) DoStream(_ context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, nil
}

func fixtureStatusClient() *statusHTTPClient {
	return &statusHTTPClient{resp: pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       []byte(statusFixture),
	}}
}

func resetStatusCache(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		statusCache.mu.Lock()
		statusCache.doc, statusCache.at = nil, time.Time{}
		statusCache.mu.Unlock()
	})
	statusCache.mu.Lock()
	statusCache.doc, statusCache.at = nil, time.Time{}
	statusCache.mu.Unlock()
}

func TestStatusViewOrdersCohortsLikeTheOfficialPage(t *testing.T) {
	resetStatusCache(t)
	view := statusViewFor(context.Background(), fixtureStatusClient())

	if len(view.Cohorts) != 3 {
		t.Fatalf("cohorts = %d, want 3", len(view.Cohorts))
	}
	wantTitles := []string{"订阅服务", "体验服务", "enterprise"}
	for i, want := range wantTitles {
		if view.Cohorts[i].Title != want {
			t.Fatalf("cohort %d title = %q, want %q", i, view.Cohorts[i].Title, want)
		}
	}
	if view.GeneratedAt != "2026-10-10 04:48" {
		t.Fatalf("generatedAt = %q", view.GeneratedAt)
	}

	free := view.Cohorts[1]
	if len(free.Rows) != 1 {
		t.Fatalf("free rows = %d, want 1", len(free.Rows))
	}
	row := free.Rows[0]
	if row.Agent != "Claude" || row.Model != "opus 5.5" {
		t.Fatalf("row identity = %q %q", row.Agent, row.Model)
	}
	if row.StateClass != "down" || row.StateLabel != statusLabelDown {
		t.Fatalf("badge = %q %q", row.StateClass, row.StateLabel)
	}
	want := map[string]string{"now": "32.2%", "h24": "45.8%", "d7": "74.4%", "p50": "30.8 s", "same": "99.9%"}
	got := map[string]string{"now": row.Now, "h24": row.H24, "d7": row.D7, "p50": row.P50, "same": row.SameModel}
	for field, wantValue := range want {
		if got[field] != wantValue {
			t.Fatalf("%s = %q, want %q", field, got[field], wantValue)
		}
	}
	if len(free.Notes) != 1 || !strings.Contains(free.Notes[0], "上游限流") || !strings.Contains(free.Notes[0], "Claude") {
		t.Fatalf("notes = %v", free.Notes)
	}

	paid := view.Cohorts[0]
	if len(paid.Rows) != 1 || paid.Rows[0].StateClass != "ok" || paid.Rows[0].StateLabel != statusLabelOK {
		t.Fatalf("paid row = %+v", paid.Rows)
	}
	for _, value := range []string{paid.Rows[0].H24, paid.Rows[0].D7, paid.Rows[0].P50, paid.Rows[0].SameModel} {
		if value != "—" {
			t.Fatalf("nullable metric = %q, want —", value)
		}
	}
	if len(paid.Notes) != 0 {
		t.Fatalf("an all-ok cohort must not list failure reasons: %v", paid.Notes)
	}

	// 没有模型的智能体也要有一行，用汇总状态顶上。
	extra := view.Cohorts[2]
	if len(extra.Rows) != 1 || extra.Rows[0].Model != "—" || extra.Rows[0].StateLabel != statusLabelSparse {
		t.Fatalf("model-less agent row = %+v", extra.Rows)
	}
}

func TestStatusViewKeepsOnlyTheProblemOnFetchFailure(t *testing.T) {
	resetStatusCache(t)
	for _, client := range []*statusHTTPClient{
		{err: errors.New("dial timeout")},
		{resp: pluginapi.HTTPResponse{StatusCode: http.StatusBadGateway}},
		{resp: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte("{not json")}},
	} {
		resetStatusCache(t)
		view := statusViewFor(context.Background(), client)
		if view.Problem != problemStatusFetch {
			t.Fatalf("problem = %q, want the fetch failure text", view.Problem)
		}
		if len(view.Cohorts) != 0 {
			t.Fatalf("a failed fetch must render no cohorts, got %d", len(view.Cohorts))
		}
	}
}

func TestStatusDocIsCachedForTheUpstreamMinute(t *testing.T) {
	resetStatusCache(t)
	client := fixtureStatusClient()
	if _, err := fetchStatusDoc(context.Background(), client); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := fetchStatusDoc(context.Background(), client); err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("two reads inside the TTL must hit the network once, hit %d", client.calls)
	}
	statusCache.mu.Lock()
	statusCache.at = time.Now().Add(-2 * statusCacheTTL)
	statusCache.mu.Unlock()
	if _, err := fetchStatusDoc(context.Background(), client); err != nil {
		t.Fatalf("stale fetch: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("an expired cache must refetch, calls = %d", client.calls)
	}
}

func TestPageRendersTheStatusPanelNextToQuota(t *testing.T) {
	resetStatusCache(t)
	host := &fakeHost{client: fixtureStatusClient()}
	page := New(&fakeFetcher{})

	resp := mustServe(t, page, quotaRequest(page), host)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	body := string(resp.Body)
	for _, want := range []string{
		`id="model-status"`, "订阅服务", "体验服务", "opus 5.5", "异常", "正常",
		"32.2%", "30.8 s", "上游限流", "2026-10-10 04:48", "mirasim.ai/zh/status",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page does not contain %q:\n%s", want, body)
		}
	}
}

func TestPageStillRendersQuotaWhenStatusFetchFails(t *testing.T) {
	resetStatusCache(t)
	host := &fakeHost{client: &statusHTTPClient{err: errors.New("no route to host")}}
	page := New(&fakeFetcher{})

	resp := mustServe(t, page, quotaRequest(page), host)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, `id="model-status"`) || !strings.Contains(body, problemStatusFetch) {
		t.Fatalf("the status panel must carry its own problem text:\n%s", body)
	}
	if !strings.Contains(body, emptyAccounts) {
		t.Fatalf("a status fetch failure must not break the quota part of the page:\n%s", body)
	}
}
