// Package browsermount connects authenticated browser reverse requests to FUSE.
package browsermount

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

const maxBody = 2 << 20
const lease = 60 * time.Second
const browserFSName = "dshgw-browser-workspace"

// Mounted.Unmount must release the host mount and wait for its FUSE server.
// Callers must first release ALL worker namespace references. We deliberately do
// not wrap Unmount in disposable timeout goroutines: those leak stuck servers.
type Mounted interface{ Unmount() error }
type MountFunc func(string, fs.Backend) (Mounted, error)
type Service struct {
	mu     sync.Mutex
	shares map[string]*share
	mount  MountFunc
	// restart must synchronously destroy the previous namespace before returning
	// success; it must not call DropTenant. MountsFor is safe inside this callback.
	restart    func(context.Context, registry.Tenant) error
	stopWorker func(context.Context, registry.Tenant) error
	recordDir  string
	registry   *registry.Registry
	tombstones map[string]tombstone
	lock       *os.File
	stopping   bool
}
type tombstone struct {
	owner, tenant string
	until         time.Time
}
type pendingCall struct {
	reply chan fs.Response
	ctx   context.Context
}
type share struct {
	mu                       sync.Mutex
	lifecycle                sync.Mutex
	id, token, owner         string
	tenant                   registry.Tenant
	path                     string
	writable                 bool
	seen                     time.Time
	mounted                  Mounted
	active, restartAttempted bool
	closed                   bool
	// published means a worker may have consumed this mount via MountsFor, even
	// without an explicit activation. detached is set only after proven teardown.
	published, detached, cleaned bool
	queue                        chan fs.Request
	pending                      map[string]pendingCall
	done                         chan struct{}
}

func New(restart func(context.Context, registry.Tenant) error) *Service {
	return NewWithMount(restart, func(path string, b fs.Backend) (Mounted, error) { return fs.MountFS(path, b) })
}
func NewWithMount(restart func(context.Context, registry.Tenant) error, mount MountFunc) *Service {
	return &Service{shares: make(map[string]*share), tombstones: make(map[string]tombstone), mount: mount, restart: restart}
}
func NewWithState(restart func(context.Context, registry.Tenant) error, mount MountFunc, stateDir string) *Service {
	if mount == nil {
		mount = func(path string, b fs.Backend) (Mounted, error) { return fs.MountFS(path, b) }
	}
	s := NewWithMount(restart, mount)
	s.recordDir = filepath.Join(stateDir, "browser-mounts")
	return s
}
func (s *Service) SetRegistry(r *registry.Registry) { s.registry = r }

// SetStopWorker configures a raw synchronous namespace stop before DropTenant
// cleanup. Configure before serving. It must NOT call DropTenant/StopWorker;
// WorkerRunner.Stop is suitable, Manager.StopWorker is not (it reenters).
func (s *Service) SetStopWorker(stop func(context.Context, registry.Tenant) error) {
	s.stopWorker = stop
}
func randomID() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Call never retries a mutation: a timeout does not prove it was not applied.
func (s *share) Call(ctx context.Context, req fs.Request) (fs.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if !s.writable && req.Op != "stat" && req.Op != "list" && req.Op != "read" && req.Op != "flush" {
		return fs.Response{OK: false, Error: &fs.RemoteError{Code: "EROFS", Message: "read-only browser directory"}}, nil
	}
	id, err := randomID()
	if err != nil {
		return fs.Response{}, err
	}
	req.ID = id
	ch := make(chan fs.Response, 1)
	s.mu.Lock()
	if s.closed || time.Since(s.seen) > lease {
		s.mu.Unlock()
		return fs.Response{}, errors.New("browser disconnected")
	}
	if len(s.pending) >= 64 {
		s.mu.Unlock()
		return fs.Response{}, errors.New("browser request queue full")
	}
	s.pending[id] = pendingCall{reply: ch, ctx: ctx}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	select {
	case s.queue <- req:
	case <-ctx.Done():
		return fs.Response{}, ctx.Err()
	case <-s.done:
		return fs.Response{}, errors.New("browser disconnected")
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return fs.Response{}, ctx.Err()
	case <-s.done:
		return fs.Response{}, errors.New("browser disconnected")
	}
}

