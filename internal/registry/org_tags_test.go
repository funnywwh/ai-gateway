package registry

import (
	"slices"
	"sort"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// tagsFor builds the tag table the inheritance tests resolve against. Priorities are chosen
// so that "priority order" and "inheritance order" are different sequences: if the
// implementation forgot to sort, these tests would still see the inheritance order and fail.
func tagsFor() []*domain.Tag {
	return []*domain.Tag{
		{ID: 1, Name: "internal", GrantsJSON: `{"models":["*"]}`, Priority: 30},
		{ID: 2, Name: "vip", GrantsJSON: `{"models":["gpt-*"]}`, Priority: 10},
		{ID: 3, Name: "free", GrantsJSON: `{"models":["small-*"]}`, Priority: 20},
		{ID: 4, Name: "own", GrantsJSON: `{"providers":["deepseek"]}`, Priority: 5},
	}
}

// orgNodes is 总部(1) ── 研发部(2) ── 平台组(3), with tags spread over the levels.
func orgNodes() []*domain.OrgNode {
	hq := int64(1)
	dev := int64(2)
	return []*domain.OrgNode{
		{ID: 1, Name: "总部", TagsJSON: `["internal"]`},
		{ID: 2, Name: "研发部", ParentID: &hq, TagsJSON: `["vip"]`},
		{ID: 3, Name: "平台组", ParentID: &dev, TagsJSON: `["free"]`},
	}
}

func names(tags []*domain.Tag) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, tag.Name)
	}
	return out
}

// buildWithOrg is the snapshot a deployment with an organization tree gets.
func buildWithOrg(accounts []*domain.Account, tags []*domain.Tag, memberships []domain.OrgMembership) *Snapshot {
	return Build(Input{Accounts: accounts, Tags: tags, OrgNodes: orgNodes(), Memberships: memberships})
}

func TestAccountInheritsOrganizationTags(t *testing.T) {
	accounts := []*domain.Account{{ID: 7, Name: "acme"}}
	snap := buildWithOrg(accounts, tagsFor(), []domain.OrgMembership{{NodeID: 3, AccountID: 7}})

	// The account itself carries no tags at all: everything it has comes from the tree.
	key := &domain.APIKey{ID: 1, AccountID: 7, Status: "active"}
	got := names(ResolveTagRecords(snap, key))
	// Priority order, not inheritance order: vip(10) before free(20) before internal(30).
	if want := []string{"vip", "free", "internal"}; !slices.Equal(got, want) {
		t.Fatalf("effective tags = %v, want %v", got, want)
	}
	if inherited := snap.InheritedTagNames(7); !slices.Equal(inherited, []string{"internal", "vip", "free"}) {
		t.Fatalf("inherited names = %v, want root-first", inherited)
	}
}

func TestAccountWithoutMembershipInheritsNothing(t *testing.T) {
	accounts := []*domain.Account{{ID: 7, Name: "acme"}}
	snap := buildWithOrg(accounts, tagsFor(), nil)
	key := &domain.APIKey{ID: 1, AccountID: 7, Status: "active"}
	if got := ResolveTagRecords(snap, key); len(got) != 0 {
		t.Fatalf("an account outside the organization tree resolved %v, want nothing", names(got))
	}
	if got := snap.InheritedTagNames(7); len(got) != 0 {
		t.Fatalf("inherited names = %v, want none", got)
	}
}

// TestOrganizationAndAccountTagsCompose is the ordering contract: inherited names come first,
// the account's own names after them, and the key's last — then everything is sorted by
// priority, so the "own" tag (priority 5) leads even though it was listed last.
func TestOrganizationAndAccountTagsCompose(t *testing.T) {
	accounts := []*domain.Account{{ID: 7, Name: "acme", TagsJSON: `["free","vip"]`}}
	snap := buildWithOrg(accounts, tagsFor(), []domain.OrgMembership{{NodeID: 2, AccountID: 7}})

	key := &domain.APIKey{ID: 1, AccountID: 7, Status: "active", TagsJSON: `["own"]`}
	got := names(ResolveTagRecords(snap, key))
	// own(5) → vip(10) → free(20) → internal(30).
	// "vip" appears both as an inherited name and on the account: it must contribute once.
	if want := []string{"own", "vip", "free", "internal"}; !slices.Equal(got, want) {
		t.Fatalf("effective tags = %v, want %v", got, want)
	}
}

