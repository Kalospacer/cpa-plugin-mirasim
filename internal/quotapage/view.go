package quotapage

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/quota"
)

// view.go assembles everything the page renders. The layout mirrors the
// panel's own quota management view: a grid of credential cards carrying
// per-window progress bars, then the quota-window calendar grid. The
// credential JSON is read only for the three display fields the card needs
// (email, plan, plan expiry); secrets never enter any view value.

// pageView is the root of the template model.
type pageView struct {
	Accounts    []accountView
	Problem     string
	Empty       bool
	ReadAt      string
	ScriptNonce string
	TotalCount  int
	LoadedCount int
	WeekGrid    gridView
	HourGrid    gridView
	// Status carries the model-availability panel read from Mirasim's
	// official status page; on fetch failure only its Problem text renders.
	// It stays nil on the bare problem pages so those render no panel at all.
	Status *statusView
}

type accountView struct {
	Number      int
	Name        string
	Key         string
	PlanResetAt string
	Email       string
	Plan        string
	PlanExpires string
	PlanDays    string
	ResetCards  string
	Groups      []limitGroupView
	Others      []groupView
	Unavailable bool
	Empty       bool
}

// limitGroupView is one labeled group of limit bars, like the panel's
// "GEMINI 模型" block, with the member windows listed underneath.
type limitGroupView struct {
	Title   string
	Members string
	Bars    []limitBarView
}

// limitBarView is one window rendered as the panel's remaining-quota row:
// the bar fills with what is LEFT, so a healthy window reads as a full bar.
type limitBarView struct {
	Label       string
	RemainText  string
	RemainClass string
	WidthPct    string
	Reset       string
	ResetAt     string
	Desc        string
}

// groupView keeps a fallback table for limit groups the card does not
// structure yet, so a new window published by the relay is never dropped.
type groupView struct {
	DisplayName string
	Buckets     []bucketView
}

type bucketView struct {
	Window      string
	Remaining   string
	Reset       string
	ResetAt     string
	Description string
}

// credentialGlance is the only part of the stored credential JSON the page
// reads for the card: the three display fields the panel's own credential
// card shows. Nothing else is unmarshalled into the view.
type credentialGlance struct {
	Email         string `json:"email"`
	Plan          string `json:"plan"`
	PlanExpiresAt *int64 `json:"plan_exp"`
}

const (
	accountGroupName = "Account limits"
	modelGroupName   = "Model limits"
)

var weekdayNames = [7]string{"日", "一", "二", "三", "四", "五", "六"}

func (p *Page) buildView(ctx context.Context, host HostServices, client pluginapi.HostHTTPClient) pageView {
	view := pageView{ReadAt: time.Now().UTC().Format("2006-01-02 15:04 MST")}
	entries, errList := host.ListAuth(ctx)
	if errList != nil {
		view.Problem = problemCredentialList
		return view
	}
	lanes := make([]gridLane, 0, len(entries)*2)
	for _, entry := range entries {
		if !isMirasimAuth(entry) {
			continue
		}
		account, accountLanes := p.accountView(ctx, host, client, entry, len(view.Accounts)+1)
		view.Accounts = append(view.Accounts, account)
		lanes = append(lanes, accountLanes...)
		if !account.Unavailable && !account.Empty {
			view.LoadedCount++
		}
	}
	view.Empty = len(view.Accounts) == 0
	view.TotalCount = len(view.Accounts)
	now := time.Now().UTC()
	view.WeekGrid = buildGridView("week", lanes, now)
	view.HourGrid = buildGridView("hour", lanes, now)
	status := statusViewFor(ctx, client)
	view.Status = &status
	return view
}

