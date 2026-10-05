package quotapage

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/quota"
)

// 主动重置次数取自额度汇总里的可用重置卡张数；没有该指标（查卡失败或中转不支持）时显示「—」。
func TestPageShowsUsableResetCardsAsResetCount(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]pluginapi.QuotaFetchResponse{
		"a1": {
			Subscription: &pluginapi.QuotaSubscription{Plan: "pro"},
			Summary:      []pluginapi.QuotaMetric{{Key: "used_5h", Value: 40}, {Key: quota.ResetCardsMetricKey, Value: 3}},
		},
		"a2": {Subscription: &pluginapi.QuotaSubscription{Plan: "pro"}},
	}}
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

	body := string(mustServe(t, page, quotaRequest(page), host).Body)
	const label = `<span class="plan-label">主动重置次数</span><span class="plan-value">`
	for _, want := range []string{label + "3 次</span>", label + "—</span>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, label+"不支持</span>") {
		t.Fatal("reset count must no longer be hard-coded as unsupported")
	}
}
