package quota

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

const (
	PlatformCreditUsedKey   = "platform_credit_used_usd"
	PlatformCreditBudgetKey = "platform_credit_budget_usd"
)

func platformCreditMetrics(window mirasim.QuotaLimitWindow) []pluginapi.QuotaMetric {
	if window.Name != "30d" || window.ModelScoped {
		return nil
	}
	// 官方客户端的 30 天额度以美分计量；只在输出美元指标时换算。
	return []pluginapi.QuotaMetric{
		{Key: PlatformCreditUsedKey, Label: "30天额度已用", Value: window.Used / 100, Format: "currency", Currency: "USD"},
		{Key: PlatformCreditBudgetKey, Label: "30天额度总额", Value: window.Budget / 100, Format: "currency", Currency: "USD"},
	}
}
