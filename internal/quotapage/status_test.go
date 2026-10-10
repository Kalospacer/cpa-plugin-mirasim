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
	"cellsStart": "2026-10-09T05:00:00.000Z",
	"cellSeconds": 1800,
	"thresholds": {"good": 99, "warn": 95, "minTurns": 20},
	"cohorts": [
		{
			"id": "free", "state": "ok",
			"agents": [{
				"id": "claude-code", "name": "Claude",
				"summary": {
					"now": {"status": "down", "availability": 32.5, "window": "15m"},
					"availability": {"h24": 39.2, "d7": 69.1},
					"latency": {"p50": 28, "p95": 96.7},
					"sameModel": 99.9,
					"intel": {"full": 99.9, "swapped": 0.1, "mismatched": 0, "cut": 0},
					"cells": [1000, 980, 940, -1]
				},
				"reasons": [{"class": "throttle", "share": 98}, {"class": "outage", "share": 2}, {"class": "capacity", "share": 0.4}],
				"models": [{
					"id": "claude-opus-5-5", "name": "opus 5.5",
					"now": {"status": "down", "availability": 32.2, "window": "15m"},
					"availability": {"h24": 45.8, "d7": 74.4},
					"latency": {"p50": 30.8},
					"sameModel": 99.9,
					"cells": [0, 955, 999, -1]
				}]
			}]
		},
		{
			"id": "paid", "state": "ok",
			"agents": [{
				"id": "kimi-code", "name": "Kimi",
				"summary": {
					"now": {"status": "ok", "availability": 100, "window": "15m"},
					"availability": {"h24": 100, "d7": 100},
					"latency": {"p50": 3, "p95": 12},
					"sameModel": 100,
					"intel": {"full": 100},
					"cells": []
				},
				"models": [{
					"id": "kimi-k3", "name": "k3",
					"now": {"status": "ok", "availability": 100, "window": "15m"},
					"availability": {"h24": null, "d7": null},
					"latency": {"p50": null},
					"sameModel": null,
					"cells": []
				}]
			}]
		},
		{
			"id": "enterprise", "state": "ok",
			"agents": [{
				"id": "codex", "name": "GPT",
				"summary": {
					"now": {"status": "sparse", "availability": null, "window": "15m"},
					"availability": {"h24": null, "d7": null},
					"latency": {"p50": null, "p95": null},
					"cells": []
				},
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
	view := statusViewFor(context.Background(), fixtureStatusClient(), false)

	if len(view.Cohorts) != 3 {
		t.Fatalf("cohorts = %d, want 3", len(view.Cohorts))
	}
	wantIDs := []string{"paid", "free", "enterprise"}
	wantTitles := []string{"订阅服务", "体验服务", "enterprise"}
	for i := range wantIDs {
		if view.Cohorts[i].ID != wantIDs[i] || view.Cohorts[i].Title != wantTitles[i] {
			t.Fatalf("cohort %d = %q %q, want %q %q", i, view.Cohorts[i].ID, view.Cohorts[i].Title, wantIDs[i], wantTitles[i])
		}
	}
	if view.GeneratedAt != "2026-10-10 04:48" {
		t.Fatalf("generatedAt = %q", view.GeneratedAt)
	}

	free := view.Cohorts[1]
	if len(free.Agents) != 1 {
		t.Fatalf("free agents = %d", len(free.Agents))
	}
	claude := free.Agents[0]
	if claude.StateClass != "down" || claude.StateLabel != agentLabelDown {
		t.Fatalf("agent badge = %q %q", claude.StateClass, claude.StateLabel)
	}
	want := map[string]string{
		"now": "32.5%", "h24": "39.2%", "d7": "69.1%",
		"p50": "28.0 s", "p95": "96.7 s", "same": "99.9%",
		"window": "近 15 分钟", "uptime": "39.2% 可用",
	}
	got := map[string]string{
		"now": claude.Now, "h24": claude.H24, "d7": claude.D7,
		"p50": claude.P50, "p95": claude.P95, "same": claude.Same,
		"window": claude.NowWindow, "uptime": claude.BarUptime,
	}
	for field, wantValue := range want {
		if got[field] != wantValue {
			t.Fatalf("agent %s = %q, want %q", field, got[field], wantValue)
		}
	}
	// 千分比格子的三档配色加无数据灰，悬停文案带北京时间与可用率。
	cellClasses := make([]string, 0, len(claude.BarCells))
	for _, cell := range claude.BarCells {
		cellClasses = append(cellClasses, cell.Class)
	}
	if got := strings.Join(cellClasses, ","); got != "ok,warn,down,none" {
		t.Fatalf("agent bar cells = %q", got)
	}
	if claude.BarCells[0].Title != "13:00–13:30 北京时间 · 可用率 100%" {
		t.Fatalf("cell title = %q", claude.BarCells[0].Title)
	}
	if claude.BarCells[1].Title != "13:30–14:00 北京时间 · 可用率 98.0%" {
		t.Fatalf("cell title = %q", claude.BarCells[1].Title)
	}
	if !strings.Contains(claude.BarTitle, "正常 2，不稳定 1，异常 1") {
		t.Fatalf("bar title = %q", claude.BarTitle)
	}
	if !strings.Contains(claude.ReasonNote, "上游限流 98%") || !strings.Contains(claude.ReasonNote, "上游繁忙 <1%") {
		t.Fatalf("reason note = %q", claude.ReasonNote)
	}
	if !strings.Contains(claude.IntelNote, "模型切换 <1%") {
		t.Fatalf("intel note = %q", claude.IntelNote)
	}

	row := claude.Models[0]
	if row.Model != "opus 5.5" || row.StateClass != "down" || row.StateLabel != statusLabelDown {
		t.Fatalf("model row = %+v", row)
	}
	rowClasses := make([]string, 0, len(row.BarCells))
	for _, cell := range row.BarCells {
		rowClasses = append(rowClasses, cell.Class)
	}
	if got := strings.Join(rowClasses, ","); got != "down,warn,ok,none" {
		t.Fatalf("model bar cells = %q", got)
	}

	paid := view.Cohorts[0].Agents[0]
	if paid.StateLabel != agentLabelOK || len(paid.BarCells) != 0 || paid.ReasonNote != "" {
		t.Fatalf("ok agent = %+v", paid)
	}
	for _, value := range []string{paid.Models[0].H24, paid.Models[0].D7, paid.Models[0].P50, paid.Models[0].SameModel} {
		if value != "—" {
			t.Fatalf("nullable metric = %q, want —", value)
		}
	}

	// 没有模型的智能体只渲染汇总卡片，不出模型表。
	extra := view.Cohorts[2].Agents[0]
	if extra.StateLabel != statusLabelSparse || len(extra.Models) != 0 {
		t.Fatalf("model-less agent = %+v", extra)
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
		view := statusViewFor(context.Background(), client, false)
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
	if _, err := fetchStatusDoc(context.Background(), client, false); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := fetchStatusDoc(context.Background(), client, false); err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("two reads inside the TTL must hit the network once, hit %d", client.calls)
	}
	statusCache.mu.Lock()
	statusCache.at = time.Now().Add(-2 * statusCacheTTL)
	statusCache.mu.Unlock()
	if _, err := fetchStatusDoc(context.Background(), client, false); err != nil {
		t.Fatalf("stale fetch: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("an expired cache must refetch, calls = %d", client.calls)
	}
	if _, err := fetchStatusDoc(context.Background(), client, true); err != nil {
		t.Fatalf("bust fetch: %v", err)
	}
	if client.calls != 3 {
		t.Fatalf("the refresh button must bypass a fresh cache, calls = %d", client.calls)
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
		`id="model-status"`, `data-cohort="paid"`, `data-cohort="free"`, "订阅服务", "体验服务",
		"部分模型异常", "opus 5.5", "上游限流 98%", "ms-bar", "2026-10-10 04:48", "mirasim.ai/zh/status",
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
