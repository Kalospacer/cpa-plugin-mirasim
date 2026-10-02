package quotapage

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"strings"
	"testing"
	"time"
)

func TestWindowDurationParsesPrefixedNames(t *testing.T) {
	for _, c := range []struct {
		name string
		want time.Duration
		ok   bool
	}{{"5h", 5 * time.Hour, true}, {"7d", 168 * time.Hour, true}, {"7d_claude", 168 * time.Hour, true}, {"weekly", 0, false}, {"0h", 0, false}} {
		got, ok := windowDuration(c.name)
		if got != c.want || ok != c.ok {
			t.Fatalf("%s: %v %v", c.name, got, ok)
		}
	}
}
func TestPlanExpiresView(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	d, left := planExpiresView(nil, now)
	if d != "—" || left != "—" {
		t.Fatal(d, left)
	}
	future := now.Add(29*24*time.Hour + 30*time.Minute).Unix()
	d, left = planExpiresView(&future, now)
	if d != "10/31 16:30 UTC" || left != "29天后" {
		t.Fatal(d, left)
	}
}
func TestCPAMeterThresholds(t *testing.T) {
	for _, c := range []struct {
		n    float64
		want string
	}{{0, "hot"}, {.299, "hot"}, {.3, "mid"}, {.699, "mid"}, {.7, "low"}, {1, "low"}} {
		if got := remainClass(c.n); got != c.want {
			t.Fatal(c.n, got)
		}
	}
}
func fixtureLane(t *testing.T, code string, remaining float64, end time.Time) gridLane {
	t.Helper()
	l, ok := laneFromBucket("account", pluginapi.QuotaBucket{Window: code, RemainingFraction: remaining, ResetTime: end.Format(time.RFC3339)})
	if !ok {
		t.Fatal(code)
	}
	return l
}
func TestCPAOneCredentialOneTrack(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	lanes := []gridLane{fixtureLane(t, "5h", .85, now.Add(2*time.Hour)), fixtureLane(t, "7d", .59, now.Add(48*time.Hour)), fixtureLane(t, "7d_claude", .9, now.Add(24*time.Hour)), fixtureLane(t, "7d_fable", 1, now.Add(24*time.Hour))}
	for _, mode := range []string{"week", "hour"} {
		g := buildGridView(mode, lanes, now)
		if len(g.Rows) != 1 || len(g.Rows[0].Titles) != 4 {
			t.Fatalf("%s rows/titles: %#v", mode, g)
		}
		want := "7d"
		count := 14
		if mode == "hour" {
			want = "5h"
			count = 12
		}
		if g.Rows[0].Period != want || len(g.Heads) != count || g.NowPct == "" {
			t.Fatalf("%s: %#v", mode, g)
		}
		if len(g.Rows[0].Windows) == 0 {
			t.Fatal("missing windows")
		}
	}
	week := buildGridView("week", lanes, now)
	if week.Heads[0].Top != "日" || week.Heads[0].Bottom != "09/27" || week.Heads[13].Bottom != "10/10" {
		t.Fatal(week.Heads)
	}
	// 周限额优先于先重置的模型限额，不能悄悄换成 Claude 额度。
	if !strings.Contains(week.Rows[0].Windows[1].Note, "59%") {
		t.Fatal(week.Rows[0].Windows)
	}
}
func TestProjectedWindowsStayWholeAcrossColumns(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	l := fixtureLane(t, "7d", .59, now.Add(48*time.Hour))
	g := buildGridView("week", []gridLane{l}, now)
	live := 0
	for _, w := range g.Rows[0].Windows {
		if w.Class == "cur" {
			live++
			if w.WidthPct != "50.000" || w.FillPct != "41.0" {
				t.Fatal(w)
			}
		}
	}
	if live != 1 {
		t.Fatal("one current window must be one DOM bar", live)
	}
}
func TestNoRemainingReusedAfterReset(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	l := fixtureLane(t, "5h", .85, now.Add(-time.Hour))
	g := buildGridView("hour", []gridLane{l}, now)
	for _, w := range g.Rows[0].Windows {
		if w.FillPct != "" {
			t.Fatalf("stale API quota reused: %#v", w)
		}
	}
}
func TestHourModeRequiresFiveHours(t *testing.T) {
	now := time.Now()
	l := fixtureLane(t, "7d", .59, now.Add(48*time.Hour))
	if g := buildGridView("hour", []gridLane{l}, now); len(g.Rows) != 0 {
		t.Fatal(g)
	}
}
func TestCredentialsWithSameEmailStaySeparate(t *testing.T) {
	now := time.Now()
	a := fixtureLane(t, "7d", .59, now.Add(48*time.Hour))
	b := a
	a.key = "one"
	b.key = "two"
	if g := buildGridView("week", []gridLane{a, b}, now); len(g.Rows) != 2 {
		t.Fatal(g.Rows)
	}
}
func TestLimitBarShowsRemaining(t *testing.T) {
	b := limitBarFromBucket(pluginapi.QuotaBucket{Window: "5h", RemainingFraction: .425}, true)
	if b.WidthPct != "42.5" || b.RemainClass != "mid" {
		t.Fatal(b)
	}
}
func TestGridEmpty(t *testing.T) {
	if g := buildGridView("week", nil, time.Now()); len(g.Rows) != 0 {
		t.Fatal(g)
	}
}
