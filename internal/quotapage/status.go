package quotapage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// status.go powers the 模型可用性 panel with Mirasim's own status page data:
// https://mirasim.ai/zh/status renders from GET /api/status, which publishes
// per-model availability computed from real calls and refreshed every minute.
// Reading it costs no account quota, so the page no longer sends a probe
// request per model the way the retired one-click test did. The panel layout
// mirrors the official page: cohort tabs, then one card per agent carrying
// summary metrics, a 48-cell 24-hour availability bar and its model table.

const (
	statusURL          = "https://mirasim.ai/api/status"
	statusCacheTTL     = time.Minute
	statusFetchTimeout = 8 * time.Second
)

// The document mirrors schema 2 of /api/status; fields the panel does not
// show are simply not declared here. Nullable numbers stay pointers so a
// model the API cannot measure yet renders "—" rather than a fake zero.
// cells values are per-mille availability (0–1000), -1 meaning no data.
type statusDoc struct {
	GeneratedAt string           `json:"generatedAt"`
	CellsStart  string           `json:"cellsStart"`
	CellSeconds int              `json:"cellSeconds"`
	Thresholds  statusThresholds `json:"thresholds"`
	Cohorts     []statusCohort   `json:"cohorts"`
	Notes       []string         `json:"notes"`
}

type statusThresholds struct {
	Good *float64 `json:"good"`
	Warn *float64 `json:"warn"`
}

type statusCohort struct {
	ID     string        `json:"id"`
	State  string        `json:"state"`
	Agents []statusAgent `json:"agents"`
}

type statusAgent struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Summary statusSummary  `json:"summary"`
	Reasons []statusReason `json:"reasons"`
	Models  []statusModel  `json:"models"`
}

type statusSummary struct {
	Now          statusNow     `json:"now"`
	Availability statusWindows `json:"availability"`
	Latency      statusLatency `json:"latency"`
	SameModel    *float64      `json:"sameModel"`
	Intel        statusIntel   `json:"intel"`
	Cells        []int         `json:"cells"`
}

type statusNow struct {
	Status       string   `json:"status"`
	Availability *float64 `json:"availability"`
	Window       string   `json:"window"`
}

type statusWindows struct {
	H24 *float64 `json:"h24"`
	D7  *float64 `json:"d7"`
}

type statusLatency struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
}

type statusIntel struct {
	Swapped    *float64 `json:"swapped"`
	Mismatched *float64 `json:"mismatched"`
	Cut        *float64 `json:"cut"`
}

type statusReason struct {
	Class string  `json:"class"`
	Share float64 `json:"share"`
}

type statusModel struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Now          statusNow     `json:"now"`
	Availability statusWindows `json:"availability"`
	Latency      statusLatency `json:"latency"`
	SameModel    *float64      `json:"sameModel"`
	Cells        []int         `json:"cells"`
}

// statusView is the panel's template model: a tab bar of service cohorts and
// one card per agent inside each cohort.
type statusView struct {
	Problem     string
	GeneratedAt string
	Notes       []string
	Cohorts     []statusCohortView
}

type statusCohortView struct {
	ID        string
	Title     string
	Desc      string
	StateNote string
	Agents    []statusAgentView
}

type statusAgentView struct {
	Name       string
	StateClass string
	StateLabel string
	Now        string
	NowWindow  string
	H24        string
	D7         string
	P50        string
	P95        string
	Same       string
	BarTitle   string
	BarCells   []statusCellView
	BarUptime  string
	ReasonNote string
	IntelNote  string
	Models     []statusRowView
}

type statusRowView struct {
	Model      string
	StateClass string
	StateLabel string
	Now        string
	H24        string
	D7         string
	P50        string
	SameModel  string
	BarTitle   string
	BarCells   []statusCellView
}

// statusCellView is one half-hour slot of the availability bar: a colour
// class plus the official hover text (Beijing-time slot and availability).
type statusCellView struct {
	Class string
	Title string
}

// statusCohortMeta fixes the display order and wording the official page
// uses for its three service cohorts. A cohort id the API adds later still
// renders, appended after the known ones under its raw id.
var statusCohortMeta = []struct {
	ID    string
	Title string
	Desc  string
}{
	{"paid", "订阅服务", "本地使用已购套餐的模型调用。"},
	{"free", "体验服务", "本地使用体验或赠送额度的模型调用。"},
	{"cloud", "云端服务", "经协作空间或接入电脑发起的模型调用。"},
}

