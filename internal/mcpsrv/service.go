// Package mcpsrv implements the read-only MCP query service: an MCP client (an LLM
// agent) connects with an account token and can only read that account's data.
package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/store"
)

// Store is the read-only persistence subset the service needs.
type Store interface {
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	ListLedger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.LedgerEntry, error)
	ListUsage(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.UsageRecord, error)
	GetBalance(ctx context.Context, accountID int64) (int64, error)
	GetRequestLog(ctx context.Context, requestID string) (*domain.RequestLogRecord, error)
	ListRequestLogs(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.RequestLogRecord, error)
	ListAPIKeys(ctx context.Context, accountID int64) ([]*domain.APIKey, error)
	UsageWindowTotals(ctx context.Context, accountID int64, from, to time.Time) (*store.UsageWindowTotals, error)
	UsageBreakdown(ctx context.Context, accountID int64, from, to time.Time, groupBy string) ([]store.UsageBreakdownRow, error)
	ListUsageCounters(ctx context.Context, accountID int64, period string) ([]store.UsageCounter, error)
	ListInvoices(ctx context.Context, accountID int64, limit int) ([]*domain.Invoice, error)
	GetInvoice(ctx context.Context, id int64) (*domain.Invoice, error)
}

// Config tunes the service.
type Config struct {
	MaxRows    int
	WindowDays int
	Currency   string
}

// Service answers the read-only tools.
type Service struct {
	store Store
	reg   *registry.Registry
	cfg   Config
	now   func() time.Time
	// reservations reports the account's in-flight holds. It is injected so this
	// package does not depend on the billing package.
	reservations func(accountID int64) int64
}

// New builds the service.
func New(store Store, reg *registry.Registry, cfg Config) *Service {
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = 1000
	}
	if cfg.WindowDays <= 0 {
		cfg.WindowDays = 30
	}
	if cfg.Currency == "" {
		cfg.Currency = "USD"
	}
	return &Service{store: store, reg: reg, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

// SetReservationReporter installs the in-flight reader used by get_dashboard.
func (s *Service) SetReservationReporter(report func(accountID int64) int64) { s.reservations = report }

// CounterPeriod is the "YYYY-MM" rollup bucket for a time.
func CounterPeriod(at time.Time) string { return at.UTC().Format("2006-01") }

// Tool is one exposed MCP tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Tools lists the read-only tools.
func (s *Service) Tools() []Tool {
	return []Tool{
		{
			Name:        "get_balance",
			Description: "Current balance, credit limit, billing mode and account status.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "get_ledger",
			Description: "Ledger entries (charges, top-ups, adjustments, refunds) for a period.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string","enum":["today","yesterday","last_7_days","last_30_days","this_month","last_month"]},"limit":{"type":"integer","minimum":1,"maximum":1000}}}`),
		},
		{
			Name:        "get_usage_summary",
			Description: "Aggregated usage for a period: requests, token dimensions and attempt status.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string","enum":["today","yesterday","last_7_days","last_30_days","this_month","last_month"]}}}`),
		},
		{
			Name:        "list_requests",
			Description: "Recent requests with their recording flags.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":1000}}}`),
		},
		{
			Name:        "get_request",
			Description: "Input text of one request (redacted). Thinking and final output text are returned only when the key opted in.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"request_id":{"type":"string"}},"required":["request_id"]}`),
		},
		{
			Name:        "get_dashboard",
			Description: "One-call summary for a period: requests, failures, tokens, charge, cost, margin, TTFT, balance and in-flight holds.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string","enum":["today","yesterday","last_7_days","last_30_days","this_month","last_month"]}}}`),
		},
		{
			Name:        "get_usage_breakdown",
			Description: "Usage grouped by model, key or day, with charge and token totals per group.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string"},"group_by":{"type":"string","enum":["model","key","day"]}}}`),
		},
		{
			Name:        "get_rate_limits",
			Description: "Configured limits per API key plus this month's usage from the rollup.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "list_invoices",
			Description: "Billing periods for this account with status and totals.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":200}}}`),
		},
		{
			Name:        "get_invoice",
			Description: "One invoice with its lines (grouped by model, key or day).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`),
		},
		{
			Name:        "get_models",
			Description: "Models available to this account with their sale prices.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

// Call executes one tool for the authenticated account.
func (s *Service) Call(ctx context.Context, accountID int64, name string, args map[string]any) (any, error) {
	switch name {
	case "get_balance":
		return s.getBalance(ctx, accountID)
	case "get_ledger":
		return s.getLedger(ctx, accountID, args)
	case "get_usage_summary":
		return s.getUsageSummary(ctx, accountID, args)
	case "list_requests":
		return s.listRequests(ctx, accountID, args)
	case "get_request":
		return s.getRequest(ctx, accountID, args)
	case "get_dashboard":
		return s.getDashboard(ctx, accountID, args)
	case "get_usage_breakdown":
		return s.getUsageBreakdown(ctx, accountID, args)
	case "get_rate_limits":
		return s.getRateLimits(ctx, accountID)
	case "list_invoices":
		return s.listInvoices(ctx, accountID, args)
	case "get_invoice":
		return s.getInvoice(ctx, accountID, args)
	case "get_models":
		return s.getModels(ctx, accountID)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (s *Service) getBalance(ctx context.Context, accountID int64) (any, error) {
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	balance, err := s.store.GetBalance(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"account":          account.Name,
		"billing_mode":     string(account.BillingMode),
		"status":           account.Status,
		"currency":         s.cfg.Currency,
		"balance_micros":   balance,
		"balance_usd":      microsToUSD(balance),
		"credit_limit_usd": microsToUSD(account.CreditLimitMicros),
		"low_balance_usd":  microsToUSD(account.LowBalanceThresholdMicros),
	}, nil
}

func (s *Service) getLedger(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	limit := s.limit(args)
	rows, err := s.store.ListLedger(ctx, accountID, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"created_at":    row.CreatedAt.Format(time.RFC3339),
			"kind":          row.Kind,
			"amount_usd":    microsToUSD(row.AmountMicros),
			"amount_micros": row.AmountMicros,
			"balance_usd":   microsToUSD(row.BalanceAfterMicros),
			"ref_type":      row.RefType,
			"ref_id":        row.RefID,
			"note":          row.Note,
		})
	}
	return map[string]any{
		"period":   map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"currency": s.cfg.Currency,
		"entries":  out,
		"count":    len(out),
		"note":     "amounts are micro-USD (1e-6 USD); positive means credit",
	}, nil
}

func (s *Service) getUsageSummary(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	rows, err := s.store.ListUsage(ctx, accountID, from, to, s.cfg.MaxRows)
	if err != nil {
		return nil, err
	}
	dims := map[string]int64{}
	statuses := map[string]int{}
	models := map[string]int{}
	byDay := map[string]int{}
	for _, row := range rows {
		var d map[string]int64
		if err := json.Unmarshal([]byte(row.DimensionsJSON), &d); err == nil {
			for k, v := range d {
				dims[k] += v
			}
		}
		statuses[row.Status]++
		models[row.Model]++
		byDay[row.CreatedAt.Format("2006-01-02")]++
	}
	return map[string]any{
		"period":          map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"attempts":        len(rows),
		"tokens":          dims,
		"statuses":        statuses,
		"by_model":        models,
		"requests_by_day": byDay,
		"note":            "one row per upstream attempt; token dimensions are provider-reported when available",
	}, nil
}

func (s *Service) listRequests(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	limit := s.limit(args)
	rows, err := s.store.ListRequestLogs(ctx, accountID, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"request_id":           row.RequestID,
			"created_at":           row.CreatedAt.Format(time.RFC3339),
			"endpoint":             row.Endpoint,
			"status":               row.Status,
			"input_recorded":       row.RequestJSON != "",
			"reasoning_recorded":   row.ReasoningRecorded,
			"output_text_recorded": row.OutputTextRecorded,
		})
	}
	return map[string]any{"requests": out, "count": len(out)}, nil
}

