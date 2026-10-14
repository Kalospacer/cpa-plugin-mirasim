package quotapage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/quota"
)

func platformCreditFixture(t *testing.T) pluginapi.QuotaFetchResponse {
	t.Helper()
	snapshot, err := mirasim.QuotaFromLimits([]byte(`{"windows":[{"name":"30d","used":19784.526462,"budget":30000,"reset_at":1794206513}]}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return quota.Normalize(credentials.Storage{Plan: "plus"}, snapshot)
}

func TestPlatformCreditFormattingAndDeadline(t *testing.T) {
	response := platformCreditFixture(t)
	planEnd := time.Date(2027, 9, 15, 7, 2, 54, 0, time.UTC).Unix()
	view := platformCreditFromResponse(response, &planEnd)
	if view == nil || view.UsedUSD != "$197.85" || view.BudgetUSD != "$300.00" || view.RemainingUSD != "$102.15" || view.RemainingPercent != "34%" || view.WidthPct != "34.1" || view.DeadlineAt != "2026-11-09T06:41:53Z" {
		t.Fatalf("incorrect credit display: %+v", view)
	}
	planEnd = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix()
	view = platformCreditFromResponse(response, &planEnd)
	if view.DeadlineAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("credit outlives plan: %+v", view)
	}
	response.Summary = nil
	if view = platformCreditFromResponse(response, nil); view != nil {
		t.Fatalf("missing measurements became zero dollars: %+v", view)
	}
}

func TestPlatformCreditUnlimited(t *testing.T) {
	response := platformCreditFixture(t)
	for i := range response.Summary {
		if response.Summary[i].Key == quota.PlatformCreditBudgetKey {
			response.Summary[i].Value = 0
		}
	}
	view := platformCreditFromResponse(response, nil)
	if view == nil || !view.Unlimited {
		t.Fatalf("zero budget should mean unlimited: %+v", view)
	}
}

func TestPageRendersPlatformCreditWithoutCredentials(t *testing.T) {
	response := platformCreditFixture(t)
	response.Summary = append(response.Summary, pluginapi.QuotaMetric{Key: quota.ResetCardsMetricKey, Value: 2})
	fetcher := &fakeFetcher{responses: map[string]pluginapi.QuotaFetchResponse{"credit": response}}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{{ID: "credit", AuthIndex: "credit", Type: "mirasim", Provider: "mirasim"}},
		auths:   map[string]pluginapi.HostAuthGetResponse{"credit": {JSON: []byte(`{"type":"mirasim","email":"sample@example.com","plan":"plus","plan_exp":1820991774,"access_token":"do-not-render-access","refresh_token":"do-not-render-refresh","device_private_key":"do-not-render-key"}`)}},
		client:  fakeHTTPClient{},
	}
	page := New(fetcher)
	result := mustServe(t, page, quotaRequest(page), host)
	body := string(result.Body)
	for _, text := range []string{"平台额度", "账号级 · 含其他设备用量", "30 天额度", "剩余 34%", "$197.85", "$300.00", "剩余 $102.15", `aria-valuenow="34.1"`, `width:34.1%`, `data-credit-exp`, "本期额度", "2026-11-09T06:41:53Z", "2 次", "续期时间"} {
		if !strings.Contains(body, text) {
			t.Fatalf("missing %q in page", text)
		}
	}
	for _, secret := range []string{"do-not-render-access", "do-not-render-refresh", "do-not-render-key"} {
		if strings.Contains(body, secret) {
			t.Fatalf("credential leaked: %s", secret)
		}
	}
	if strings.Count(body, `class="platform-credit"`) != 1 {
		t.Fatal("account credit duplicated")
	}
	view := page.buildView(context.Background(), host, host.client, false)
	if len(view.Accounts) != 1 || view.Accounts[0].Empty || len(view.Accounts[0].Groups) != 0 {
		t.Fatalf("credit-only account was empty or duplicated: %+v", view.Accounts)
	}
	if path := os.Getenv("MIRASIM_CREDIT_PREVIEW_PATH"); path != "" {
		if err := os.WriteFile(path, result.Body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
