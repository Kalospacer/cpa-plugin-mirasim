package quotapage

import (
	"fmt"
	"strconv"
	"time"
)

// 时间轴移植自管理中心 quotaTimelineModel.ts：一个凭证只选一个窗口，
// 每个窗口在完整时间轨道上定位，不再按日期格拆成碎片。浏览器会用本地时区重算。
type gridLane struct {
	key, label, chip, title, remainNote string
	remaining                           float64
	duration                            time.Duration
	begin, end                          time.Time
}

type gridView struct {
	Kind, RangeLabel, NowPct string
	Heads                    []gridHeadCell
	Rows                     []gridRowView
}
type gridHeadCell struct{ Top, Bottom string }
type laneTitleView struct{ Chip, Text string }
type sourceWindow struct {
	Code       string
	DurationMS int64
	ResetAt    string
	Remaining  string
}
type gridRowView struct {
	Key, Label, Period string
	Titles             []laneTitleView
	Sources            []sourceWindow
	Windows            []gridSegView
}
type gridSegView struct {
	Class, LeftPct, WidthPct, Note, Tooltip, FillPct string
}

// 与 CPA 相同，周视图从所在周的星期日开始，小时视图从今天开始。
func buildGridView(kind string, lanes []gridLane, now time.Time) gridView {
	if len(lanes) == 0 {
		return gridView{}
	}
	start := midnightUTC(now)
	count, width, spanLabel := 14, 24*time.Hour, "两周"
	if kind == "hour" {
		count, width, spanLabel = 12, 6*time.Hour, "三天"
	} else {
		start = start.Add(-time.Duration(start.Weekday()) * 24 * time.Hour)
	}
	end := start.Add(time.Duration(count) * width)
	grid := gridView{Kind: kind, RangeLabel: start.Format("01/02") + " – " + end.Add(-time.Second).Format("01/02") + " · " + spanLabel + " · 当前"}
	grid.NowPct = strconv.FormatFloat(float64(now.Sub(start))/float64(end.Sub(start))*100, 'f', 2, 64)
	for i := 0; i < count; i++ {
		at := start.Add(time.Duration(i) * width)
		h := gridHeadCell{Top: weekdayNames[at.Weekday()], Bottom: at.Format("01/02")}
		if kind == "hour" && at.Hour() != 0 {
			h.Top = ""
			h.Bottom = at.Format("15:04")
		}
		grid.Heads = append(grid.Heads, h)
	}
	groups := map[string][]gridLane{}
	order := []string{}
	for _, l := range lanes {
		key := l.key
		if key == "" {
			key = l.label
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], l)
	}
	for _, key := range order {
		all := groups[key]
		row := gridRowView{Key: key, Label: all[0].label}
		for _, l := range all {
			row.Titles = append(row.Titles, laneTitleView{Chip: l.chip, Text: l.title})
			row.Sources = append(row.Sources, sourceWindow{Code: l.chip, DurationMS: l.duration.Milliseconds(), ResetAt: l.end.Format(time.RFC3339Nano), Remaining: strconv.FormatFloat(l.remaining, 'f', 1, 64)})
		}
		chosen, ok := pickTimelineWindow(kind, all)
		if ok {
			if chosen.duration < 24*time.Hour {
				row.Period = fmt.Sprintf("%dh", int(chosen.duration/time.Hour))
			} else {
				row.Period = fmt.Sprintf("%dd", int(chosen.duration/(24*time.Hour)))
			}
			row.Windows = projectWindows(chosen, start, end, now, kind)
		}
		// 小时模式只保留确有 5h 窗口的凭证，不把周限额伪装成短窗口。
		if kind == "hour" && !ok {
			continue
		}
		grid.Rows = append(grid.Rows, row)
	}
	return grid
}

// Mirasim 的账户 7d/5h 优先于同周期模型窗口，避免模型额度替换账户额度。
func pickTimelineWindow(kind string, lanes []gridLane) (gridLane, bool) {
	preferred := "7d"
	if kind == "hour" {
		preferred = "5h"
	}
	for _, l := range lanes {
		if l.chip == preferred {
			return l, true
		}
	}
	var best gridLane
	found := false
	for _, l := range lanes {
		if kind == "hour" && l.duration != 5*time.Hour {
			continue
		}
		if !found || l.duration > best.duration || (l.duration == best.duration && l.end.Before(best.end)) {
			best = l
			found = true
		}
	}
	return best, found
}

// 对齐 CPA windowsIn/projectLane：跨整条轨道算 left/width，重置后的
// 预测窗口不沿用当前窗口的剩余量。只当前 API 窗口有用量填充。
func projectWindows(lane gridLane, start, end, now time.Time, kind string) []gridSegView {
	period := lane.duration
	if period <= 0 {
		return nil
	}
	boundary := lane.end.Add(time.Duration(floorDiv(start.Sub(lane.end), period)) * period)
	windows := []gridSegView{}
	for boundary.Add(-period).Before(end) {
		begin := boundary.Add(-period)
		if boundary.After(start) && begin.Before(end) {
			leftAt, rightAt := begin, boundary
			if leftAt.Before(start) {
				leftAt = start
			}
			if rightAt.After(end) {
				rightAt = end
			}
			left := float64(leftAt.Sub(start)) / float64(end.Sub(start)) * 100
			width := float64(rightAt.Sub(leftAt)) / float64(end.Sub(start)) * 100
			state := "next"
			if !boundary.After(now) {
				state = "past"
			} else if !begin.After(now) {
				state = "cur"
			}
			timeFormat := "01/02 15:04"
			threshold := 9.0
			if kind == "hour" {
				timeFormat = "15:04"
				threshold = 4.5
			}
			note := boundary.Format(timeFormat)
			fill := ""
			if state == "cur" && boundary.Equal(lane.end) {
				note = lane.remainNote + note
				fill = strconv.FormatFloat(100-lane.remaining, 'f', 1, 64)
			}
			tooltip := begin.Format("2006-01-02 15:04 UTC") + " → " + boundary.Format("2006-01-02 15:04 UTC")
			if width <= threshold {
				note = ""
			}
			windows = append(windows, gridSegView{Class: state, LeftPct: strconv.FormatFloat(left, 'f', 3, 64), WidthPct: strconv.FormatFloat(width, 'f', 3, 64), Note: note, Tooltip: tooltip, FillPct: fill})
		}
		boundary = boundary.Add(period)
	}
	return windows
}

func floorDiv(a, b time.Duration) int {
	q := int(a / b)
	if a < 0 && a%b != 0 {
		q--
	}
	return q
}
