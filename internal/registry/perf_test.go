package registry

import (
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// The tag-resolution path runs on every authenticated request — the model list, the quota
// limits and the route plan each ask for the same effective tags — so its allocation count
// is a contract, not a detail. These benchmarks and the AllocsPerRun guard below are what
// keep a future change (organization inheritance was the first one) from quietly turning a
// table lookup back into JSON parsing per call.
//
// Measured on this machine (12th Gen i7-12700K), 3 runs each:
//
//	                                  before M49              after M49
//	without key tags          512 ns   320 B  11 allocs    3.5 ns    0 B  0 allocs
//	with key tags             800 ns   536 B  17 allocs    370 ns  288 B  9 allocs
//	routing.BenchmarkPlan    3402 ns  4948 B  45 allocs   3459 ns 4948 B 45 allocs
//
// The account side is decoded once per snapshot build instead of once per call, which is also
// what makes organization inheritance affordable: ancestor tags cost nothing at request time
// because they are already folded in when the snapshot is built. Plan's allocation count is
// unchanged, which is the bar this design has to hold: an organization membership must not
// make the routing path more expensive than it was before the feature existed.

func perfTagSnapshot(tags []*domain.Tag, accounts []*domain.Account) *Snapshot {
	return NewSnapshot(accounts, nil, nil, nil, nil, nil, tags)
}

func perfTags() []*domain.Tag {
	return []*domain.Tag{
		{ID: 1, Name: "free", GrantsJSON: `{"models":["*"],"providers":["*"]}`, Priority: 100},
		{ID: 2, Name: "team", GrantsJSON: `{"models":["gpt-*"]}`, PolicyJSON: `{"rpm":60}`, Priority: 10},
		{ID: 3, Name: "vip", GrantsJSON: `{"models":["*"]}`, PolicyJSON: `{"rpm":600}`, Priority: 20},
	}
}

func perfAccounts() []*domain.Account {
	return []*domain.Account{
		{ID: 1, Name: "acct-1", TagsJSON: `["free","team"]`},
		{ID: 2, Name: "acct-2", TagsJSON: `["vip"]`},
	}
}

// BenchmarkResolveTagRecordsWithoutKeyTags is the fast path: the caller's key carries no
// tags of its own, so the answer is entirely the account side and can be served without
// building anything.
func BenchmarkResolveTagRecordsWithoutKeyTags(b *testing.B) {
	snap := perfTagSnapshot(perfTags(), perfAccounts())
	key := &domain.APIKey{ID: 1, AccountID: 1, Status: "active"}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if got := ResolveTagRecords(snap, key); len(got) != 2 {
			b.Fatalf("resolved %d tags, want 2", len(got))
		}
	}
}

// BenchmarkResolveTagRecordsWithKeyTags is the general path: account tags and the key's own
// tags are unioned (account names win a name collision) and ordered by priority.
func BenchmarkResolveTagRecordsWithKeyTags(b *testing.B) {
	snap := perfTagSnapshot(perfTags(), perfAccounts())
	key := &domain.APIKey{ID: 1, AccountID: 1, Status: "active", TagsJSON: `["vip"]`}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if got := ResolveTagRecords(snap, key); len(got) != 3 {
			b.Fatalf("resolved %d tags, want 3", len(got))
		}
	}
}

// TestResolveTagRecordsFastPathDoesNotAllocate pins the zero-allocation promise of the fast
// path. A regression here is invisible in behaviour and obvious in production, which is
// exactly the kind of change a test has to catch.
func TestResolveTagRecordsFastPathDoesNotAllocate(t *testing.T) {
	snap := perfTagSnapshot(perfTags(), perfAccounts())
	key := &domain.APIKey{ID: 1, AccountID: 1, Status: "active"}
	allocs := testing.AllocsPerRun(200, func() {
		ResolveTagRecords(snap, key)
	})
	if allocs != 0 {
		t.Fatalf("resolving the account's own tags allocates %.1f times per call, want 0: "+
			"the account side is materialized when the snapshot is built and must be returned "+
			"as-is (callers only read it)", allocs)
	}
}
