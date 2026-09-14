package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// getDashboard answers the "how is my account doing" question in one call.
func (s *Service) getDashboard(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	totals, err := s.store.UsageWindowTotals(ctx, accountID, from, to)
	if err != nil {
		return nil, err
	}
	balance, err := s.store.GetBalance(ctx, accountID)
	if err != nil {
		return nil, err
	}
	byModel, err := s.store.UsageBreakdown(ctx, accountID, from, to, "model")
	if err != nil {
		return nil, err
	}

	models := make([]map[string]any, 0, len(byModel))
	for _, row := range byModel {
		models = append(models, map[string]any{
			"model": row.GroupKey, "requests": row.Requests, "failed": row.Failed,
			"charge_usd": microsToUSD(row.ChargeMicros),
		})
	}
	margin := totals.ChargeMicros - totals.CostMicros
	errorRateBP := int64(0)
	if totals.Attempts > 0 {
		errorRateBP = totals.Failed * 10000 / totals.Attempts
	}
	estimatedBP := int64(0)
	if totals.Attempts > 0 {
		estimatedBP = totals.Estimated * 10000 / totals.Attempts
	}

	inFlight := int64(0)
	if s.reservations != nil {
		inFlight = s.reservations(accountID)
	}
	payload := map[string]any{
		"period":   map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"currency": s.cfg.Currency,
		"requests": map[string]any{
			"attempts": totals.Attempts, "failed": totals.Failed,
			"error_rate_bp": errorRateBP,
		},
		"tokens": map[string]any{
			"input": totals.PromptTokens, "output": totals.CompletionTokens,
			"total": totals.PromptTokens + totals.CompletionTokens,
		},
		"money": map[string]any{
			"charge_usd": microsToUSD(totals.ChargeMicros), "charge_micros": totals.ChargeMicros,
			"cost_usd": microsToUSD(totals.CostMicros), "cost_micros": totals.CostMicros,
			"margin_usd": microsToUSD(margin), "margin_micros": margin,
		},
		"latency": map[string]any{
			"ttft_avg_ms": totals.TTFTAvgMS, "ttft_p95_ms": totals.TTFTP95MS,
			"ttft_p95_estimated": totals.TTFTP95Estimated,
		},
		"balance": map[string]any{
			"balance_usd": microsToUSD(balance), "balance_micros": balance,
			"in_flight_usd": microsToUSD(inFlight), "in_flight_micros": inFlight,
			"available_usd": microsToUSD(balance - inFlight),
		},
		"estimated_ratio_bp": estimatedBP,
		"by_model":           models,
		"note":               "one row per upstream attempt; cost is what the gateway paid, charge is what the account was billed",
	}
	return payload, nil
}

// getUsageBreakdown groups usage so an agent can answer "which model costs most".
func (s *Service) getUsageBreakdown(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	groupBy, _ := args["group_by"].(string)
	if strings.TrimSpace(groupBy) == "" {
		groupBy = "model"
	}
	rows, err := s.store.UsageBreakdown(ctx, accountID, from, to, groupBy)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"group": row.GroupKey, "requests": row.Requests, "failed": row.Failed,
			"input_tokens": row.PromptTokens, "output_tokens": row.CompletionTokens,
			"charge_usd": microsToUSD(row.ChargeMicros), "charge_micros": row.ChargeMicros,
			"cost_usd": microsToUSD(row.CostMicros),
		})
	}
	return map[string]any{
		"period":   map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"group_by": groupBy,
		"currency": s.cfg.Currency,
		"groups":   out,
		"count":    len(out),
	}, nil
}

