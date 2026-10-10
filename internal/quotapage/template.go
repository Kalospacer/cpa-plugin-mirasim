package quotapage

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	problemNotFound       = "找不到此配额页面。 / This quota page does not exist."
	problemNoCallbacks    = "托管方未提供凭据回调，暂时无法读取配额。 / The host did not provide credential callbacks, so limits cannot be read right now."
	problemCredentialList = "托管方未返回凭据列表，暂时无法读取配额。 / The host did not return the credential list, so limits cannot be read right now."
	problemStatusFetch    = "Mirasim 官方状态数据暂时读不到；配额数据不受影响。 / Live model availability could not be read; quota figures are unaffected."
	emptyAccounts         = "尚未安装 Mirasim 凭据。 / No Mirasim credential is installed yet."
	unavailableAccount    = "此账户的配额暂时无法读取。 / The limits for this account could not be read right now."
	emptyAccount          = "此账户未发布限额窗口。 / This account publishes no limit windows."
	emptyStatus           = "官方状态页暂未发布模型数据。 / The status page reports no models yet."
	groupFallbackName     = "Limits"
	statusEnabled         = "启用"
	statusDisabled        = "已禁用"
	statusUnavailable     = "不可用"
	accountWord           = "账户"
	labelFiveHour         = "5小时限额"
	labelWeekly           = "周限额"
	daysLeftFormat        = "%d天后"
	// 与官方状态页一致的徽章与分组文案。
	statusLabelOK         = "正常"
	statusLabelDegraded   = "不稳定"
	statusLabelDown       = "异常"
	statusLabelSparse     = "样本不足"
	statusLabelPending    = "尚未开始统计"
	statusLabelUnreadable = "暂时读不到"
	statusLabelNone       = "数据中断"
	agentLabelOK          = "运行正常"
	agentLabelDegraded    = "部分模型不稳定"
	agentLabelDown        = "部分模型异常"
)

// HTML/CSS/JS 编译进插件，部署仍为单一插件文件，不请求外部资源。
//
//go:embed page.html
var pageHTML string

//go:embed page.css
var pageCSS string

//go:embed page.js
var pageJS string

func renderResponse(status int, view pageView) (pluginapi.ManagementResponse, error) {
	if view.ReadAt == "" {
		view.ReadAt = time.Now().UTC().Format("2006-01-02 15:04 MST")
	}
	view.ScriptNonce = rand.Text()
	var body bytes.Buffer
	if err := pageTemplate.Execute(&body, view); err != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("render Mirasim quota page: %w", err)
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: pageHeaders(view.ScriptNonce), Body: body.Bytes()}, nil
}

func pageHeaders(nonce string) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	// No connect-src: the page issues no subrequests of its own. Quota and
	// status data are both fetched server-side while the page is rendered.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; base-uri 'none'; frame-ancestors 'self'; form-action 'none'")
	return h
}

var pageTemplate = template.Must(template.New("mirasim-quota").Funcs(template.FuncMap{
	"emptyAccounts":      func() string { return emptyAccounts },
	"unavailableAccount": func() string { return unavailableAccount },
	"emptyAccount":       func() string { return emptyAccount },
	"list":               func(v ...any) []any { return v },
	// 仅信任编译时固定资源；凭证字段始终走 html/template 的默认转义。
	"pageCSS": func() template.CSS { return template.CSS(pageCSS) },
	"pageJS":  func() template.JS { return template.JS(pageJS) },
	"emptyStatus": func() string { return emptyStatus },
}).Parse(pageHTML))