func (p *Page) accountView(ctx context.Context, host HostServices, client pluginapi.HostHTTPClient, entry pluginapi.HostAuthFileEntry, number int) (accountView, []gridLane) {
	account := accountView{Number: number, ResetCards: "—"}
	account.Name = strings.TrimSpace(entry.Name)
	account.Key = entry.AuthIndex
	account.Email = strings.TrimSpace(entry.Email)

	auth, errGet := host.GetAuth(ctx, entry.AuthIndex)
	if errGet != nil || len(auth.JSON) == 0 {
		account.Unavailable = true
		return account, nil
	}
	var glance credentialGlance
	if errDecode := json.Unmarshal(auth.JSON, &glance); errDecode == nil {
		if email := strings.TrimSpace(glance.Email); email != "" {
			account.Email = email
		}
		account.Plan = strings.TrimSpace(glance.Plan)
		account.PlanExpires, account.PlanDays = planExpiresView(glance.PlanExpiresAt, time.Now().UTC())
		if glance.PlanExpiresAt != nil && *glance.PlanExpiresAt > 0 {
			account.PlanResetAt = time.Unix(*glance.PlanExpiresAt, 0).UTC().Format(time.RFC3339)
		}
	}

	response, errFetch := p.fetcher.FetchQuota(ctx, pluginapi.QuotaFetchRequest{
		AuthIndex:   entry.AuthIndex,
		AuthID:      entry.ID,
		Provider:    credentials.Provider,
		StorageJSON: auth.JSON,
		HTTPClient:  client,
	})
	if errFetch != nil {
		account.Unavailable = true
		return account, nil
	}
	if response.Subscription != nil {
		if plan := strings.TrimSpace(response.Subscription.Plan); plan != "" {
			account.Plan = plan
		}
	}
	// 每张可用重置卡对应一次主动重置；没有这项指标说明查卡失败或中转不支持，保持「—」。
	for _, metric := range response.Summary {
		if metric.Key == quota.ResetCardsMetricKey {
			account.ResetCards = fmt.Sprintf("%d 次", int(metric.Value))
			break
		}
	}

	label := account.Name
	if label == "" {
		label = account.Label()
	}
	lanes := make([]gridLane, 0, 4)
	for _, group := range response.Groups {
		if len(group.Buckets) == 0 {
			continue
		}
		name := strings.TrimSpace(group.DisplayName)
		switch name {
		case accountGroupName, modelGroupName:
			rendered := limitGroupView{Title: groupTitle(name)}
			members := make([]string, 0, len(group.Buckets))
			for _, bucket := range group.Buckets {
				members = append(members, strings.TrimSpace(bucket.Window))
				rendered.Bars = append(rendered.Bars, limitBarFromBucket(bucket, name == accountGroupName))
				if lane, okLane := laneFromBucket(label, bucket); okLane {
					lane.key = account.Key
					lanes = append(lanes, lane)
				}
			}
			rendered.Members = strings.Join(members, ", ")
			account.Groups = append(account.Groups, rendered)
		default:
			others := groupView{DisplayName: name}
			if others.DisplayName == "" {
				others.DisplayName = groupFallbackName
			}
			for _, bucket := range group.Buckets {
				reset, resetAt := formatReset(bucket.ResetTime)
				others.Buckets = append(others.Buckets, bucketView{
					Window:      valueOrDash(bucket.Window),
					Remaining:   formatPercent(bucket.RemainingFraction),
					Reset:       reset,
					ResetAt:     resetAt,
					Description: valueOrDash(bucket.Description),
				})
				if lane, okLane := laneFromBucket(label, bucket); okLane {
					lane.key = account.Key
					lanes = append(lanes, lane)
				}
			}
			account.Others = append(account.Others, others)
		}
	}
	account.Empty = len(account.Groups) == 0 && len(account.Others) == 0
	return account, lanes
}

// Label is how the card and the grid name the credential: the email when one
// is known, otherwise the account number the page assigned.
func (a accountView) Label() string {
	if strings.TrimSpace(a.Email) != "" {
		return strings.TrimSpace(a.Email)
	}
	return fmt.Sprintf("%s %d", accountWord, a.Number)
}

// groupTitle maps the provider's two known groups to the panel's section
// caption style; unknown groups keep their own name.
func groupTitle(name string) string {
	switch name {
	case accountGroupName:
		return "ACCOUNT 限额"
	case modelGroupName:
		return "MODEL 限额"
	default:
		return strings.ToUpper(name)
	}
}

var windowNamePattern = regexp.MustCompile(`^(\d+)\s*([hdHD])`)

// windowDuration resolves a window name ("5h", "7d", "7d_claude") into its
// length. A name without the numeric prefix does not reach the grid.
func windowDuration(name string) (time.Duration, bool) {
	match := windowNamePattern.FindStringSubmatch(strings.TrimSpace(name))
	if match == nil {
		return 0, false
	}
	amount, errParse := strconv.Atoi(match[1])
	if errParse != nil || amount <= 0 {
		return 0, false
	}
	if strings.EqualFold(match[2], "h") {
		return time.Duration(amount) * time.Hour, true
	}
	return time.Duration(amount) * 24 * time.Hour, true
}

