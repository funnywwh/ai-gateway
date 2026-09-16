// Package audit writes a secret-free append-only security event stream.
package audit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

type Event struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Tenant   string    `json:"tenant,omitempty"`
	RemoteIP string    `json:"remote_ip,omitempty"`
	Method   string    `json:"method,omitempty"`
	Path     string    `json:"path,omitempty"`
	Origin   []string  `json:"origin,omitempty"`
	Reason   string    `json:"reason"`
	Status   int       `json:"status"`
}
type Sink interface{ Write(Event) error }
type JSONL struct {
	Path string
	mu   sync.Mutex
	Now  func() time.Time
}

func (s *JSONL) Write(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Time.IsZero() {
		if s.Now != nil {
			event.Time = s.Now().UTC()
		} else {
			event.Time = time.Now().UTC()
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return securefile.WithLock(s.Path+".lock", func() error {
		if info, err := os.Stat(s.Path); err == nil {
			if info.Mode().Perm() != 0o600 {
				return errors.New("audit file permissions are broader than 0600")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		file, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err := file.Write(data); err != nil {
			return err
		}
		return file.Sync()
	})
}