func TestInheritanceOrderIsStableAtEqualPriority(t *testing.T) {
	// Every tag has the same priority, so the input order decides: the stable sort must keep
	// ancestors before descendants and the account's own names last of the account side.
	flat := []*domain.Tag{
		{ID: 1, Name: "internal", Priority: 100},
		{ID: 2, Name: "vip", Priority: 100},
		{ID: 3, Name: "free", Priority: 100},
		{ID: 4, Name: "own", Priority: 100},
	}
	accounts := []*domain.Account{{ID: 7, Name: "acme", TagsJSON: `["free"]`}}
	snap := buildWithOrg(accounts, flat, []domain.OrgMembership{{NodeID: 3, AccountID: 7}})
	key := &domain.APIKey{ID: 1, AccountID: 7, TagsJSON: `["own"]`}
	got := names(ResolveTagRecords(snap, key))
	if want := []string{"internal", "vip", "free", "own"}; !slices.Equal(got, want) {
		t.Fatalf("equal-priority order = %v, want %v (ancestors, then the account's own, then the key's)", got, want)
	}
}

// TestSeveralMembershipsUnionTheirChains covers the many-to-many choice: an account in two
// departments sees both chains, and the shared ancestor contributes once.
func TestSeveralMembershipsUnionTheirChains(t *testing.T) {
	accounts := []*domain.Account{{ID: 7, Name: "acme"}}
	snap := buildWithOrg(accounts, tagsFor(), []domain.OrgMembership{
		{NodeID: 3, AccountID: 7}, // 平台组 → 研发部 → 总部
		{NodeID: 2, AccountID: 7}, // 研发部 → 总部 (already covered)
	})
	key := &domain.APIKey{ID: 1, AccountID: 7, Status: "active"}
	got := names(ResolveTagRecords(snap, key))
	if want := []string{"vip", "free", "internal"}; !slices.Equal(got, want) {
		t.Fatalf("effective tags = %v, want %v", got, want)
	}
}

// TestUnknownInheritedTagNameIsDropped pins the tolerance that makes a typo non-fatal: a node
// may name a tag that does not exist, and then it simply contributes nothing.
func TestUnknownInheritedTagNameIsDropped(t *testing.T) {
	hq := int64(1)
	nodes := []*domain.OrgNode{
		{ID: 1, Name: "总部", TagsJSON: `["ghost","internal"]`},
		{ID: 2, Name: "研发部", ParentID: &hq, TagsJSON: `["vip"]`},
	}
	snap := Build(Input{
		Accounts: []*domain.Account{{ID: 7, Name: "acme"}}, Tags: tagsFor(), OrgNodes: nodes,
		Memberships: []domain.OrgMembership{{NodeID: 2, AccountID: 7}},
	})
	got := names(ResolveTagRecords(snap, &domain.APIKey{ID: 1, AccountID: 7}))
	if want := []string{"vip", "internal"}; !slices.Equal(got, want) {
		t.Fatalf("effective tags = %v, want %v", got, want)
	}
}

// TestSnapshotSurvivesACorruptTree documents the defensive posture: a cycle in the data (which
// the write path refuses, but a hand-edited database can hold) must not hang the resolver.
func TestSnapshotSurvivesACorruptTree(t *testing.T) {
	nodes := []*domain.OrgNode{
		{ID: 1, Name: "a", TagsJSON: `["vip"]`},
		{ID: 2, Name: "b", TagsJSON: `["free"]`},
	}
	one, two := int64(1), int64(2)
	nodes[0].ParentID = &two
	nodes[1].ParentID = &one
	snap := Build(Input{
		Accounts: []*domain.Account{{ID: 7, Name: "acme"}}, Tags: tagsFor(), OrgNodes: nodes,
		Memberships: []domain.OrgMembership{{NodeID: 1, AccountID: 7}},
	})
	got := names(ResolveTagRecords(snap, &domain.APIKey{ID: 1, AccountID: 7}))
	if len(got) == 0 {
		t.Fatal("a cycle must still resolve the tags it can reach, not resolve nothing")
	}
}

// TestNewSnapshotKeepsTheAccountSideOfTagResolution guards the 12 existing callers of the old
// constructor: they pass no organization data, and their account tags must still resolve.
func TestNewSnapshotKeepsTheAccountSideOfTagResolution(t *testing.T) {
	snap := NewSnapshot([]*domain.Account{{ID: 7, Name: "acme", TagsJSON: `["vip"]`}},
		nil, nil, nil, nil, nil, tagsFor())
	got := names(ResolveTagRecords(snap, &domain.APIKey{ID: 1, AccountID: 7, Status: "active"}))
	if want := []string{"vip"}; !slices.Equal(got, want) {
		t.Fatalf("NewSnapshot resolved %v, want %v", got, want)
	}
}