// laneFromBucket places one bucket on the calendar when its reset time is a
// valid instant and its window name carries a length.
func laneFromBucket(label string, bucket pluginapi.QuotaBucket) (gridLane, bool) {
	duration, okDuration := windowDuration(bucket.Window)
	if !okDuration {
		return gridLane{}, false
	}
	end, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(bucket.ResetTime))
	if errParse != nil {
		return gridLane{}, false
	}
	end = end.UTC()
	remain := bucket.RemainingFraction
	if remain < 0 {
		remain = 0
	}
	if remain > 1 {
		remain = 1
	}
	laneLabel := windowLabel(bucket.Window, true) + " " + strconv.FormatFloat(remain*100, 'f', 0, 64) + "%"
	return gridLane{label: label, chip: strings.TrimSpace(bucket.Window), title: laneLabel, remaining: remain * 100,
		remainNote: strconv.FormatFloat(remain*100, 'f', 0, 64) + "% · ",
		duration:   duration, begin: end.Add(-duration), end: end}, true
}

func midnightUTC(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// limitBarFromBucket turns one normalized bucket into the panel's remaining-
// quota row: the fill is the fraction still available, so green means quota
// left. 与 CPA 一致：剩余至少 70% 为绿，至少 30% 为黄，其余为红。
func limitBarFromBucket(bucket pluginapi.QuotaBucket, accountScoped bool) limitBarView {
	remaining := bucket.RemainingFraction
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	reset, resetAt := formatReset(bucket.ResetTime)
	return limitBarView{
		Label:       windowLabel(bucket.Window, accountScoped),
		RemainText:  remainText(remaining, bucket.ResetTime),
		RemainClass: remainClass(remaining),
		WidthPct:    strconv.FormatFloat(remaining*100, 'f', 1, 64),
		Reset:       reset,
		ResetAt:     resetAt,
		Desc:        strings.TrimSpace(bucket.Description),
	}
}

// remainText mirrors the panel's "剩余 94%" / "额度可用" phrasing.
func remainText(remaining float64, resetTime string) string {
	if remaining <= 0 {
		return "已用尽"
	}
	if remaining >= 1 && strings.TrimSpace(resetTime) == "" {
		return "额度可用"
	}
	return "剩余 " + strconv.FormatFloat(remaining*100, 'f', 0, 64) + "%"
}

func remainClass(remaining float64) string {
	switch {
	case remaining < 0.3:
		return "hot"
	case remaining < 0.7:
		return "mid"
	default:
		return "low"
	}
}

// windowLabel gives the two account windows their panel names and leaves
// every other window under its published name.
func windowLabel(name string, accountScoped bool) string {
	trimmed := strings.TrimSpace(name)
	if accountScoped {
		switch trimmed {
		case "5h":
			return labelFiveHour
		case "7d":
			return labelWeekly
		}
	}
	return valueOrDash(trimmed)
}

// planExpiresView renders the stored plan expiry like the panel's card:
// "11/01 13:18 UTC" plus a day countdown. A credential without the claim
// stays a dash.
func planExpiresView(expiresAt *int64, now time.Time) (display, daysLeft string) {
	if expiresAt == nil || *expiresAt <= 0 {
		return "—", "—"
	}
	instant := time.Unix(*expiresAt, 0).UTC()
	days := int(instant.Sub(now).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return instant.Format("01/02 15:04") + " UTC", fmt.Sprintf(daysLeftFormat, days)
}

func valueOrDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—"
	}
	return value
}

func formatPercent(fraction float64) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	return fmt.Sprintf("%.1f%%", fraction*100)
}

// formatReset keeps UTC as a readable fallback and returns the instant for
// the browser to display in its own time zone and use for the countdown.
func formatReset(value string) (display, instant string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—", ""
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil {
		return value, ""
	}
	utc := parsed.UTC()
	// JavaScript dates have millisecond precision; keep the datetime within
	// the browser's standardized ISO 8601 parsing range.
	return utc.Format("2006-01-02 15:04 UTC"), utc.Truncate(time.Millisecond).Format(time.RFC3339Nano)
}