// prepare creates only new directories under a trusted registry workspace.
func prepare(workspace, id string) (string, error) {
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || workspace == "/" {
		return "", errors.New("invalid workspace")
	}
	for p := workspace; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink or non-directory workspace")
		}
		if p == "/" {
			break
		}
	}
	parent := filepath.Join(workspace, "browser")
	if err := os.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("browser mount container must be private directory")
	}
	path := filepath.Join(parent, id)
	if err := os.Mkdir(path, 0700); err != nil {
		return "", err
	}
	return path, nil
}
func (s *Service) open(t registry.Tenant, owner, name string, writable bool) (*share, error) {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "\x00\n\r") {
		return nil, errors.New("invalid display name")
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	token, err := randomID()
	if err != nil {
		return nil, err
	}
	sh := &share{id: id, token: token, owner: owner, tenant: t, writable: writable, seen: time.Now(), queue: make(chan fs.Request, 64), pending: make(map[string]pendingCall), done: make(chan struct{})}
	sh.lifecycle.Lock()
	defer sh.lifecycle.Unlock()
	s.mu.Lock()
	count := 0
	for _, old := range s.shares {
		if old.tenant.Name == t.Name {
			count++
		}
	}
	if s.stopping || count >= 4 || len(s.shares) >= 128 {
		s.mu.Unlock()
		return nil, errors.New("mount limit reached or service stopping")
	}
	s.shares[token] = sh
	s.mu.Unlock()
	path, err := prepare(t.Workspace, id)
	sh.mu.Lock()
	sh.path = path
	sh.mu.Unlock()
	if err == nil {
		err = s.writeRecord(mountRecord{ID: id, Tenant: t.Name, Workspace: t.Workspace, Path: path, State: "preparing"})
	}
	if err == nil {
		sh.mu.Lock()
		closed := sh.closed
		sh.mu.Unlock()
		if closed {
			err = errors.New("browser disconnected")
		} else {
			var mounted Mounted
			mounted, err = s.mount(path, sh)
			sh.mu.Lock()
			sh.mounted = mounted
			sh.mu.Unlock()
			if err == nil {
				err = s.writeRecord(mountRecord{ID: id, Tenant: t.Name, Workspace: t.Workspace, Path: path, State: "ready"})
			}
		}
	}
	sh.mu.Lock()
	if err == nil && sh.closed {
		err = errors.New("browser disconnected")
	}
	if err == nil {
		sh.published = true
	}
	sh.mu.Unlock()
	if err != nil {
		sh.disconnect()
		// Never published; keep failed rollback in shares and on disk for retry.
		return nil, errors.Join(err, s.cleanupLocked(sh))
	}
	return sh, nil
}
func (s *Service) get(token, tenant, owner string) (*share, error) {
	s.mu.Lock()
	sh := s.shares[token]
	s.mu.Unlock()
	if sh == nil || sh.tenant.Name != tenant || sh.owner != owner {
		return nil, errors.New("unknown directory capability")
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.closed || time.Since(sh.seen) > lease {
		return nil, errors.New("directory disconnected")
	}
	sh.seen = time.Now()
	return sh, nil
}
func (s *Service) MountsFor(tenant string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for _, sh := range s.shares {
		sh.mu.Lock()
		if sh.tenant.Name == tenant && sh.published && sh.path != "" && sh.mounted != nil && !sh.closed {
			paths = append(paths, sh.path)
		}
		sh.mu.Unlock()
	}
	return paths
}
func (sh *share) disconnect() {
	sh.mu.Lock()
	if !sh.closed {
		sh.closed = true
		close(sh.done)
	}
	sh.mu.Unlock()
}

type payload struct {
	Token    string      `json:"token"`
	Name     string      `json:"name"`
	Writable bool        `json:"writable"`
	ID       string      `json:"id"`
	Result   fs.Response `json:"result"`
}

func (s *Service) ServeTenant(w http.ResponseWriter, r *http.Request, t registry.Tenant, owner string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var p payload
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&p)
	if err == nil {
		var tail any
		if dec.Decode(&tail) != io.EOF {
			err = errors.New("expected one JSON body")
		}
	}
	var value any
	if err == nil {
		value, err = s.dispatch(r.Context(), strings.TrimPrefix(r.URL.Path, "/browser-workspace/"), t, owner, p)
	}
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": "browser/failed", "message": err.Error()}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "value": value})
}
func (s *Service) dispatch(ctx context.Context, op string, t registry.Tenant, owner string, p payload) (any, error) {
	if op == "hello" {
		return map[string]any{"version": 1, "maxBytes": 1 << 20}, nil
	}
	if op == "open" {
		sh, err := s.open(t, owner, p.Name, p.Writable)
		if err != nil {
			return nil, err
		}
		sh.mu.Lock()
		path := sh.path
		sh.mu.Unlock()
		return map[string]any{"token": sh.token, "id": sh.id, "mountpoint": path}, nil
	}
	var sh *share
	var err error
	if op == "close" {
		s.mu.Lock()
		candidate := s.shares[p.Token]
		s.mu.Unlock()
		if candidate != nil && candidate.tenant.Name == t.Name && candidate.owner == owner {
			sh = candidate
		} else {
			sh, err = s.get(p.Token, t.Name, owner)
		}
	} else {
		sh, err = s.get(p.Token, t.Name, owner)
	}
	if err != nil || sh == nil {
		if op == "close" {
			s.mu.Lock()
			ts, ok := s.tombstones[p.Token]
			if ok && time.Now().After(ts.until) {
				delete(s.tombstones, p.Token)
				ok = false
			}
			s.mu.Unlock()
			if ok && ts.tenant == t.Name && ts.owner == owner {
				return map[string]bool{"closed": true}, nil
			}
		}
		if err == nil {
			err = errors.New("unknown directory capability")
		}
		return nil, err
	}
	switch op {
	case "heartbeat":
		return map[string]bool{"alive": true}, nil
	case "poll":
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case req := <-sh.queue:
				sh.mu.Lock()
				pending, exists := sh.pending[req.ID]
				closed := sh.closed
				sh.mu.Unlock()
				if closed {
					return nil, errors.New("disconnected")
				}
				if !exists || pending.ctx.Err() != nil {
					continue
				}
				return map[string]any{"requests": []fs.Request{req}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-sh.done:
				return nil, errors.New("disconnected")
			case <-timer.C:
				return map[string]any{"requests": []fs.Request{}}, nil
			}
		}
	case "respond":
		sh.mu.Lock()
		pending, exists := sh.pending[p.ID]
		if exists {
			delete(sh.pending, p.ID)
		}
		sh.mu.Unlock()
		if !exists || pending.ctx.Err() != nil {
			return map[string]bool{"accepted": false}, nil
		}
		p.Result.ID = p.ID
		pending.reply <- p.Result
		return map[string]bool{"accepted": true}, nil
	case "activate":
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		go func() {
			select {
			case <-sh.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		sh.lifecycle.Lock()
		defer sh.lifecycle.Unlock()
		sh.mu.Lock()
		closed, active := sh.closed, sh.active
		sh.mu.Unlock()
		if closed {
			return nil, errors.New("disconnected")
		}
		if !active {
			check, err := sh.Call(ctx, fs.Request{Op: "stat", Path: ""})
			if err != nil {
				return nil, err
			}
			if !check.OK || check.Value.Kind != "directory" {
				return nil, errors.New("browser root unavailable")
			}
			if s.restart == nil {
				return nil, errors.New("worker restart unavailable")
			}
			sh.mu.Lock()
			sh.restartAttempted = true
			sh.mu.Unlock()
			if err := s.restart(ctx, t); err != nil {
				return nil, fmt.Errorf("worker restart: %w", err)
			}
			sh.mu.Lock()
			sh.active = true
			sh.mu.Unlock()
		}
		sh.mu.Lock()
		path := sh.path
		sh.mu.Unlock()
		return map[string]string{"id": sh.id, "mountpoint": path}, nil
	case "close":
		if err := s.closeContext(ctx, sh, false); err != nil {
			return nil, err
		}
		return map[string]bool{"closed": true}, nil
	default:
		return nil, errors.New("unknown endpoint")
	}
}