// statusCache is process-wide like processSegment: CPA rebuilds the Page on
// every config apply, and the upstream file regenerates once a minute, so a
// short shared TTL keeps repeated panel visits off the network without going
// stale between reloads.
var statusCache = struct {
	mu  sync.Mutex
	at  time.Time
	doc *statusDoc
}{}

func fetchStatusDoc(ctx context.Context, client pluginapi.HostHTTPClient, bust bool) (*statusDoc, error) {
	if !bust {
		statusCache.mu.Lock()
		doc, at := statusCache.doc, statusCache.at
		statusCache.mu.Unlock()
		if doc != nil && time.Since(at) < statusCacheTTL {
			return doc, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, statusFetchTimeout)
	defer cancel()
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     statusURL,
		Headers: http.Header{"Accept": {"application/json"}},
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Mirasim status answered HTTP %d", resp.StatusCode)
	}
	var fresh statusDoc
	if errDecode := json.Unmarshal(resp.Body, &fresh); errDecode != nil {
		return nil, fmt.Errorf("Mirasim status returned unreadable data: %w", errDecode)
	}

	statusCache.mu.Lock()
	statusCache.doc, statusCache.at = &fresh, time.Now()
	statusCache.mu.Unlock()
	return &fresh, nil
}

// statusViewFor turns the official document into the panel's view model. A
// failed fetch only marks this one panel; the quota cards around it render
// normally.
func statusViewFor(ctx context.Context, client pluginapi.HostHTTPClient, bust bool) statusView {
	if client == nil {
		return statusView{Problem: problemStatusFetch}
	}
	doc, err := fetchStatusDoc(ctx, client, bust)
	if err != nil {
		return statusView{Problem: problemStatusFetch}
	}
	view := statusView{GeneratedAt: statusGeneratedAt(doc.GeneratedAt), Notes: doc.Notes}
	bars := statusBarClock{start: statusCellsStart(doc.CellsStart), seconds: doc.CellSeconds, thresholds: doc.Thresholds}
	byID := make(map[string]statusCohort, len(doc.Cohorts))
	for _, cohort := range doc.Cohorts {
		byID[cohort.ID] = cohort
	}
	seen := make(map[string]bool, len(doc.Cohorts))
	for _, meta := range statusCohortMeta {
		cohort, ok := byID[meta.ID]
		if !ok {
			continue
		}
		seen[meta.ID] = true
		view.Cohorts = append(view.Cohorts, cohortViewFor(cohort, meta, bars))
	}
	for _, cohort := range doc.Cohorts {
		if !seen[cohort.ID] {
			view.Cohorts = append(view.Cohorts, cohortViewFor(cohort, struct {
				ID    string
				Title string
				Desc  string
			}{ID: cohort.ID, Title: cohort.ID}, bars))
		}
	}
	return view
}

func cohortViewFor(cohort statusCohort, meta struct {
	ID    string
	Title string
	Desc  string
}, bars statusBarClock) statusCohortView {
	view := statusCohortView{ID: meta.ID, Title: meta.Title, Desc: meta.Desc}
	// A cohort counting nothing yet still gets its block, with the state the
	// API reported instead of an empty table.
	if state := strings.ToLower(strings.TrimSpace(cohort.State)); state != "" && state != "ok" {
		_, label := statusBadge(state)
		view.StateNote = "此分组" + label + "。"
	}
	for _, agent := range cohort.Agents {
		view.Agents = append(view.Agents, agentViewFor(agent, bars))
	}
	return view
}

