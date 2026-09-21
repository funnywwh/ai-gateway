// Package session persists browser-to-worker session mappings without storing
// browser bearer tokens in plaintext.
package session

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

const diskVersion = 1
const maxFileBytes = 64 << 20

var ErrNotFound = errors.New("dshgw: session not found")
var ErrCapacity = errors.New("dshgw: session capacity reached")

type Upstream struct {
	Name       string    `json:"name"`
	Value      string    `json:"value"`
	Authority  string    `json:"authority"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Generation uint64    `json:"generation"`
}

type Session struct {
	Tenant             string    `json:"tenant"`
	ExpiresAt          time.Time `json:"expires_at"`
	Upstream           *Upstream `json:"upstream,omitempty"`
	UpstreamGeneration uint64    `json:"upstream_generation,omitempty"`
}

type Store interface {
	Issue(tenant string, ttl time.Duration) (string, error)
	Get(token string) (*Session, error)
	Touch(token string, ttl time.Duration) error
	Delete(token string) error
	SetUpstream(token string, u *Upstream) error
	SetUpstreamIf(token string, generation uint64, u *Upstream) (bool, error)
	ClearUpstream(token string) error
	ClearUpstreamIf(token string, generation uint64) (bool, error)
	DeleteTenant(tenant string) error
	// CountTenant reports how many unexpired sessions one tenant still holds. Logout reads it
	// to decide whether the last reader left: the tenant's dsh is stopped only then (M69).
	CountTenant(tenant string) (int, error)
}

type diskState struct {
	Version  int                 `json:"version"`
	Sessions map[string]*Session `json:"sessions"`
}

type FileStore struct {
	mu         sync.Mutex
	path       string
	now        func() time.Time
	records    map[string]*Session
	fileInfo   os.FileInfo
	maxRecords int
}

func NewFileStore(path string) (*FileStore, error) { return NewFileStoreWithLimit(path, 10000) }
func NewFileStoreWithLimit(path string, maxRecords int) (*FileStore, error) {
	if maxRecords < 1 {
		return nil, errors.New("session limit must be positive")
	}
	store, err := newFileStore(path, time.Now)
	if err == nil {
		store.maxRecords = maxRecords
	}
	return store, err
}

func newFileStore(path string, now func() time.Time) (*FileStore, error) {
	s := &FileStore{path: path, now: now, records: map[string]*Session{}, maxRecords: 10000}
	if err := s.reloadLocked(true); err != nil {
		return nil, err
	}
	return s, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *FileStore) Issue(tenant string, ttl time.Duration) (string, error) {
	if tenant == "" || ttl <= 0 {
		return "", errors.New("tenant and ttl are required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	err := s.mutate(func() (bool, error) {
		if len(s.records) >= s.maxRecords {
			return false, ErrCapacity
		}
		hash := tokenHash(token)
		if _, collision := s.records[hash]; collision {
			return false, errors.New("random session token collision")
		}
		s.records[hash] = &Session{Tenant: tenant, ExpiresAt: s.now().UTC().Add(ttl)}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *FileStore) Get(token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(false); err != nil {
		return nil, err
	}
	record, ok := s.records[tokenHash(token)]
	if !ok || !record.ExpiresAt.After(s.now()) {
		return nil, ErrNotFound
	}
	return copySession(record), nil
}

func (s *FileStore) Touch(token string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("ttl must be positive")
	}
	return s.mutate(func() (bool, error) {
		record, ok := s.records[tokenHash(token)]
		now := s.now().UTC()
		if !ok || !record.ExpiresAt.After(now) {
			return false, ErrNotFound
		}
		// Throttle persistent renewal to 1% of TTL, bounded by a minute/hour.
		// The browser is renewed on each response; server expiry is conservative.
		window := ttl / 100
		if window < time.Minute {
			window = time.Minute
		}
		if window > time.Hour {
			window = time.Hour
		}
		if record.ExpiresAt.Sub(now) > ttl-window {
			return false, nil
		}
		record.ExpiresAt = now.Add(ttl)
		return true, nil
	})
}

func (s *FileStore) Delete(token string) error {
	return s.mutate(func() (bool, error) {
		hash := tokenHash(token)
		if _, ok := s.records[hash]; !ok {
			return false, nil
		}
		delete(s.records, hash)
		return true, nil
	})
}

func (s *FileStore) DeleteTenant(tenant string) error {
	// A root lifecycle command must not create the gateway's first lock file.
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return s.mutate(func() (bool, error) {
		dirty := false
		for hash, record := range s.records {
			if record.Tenant == tenant {
				delete(s.records, hash)
				dirty = true
			}
		}
		return dirty, nil
	})
}

// CountTenant reports how many unexpired sessions one tenant still holds.
//
// Logout is the caller (M69): a tenant whose last session signed out has its dsh stopped, and
// one that still has a live session anywhere — another window, another browser — keeps it, so
// that signing out in one tab cannot kill a turn somebody is running in the other.
func (s *FileStore) CountTenant(tenant string) (int, error) {
	if tenant == "" {
		return 0, errors.New("tenant is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(false); err != nil {
		return 0, err
	}
	now := s.now()
	count := 0
	for _, record := range s.records {
		if record.Tenant == tenant && record.ExpiresAt.After(now) {
			count++
		}
	}
	return count, nil
}

func (s *FileStore) SetUpstream(token string, upstream *Upstream) error {
	_, err := s.setUpstream(token, nil, upstream)
	return err
}

// SetUpstreamIf prevents a delayed response from restoring an older worker
// cookie after another request has cleared/refreshed that cookie.
func (s *FileStore) SetUpstreamIf(token string, generation uint64, upstream *Upstream) (bool, error) {
	return s.setUpstream(token, &generation, upstream)
}

func (s *FileStore) setUpstream(token string, expected *uint64, upstream *Upstream) (changed bool, err error) {
	if upstream == nil || upstream.Name == "" || upstream.Value == "" || upstream.Authority == "" {
		return false, errors.New("complete upstream cookie is required")
	}
	err = s.mutate(func() (bool, error) {
		record, ok := s.records[tokenHash(token)]
		if !ok || !record.ExpiresAt.After(s.now()) {
			return false, ErrNotFound
		}
		if expected != nil && (record.Upstream == nil || record.Upstream.Generation != *expected) {
			return false, nil
		}
		next := *upstream
		if record.Upstream != nil && record.Upstream.Generation > record.UpstreamGeneration {
			record.UpstreamGeneration = record.Upstream.Generation
		}
		if record.UpstreamGeneration == ^uint64(0) {
			return false, errors.New("upstream generation exhausted")
		}
		record.UpstreamGeneration++
		next.Generation = record.UpstreamGeneration
		record.Upstream = &next
		changed = true
		return true, nil
	})
	return changed && err == nil, err
}

func (s *FileStore) ClearUpstream(token string) error {
	return s.mutate(func() (bool, error) {
		record, ok := s.records[tokenHash(token)]
		if !ok {
			return false, ErrNotFound
		}
		if record.Upstream == nil {
			return false, nil
		}
		record.Upstream = nil
		return true, nil
	})
}

func (s *FileStore) ClearUpstreamIf(token string, generation uint64) (cleared bool, err error) {
	err = s.mutate(func() (bool, error) {
		record, ok := s.records[tokenHash(token)]
		if !ok {
			return false, ErrNotFound
		}
		if record.Upstream == nil || record.Upstream.Generation != generation {
			return false, nil
		}
		record.Upstream = nil
		cleared = true
		return true, nil
	})
	return cleared && err == nil, err
}

func (s *FileStore) mutate(operation func() (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return securefile.WithLock(s.path+".lock", func() error {
		if err := s.reloadLocked(true); err != nil {
			return err
		}
		dirty, err := operation()
		if err != nil || !dirty {
			return err
		}
		if err := s.saveLocked(); err != nil {
			// Never serve an uncommitted in-memory mutation after a failed save.
			s.fileInfo = nil
			s.records = map[string]*Session{}
			return err
		}
		return nil
	})
}

func (s *FileStore) reloadLocked(force bool) error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.records = map[string]*Session{}
		s.fileInfo = nil
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&^0o600 != 0 || info.Size() > maxFileBytes {
		return errors.New("sessions file must be a mode-0600 regular file of at most 64 MiB")
	}
	if !force && s.fileInfo != nil && os.SameFile(info, s.fileInfo) && info.ModTime().Equal(s.fileInfo.ModTime()) && info.Size() == s.fileInfo.Size() {
		return nil
	}
	data, err := securefile.ReadLimitedRegular(s.path, maxFileBytes)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var state diskState
	if err := dec.Decode(&state); err != nil {
		return fmt.Errorf("decode sessions: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("sessions file has trailing JSON")
		}
		return err
	}
	if state.Version != diskVersion {
		return fmt.Errorf("unsupported sessions version %d", state.Version)
	}
	next := map[string]*Session{}
	now := s.now()
	for hash, record := range state.Sessions {
		decodedHash, hashErr := hex.DecodeString(hash)
		if hashErr != nil || len(decodedHash) != sha256.Size || record == nil || record.Tenant == "" {
			return errors.New("invalid session record")
		}
		if record.ExpiresAt.After(now) {
			copy := copySession(record)
			if copy.Upstream != nil && copy.Upstream.Generation > copy.UpstreamGeneration {
				copy.UpstreamGeneration = copy.Upstream.Generation
			}
			next[hash] = copy
		}
	}
	s.records = next
	s.fileInfo = info
	return nil
}

func (s *FileStore) saveLocked() error {
	data, err := json.MarshalIndent(diskState{Version: diskVersion, Sessions: s.records}, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxFileBytes {
		return errors.New("sessions file exceeds 64 MiB")
	}
	if err := securefile.WriteAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	s.fileInfo, err = os.Lstat(s.path)
	return err
}

func copySession(in *Session) *Session {
	if in == nil {
		return nil
	}
	out := *in
	if in.Upstream != nil {
		upstream := *in.Upstream
		out.Upstream = &upstream
	}
	return &out
}
