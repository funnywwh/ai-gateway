package billing

import (
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

func TestResolveMarkupPrecedence(t *testing.T) {
	key := func(policy string) *domain.APIKey { return &domain.APIKey{PolicyJSON: policy} }
	tag := func(policy string) *domain.Tag { return &domain.Tag{PolicyJSON: policy} }

	cases := []struct {
		name    string
		key     *domain.APIKey
		tags    []*domain.Tag
		account *domain.Account
		model   int
		def     int
		wantBP  int
		wantSrc string
	}{
		{
			name: "key policy wins over everything",
			key:  key(`{"margin_bp":9000}`), tags: []*domain.Tag{tag(`{"margin_bp":11000}`)},
			account: &domain.Account{MarkupOverrideBP: 12000, MarkupOverrideSet: true},
			model:   13000, def: 14000, wantBP: 9000, wantSrc: MarkupSourceKey,
		},
		{
			name: "the highest-priority tag wins",
			key:  key(""), tags: []*domain.Tag{tag(`{"margin_bp":11000}`), tag(`{"margin_bp":11500}`)},
			account: &domain.Account{MarkupOverrideBP: 12000, MarkupOverrideSet: true},
			model:   13000, def: 14000, wantBP: 11500, wantSrc: MarkupSourceTag,
		},
		{
			name:    "account override beats the model",
			key:     key(""),
			account: &domain.Account{MarkupOverrideBP: 12000, MarkupOverrideSet: true},
			model:   13000, def: 14000, wantBP: 12000, wantSrc: MarkupSourceAccount,
		},
		{
			name:    "no override means the model value",
			key:     key(""),
			account: &domain.Account{MarkupOverrideBP: 0},
			model:   13000, def: 14000, wantBP: 13000, wantSrc: MarkupSourceModel,
		},
		{
			name:    "nothing set falls back to the global default",
			key:     key(`{"strategy":"strict_order"}`),
			account: &domain.Account{}, def: 14000, wantBP: 14000, wantSrc: MarkupSourceDefault,
		},
		{
			name:    "a zero override is honoured, not treated as unset",
			key:     key(""),
			account: &domain.Account{MarkupOverrideBP: 0, MarkupOverrideSet: true},
			model:   13000, def: 14000, wantBP: 0, wantSrc: MarkupSourceAccount,
		},
	}
	for _, tc := range cases {
		got := ResolveMarkup(tc.key, tc.tags, tc.account, tc.model, tc.def)
		if got.BP != tc.wantBP || got.Source != tc.wantSrc || !got.Set {
			t.Errorf("%s: got %+v, want bp=%d source=%s", tc.name, got, tc.wantBP, tc.wantSrc)
		}
	}
}

func TestResolveMarkupIgnoresMalformedPolicies(t *testing.T) {
	key := &domain.APIKey{PolicyJSON: `not json`}
	tags := []*domain.Tag{{PolicyJSON: `{"margin_bp":-5}`}}
	got := ResolveMarkup(key, tags, &domain.Account{}, 12500, 10000)
	if got.BP != 12500 || got.Source != MarkupSourceModel {
		t.Fatalf("got %+v, want the model value", got)
	}
}