// getRateLimits reports the configured limits per key plus this month's usage.
func (s *Service) getRateLimits(ctx context.Context, accountID int64) (any, error) {
	keys, err := s.store.ListAPIKeys(ctx, accountID)
	if err != nil {
		return nil, err
	}
	period := CounterPeriod(s.now())
	counters, err := s.store.ListUsageCounters(ctx, accountID, period)
	if err != nil {
		return nil, err
	}
	used := map[int64]map[string]any{}
	for _, counter := range counters {
		entry := used[counter.APIKeyID]
		if entry == nil {
			entry = map[string]any{"requests": int64(0), "tokens": int64(0), "charge_usd": "0.000000"}
		}
		entry["requests"] = entry["requests"].(int64) + counter.Requests
		entry["tokens"] = entry["tokens"].(int64) + counter.Tokens
		entry["charge_usd"] = microsToUSD(int64(0)) // recomputed below
		used[counter.APIKeyID] = entry
	}
	// Sum the charge separately so the currency formatting stays consistent.
	charge := map[int64]int64{}
	for _, counter := range counters {
		charge[counter.APIKeyID] += counter.ChargeMicros
	}
	for keyID, entry := range used {
		entry["charge_usd"] = microsToUSD(charge[keyID])
	}

	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		accountTags := []string{}
		effectiveTags := jsonArray(key.TagsJSON)
		if s.reg != nil {
			if snap := s.reg.Snapshot(); snap != nil {
				if account := snap.AccountByID[key.AccountID]; account != nil {
					accountTags = jsonArray(account.TagsJSON)
				}
				effectiveTags = registry.ResolveTagNames(snap, key)
			}
		}
		entry := map[string]any{
			"api_key_id": key.ID, "name": key.Name, "status": key.Status,
			"tags":         jsonArray(key.TagsJSON),
			"account_tags": accountTags, "effective_tags": effectiveTags,
		}
		// The report reads the same document admission reads (domain.ParsePolicy), so a
		// limit can never show up here while being ignored on the request path. It used to
		// read a nested policy["rate_limit"], a shape nothing enforced.
		parsed, unknown, parseErr := domain.ParsePolicy(key.PolicyJSON)
		if parseErr != nil {
			entry["configured_limits"] = map[string]any{}
			entry["policy_error"] = parseErr.Error()
		} else {
			entry["configured_limits"] = parsed.Configured()
			if notEnforced := parsed.UnenforcedFields(); len(notEnforced) > 0 {
				entry["not_enforced"] = notEnforced
			}
			if len(unknown) > 0 {
				entry["ignored_policy_fields"] = unknown
			}
		}
		used := used[key.ID]
		if used == nil {
			used = map[string]any{"requests": int64(0), "tokens": int64(0), "charge_usd": microsToUSD(0)}
		}
		entry["used_this_period"] = used
		out = append(out, entry)
	}
	return map[string]any{
		"period": period,
		"keys":   out,
		"note": "configured_limits are the key policy's own quota fields; tag policies merge in at " +
			"enforcement time and are not merged here. rpm/tpm/concurrency are enforced; anything " +
			"listed in not_enforced is stored but not checked yet. used_this_period comes from the " +
			"monthly rollup. Real-time sliding-window headroom is process state and is not reported here.",
	}, nil
}

// listInvoices lists the account's invoices (newest first).
func (s *Service) listInvoices(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	invoices, err := s.store.ListInvoices(ctx, accountID, s.limit(args))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(invoices))
	for _, invoice := range invoices {
		out = append(out, invoiceSummary(invoice))
	}
	return map[string]any{"invoices": out, "count": len(out), "currency": s.cfg.Currency}, nil
}

// getInvoice returns one invoice with its lines.
func (s *Service) getInvoice(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	id := int64Arg(args["id"])
	if id == 0 {
		return nil, fmt.Errorf("id is required")
	}
	invoice, err := s.store.GetInvoice(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("invoice not found")
	}
	// Another account's invoice is reported as missing, so ids cannot be probed.
	if invoice.AccountID != accountID {
		return nil, fmt.Errorf("invoice not found")
	}
	lines := make([]map[string]any, 0, len(invoice.Lines))
	for _, line := range invoice.Lines {
		lines = append(lines, map[string]any{
			"group": line.GroupKey, "group_type": line.GroupType,
			"requests": line.Requests, "input_tokens": line.PromptTokens,
			"output_tokens": line.CompletionTokens,
			"charge_usd":    microsToUSD(line.ChargeMicros), "charge_micros": line.ChargeMicros,
		})
	}
	payload := invoiceSummary(invoice)
	payload["lines"] = lines
	return payload, nil
}

func invoiceSummary(invoice *domain.Invoice) map[string]any {
	return map[string]any{
		"id": invoice.ID, "status": invoice.Status, "currency": invoice.Currency,
		"period_start":        invoice.PeriodStart.Format(time.RFC3339),
		"period_end":          invoice.PeriodEnd.Format(time.RFC3339),
		"total_charge_usd":    microsToUSD(invoice.TotalChargeMicros),
		"total_charge_micros": invoice.TotalChargeMicros,
		"total_cost_usd":      microsToUSD(invoice.TotalCostMicros),
	}
}

func jsonArray(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []string{}
	}
	return values
}

func int64Arg(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return parsed
		}
	}
	return 0
}
