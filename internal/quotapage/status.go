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
// request per model the way the retired one-click test did.

const (
	statusURL          = "https://mirasim.ai/api/status"
	statusCacheTTL     = time.Minute
	statusFetchTimeout = 8 * time.Second
)

// The document mirrors schema 2 of /api/status; fields the panel does not
// show are simply not declared here. Nullable numbers stay pointers so a
// model the API cannot measure yet renders "—" rather than a fake zero.
type statusDoc struct {
	GeneratedAt string         `json:"generatedAt"`
	Cohorts     []statusCohort `json:"cohorts"`
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
	Now statusNow `json:"now"`
}

type statusNow struct {
	Status       string   `json:"status"`
	Availability *float64 `json:"availability"`
}

type statusReason struct {
	Class string  `json:"class"`
	Share float64 `json:"share"`
}

type statusModel struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Now          statusNow `json:"now"`
	Availability struct {
		H24 *float64 `json:"h24"`
		D7  *float64 `json:"d7"`
	} `json:"availability"`
	Latency struct {
		P50 *float64 `json:"p50"`
	} `json:"latency"`
	SameModel *float64 `json:"sameModel"`
}

// statusView is the panel's template model: one block per service cohort,
// each holding a flat model table plus a note line for agents that reported
// failure reasons.
type statusView struct {
	Problem     string
	GeneratedAt string
	Cohorts     []statusCohortView
}

type statusCohortView struct {
	Title string
	Desc  string
	Rows  []statusRowView
	Notes []string
}

type statusRowView struct {
	Agent      string
	Model      string
	StateClass string
	StateLabel string
	Now        string
	H24        string
	D7         string
	P50        string
	SameModel  string
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
	view := statusView{GeneratedAt: statusGeneratedAt(doc.GeneratedAt)}
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
		view.Cohorts = append(view.Cohorts, cohortViewFor(cohort, meta.Title, meta.Desc))
	}
	for _, cohort := range doc.Cohorts {
		if !seen[cohort.ID] {
			view.Cohorts = append(view.Cohorts, cohortViewFor(cohort, cohort.ID, ""))
		}
	}
	return view
}

func cohortViewFor(cohort statusCohort, title, desc string) statusCohortView {
	view := statusCohortView{Title: title, Desc: desc}
	// A cohort counting nothing yet still gets its block, with the state the
	// API reported instead of an empty table.
	if state := strings.ToLower(strings.TrimSpace(cohort.State)); state != "" && state != "ok" {
		_, label := statusBadge(state)
		view.Notes = append(view.Notes, "此分组"+label+"。")
	}
	for _, agent := range cohort.Agents {
		name := strings.TrimSpace(agent.Name)
		if name == "" {
			name = agent.ID
		}
		if len(agent.Models) == 0 {
			class, label := statusBadge(agent.Summary.Now.Status)
			view.Rows = append(view.Rows, statusRowView{
				Agent: name, Model: "—", StateClass: class, StateLabel: label,
				Now: statusPercent(agent.Summary.Now.Availability),
				H24: "—", D7: "—", P50: "—", SameModel: "—",
			})
		}
		for _, model := range agent.Models {
			class, label := statusBadge(model.Now.Status)
			modelName := strings.TrimSpace(model.Name)
			if modelName == "" {
				modelName = model.ID
			}
			view.Rows = append(view.Rows, statusRowView{
				Agent: name, Model: modelName, StateClass: class, StateLabel: label,
				Now:       statusPercent(model.Now.Availability),
				H24:       statusPercent(model.Availability.H24),
				D7:        statusPercent(model.Availability.D7),
				P50:       statusLatency(model.Latency.P50),
				SameModel: statusPercent(model.SameModel),
			})
		}
		// 官方页面会给非正常的智能体标出近 1 小时失败原因，保留在分组下方；
		// 占比不到 1% 的噪声项不列。
		if class, _ := statusBadge(agent.Summary.Now.Status); class != "ok" && len(agent.Reasons) > 0 {
			parts := make([]string, 0, len(agent.Reasons))
			for _, reason := range agent.Reasons {
				if reason.Share >= 1 {
					parts = append(parts, fmt.Sprintf("%s %.0f%%", statusReasonLabel(reason.Class), reason.Share))
				}
			}
			if len(parts) > 0 {
				view.Notes = append(view.Notes, name+"：近 1 小时失败原因 — "+strings.Join(parts, "、"))
			}
		}
	}
	return view
}

// statusBadge maps the status page's per-model state to the panel's badge.
// Values the API may add later render as neutral grey rather than a wrong
// colour.
func statusBadge(status string) (class, label string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return "ok", statusLabelOK
	case "degraded":
		return "degraded", statusLabelDegraded
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

func statusPercent(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", *value)
}

// statusLatency renders the first-token p50 the API reports, in seconds.
func statusLatency(value *float64) string {
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