// TestResolutionMatchesThePreM49Implementation is the equivalence check for the refactor that
// moved account tag resolution into the snapshot build. The old implementation is reproduced
// here verbatim (parse both lists, first occurrence wins, look up, stable sort by priority) and
// both must agree on every shape in the table.
func TestResolutionMatchesThePreM49Implementation(t *testing.T) {
	oldResolve := func(snap *Snapshot, key *domain.APIKey) []*domain.Tag {
		names := make([]string, 0, 4)
		if account := snap.AccountByID[key.AccountID]; account != nil {
			names = appendTagNames(names, account.TagsJSON)
		}
		names = appendTagNames(names, key.TagsJSON)

		seen := make(map[string]struct{}, len(names))
		tags := make([]*domain.Tag, 0, len(names))
		for _, name := range names {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			if tag := snap.TagByName[name]; tag != nil {
				tags = append(tags, tag)
			}
		}
		sort.SliceStable(tags, func(i, j int) bool { return tags[i].Priority < tags[j].Priority })
		return tags
	}

	cases := []struct {
		name   string
		acct   *domain.Account
		key    *domain.APIKey
		absent bool // the account id is not in the snapshot at all
	}{
		{name: "no tags anywhere", acct: &domain.Account{ID: 7, Name: "a"}, key: &domain.APIKey{AccountID: 7}},
		{name: "account tags only", acct: &domain.Account{ID: 7, TagsJSON: `["vip","free"]`}, key: &domain.APIKey{AccountID: 7}},
		{name: "key tags only", acct: &domain.Account{ID: 7, Name: "a"}, key: &domain.APIKey{AccountID: 7, TagsJSON: `["own"]`}},
		{
			name: "both, with a name on each side",
			acct: &domain.Account{ID: 7, TagsJSON: `["vip"]`},
			key:  &domain.APIKey{AccountID: 7, TagsJSON: `["own","vip"]`},
		},
		{name: "unknown names on both sides", acct: &domain.Account{ID: 7, TagsJSON: `["ghost"]`}, key: &domain.APIKey{AccountID: 7, TagsJSON: `["phantom"]`}},
		{name: "malformed account tags", acct: &domain.Account{ID: 7, TagsJSON: `not json`}, key: &domain.APIKey{AccountID: 7, TagsJSON: `["vip"]`}},
		{name: "malformed key tags", acct: &domain.Account{ID: 7, TagsJSON: `["vip"]`}, key: &domain.APIKey{AccountID: 7, TagsJSON: `{`}},
		{name: "empty name entries", acct: &domain.Account{ID: 7, TagsJSON: `["","  "]`}, key: &domain.APIKey{AccountID: 7, TagsJSON: `[" vip "]`}},
		{name: "duplicate names on one side", acct: &domain.Account{ID: 7, TagsJSON: `["vip","vip"]`}, key: &domain.APIKey{AccountID: 7, TagsJSON: `["own","own"]`}},
		{name: "account id not in the snapshot", acct: nil, key: &domain.APIKey{AccountID: 99, TagsJSON: `["vip"]`}, absent: true},
		{name: "no account on the key", acct: &domain.Account{ID: 7, TagsJSON: `["vip"]`}, key: &domain.APIKey{AccountID: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accounts := []*domain.Account{}
			if tc.acct != nil {
				accounts = append(accounts, tc.acct)
			}
			// Both constructors must agree: Build is what Reload uses, NewSnapshot what the
			// other callers use, and neither may drift from the other.
			for _, snap := range []*Snapshot{
				Build(Input{Accounts: accounts, Tags: tagsFor()}),
				NewSnapshot(accounts, nil, nil, nil, nil, nil, tagsFor()),
			} {
				want := names(oldResolve(snap, tc.key))
				got := names(ResolveTagRecords(snap, tc.key))
				if !slices.Equal(got, want) {
					t.Fatalf("new resolver = %v, the pre-M49 resolver = %v", got, want)
				}
			}
		})
	}
}

// TestFastPathReturnsTheSnapshotsOwnSliceHoldingNoAllocation is the mechanism behind the
// zero-allocation promise: the fast path must hand back the precomputed slice itself, not a
// copy that happens to be empty.
func TestFastPathReturnsTheSnapshotsOwnSliceHoldingNoAllocation(t *testing.T) {
	accounts := []*domain.Account{{ID: 7, Name: "acme"}}
	snap := buildWithOrg(accounts, tagsFor(), []domain.OrgMembership{{NodeID: 2, AccountID: 7}})
	key := &domain.APIKey{ID: 1, AccountID: 7, Status: "active"}

	first := ResolveTagRecords(snap, key)
	second := ResolveTagRecords(snap, key)
	if len(first) == 0 {
		t.Fatal("expected inherited tags")
	}
	if &first[0] != &second[0] {
		t.Fatal("the fast path allocated a fresh slice; it must return the snapshot's own")
	}
}

// TestOrgNodePathRendersTheChain feeds the console's "所属组织" column.
func TestOrgNodePathRendersTheChain(t *testing.T) {
	snap := Build(Input{OrgNodes: orgNodes()})
	if got := snap.OrgNodePath(3); got != "总部/研发部/平台组" {
		t.Fatalf("path = %q", got)
	}
	if got := snap.OrgNodePath(999); got != "" {
		t.Fatalf("unknown node path = %q, want empty", got)
	}
}
