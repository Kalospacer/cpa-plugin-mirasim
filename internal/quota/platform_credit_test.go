package quota

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

func TestPlatformCreditUsesObservedCents(t *testing.T) {
	snapshot, err := mirasim.QuotaFromLimits([]byte(`{"windows":[{"name":"30d","used":19784.526462,"budget":30000,"reset_at":1794206513}]}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response := Normalize(credentials.Storage{}, snapshot)
	values := map[string]float64{}
	for _, metric := range response.Summary {
		if metric.Key == PlatformCreditUsedKey || metric.Key == PlatformCreditBudgetKey {
			if metric.Format != "currency" || metric.Currency != "USD" {
				t.Fatalf("incorrect currency metric: %+v", metric)
			}
			values[metric.Key] = metric.Value
		}
	}
	if len(values) != 2 || math.Abs(values[PlatformCreditUsedKey]-197.84526462) > 1e-9 || values[PlatformCreditBudgetKey] != 300 {
		t.Fatalf("amount was dropped, rounded early or converted twice: %+v", values)
	}
	if len(response.Groups) != 1 || response.Groups[0].Buckets[0].RemainingFraction != .341 {
		t.Fatalf("existing quota fraction changed: %+v", response)
	}
}

func TestPlatformCreditDoesNotSumUnrelatedWindows(t *testing.T) {
	for _, window := range []mirasim.QuotaLimitWindow{
		{Name: "7d", Used: 100, Budget: 200},
		{Name: "30d", Used: 100, Budget: 200, ModelScoped: true},
		{Name: "30d_claude", Used: 100, Budget: 200, ModelScoped: true},
	} {
		if metrics := platformCreditMetrics(window); len(metrics) != 0 {
			t.Fatalf("unrelated window became account credit: %+v", metrics)
		}
	}
	metrics := platformCreditMetrics(mirasim.QuotaLimitWindow{Name: "30d", Used: 125, Budget: 0})
	if len(metrics) != 2 || metrics[0].Value != 1.25 || metrics[1].Value != 0 {
		t.Fatalf("unlimited window measurements changed: %+v", metrics)
	}
}
