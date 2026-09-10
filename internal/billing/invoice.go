package billing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
)

// PeriodConfig describes how a billing period is cut.
type PeriodConfig struct {
	// StartDay moves the period boundary to this day of the month (1-28); 0 means
	// the natural month.
	StartDay int
	// Timezone is the location the boundary is expressed in.
	Timezone string
	// GroupBy is the default invoice line grouping: model|key|day.
	GroupBy string
}

// PeriodFor returns the billing period containing at. It is a pure function so the
// same period can be recomputed for a replay or a test.
func PeriodFor(at time.Time, cfg PeriodConfig) (time.Time, time.Time, error) {
	location := time.UTC
	if strings.TrimSpace(cfg.Timezone) != "" {
		loaded, err := pricing.LoadLocation(cfg.Timezone)
		if err != nil {
			return time.Time{}, time.Time{}, domain.ErrInvalidRequest("unknown billing timezone " + cfg.Timezone)
		}
		location = loaded
	}
	local := at.In(location)
	startDay := cfg.StartDay
	if startDay < 1 || startDay > 28 {
		startDay = 1
	}

	var start time.Time
	if startDay == 1 {
		start = time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
	} else if local.Day() >= startDay {
		start = time.Date(local.Year(), local.Month(), startDay, 0, 0, 0, 0, location)
	} else {
		start = time.Date(local.Year(), local.Month()-1, startDay, 0, 0, 0, 0, location)
	}
	end := time.Date(start.Year(), start.Month()+1, start.Day(), 0, 0, 0, 0, location)
	return start.UTC(), end.UTC(), nil
}

// BuildInvoice materialises (or returns) the invoice of one period. Lines are
// aggregated from usage rows, which stay the source of truth: an invoice is a
// snapshot of them, not a second ledger.
func (s *Service) BuildInvoice(
	ctx context.Context,
	accountID int64,
	start, end time.Time,
	groupBy string,
	replace bool,
	currency string,
	note string,
) (*domain.Invoice, bool, error) {
	if accountID == 0 {
		return nil, false, domain.ErrInvalidRequest("account_id is required")
	}
	if !end.After(start) {
		return nil, false, domain.ErrInvalidRequest("the period end must be after its start")
	}
	switch groupBy {
	case "", "model", "key", "day":
	default:
		return nil, false, domain.ErrInvalidRequest("group_by must be model, key or day")
	}
	if groupBy == "" {
		groupBy = "model"
	}

	lines, err := s.store.AggregateInvoiceLines(ctx, accountID, start, end, groupBy)
	if err != nil {
		return nil, false, err
	}
	var totalCost, totalCharge int64
	for _, line := range lines {
		totalCost += line.CostMicros
		totalCharge += line.ChargeMicros
	}
	if currency == "" {
		currency = "USD"
	}
	invoice := &domain.Invoice{
		AccountID: accountID, PeriodStart: start.UTC(), PeriodEnd: end.UTC(),
		Status: "draft", Currency: currency,
		TotalCostMicros: totalCost, TotalChargeMicros: totalCharge, Note: note,
	}
	id, created, err := s.store.PutInvoice(ctx, invoice, lines, replace)
	if err != nil {
		return nil, false, err
	}
	invoice.ID = id
	stored, err := s.store.GetInvoice(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// InvoiceAction moves an invoice through its state machine. Paying writes the
// repayment ledger entry for postpaid accounts, which is what makes the money move.
func (s *Service) InvoiceAction(ctx context.Context, id int64, action, actor string) (*domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	switch action {
	case "issue":
		if invoice.Status != "draft" {
			return nil, domain.ErrConflict("only a draft invoice can be issued (current status: " + invoice.Status + ")")
		}
		if err := s.store.SetInvoiceStatus(ctx, id, "issued", now); err != nil {
			return nil, err
		}
	case "void":
		if invoice.Status == "void" {
			return nil, domain.ErrConflict("the invoice is already void")
		}
		if invoice.Status == "paid" {
			return nil, domain.ErrConflict("a paid invoice cannot be voided; record an adjustment instead")
		}
		if err := s.store.SetInvoiceStatus(ctx, id, "void", now); err != nil {
			return nil, err
		}
	case "pay":
		if invoice.Status != "issued" {
			return nil, domain.ErrConflict("only an issued invoice can be paid (current status: " + invoice.Status + ")")
		}
		account, err := s.store.GetAccount(ctx, invoice.AccountID)
		if err != nil {
			return nil, err
		}
		if account.BillingMode == domain.BillingPostpaid && invoice.TotalChargeMicros > 0 {
			// Postpaid: the period was invoiced and now the customer settles it.
			entry := &domain.LedgerEntry{
				AccountID: invoice.AccountID, Kind: "topup",
				AmountMicros: invoice.TotalChargeMicros,
				RefType:      "invoice", RefID: fmt.Sprintf("%d", invoice.ID),
				IdemKey:   fmt.Sprintf("invoice:%d:payment", invoice.ID),
				Note:      "settlement of the billing period",
				Actor:     actor,
				CreatedAt: now,
			}
			if _, err := s.store.AppendLedger(ctx, []*domain.LedgerEntry{entry}); err != nil {
				return nil, err
			}
		}
		if err := s.store.SetInvoiceStatus(ctx, id, "paid", now); err != nil {
			return nil, err
		}
	default:
		return nil, domain.ErrInvalidRequest("action must be issue, void or pay")
	}
	return s.store.GetInvoice(ctx, id)
}

// InvoiceCSV renders an invoice as CSV (the format finance teams actually import).
func InvoiceCSV(invoice *domain.Invoice) string {
	var builder strings.Builder
	builder.WriteString("group_type,group_key,requests,prompt_tokens,completion_tokens,cost_micros,charge_micros\n")
	lines := append([]domain.InvoiceLine(nil), invoice.Lines...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].ChargeMicros > lines[j].ChargeMicros })
	for _, line := range lines {
		builder.WriteString(fmt.Sprintf("%s,%s,%d,%d,%d,%d,%d\n",
			csvField(line.GroupType), csvField(line.GroupKey), line.Requests, line.PromptTokens,
			line.CompletionTokens, line.CostMicros, line.ChargeMicros))
	}
	builder.WriteString(fmt.Sprintf("total,,,,,%d,%d\n", invoice.TotalCostMicros, invoice.TotalChargeMicros))
	return builder.String()
}

func csvField(value string) string {
	if strings.ContainsAny(value, ",\"\n") {
		return "\"" + strings.ReplaceAll(value, "\"", "\"\"") + "\""
	}
	return value
}
