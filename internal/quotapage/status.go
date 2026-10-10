package quotapage

import (
	"context"
	"encoding/json"
	"fmt"
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
	statusCellCount    = 48
)

// The document mirrors schema 2 of /api/status; fields the panel does not
// show are simply not declared here. Nullable numbers stay pointers so a
// model the API cannot measure yet renders "—" rather than a fake zero.
// cells values are per-mille availability (0–1000), -1 meaning no data.
type statusDoc struct {
	GeneratedAt string           `json:"generatedAt"`
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
	BarCells   []string
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
	BarCells   []string
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

func fetchStatusDoc(ctx context.Context, client pluginapi.HostHTTPClient) (*statusDoc, error) {
	statusCache.mu.Lock()
	doc, at := statusCache.doc, statusCache.at
	statusCache.mu.Unlock()
	if doc != nil && time.Since(at) < statusCacheTTL {
		return doc, nil
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
func statusViewFor(ctx context.Context, client pluginapi.HostHTTPClient) statusView {
	if client == nil {
		return statusView{Problem: problemStatusFetch}
	}
	doc, err := fetchStatusDoc(ctx, client)
	if err != nil {
		return statusView{Problem: problemStatusFetch}
	}
	view := statusView{GeneratedAt: statusGeneratedAt(doc.GeneratedAt), Notes: doc.Notes}
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
		view.Cohorts = append(view.Cohorts, cohortViewFor(cohort, meta, doc.Thresholds))
	}
	for _, cohort := range doc.Cohorts {
		if !seen[cohort.ID] {
			view.Cohorts = append(view.Cohorts, cohortViewFor(cohort, struct {
				ID    string
				Title string
				Desc  string
			}{ID: cohort.ID, Title: cohort.ID}, doc.Thresholds))
		}
	}
	return view
}

func cohortViewFor(cohort statusCohort, meta struct {
	ID    string
	Title string
	Desc  string
}, thresholds statusThresholds) statusCohortView {
	view := statusCohortView{ID: meta.ID, Title: meta.Title, Desc: meta.Desc}
	// A cohort counting nothing yet still gets its block, with the state the
	// API reported instead of an empty table.
	if state := strings.ToLower(strings.TrimSpace(cohort.State)); state != "" && state != "ok" {
		_, label := statusBadge(state)
		view.StateNote = "此分组" + label + "。"
	}
	for _, agent := range cohort.Agents {
		view.Agents = append(view.Agents, agentViewFor(agent, thresholds))
	}
	return view
}

func agentViewFor(agent statusAgent, thresholds statusThresholds) statusAgentView {
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
		BarCells:  statusCellClasses(summary.Cells, thresholds),
	}
	// 汇总条中央的标注就是官方页面显示的 24 小时可用率。
	if summary.Availability.H24 != nil {
		view.BarUptime = fmt.Sprintf("%.1f%% 可用", *summary.Availability.H24)
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
			BarCells:  statusCellClasses(model.Cells, thresholds),
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

// statusCellClasses maps one 24-hour cell row to the official bar's colour
// classes: per-mille availability ≥ good·10 is ok, ≥ warn·10 is warn, lower
// is down, and negative values mark cells with no data.
func statusCellClasses(cells []int, thresholds statusThresholds) []string {
	if len(cells) == 0 {
		return nil
	}
	good, warn := 990, 950
	if thresholds.Good != nil {
		good = int(*thresholds.Good * 10)
	}
	if thresholds.Warn != nil {
		warn = int(*thresholds.Warn * 10)
	}
	classes := make([]string, 0, len(cells))
	for _, cell := range cells {
		switch {
		case cell < 0:
			classes = append(classes, "none")
		case cell >= good:
			classes = append(classes, "ok")
		case cell >= warn:
			classes = append(classes, "warn")
		default:
			classes = append(classes, "down")
		}
	}
	return classes
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

// statusShare renders a failure/change share the official way: whole
// percentages, and "<1%" for sub-percent noise instead of "0%".
func statusShare(share float64) string {
	if share > 0 && share < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", share)
}

func statusPercent(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", *value)
}

// statusLatencyText renders the first-token latency the API reports, in seconds.
func statusLatencyText(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f s", *value)
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
