// Package apikey verifies bearer API keys on the hot path: prefix-indexed lookup,
// constant-time hash comparison and a short-lived positive/negative cache so normal
// request handling never blocks on SQLite.
package apikey

import (
	"context"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

// Store is the persistence subset the verifier needs.
type Store interface {
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (*domain.APIKey, error)
	// GetAPIKey resolves a key by database id. It exists for the console chat, which is an
	// in-process client of the data plane: the browser session says *which* key may be
	// spent, and no plaintext key ever exists to send as a bearer token.
	GetAPIKey(ctx context.Context, id int64) (*domain.APIKey, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	TouchAPIKey(ctx context.Context, id int64) error
}

// Config tunes the verifier.
type Config struct {
	// TTL is how long a successful verification is cached.
	TTL time.Duration
	// NegativeTTL is how long a failed verification is remembered (protects against
	// invalid keys hammering the database).
	NegativeTTL time.Duration
	// MaxEntries bounds the cache; expired entries are purged before eviction.
	MaxEntries int
	// TouchInterval throttles last_used_at updates.
	TouchInterval time.Duration
}

// DefaultConfig mirrors the configuration defaults (auth.key_cache_ttl_s = 30).
func DefaultConfig() Config {
	return Config{
		TTL:           30 * time.Second,
		NegativeTTL:   5 * time.Second,
		MaxEntries:    10000,
		TouchInterval: 60 * time.Second,
	}
}

type entry struct {
	key       *domain.APIKey
	account   *domain.Account
	err       error
	expiresAt time.Time
	touchedAt time.Time
}

// Verifier resolves bearer tokens to API keys and accounts.
type Verifier struct {
	store Store
	cfg   Config

	mu    sync.RWMutex
	cache map[string]*entry
	// touchedIDs throttles last_used_at for keys resolved by id (console chat), which have
	// no cache entry to hang the timestamp on.
	touchedIDs map[int64]time.Time

	now func() time.Time
}

// New builds a verifier.
func New(store Store, cfg Config) *Verifier {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultConfig().TTL
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = DefaultConfig().NegativeTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultConfig().MaxEntries
	}
	if cfg.TouchInterval <= 0 {
		cfg.TouchInterval = DefaultConfig().TouchInterval
	}
	return &Verifier{
		store:      store,
		cfg:        cfg,
		cache:      make(map[string]*entry),
		touchedIDs: make(map[int64]time.Time),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock (tests).
func (v *Verifier) SetClock(now func() time.Time) { v.now = now }

// Verify authenticates a bearer token and returns the key together with its account.
func (v *Verifier) Verify(ctx context.Context, token string) (*domain.APIKey, *domain.Account, error) {
	raw := secret.Normalize(token)
	if raw == "" {
		return nil, nil, domain.ErrUnauthorized("missing API key")
	}
	prefix := secret.Prefix(raw)
	wantHash := secret.Hash(raw)
	now := v.now()

	if e, ok := v.lookup(prefix, now); ok {
		if e.err != nil {
			return nil, nil, e.err
		}
		if !secret.Equal(e.key.KeyHash, wantHash) {
			return nil, nil, domain.ErrUnauthorized("invalid API key")
		}
		v.touchAsync(prefix, e, now)
		return e.key, e.account, nil
	}

	key, err := v.store.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if domain.IsUnauthorized(err) {
			v.storeNegative(prefix, now)
			return nil, nil, domain.ErrUnauthorized("invalid API key")
		}
		return nil, nil, err
	}
	if !secret.Equal(key.KeyHash, wantHash) {
		v.storeNegative(prefix, now)
		return nil, nil, domain.ErrUnauthorized("invalid API key")
	}
	if key.Status != "active" {
		v.storeNegative(prefix, now)
		return nil, nil, domain.ErrUnauthorized("API key is not active")
	}
	if key.ExpiresAt != nil && now.After(*key.ExpiresAt) {
		v.storeNegative(prefix, now)
		return nil, nil, domain.ErrUnauthorized("API key has expired")
	}

	account, err := v.store.GetAccount(ctx, key.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if account.Status != "active" {
		// Credentials are valid but the account is suspended: 402, not 401.
		return nil, nil, domain.ErrInsufficientQuota("account " + account.Name + " is " + account.Status)
	}

	// Seed touchedAt from the persisted last_used_at so the throttle also applies to
	// the first request after a cache miss.
	touchedAt := time.Time{}
	if key.LastUsedAt != nil {
		touchedAt = *key.LastUsedAt
	}
	e := &entry{key: key, account: account, expiresAt: now.Add(v.cfg.TTL), touchedAt: touchedAt}
	v.put(prefix, e)
	v.touchAsync(prefix, e, now)
	return key, account, nil
}

// VerifyID authenticates a key by its database id, applying exactly the same validity
// checks as Verify: the key exists, is active, has not expired, and its account is active.
//
// It is the in-process variant used by the console chat, where the browser session names
// which key may be spent instead of holding its plaintext. It deliberately does not read
// the verification cache: a chat turn can run for minutes, and a key revoked in the middle
// of a conversation must stop working on the very next step. The cost is one indexed row
// read per model step, which is noise next to a multi-second upstream call.
func (v *Verifier) VerifyID(ctx context.Context, id int64) (*domain.APIKey, *domain.Account, error) {
	now := v.now()
	key, err := v.store.GetAPIKey(ctx, id)
	if err != nil {
		if domain.IsUnauthorized(err) {
			return nil, nil, domain.ErrUnauthorized("invalid API key")
		}
		return nil, nil, err
	}
	if key.Status != "active" {
		return nil, nil, domain.ErrUnauthorized("API key is not active")
	}
	if key.ExpiresAt != nil && now.After(*key.ExpiresAt) {
		return nil, nil, domain.ErrUnauthorized("API key has expired")
	}
	account, err := v.store.GetAccount(ctx, key.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if account.Status != "active" {
		return nil, nil, domain.ErrInsufficientQuota("account " + account.Name + " is " + account.Status)
	}
	v.touchByID(ctx, key, now)
	return key, account, nil
}

// touchByID refreshes last_used_at for a key resolved by id, under the same throttle that
// touchAsync applies to bearer traffic.
func (v *Verifier) touchByID(ctx context.Context, key *domain.APIKey, now time.Time) {
	v.mu.Lock()
	last, seen := v.touchedIDs[key.ID]
	if !seen && key.LastUsedAt != nil {
		last, seen = *key.LastUsedAt, true
	}
	if seen && now.Sub(last) < v.cfg.TouchInterval {
		v.mu.Unlock()
		return
	}
	v.touchedIDs[key.ID] = now
	v.mu.Unlock()
	_ = v.store.TouchAPIKey(ctx, key.ID)
}

// Invalidate drops one cache entry (called by the admin API after a key write).
func (v *Verifier) Invalidate(prefix string) {
	v.mu.Lock()
	delete(v.cache, prefix)
	v.mu.Unlock()
}

// InvalidateAll clears the cache (account/tag changes).
func (v *Verifier) InvalidateAll() {
	v.mu.Lock()
	v.cache = make(map[string]*entry, len(v.cache))
	v.mu.Unlock()
}

// Size reports the current cache size (diagnostics).
func (v *Verifier) Size() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.cache)
}

func (v *Verifier) lookup(prefix string, now time.Time) (*entry, bool) {
	v.mu.RLock()
	e, ok := v.cache[prefix]
	v.mu.RUnlock()
	if !ok || now.After(e.expiresAt) {
		return nil, false
	}
	return e, true
}

func (v *Verifier) storeNegative(prefix string, now time.Time) {
	v.put(prefix, &entry{
		err:       domain.ErrUnauthorized("invalid API key"),
		expiresAt: now.Add(v.cfg.NegativeTTL),
	})
}

func (v *Verifier) put(prefix string, e *entry) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= v.cfg.MaxEntries {
		v.purgeLocked(v.now())
	}
	v.cache[prefix] = e
}

// purgeLocked removes expired entries; if the cache is still full it evicts the
// entry that expires soonest. Callers must hold the write lock.
func (v *Verifier) purgeLocked(now time.Time) {
	for prefix, e := range v.cache {
		if now.After(e.expiresAt) {
			delete(v.cache, prefix)
		}
	}
	if len(v.cache) < v.cfg.MaxEntries {
		return
	}
	var oldestPrefix string
	var oldest time.Time
	for prefix, e := range v.cache {
		if oldestPrefix == "" || e.expiresAt.Before(oldest) {
			oldestPrefix, oldest = prefix, e.expiresAt
		}
	}
	if oldestPrefix != "" {
		delete(v.cache, oldestPrefix)
	}
}

// touchAsync refreshes last_used_at at most once per TouchInterval, out of band.
func (v *Verifier) touchAsync(prefix string, e *entry, now time.Time) {
	if now.Sub(e.touchedAt) < v.cfg.TouchInterval {
		return
	}
	v.mu.Lock()
	if cur, ok := v.cache[prefix]; ok && cur == e {
		e.touchedAt = now
	}
	v.mu.Unlock()

	id := e.key.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = v.store.TouchAPIKey(ctx, id)
	}()
}