func agentViewFor(agent statusAgent, bars statusBarClock) statusAgentView {
	name := strings.TrimSpace(agent.Name)
	if name == "" {
		name = agent.ID
	}
	summary := agent.Summary
	class, label := agentBadge(summary.Now.Status)
	view := statusAgentView{
		Name: name, StateClass: class, StateLabel: label,
		Now:       statusPercent(summary.Now.Availability),
		NowWindow: statusWindowLabel(summary.Now.Window),
		H24:       statusPercent(summary.Availability.H24),
		D7:        statusPercent(summary.Availability.D7),
		P50:       statusLatencyText(summary.Latency.P50),
		P95:       statusLatencyText(summary.Latency.P95),
		Same:      statusPercent(summary.SameModel),
		BarTitle:  statusBarTitle(name, summary.Availability.H24, summary.Cells, bars.thresholds),
		BarCells:  bars.cells(summary.Cells),
	}
	// 汇总条中央的标注就是官方页面显示的 24 小时可用率。
	if summary.Availability.H24 != nil {
		view.BarUptime = statusAvailability(*summary.Availability.H24) + " 可用"
	}
	// 官方页面会给非正常的智能体标出近 1 小时失败原因。
	if class != "ok" && len(agent.Reasons) > 0 {
		parts := make([]string, 0, len(agent.Reasons))
		for _, reason := range agent.Reasons {
			parts = append(parts, statusReasonLabel(reason.Class)+" "+statusShare(reason.Share))
		}
		view.ReasonNote = "近 1 小时失败原因：" + strings.Join(parts, " · ")
	}
	// 近 24 小时与所选模型不一致的调用占比，官方以「模型或配置变化」列出。
	intelParts := make([]string, 0, 3)
	for _, pair := range []struct {
		label string
		share *float64
	}{
		{"模型切换", summary.Intel.Swapped},
		{"型号不符", summary.Intel.Mismatched},
		{"能力降级", summary.Intel.Cut},
	} {
		if pair.share != nil && *pair.share > 0 {
			intelParts = append(intelParts, pair.label+" "+statusShare(*pair.share))
		}
	}
	if len(intelParts) > 0 {
		view.IntelNote = "模型或配置变化（近 24 小时）：" + strings.Join(intelParts, " · ")
	}
	for _, model := range agent.Models {
		modelName := strings.TrimSpace(model.Name)
		if modelName == "" {
			modelName = model.ID
		}
		modelClass, modelLabel := statusBadge(model.Now.Status)
		view.Models = append(view.Models, statusRowView{
			Model: modelName, StateClass: modelClass, StateLabel: modelLabel,
			Now:       statusPercent(model.Now.Availability),
			H24:       statusPercent(model.Availability.H24),
			D7:        statusPercent(model.Availability.D7),
			P50:       statusLatencyText(model.Latency.P50),
			SameModel: statusPercent(model.SameModel),
			BarTitle:  statusBarTitle(modelName, model.Availability.H24, model.Cells, bars.thresholds),
			BarCells:  bars.cells(model.Cells),
		})
	}
	return view
}

// statusBadge maps a model-level state to the badge classes and wording the
// official page uses. Values the API may add later render as neutral grey
// rather than a wrong colour.
func statusBadge(status string) (class, label string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return "ok", statusLabelOK
	case "degraded":
		return "warn", statusLabelDegraded
	case "down":
		return "down", statusLabelDown
	case "sparse":
		return "none", statusLabelSparse
	case "pending":
		return "none", statusLabelPending
	case "unavailable":
		return "none", statusLabelUnreadable
	default:
		return "none", statusLabelNone
	}
}

// agentBadge is the same mapping with the official agent-level wording
// (部分模型… / 运行正常 instead of 异常/正常).
func agentBadge(status string) (class, label string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return "ok", agentLabelOK
	case "degraded":
		return "warn", agentLabelDegraded
	case "down":
		return "down", agentLabelDown
	default:
		return statusBadge(status)
	}
}

// statusBarClock carries what every availability bar needs to colour and
// caption its cells: the cell grid's start instant, slot width and the
// good/warn thresholds the API reports.
type statusBarClock struct {
	start      time.Time
	seconds    int
	thresholds statusThresholds
}

// cells maps one 24-hour cell row to view cells: per-mille availability ≥
// good·10 is ok, ≥ warn·10 is warn, lower is down, negative marks a slot
// with no data. The hover title mirrors the official page: the slot's
// Beijing-time range and its availability.
func (b statusBarClock) cells(cells []int) []statusCellView {
	if len(cells) == 0 {
		return nil
	}
	good, warn := 990, 950
	if b.thresholds.Good != nil {
		good = int(*b.thresholds.Good * 10)
	}
	if b.thresholds.Warn != nil {
		warn = int(*b.thresholds.Warn * 10)
	}
	out := make([]statusCellView, 0, len(cells))
	for i, cell := range cells {
		var class string
		switch {
		case cell < 0:
			class = "none"
		case cell >= good:
			class = "ok"
		case cell >= warn:
			class = "warn"
		default:
			class = "down"
		}
		out = append(out, statusCellView{Class: class, Title: b.title(i, cell)})
	}
	return out
}

