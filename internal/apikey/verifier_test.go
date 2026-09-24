package apikey

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

type fakeStore struct {
	mu sync.Mutex
	key *domain.APIKey
	// siblings are further rows that answer lookups. The M87 regression needs two keys that share
	// a display prefix, so one row is not enough.
	siblings        []*domain.APIKey
	account         *domain.Account
	getKeyCalls     int
	getAccountCalls int
	touchCalls      int
}

func (f *fakeStore) GetAPIKeyByHash(ctx context.Context, hash string) (*domain.APIKey, error) {
	f.mu.Lock()
	f.getKeyCalls++
	rows := append([]*domain.APIKey{f.key}, f.siblings...)
	f.mu.Unlock()
	for _, k := range rows {
		if k != nil && k.KeyHash == hash {
			return k, nil
		}
	}
	return nil, domain.ErrUnauthorized("invalid API key")
}

// GetAPIKey resolves a key by id, which is how the console chat names the key it bills.
func (f *fakeStore) GetAPIKey(ctx context.Context, id int64) (*domain.APIKey, error) {
	f.mu.Lock()
	f.getKeyCalls++
	f.mu.Unlock()
	if f.key == nil || f.key.ID != id {
		return nil, domain.ErrUnauthorized("invalid API key")
	}
	return f.key, nil
}

func (f *fakeStore) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	f.mu.Lock()
	f.getAccountCalls++
	f.mu.Unlock()
	if f.account == nil {
		return nil, domain.ErrNotFound("account")
	}
	return f.account, nil
}

func (f *fakeStore) TouchAPIKey(ctx context.Context, id int64) error {
	f.mu.Lock()
	f.touchCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getKeyCalls, f.getAccountCalls, f.touchCalls
}

const token = "sk-gw-test-token-1234567890"

func fixture() (*fakeStore, *Verifier, *time.Time) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		key: &domain.APIKey{
			ID: 1, AccountID: 7, Name: "dev",
			KeyPrefix: secret.Prefix(token), KeyHash: secret.Hash(token),
			Status: "active", RecordInputMode: "inherit",
		},
		account: &domain.Account{ID: 7, Name: "acme", Status: "active", BillingMode: domain.BillingPostpaid},
	}
	v := New(store, Config{TTL: 30 * time.Second, NegativeTTL: 5 * time.Second, MaxEntries: 100, TouchInterval: time.Minute})
	v.SetClock(func() time.Time { return now })
	return store, v, &now
}

func TestVerifyHappyPathAndCache(t *testing.T) {
	store, v, now := fixture()
	ctx := context.Background()

	key, account, err := v.Verify(ctx, "Bearer "+token)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != 1 || account.Name != "acme" {
		t.Fatalf("verify mismatch: %+v %+v", key, account)
	}

	// Second call must be served from cache.
	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	if k, a, _ := store.counts(); k != 1 || a != 1 {
		t.Fatalf("expected a single store lookup, got key=%d account=%d", k, a)
	}

	// After the TTL the cache entry expires.
	*now = now.Add(31 * time.Second)
	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := store.counts(); k != 2 {
		t.Fatalf("expected a refresh after TTL, got %d lookups", k)
	}
}

// A token that shares a known key's display prefix but is a different secret must be rejected:
// the lookup key is the hash (M87), so wearing the same label can never admit a stranger.
func TestVerifyRejectsTokenSharingAKnownPrefix(t *testing.T) {
	store, v, _ := fixture()
	other := secret.Prefix(token) + "different-suffix"
	if secret.Prefix(other) != secret.Prefix(token) {
		t.Fatalf("fixture must produce a shared display prefix: %q vs %q", secret.Prefix(other), secret.Prefix(token))
	}
	if _, _, err := v.Verify(context.Background(), other); !domain.IsUnauthorized(err) {
		t.Fatalf("expected 401, got %v", err)
	}
	if k, _, _ := store.counts(); k != 1 {
		t.Fatalf("store lookups = %d", k)
	}
}

// Two keys may legitimately share a display prefix (sub2api lets callers pick their own key
// values, so a migration meets this on day one). Each one authenticates by its own hash, and
// each keeps its own cache entry.
func TestVerifyAcceptsTwoKeysSharingADisplayPrefix(t *testing.T) {
	store, v, _ := fixture()
	ctx := context.Background()

	siblingToken := "sk-gw-test-token-1234567890-sibling"
	if secret.Prefix(siblingToken) != secret.Prefix(token) {
		t.Fatalf("fixture must produce a shared display prefix")
	}
	store.siblings = []*domain.APIKey{{
		ID: 2, AccountID: 8, Name: "sibling",
		KeyPrefix: secret.Prefix(siblingToken), KeyHash: secret.Hash(siblingToken),
		Status: "active", RecordInputMode: "inherit",
	}}

	first, _, err := v.Verify(ctx, token)
	if err != nil {
		t.Fatalf("the first key of the pair must verify: %v", err)
	}
	second, _, err := v.Verify(ctx, siblingToken)
	if err != nil {
		t.Fatalf("the second key of the pair must verify: %v", err)
	}
	if first.ID == second.ID || first.AccountID == second.AccountID {
		t.Fatalf("the two keys must stay distinct: %+v %+v", first, second)
	}
	if v.Size() != 2 {
		t.Fatalf("a shared prefix must not collapse the entries: size = %d", v.Size())
	}
	// Both are cached, so a repeat costs no store lookup.
	before, _, _ := store.counts()
	if _, _, err := v.Verify(ctx, siblingToken); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := store.counts(); after != before {
		t.Fatalf("a cached key re-read the store: %d -> %d", before, after)
	}
}