func (s *Service) getRequest(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	requestID, _ := args["request_id"].(string)
	if strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	row, err := s.store.GetRequestLog(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("request not found")
	}
	// Cross-account access is reported as "not found" so existence is not leaked.
	if row.AccountID != accountID {
		return nil, fmt.Errorf("request not found")
	}
	out := map[string]any{
		"request_id": row.RequestID,
		"endpoint":   row.Endpoint,
		"status":     row.Status,
		"created_at": row.CreatedAt.Format(time.RFC3339),
	}
	if row.RequestJSON != "" {
		out["input"] = json.RawMessage(row.RequestJSON)
		out["input_recorded"] = true
	} else {
		out["input_recorded"] = false
		out["input_unavailable_reason"] = "input recording is disabled for this API key"
	}
	if row.ReasoningRecorded && row.ResponseReasoning != "" {
		out["reasoning"] = row.ResponseReasoning
		out["reasoning_recorded"] = true
	} else {
		out["reasoning_recorded"] = false
		out["reasoning_unavailable_reason"] = "thinking text is not recorded for this API key (enable it in the admin console)"
	}
	if row.OutputTextRecorded && row.ResponseText != "" {
		out["output_text"] = row.ResponseText
		out["output_text_recorded"] = true
	} else {
		out["output_text_recorded"] = false
		out["output_text_unavailable_reason"] = "final output text is not recorded for this API key (enable it in the admin console)"
	}
	return out, nil
}

func (s *Service) getModels(ctx context.Context, accountID int64) (any, error) {
	snap := s.reg.Snapshot()
	names := make([]string, 0, len(snap.ModelByName))
	for name, model := range snap.ModelByName {
		if model.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		entry := map[string]any{"id": name, "object": "model", "currency": s.cfg.Currency}
		if raw := strings.TrimSpace(snap.ModelByName[name].SalePricingJSON); raw != "" {
			var pricing map[string]any
			if err := json.Unmarshal([]byte(raw), &pricing); err == nil {
				entry["pricing"] = pricing
			}
		}
		out = append(out, entry)
	}
	return map[string]any{"models": out, "count": len(out)}, nil
}

// period resolves the requested window, clamped to the configured maximum.
func (s *Service) period(args map[string]any) (time.Time, time.Time) {
	now := s.now()
	name, _ := args["period"].(string)
	var from time.Time
	switch name {
	case "today":
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "yesterday":
		day := now.AddDate(0, 0, -1)
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
			time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "last_30_days":
		from = now.AddDate(0, 0, -30)
	case "this_month":
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "last_month":
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return first.AddDate(0, -1, 0), first
	case "last_7_days", "":
		from = now.AddDate(0, 0, -7)
	default:
		from = now.AddDate(0, 0, -7)
	}
	if max := now.AddDate(0, 0, -s.cfg.WindowDays); from.Before(max) {
		from = max
	}
	return from, now
}

func (s *Service) limit(args map[string]any) int {
	limit := s.cfg.MaxRows
	if raw, ok := args["limit"]; ok {
		switch v := raw.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		}
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > s.cfg.MaxRows {
		limit = s.cfg.MaxRows
	}
	return limit
}

func microsToUSD(micros int64) float64 {
	return float64(micros) / 1_000_000
}
