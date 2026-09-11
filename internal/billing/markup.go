package billing

import (
	"encoding/json"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// Markup sources, from most specific to least.
const (
	MarkupSourceKey     = "key"
	MarkupSourceTag     = "tag"
	MarkupSourceAccount = "account"
	MarkupSourceModel   = "model"
	MarkupSourceDefault = "default"
)

// MarkupResolution is the multiplier a request is charged with and where it came from.
type MarkupResolution struct {
	BP     int
	Source string
	// Set reports whether anything explicit was found; false means "use the engine's
	// own fallback", which callers pass through unchanged.
	Set bool
}

// marginWire is the policy shape carrying a per-key/per-tag multiplier.
type marginWire struct {
	MarginBP *int `json:"margin_bp"`
}

// policyMarginBP reads margin_bp from a policy document, tolerating other fields.
func policyMarginBP(raw string) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	var wire marginWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return 0, false
	}
	if wire.MarginBP == nil || *wire.MarginBP < 0 {
		return 0, false
	}
	return *wire.MarginBP, true
}

// ResolveMarkup walks the precedence chain for the customer-facing multiplier:
// key policy, then tag policies (ascending priority, later wins), then the account
// override, then the model's own sale rule set, and finally the configured default.
//
// The chain is a pure function so the console can show the same answer it charges.
func ResolveMarkup(key *domain.APIKey, tags []*domain.Tag, account *domain.Account, modelMarkupBP int, defaultBP int) MarkupResolution {
	if key != nil {
		if bp, ok := policyMarginBP(key.PolicyJSON); ok {
			return MarkupResolution{BP: bp, Source: MarkupSourceKey, Set: true}
		}
	}
	resolved := MarkupResolution{}
	for _, tag := range tags {
		if tag == nil {
			continue
		}
		if bp, ok := policyMarginBP(tag.PolicyJSON); ok {
			resolved = MarkupResolution{BP: bp, Source: MarkupSourceTag, Set: true}
		}
	}
	if resolved.Set {
		return resolved
	}
	if account != nil && account.MarkupOverrideBP >= 0 && account.MarkupOverrideSet {
		return MarkupResolution{BP: account.MarkupOverrideBP, Source: MarkupSourceAccount, Set: true}
	}
	if modelMarkupBP > 0 {
		return MarkupResolution{BP: modelMarkupBP, Source: MarkupSourceModel, Set: true}
	}
	if defaultBP > 0 {
		return MarkupResolution{BP: defaultBP, Source: MarkupSourceDefault, Set: true}
	}
	return MarkupResolution{}
}