// A failed verification is remembered against the presented token, never against its display
// prefix: otherwise anyone who knew a prefix (a label, not a secret) could hold that key down
// for as long as they kept presenting garbage — and two keys sharing a prefix shared their fate.
func TestNegativeCacheDoesNotPoisonASharedPrefix(t *testing.T) {
	store, v, _ := fixture()
	ctx := context.Background()

	garbage := secret.Prefix(token) + "-garbage-suffix"
	if secret.Prefix(garbage) != secret.Prefix(token) {
		t.Fatalf("fixture must produce a shared display prefix")
	}
	for i := 0; i < 3; i++ {
		if _, _, err := v.Verify(ctx, garbage); !domain.IsUnauthorized(err) {
			t.Fatalf("expected 401 for the impostor, got %v", err)
		}
	}
	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatalf("the real key must still verify: %v", err)
	}
	if k, _, _ := store.counts(); k != 2 {
		t.Fatalf("expected one lookup per distinct token, got %d", k)
	}
}

func TestVerifyCachesNegativeResults(t *testing.T) {
	store, v, _ := fixture()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, _, err := v.Verify(ctx, "sk-gw-unknown-key-000000"); !domain.IsUnauthorized(err) {
			t.Fatalf("expected 401, got %v", err)
		}
	}
	if k, _, _ := store.counts(); k != 1 {
		t.Fatalf("negative cache must collapse repeated lookups, got %d", k)
	}
}

func TestVerifyRejectsInactiveExpiredAndSuspended(t *testing.T) {
	ctx := context.Background()

	t.Run("inactive", func(t *testing.T) {
		store, v, _ := fixture()
		store.key.Status = "disabled"
		if _, _, err := v.Verify(ctx, token); !domain.IsUnauthorized(err) {
			t.Fatalf("expected 401, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		store, v, now := fixture()
		past := now.Add(-time.Hour)
		store.key.ExpiresAt = &past
		if _, _, err := v.Verify(ctx, token); !domain.IsUnauthorized(err) {
			t.Fatalf("expected 401, got %v", err)
		}
	})

	t.Run("suspended account", func(t *testing.T) {
		store, v, _ := fixture()
		store.account.Status = "suspended"
		_, _, err := v.Verify(ctx, token)
		if !domain.HasStatus(err, 402) {
			t.Fatalf("suspended account must yield 402, got %v", err)
		}
	})
}

func TestVerifyEmptyToken(t *testing.T) {
	_, v, _ := fixture()
	if _, _, err := v.Verify(context.Background(), "   "); !domain.IsUnauthorized(err) {
		t.Fatalf("expected 401 for an empty token, got %v", err)
	}
	if _, _, err := v.Verify(context.Background(), "Bearer "); !domain.IsUnauthorized(err) {
		t.Fatalf("expected 401 for an empty bearer token, got %v", err)
	}
}

func TestInvalidateForcesReload(t *testing.T) {
	store, v, _ := fixture()
	ctx := context.Background()

	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := store.counts(); k != 1 {
		t.Fatalf("lookups = %d", k)
	}

	v.Invalidate(secret.Hash(token))
	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := store.counts(); k != 2 {
		t.Fatalf("invalidate must force a reload, lookups = %d", k)
	}

	v.InvalidateAll()
	if v.Size() != 0 {
		t.Fatalf("InvalidateAll must clear the cache, size = %d", v.Size())
	}
}

func TestTouchIsThrottledAndAsynchronous(t *testing.T) {
	store, v, now := fixture()
	ctx := context.Background()

	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	// First verify already touched.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, touch := store.counts(); touch >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, touch := store.counts(); touch != 1 {
		t.Fatalf("expected exactly one touch, got %d", touch)
	}

	// Within the touch interval no further writes happen.
	*now = now.Add(10 * time.Second)
	for i := 0; i < 5; i++ {
		if _, _, err := v.Verify(ctx, token); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if _, _, touch := store.counts(); touch != 1 {
		t.Fatalf("touch must be throttled, got %d", touch)
	}

	// Past the interval it touches again.
	*now = now.Add(2 * time.Minute)
	if _, _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, touch := store.counts(); touch >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, touch := store.counts(); touch != 2 {
		t.Fatalf("expected a second touch after the interval, got %d", touch)
	}
}

func TestCacheCapacityIsBounded(t *testing.T) {
	store := &fakeStore{}
	v := New(store, Config{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 8, TouchInterval: time.Minute})
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		tok := "sk-gw-unknown-" + itoa(int64(i)) + "-suffix-padding"
		_, _, _ = v.Verify(ctx, tok)
	}
	if v.Size() > 8 {
		t.Fatalf("cache exceeded MaxEntries: %d", v.Size())
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var out []byte
	for v > 0 {
		out = append([]byte{byte('0' + v%10)}, out...)
		v /= 10
	}
	return string(out)
}
