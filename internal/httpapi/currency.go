package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
)

// This file is the transport side of M22: it exposes the currency table to the
// console, rejects rule sets whose currency the gateway cannot convert, and makes
// sure the request path prices in the ledger currency.
//
// The ledger currency itself comes from configuration (billing.currency); a model
// may declare a currency of its own, and then a rate must exist for it.

// fxTable returns the live currency table. Without an FX store (or without a
// configuration at all) the table is empty, which disables conversion — the same
// numbers a single-currency deployment produced before M22.
func (s *Server) fxTable() pricing.FXTable {
	if s.deps.FX != nil {
		return s.deps.FX.Snapshot()
	}
	if s.deps.Config == nil {
		return pricing.FXTable{}
	}
	table, err := pricing.NewFXTable(s.deps.Config.Billing.Currency, s.deps.Config.Billing.FXRates)
	if err != nil {
		return pricing.FXTable{}
	}
	return table
}

// ledgerCurrency is the currency the ledger is kept in.
func (s *Server) ledgerCurrency() string {
	if s.deps.Config != nil && strings.TrimSpace(s.deps.Config.Billing.Currency) != "" {
		return strings.ToUpper(strings.TrimSpace(s.deps.Config.Billing.Currency))
	}
	if ledger := s.fxTable().Ledger; ledger != "" {
		return ledger
	}
	return "USD"
}

// displayCurrency is the console's default display currency.
func (s *Server) displayCurrency() string {
	if s.deps.Config != nil {
		if code := strings.ToUpper(strings.TrimSpace(s.deps.Config.Billing.DisplayCurrency)); code != "" {
			return code
		}
	}
	return s.ledgerCurrency()
}

// validateRuleSetCurrency rejects a rule set the gateway could not price: an
// unknown currency shape, or a currency without a rate in the live table. It is
// deliberately a write-time check — a typo must fail here, while a rule set that
// reaches the database by hand degrades visibly at settlement instead.
func (s *Server) validateRuleSetCurrency(set *pricing.RuleSet) error {
	if set == nil || set.Currency == "" {
		return nil
	}
	code, err := pricing.NormalizeCurrency(set.Currency)
	if err != nil {
		return err
	}
	set.Currency = code
	ledger := s.ledgerCurrency()
	if code == ledger {
		return nil
	}
	if _, ok := s.fxTable().RateOf(code); !ok {
		return domain.ErrInvalidRequest("currency " + code + " has no exchange rate; add it to billing.fx_rates (console: 设置 → 汇率表) first")
	}
	return nil
}

// missingRates lists the currencies rule sets declare but the FX table cannot
// convert. Those models cannot be priced, so the console shows them in red.
func (s *Server) missingRates() []string {
	if s.deps.Registry == nil {
		return nil
	}
	table := s.fxTable()
	ledger := s.ledgerCurrency()
	seen := map[string]bool{}
	out := []string{}
	for _, model := range s.deps.Registry.Snapshot().Models {
		seen[pricing.DeclaredCurrency(model.SalePricingJSON)] = true
	}
	for _, mapping := range s.deps.Registry.Snapshot().ProviderModels {
		seen[pricing.DeclaredCurrency(mapping.PricingRulesJSON)] = true
	}
	for code := range seen {
		if code == "" || code == ledger || table.Enabled() && code == table.Ledger {
			continue
		}
		if _, ok := table.RateOf(code); !ok {
			out = append(out, code)
		}
	}
	sort.Strings(out)
	return out
}

// currencyPayload describes the live currency table for the console: the ledger
// currency, the default display currency, every convertible currency with the rate
// to use, and the currencies that are missing a rate.
func (s *Server) currencyPayload(ctx context.Context) map[string]any {
	table := s.fxTable()
	ledger := s.ledgerCurrency()
	override := map[string]int64{}
	source := "config"
	if rates, found := s.fxRatesFromSettings(ctx); found {
		override = rates
		source = "settings"
	}

	currencies := []map[string]any{{
		"code": ledger, "rate_micros": pricing.RateScale, "rate_source": "ledger",
	}}
	for _, code := range table.Codes() {
		if code == ledger {
			continue
		}
		rate, ok := table.RateOf(code)
		if !ok {
			continue
		}
		rateSource := "config"
		if _, ok := override[code]; ok {
			rateSource = "settings"
		}
		currencies = append(currencies, map[string]any{
			"code": code, "rate_micros": rate, "rate_source": rateSource,
		})
	}
	payload := map[string]any{
		"ledger_currency":  ledger,
		"display_currency": s.displayCurrency(),
		"fx_source":        source,
		"currencies":       currencies,
		"missing_rates":    s.missingRates(),
	}
	return payload
}

// fxRatesFromSettings reads the console override of the FX table.
func (s *Server) fxRatesFromSettings(ctx context.Context) (map[string]int64, bool) {
	if s.deps.Settings == nil {
		return nil, false
	}
	raw, found, err := s.deps.Settings.GetSetting(ctx, pricing.SettingFXRates)
	if err != nil || !found {
		return nil, false
	}
	rates, err := pricing.ParseFXRates(raw)
	if err != nil {
		return nil, false
	}
	return rates, len(rates) > 0
}

// normalizeCurrencyInDocument rewrites just the "currency" key of a stored rule
// document to its normalized form, leaving every other key (including the rule
// array) byte-identical. Rule sets are edited by hand, so the write path must not
// reformat a document it was handed.
func normalizeCurrencyInDocument(raw string, set *pricing.RuleSet) string {
	if set == nil || set.Currency == "" || strings.TrimSpace(raw) == "" {
		return raw
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return raw
	}
	encoded, err := json.Marshal(set.Currency)
	if err != nil {
		return raw
	}
	document["currency"] = encoded
	out, err := json.Marshal(document)
	if err != nil {
		return raw
	}
	return string(out)
}

// configuredFXRates is the rate table from configuration (before the console
// override is layered on top).
func (s *Server) configuredFXRates() map[string]int64 {
	if s.deps.Config == nil {
		return nil
	}
	return s.deps.Config.Billing.FXRates
}

// reloadFX rebuilds the live currency table after the console override changed. A
// failure is logged rather than failing the write that already landed (the same
// rule reloadHooks follows).
func (s *Server) reloadFX(ctx context.Context, actor string) {
	if s.deps.ReloadFX == nil {
		return
	}
	if err := s.deps.ReloadFX(ctx); err != nil {
		s.deps.Log.Error("reloading the fx table after an admin write failed", "err", err, "actor", actor)
	}
}

// handleAdminGetBillingCurrency serves the currency table the console needs to
// offer a display-currency choice and to warn about models it cannot price.
func (s *Server) handleAdminGetBillingCurrency(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.currencyPayload(r.Context()))
}