// title renders one cell's official hover text: "05:30–06:00 北京时间 · 可用率
// 98.5%". A slot with no data reports 100% there, the same quirk the
// official page has, while still drawing grey.
func (b statusBarClock) title(index, cell int) string {
	availability := cell
	if availability < 0 {
		availability = 1000
	}
	pct := statusAvailability(float64(availability) / 10)
	if b.start.IsZero() || b.seconds <= 0 {
		return "可用率 " + pct
	}
	begin := b.start.Add(time.Duration(index*b.seconds) * time.Second)
	end := begin.Add(time.Duration(b.seconds) * time.Second)
	return statusBeijingHM(begin) + "–" + statusBeijingHM(end) + " 北京时间 · 可用率 " + pct
}

// statusBarTitle builds the whole-bar hover summary the official page shows:
// name, 24-hour availability and the ok/warn/down counts across the slots.
// Slots with no data count as ok, the same quirk the official page has.
func statusBarTitle(name string, h24 *float64, cells []int, thresholds statusThresholds) string {
	if len(cells) == 0 {
		return ""
	}
	uptime := 100.0
	if h24 != nil {
		uptime = *h24
	}
	good, warn := 990, 950
	if thresholds.Good != nil {
		good = int(*thresholds.Good * 10)
	}
	if thresholds.Warn != nil {
		warn = int(*thresholds.Warn * 10)
	}
	var ok, degraded, down int
	for _, cell := range cells {
		switch {
		case cell < 0 || cell >= good:
			ok++
		case cell >= warn:
			degraded++
		default:
			down++
		}
	}
	return fmt.Sprintf("%s 最近 24 小时可用率 %s。%d 个 30 分钟时段中：正常 %d，不稳定 %d，异常 %d。",
		name, statusAvailability(uptime), len(cells), ok, degraded, down)
}

// statusReasonLabel maps the status page's failure classes to its own zh
// wording; an unknown class shows its raw id so a new class is never hidden.
func statusReasonLabel(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "capacity":
		return "上游繁忙"
	case "outage":
		return "平台或上游故障"
	case "throttle":
		return "上游限流"
	case "other":
		return "其他"
	default:
		return class
	}
}

// statusWindowLabel renders the API's window strings the way the official
// page captions them.
func statusWindowLabel(window string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "15m":
		return "近 15 分钟"
	case "1h":
		return "近 1 小时"
	default:
		if trimmed := strings.TrimSpace(window); trimmed != "" {
			return "近 " + trimmed
		}
		return ""
	}
}

// statusShare renders a failure/change share the official way: "<1%" for
// sub-percent noise, integers whole, everything else one decimal.
func statusShare(share float64) string {
	if share > 0 && share < 1 {
		return "<1%"
	}
	if share == math.Trunc(share) {
		return fmt.Sprintf("%.0f%%", share)
	}
	return fmt.Sprintf("%.1f%%", share)
}

// statusAvailability formats a percentage the official way: 100 shows as
// "100%", everything else keeps one decimal.
func statusAvailability(value float64) string {
	if value >= 100 {
		return "100%"
	}
	return fmt.Sprintf("%.1f%%", value)
}

func statusPercent(value *float64) string {
	if value == nil {
		return "—"
	}
	return statusAvailability(*value)
}

// statusLatencyText renders the first-token latency the API reports, in
// seconds.
func statusLatencyText(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f s", *value)
}

// statusBeijingHM formats an instant as HH:MM Beijing time (UTC+8) the way
// the official page labels its cells.
func statusBeijingHM(value time.Time) string {
	return value.Add(8 * time.Hour).UTC().Format("15:04")
}

func statusCellsStart(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func statusGeneratedAt(raw string) string {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			return trimmed
		}
		return "—"
	}
	return parsed.UTC().Format("2006-01-02 15:04")
}
