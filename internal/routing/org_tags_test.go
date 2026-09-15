package routing

import (
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// orgFixture is the organization shape these tests inherit from:
//
//	总部(1)[acme-tag] ── 研发部(2)[gpt-tag] ── 平台组(3)[slow-tag]
//
// The tags deliberately differ in what they contribute — models, a rate limit, a routing
// strategy — so each inheritance claim is observable in a different part of the plan.
func orgFixture(t *testing.T, memberships []domain.OrgMembership) *registry.Snapshot {
	t.Helper()
	hq, dev := int64(1), int64(2)
	nodes := []*domain.OrgNode{
		{ID: 1, Name: "总部", TagsJSON: `["acme-tag"]`},
		{ID: 2, Name: "研发部", ParentID: &hq, TagsJSON: `["gpt-tag"]`},
		{ID: 3, Name: "平台组", ParentID: &dev, TagsJSON: `["slow-tag"]`},
	}
	tags := []*domain.Tag{
		{ID: 1, Name: "acme-tag", Priority: 30, GrantsJSON: `{"models":["acme-model"]}`},
		{ID: 2, Name: "gpt-tag", Priority: 20, GrantsJSON: `{"providers":["deepseek"]}`},
		{ID: 3, Name: "slow-tag", Priority: 10, PolicyJSON: `{"strategy":"strict"}`},
	}
	return registry.Build(registry.Input{
		Accounts:    []*domain.Account{{ID: 1, Name: "acme"}},
		Tags:        tags,
		OrgNodes:    nodes,
		Memberships: memberships,
	})
}

// TestOrganizationTagsWidenTheGrant is the core promise of the feature: a tag attached to a
// department reaches the department's accounts, so an account whose key grants nothing can
// still reach the models and providers its organization was given.
func TestOrganizationTagsWidenTheGrant(t *testing.T) {
	snap := orgFixture(t, []domain.OrgMembership{{NodeID: 3, AccountID: 1}})
	r := newRouter(Config{DefaultGrant: "none"}, snap)

	// Neither the key nor the account carries any tag of its own.
	key := keyWith("", `{"models":["own-model"]}`, "")
	grant := r.Authorize(key, r.ResolveTags(snap, key))

	for _, model := range []string{"own-model", "acme-model"} {
		if !grant.Models[model] {
			t.Errorf("model %q is not granted; the organization tags did not reach the grant: %v", model, grant.Models)
		}
	}
	if !grant.Providers["deepseek"] {
		t.Errorf("provider deepseek is not granted: %v", grant.Providers)
	}
	if grant.Models["*"] {
		t.Error("default_grant=none must not be applied once the organization grants something")
	}
}

// TestOrganizationTagsReachTheRoutePlan proves the inheritance arrives through the same path
// the data plane uses, not just through a direct Authorize call.
func TestOrganizationTagsReachTheRoutePlan(t *testing.T) {
	snap := orgFixture(t, []domain.OrgMembership{{NodeID: 2, AccountID: 1}})
	r := newRouter(Config{DefaultGrant: "none"}, snap)
	key := keyWith("", "", "")

	grant := r.Authorize(key, r.ResolveTags(snap, key))
	if !grant.Providers["deepseek"] {
		t.Fatalf("the department's provider grant did not reach the plan: %v", grant.Providers)
	}
}

// TestOrganizationPolicyParticipatesInTheMerge checks the other half of the feature: an
// inherited tag's policy is merged, and the key's own policy still wins because it is applied
// last.
func TestOrganizationPolicyParticipatesInTheMerge(t *testing.T) {
	snap := orgFixture(t, []domain.OrgMembership{{NodeID: 3, AccountID: 1}})
	r := newRouter(Config{}, snap)

	key := keyWith("", "", "")
	grant := r.Authorize(key, r.ResolveTags(snap, key))
	if !grant.Policy.StrategySet || grant.Policy.Strategy != "strict" {
		t.Fatalf("the inherited policy was not merged: %+v", grant.Policy)
	}

	// The key overrides what it inherited.
	overriding := keyWith("", "", `{"strategy":"weighted_random"}`)
	grant = r.Authorize(overriding, r.ResolveTags(snap, overriding))
	if grant.Policy.Strategy != "weighted_random" {
		t.Fatalf("the key policy must win over the inherited one: %+v", grant.Policy)
	}
}

// TestLeavingTheOrganizationRevokesTheGrant is the recycling direction: the same key that was
// widened by membership loses the extra permissions once the account is moved out.
func TestLeavingTheOrganizationRevokesTheGrant(t *testing.T) {
	inside := orgFixture(t, []domain.OrgMembership{{NodeID: 2, AccountID: 1}})
	outside := orgFixture(t, nil)
	key := keyWith("", "", "")

	r := newRouter(Config{DefaultGrant: "none"}, inside)
	if !r.Authorize(key, r.ResolveTags(inside, key)).Providers["deepseek"] {
		t.Fatal("precondition: membership must grant the provider")
	}
	// The same key against the tree it is no longer part of: the grant is gone.
	left := newRouter(Config{DefaultGrant: "none"}, outside)
	if left.Authorize(key, left.ResolveTags(outside, key)).Providers["deepseek"] {
		t.Fatal("an account that left the organization must lose the inherited grant")
	}
}

// TestInheritedNamesAppearInTheEffectiveTagList is what the console shows as 生效标签.
func TestInheritedNamesAppearInTheEffectiveTagList(t *testing.T) {
	snap := orgFixture(t, []domain.OrgMembership{{NodeID: 3, AccountID: 1}})
	key := keyWith(`["own-tag"]`, "", "")
	got := registry.ResolveTagNames(snap, key)
	// slow-tag(10) → gpt-tag(20) → acme-tag(30); own-tag is unknown to the tag table so it is
	// dropped, exactly as it would have been before organizations existed.
	if want := "slow-tag,gpt-tag,acme-tag"; strings.Join(got, ",") != want {
		t.Fatalf("effective tags = %v, want %s", got, want)
	}
}
