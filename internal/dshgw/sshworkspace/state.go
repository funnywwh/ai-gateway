package sshworkspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// Mount is one recorded sshfs mount: what the gateway made, where it lands, and which
// account owns it.
type Mount struct {
	Tenant string `json:"tenant"`
	// Host is the ssh alias or user@host the mount was made through.
	Host string `json:"host"`
	// Remote is the path as the user asked for it, CanonicalRemote as the remote host
	// reports it (pwd -P). The canonical form is the de-duplication key.
	Remote          string    `json:"remote"`
	CanonicalRemote string    `json:"canonical_remote"`
	Mountpoint      string    `json:"mountpoint"`
	CreatedAt       time.Time `json:"created_at"`
}

// stateVersion is bumped when the document's shape changes; a document from another version
// is refused instead of being half-read (same rule as the workspace domain).
const stateVersion = 1

type stateDocument struct {
	Version int     `json:"version"`
	Mounts  []Mount `json:"mounts"`
}

// Store is the durable mount record at <state_dir>/ssh-mounts.json.
//
// It is the gateway's memory of what it mounted: the worker profile binds these mount points
// at every start, and a restart of dshgw re-mounts what the file lists. A corrupted file is
// reported, never guessed at — mounting from a half-read record could expose one tenant's
// remote directory to another.
type Store struct {
	path string
}

// NewStore opens (not yet reads) the record file.
func NewStore(path string) *Store { return &Store{path: path} }

// Path is the record file this store owns.
func (s *Store) Path() string { return s.path }

// Load returns every recorded mount. A missing file is an empty record, not an error: a
// fresh deployment has mounted nothing yet.
func (s *Store) Load() ([]Mount, error) {
	data, err := securefile.ReadLimitedRegular(s.path, 1<<20)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Mount{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	var doc stateDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	if doc.Version != stateVersion {
		return nil, fmt.Errorf("%s has version %d, want %d", s.path, doc.Version, stateVersion)
	}
	if doc.Mounts == nil {
		doc.Mounts = []Mount{}
	}
	return doc.Mounts, nil
}

// Save writes the whole record atomically at 0600.
func (s *Store) Save(mounts []Mount) error {
	if mounts == nil {
		mounts = []Mount{}
	}
	// The copy stays non-nil when the list is empty: appending onto a nil slice returns nil, and
	// a nil slice marshals as `null` — removing the last mount used to leave `"mounts": null` in
	// a file an operator reads during an incident, while the account's mirror wrote `[]`. The two
	// must not disagree about the same fact.
	sorted := append(make([]Mount, 0, len(mounts)), mounts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Tenant != sorted[j].Tenant {
			return sorted[i].Tenant < sorted[j].Tenant
		}
		return sorted[i].Mountpoint < sorted[j].Mountpoint
	})
	data, err := json.MarshalIndent(stateDocument{Version: stateVersion, Mounts: sorted}, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(s.path, append(data, '\n'), 0o600)
}

// Add records one mount, replacing any record for the same mount point. Replacing rather
// than appending is what makes a repeated open idempotent.
func (s *Store) Add(mount Mount) ([]Mount, error) {
	mounts, err := s.Load()
	if err != nil {
		return nil, err
	}
	next := make([]Mount, 0, len(mounts)+1)
	for _, existing := range mounts {
		if existing.Mountpoint != mount.Mountpoint {
			next = append(next, existing)
		}
	}
	next = append(next, mount)
	if err := s.Save(next); err != nil {
		return nil, err
	}
	return next, nil
}

// Remove forgets one mount point.
func (s *Store) Remove(mountpoint string) ([]Mount, error) {
	mounts, err := s.Load()
	if err != nil {
		return nil, err
	}
	next := make([]Mount, 0, len(mounts))
	for _, existing := range mounts {
		if existing.Mountpoint != mountpoint {
			next = append(next, existing)
		}
	}
	if len(next) == len(mounts) {
		return mounts, nil
	}
	if err := s.Save(next); err != nil {
		return nil, err
	}
	return next, nil
}

// ForTenant returns one account's mounts, in mount-point order.
func (s *Store) ForTenant(tenant string) ([]Mount, error) {
	mounts, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := make([]Mount, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Tenant == tenant {
			out = append(out, mount)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out, nil
}

// Find returns the record for one mount point.
func (s *Store) Find(mountpoint string) (Mount, bool, error) {
	mounts, err := s.Load()
	if err != nil {
		return Mount{}, false, err
	}
	for _, mount := range mounts {
		if mount.Mountpoint == mountpoint {
			return mount, true, nil
		}
	}
	return Mount{}, false, nil
}
