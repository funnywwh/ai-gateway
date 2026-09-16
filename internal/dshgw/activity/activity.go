// Package activity stores mutable login metadata separately from the root-owned
// provisioning registry.
package activity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

type Recorder interface{ MarkLogin(string, time.Time) error }
type disk struct {
	Version   int                  `json:"version"`
	LastLogin map[string]time.Time `json:"last_login"`
}
type Store struct {
	Path string
	mu   sync.Mutex
}

func (s *Store) MarkLogin(tenant string, at time.Time) error {
	if !config.ValidTenantName(tenant) {
		return errors.New("invalid tenant name")
	}
	return s.update(func(state *disk) { state.LastLogin[tenant] = at.UTC() })
}
func (s *Store) Delete(tenant string) error {
	if !config.ValidTenantName(tenant) {
		return errors.New("invalid tenant name")
	}
	if _, err := os.Stat(s.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return securefile.WithLock(s.Path+".lock", func() error {
		if _, err := os.Stat(s.Path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		state, err := s.load()
		if err != nil {
			return err
		}
		delete(state.LastLogin, tenant)
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		return securefile.WriteAtomic(s.Path, append(data, '\n'), 0o600)
	})
}
func (s *Store) LastLogin(tenant string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return time.Time{}, err
	}
	return state.LastLogin[tenant], nil
}
func (s *Store) update(change func(*disk)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return securefile.WithLock(s.Path+".lock", func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		change(&state)
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		return securefile.WriteAtomic(s.Path, append(data, '\n'), 0o600)
	})
}
func (s *Store) load() (disk, error) {
	data, err := securefile.ReadLimitedRegular(s.Path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return disk{Version: 1, LastLogin: map[string]time.Time{}}, nil
	}
	if err != nil {
		return disk{}, err
	}
	if err := securefile.CheckPermissions(s.Path, 0o600); err != nil {
		return disk{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var state disk
	if err := dec.Decode(&state); err != nil {
		return disk{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return disk{}, errors.New("activity file has trailing JSON")
		}
		return disk{}, err
	}
	if state.Version != 1 {
		return disk{}, errors.New("unsupported activity version")
	}
	if state.LastLogin == nil {
		state.LastLogin = map[string]time.Time{}
	}
	return state, nil
}
