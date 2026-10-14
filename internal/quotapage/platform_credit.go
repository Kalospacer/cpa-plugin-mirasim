package quotapage

import (
	"fmt"
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/quota"
)

type platformCreditView struct {
	UsedUSD, BudgetUSD, RemainingUSD string
	RemainingPercent, WidthPct       string
	RemainClass                      string
	Unlimited                        bool
	DeadlineAt, DeadlineText         string
}

func platformCreditFromResponse(response pluginapi.QuotaFetchResponse, planExpiresAt *int64) *platformCreditView {
	var used, budget float64
	var hasUsed, hasBudget bool
	for _, metric := range response.Summary {
		if metric.Format != "currency" || metric.Currency != "USD" {
			continue
		}
		switch metric.Key {
		case quota.PlatformCreditUsedKey:
			used, hasUsed = metric.Value, true
		case quota.PlatformCreditBudgetKey:
			budget, hasBudget = metric.Value, true
		}
	}
	if !hasUsed || !hasBudget {
		return nil
	}
	for _, group := range response.Groups {
		if group.DisplayName != accountGroupName {
			continue
		}
		for _, bucket := range group.Buckets {
			if bucket.Window != "30d" {
				continue
			}
			remainingPercent := math.Max(0, math.Min(100, bucket.RemainingFraction*100))
			view := &platformCreditView{
				UsedUSD: fmt.Sprintf("$%.2f", used), BudgetUSD: fmt.Sprintf("$%.2f", budget),
				RemainingUSD:     fmt.Sprintf("$%.2f", math.Max(0, budget-used)),
				RemainingPercent: fmt.Sprintf("%.0f%%", remainingPercent), WidthPct: fmt.Sprintf("%.1f", remainingPercent),
				RemainClass: remainClass(bucket.RemainingFraction), Unlimited: budget == 0,
			}
			// 本期额度不能晚于套餐终止时间；与套餐续期时间分别展示。
			deadline, err := time.Parse(time.RFC3339, bucket.ResetTime)
			if planExpiresAt != nil && *planExpiresAt > 0 {
				planEnd := time.Unix(*planExpiresAt, 0).UTC()
				if err == nil && planEnd.Before(deadline) {
					deadline = planEnd
				}
			}
			if err == nil {
				view.DeadlineAt = deadline.UTC().Format(time.RFC3339)
				view.DeadlineText = deadline.UTC().Format("01/02 15:04") + " UTC"
			}
			return view
		}
	}
	return nil
}
