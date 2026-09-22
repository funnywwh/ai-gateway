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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

const maxBody = 2 << 20
const lease = 60 * time.Second

// reconnectGrace is how long a mount survives the loss of the page that owns it, so a
// reload (or a closed-and-reopened tab) can resume the SAME mount instead of tearing it
// down and rebuilding it. A page reload cancels the poll request, which is the mount's
// browser side — without a grace window that cancellation is final, and rebuilding costs a
// worker restart plus a new mount point that DSH's own workspace entry does not know about.
//
// The window is deliberately longer than a reload and shorter than an operator would notice
// as "leaked": a resumed mount is never advertised to a worker start while disconnected, so
// the window cannot block a worker the way an unanswerable mount would.
const reconnectGrace = 45 * time.Second

const browserFSName = "dshgw-browser-workspace"

// maxMountsPerTenant bounds how many browser directories one account may have mounted at
// once. Every mount is one long-poll connection held by the page, one FUSE server and one
// entry in the worker's bind list, so the bound is what keeps a page from starving its own
// origin: a browser opens at most six concurrent connections per origin over HTTP/1.1.
//
// The limit is deliberately separate from the global share cap: exceeding it is an ordinary
// user-facing condition ("close one of your directories first"), not a gateway failure, and
// the client turns the message below into that sentence.
const maxMountsPerTenant = 4

// errPerAccountMountLimit is returned by open; the client matches this text to explain the
// limit to the operator in their own words.
func errPerAccountMountLimit() error {
	return fmt.Errorf("per-account browser directory limit reached: %d", maxMountsPerTenant)
}

// validDirectoryKey is the one shape a client-supplied stable directory key may have. The key
// names the mount point, so it must be exactly ONE safe path segment: the LOCAL directory's
// own name ("aosp", "My Docs"), which is what makes the picker show a name a person
// recognises, or the 32/48 hex id a client that predates directory names still sends — so an
// already mapped local directory keeps its path, its workspace entry and its sessions.
//
// Anything that could traverse, hide, pad or collide with the container itself is refused
// here rather than sanitised silently.
func validDirectoryKey(key string) bool {
	if key == "" || len(key) > 255 || key == "." || key == ".." {
		return false
	}
	// A hidden or whitespace-padded name is refused rather than trimmed: the mount point must
	// be exactly what the client asked for and exactly what the operator reads in the picker.
	if strings.HasPrefix(key, ".") || strings.TrimSpace(key) != key {
		return false
	}
	if strings.ContainsAny(key, `/\`) {
		return false
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// directoryNameSuffixLimit is how many "-N" variants `allocate` tries before it falls back to
// an opaque tail. A local directory name is not unique inside one account ("Downloads" twice),
// and every candidate has to stay readable.
const directoryNameSuffixLimit = 9

// withSuffix appends a disambiguating suffix while keeping the result one segment of at most
// 255 bytes: a filesystem name limit, and the same bound validDirectoryKey enforces. The base
// is cut on a rune boundary so a multi-byte name cannot be split into invalid UTF-8.
func withSuffix(base, suffix string) string {
	cut := 255 - len(suffix)
	if cut < 0 {
		cut = 0
	}
	if cut > len(base) {
		cut = len(base)
	}
	// Only a CUT base can split a rune: when nothing has to be dropped, base[:cut] is the whole
	// name and base[cut] is out of range.
	for cut > 0 && cut < len(base) && !utf8.RuneStart(base[cut]) {
		cut--
	}
	return base[:cut] + suffix
}

// allocateName answers one `allocate` handshake: a mount directory name this account no
// longer holds, derived from the local directory's own name.
//
// The arbitration is deliberately advisory rather than a reservation. Identity here is
// client-owned — the key IS the virtual path of a local directory — so the gateway can only
// see what it can see: a name whose directory already exists under this account's container
// (a stable mount point outlives its mount) or that a live share is serving right now. A name
// is therefore only "taken" once some local directory has actually mounted with it.
//
// An unusable proposal still returns a usable key (the legacy random id): refusing here would
// leave the operator with a folder that can never be mounted because its local name is, for
// example, hidden or padded. The client reports that degradation itself.
func (s *Service) allocateName(t registry.Tenant, desired string) string {
	taken := make(map[string]bool)
	s.mu.Lock()
	for _, sh := range s.shares {
		if sh.tenant.Name == t.Name {
			taken[sh.id] = true
		}
	}
	s.mu.Unlock()
	if entries, err := os.ReadDir(filepath.Join(t.Workspace, "browser")); err == nil {
		for _, e := range entries {
			taken[e.Name()] = true
		}
	}
	if !validDirectoryKey(desired) {
		id, err := randomID()
		if err != nil {
			return ""
		}
		return id
	}
	candidates := []string{desired}
	for n := 2; n <= directoryNameSuffixLimit; n++ {
		candidates = append(candidates, withSuffix(desired, fmt.Sprintf("-%d", n)))
	}
	for _, candidate := range candidates {
		if !taken[candidate] {
			return candidate
		}
	}
	var tail [4]byte
	if _, err := rand.Read(tail[:]); err != nil {
		return withSuffix(desired, "-"+strings.Repeat("0", 8))
	}
	return withSuffix(desired, "-"+hex.EncodeToString(tail[:]))
}

// errDuplicateDirectoryKey is what open answers when a share still holds that key — live, or
// waiting out its reconnect grace. It is refused instead of mounted twice: two mounts at one
// path would let two browsers write the same local directory through the kernel at once, and
// the second FUSE mount would land on a directory that is already a mount point.
//
// A stable key is what makes the virtual path a mapping of the LOCAL directory rather than of
// one mount: the same saved folder always mounts at <workspace>/browser/<key>, so DSH's own
// workspace entry (which is reused by canonical path) keeps its id, its title and its session
// membership across disconnect/reconnect, page reloads and gateway restarts. The key is the
// local directory's own name, so that path also reads as the directory a person picked.
func errDuplicateDirectoryKey() error {
	return errors.New("directory key already mounted for this account")
}

// Mounted.Unmount must release the host mount and wait for its FUSE server.
// Callers must first release ALL worker namespace references. We deliberately do
// not wrap Unmount in disposable timeout goroutines: those leak stuck servers.
type Mounted interface{ Unmount() error }
type MountFunc func(string, fs.Backend) (Mounted, error)
type Service struct {
	mu     sync.Mutex
	shares map[string]*share
	mount  MountFunc
	// detach is the forced detach used when the graceful unmount cannot remove the mount from
	// the table (M76). It is a field so tests drive the escalation without a real mount;
	// production uses browserworkspace's ForceUnmount ladder.
	detach func(path string) error
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
	// path is the mount point this capability released. A stable mount point outlives its
	// mount, so the operator deleting a folder AFTER disconnecting it has no live share left
	// to purge through: the tombstone is what still knows which directory that key owned.
	path       string
	persistent bool
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
	// persistent marks a share mounted with a client-supplied stable directory key: its
	// mount point is the virtual path of that local directory and outlives the mount, so a
	// disconnect only unmounts and never removes the path DSH's workspace entry points at.
	// Only an explicit purge (the operator removing the folder) releases it.
	persistent bool
	// final marks a share whose teardown is owned by a caller that has already excluded it for
	// good (logout, tenant drop, shutdown): a reaper pass may retry the detach, but nothing may
	// ever restart a worker to release a namespace for it (M76). Without it, a logout whose
	// detach failed would have its dsh started again by the next expiry tick, which is what the
	// deployment host showed on 2026-09-22.
	final bool
	// disconnectedAt is when the browser side went away while a reload could still bring it
	// back. It is what turns a canceled poll from "this mount is finished" into "this mount
	// is waiting", and it bounds that waiting: past reconnectGrace the mount is closed for
	// real. Zero means the mount was never disconnected, or its grace has run out.
	disconnectedAt time.Time
	// resumed counts the browser sides that took this mount back after a disconnect. It is
	// evidence for tests and operators: a mount that keeps being resumed is a flapping page.
	resumed int
	// revoked marks a share whose browser side was replaced by a resume that took over. It
	// lasts only for the life of that share: every stale poll/respond from the replaced page
	// is answered with "revoked" so it stops, and the flag is cleared when the replacing page
	// starts its own poll loop.
	revoked bool
	// polling marks the browser side that is holding this mount's poll connection right now.
	// It is what a resume must not steal: a page that is still answering owns its mount,
	// whereas a page whose request the browser never even sent (a dropped connection, a
	// blocked request, a navigation) has no claim on it — even though the gateway has not
	// seen that request die yet.
	polling bool
	// published means a worker may have consumed this mount via MountsFor, even
	// without an explicit activation. detached is set only after proven teardown.
	published, detached, cleaned bool
	queue                        chan fs.Request
	pending                      map[string]pendingCall
	// done is closed exactly once, when the share is torn down for good (cleanupLocked):
	// it is the "this capability is gone" signal the poll loop waits on.
	done chan struct{}
	// abort is replaced by a fresh channel every time the browser side goes away, so
	// operations already queued for the departed page fail at once instead of hanging until
	// their own deadline — while operations queued AFTER a resume are unaffected, because
	// they captured the new channel. Keeping this separate from done is what lets a mount
	// outlive its page without lying about either fact.
	abort chan struct{}
}

// serving is the one liveness rule of the service: the browser behind this share is here
// now and within its lease, so an operation may be queued for it. Requires sh.mu.
//
// A mount waiting to be resumed is NOT serving: it holds its kernel mount and its mount
// point so the returning page can keep using the same path, but nothing may be queued for
// it. Binding one into a worker start is the failure this rule exists to prevent (see
// MountsFor): the profile resolves the path and bubblewrap stats it, so an unanswerable
// mount blocks the whole worker start for the FUSE timeout.
func (s *share) serving() bool {
	return !s.closed && s.disconnectedAt.IsZero() && time.Since(s.seen) <= lease
}

// awaitingResume reports whether this share is waiting for its browser to come back.
// Requires sh.mu.
func (s *share) awaitingResume() bool {
	return !s.closed && !s.disconnectedAt.IsZero() && time.Since(s.disconnectedAt) <= reconnectGrace
}

func New(restart func(context.Context, registry.Tenant) error) *Service {
	return NewWithMount(restart, func(path string, b fs.Backend) (Mounted, error) { return fs.MountFS(path, b) })
}
func NewWithMount(restart func(context.Context, registry.Tenant) error, mount MountFunc) *Service {
	return &Service{shares: make(map[string]*share), tombstones: make(map[string]tombstone), mount: mount, restart: restart, detach: fs.ForceUnmount}
}
func NewWithState(restart func(context.Context, registry.Tenant) error, mount MountFunc, stateDir string) *Service {
	if mount == nil {
		mount = func(path string, b fs.Backend) (Mounted, error) { return fs.MountFS(path, b) }
	}
	s := NewWithMount(restart, mount)
	s.recordDir = filepath.Join(stateDir, "browser-mounts")
	return s
}

// SetForceDetach overrides the forced detach (M76). Configure before serving; a test uses it to
// state what a mount the graceful path cannot take answers to.
func (s *Service) SetForceDetach(detach func(path string) error) { s.detach = detach }

// forceDetach runs the forced detach, or reports that the deployment has none (a Service built
// by hand in a test rather than through a constructor).
func (s *Service) forceDetach(path string) error {
	if s.detach == nil {
		return errors.New("forced detach unavailable")
	}
	return s.detach(path)
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

// serveable reports whether the browser behind this share can still answer an operation.
// Requires sh.mu.
//
// This is the one liveness rule of the service: Call refuses to queue a request as soon as
// it is false, so a share that is not serveable must not be bound into a worker either.
// Binding one is not harmless — the sandbox profile resolves every advertised mount path
// and bubblewrap stats every bind source, so a mount with no browser behind it blocks for
// the whole FUSE timeout and then fails the worker start, which surfaces as "worker restart
// before close" and an unconfirmed cleanup on an unrelated mount (M65 real-host report).
// A mount waiting to be resumed is exactly such a mount, so it is excluded from the first
// moment the page goes away (see serving).
func (s *share) serveable() bool {
	return s.serving()
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
	if !s.serveable() {
		s.mu.Unlock()
		return fs.Response{}, errors.New("browser disconnected")
	}
	if len(s.pending) >= 64 {
		s.mu.Unlock()
		return fs.Response{}, errors.New("browser request queue full")
	}
	// The abort channel is captured here, not read later: an operation belongs to the
	// browser side that was here when it was queued.
	abort := s.abort
	s.pending[id] = pendingCall{reply: ch, ctx: ctx}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	select {
	case s.queue <- req:
	case <-ctx.Done():
		return fs.Response{}, ctx.Err()
	case <-abort:
		return fs.Response{}, errors.New("browser disconnected")
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return fs.Response{}, ctx.Err()
	case <-abort:
		return fs.Response{}, errors.New("browser disconnected")
	}
}

// prepare creates only new directories under a trusted registry workspace.
//
// A stable directory key means the mount point outlives the mount that used it: after a
// disconnect the same path is mounted again, which is what keeps DSH's workspace entry (and
// the sessions grouped under it) valid. So an existing path is reused rather than rejected,
// but only when it is exactly what this function would have created: a private, symlink-free
// directory inside the private container. A leftover it would have to mount over — a foreign
// symlink, a wider mode, or files it did not put there — is refused loudly.
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
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := reusableMountPoint(path); err != nil {
			return "", err
		}
	}
	return path, nil
}

// reusableMountPoint accepts the directory a previous mount of the same key left behind.
func reusableMountPoint(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("browser mount point %s is not a private directory", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	// Unmounting a FUSE mount leaves the mount point empty, so anything here was written by
	// something other than this service — mounting over it would hide it silently.
	if len(entries) != 0 {
		return fmt.Errorf("browser mount point %s is not empty", path)
	}
	return nil
}

// openRequest is one `open` handshake. Key is empty only for callers that do not need a
// stable virtual path (tests, and any older client): then a fresh random id is generated and
// the mount point is removed again when the mount ends, exactly as before stable keys.
//
// A non-empty Key is the mount directory name the client chose for one local directory —
// normally that directory's own name, arbitrated once through `allocate` — or the legacy
// random id an already saved folder still carries.
type openRequest struct {
	Name     string
	Writable bool
	Key      string
}

func (s *Service) open(t registry.Tenant, owner, name string, writable bool) (*share, error) {
	return s.openWith(t, owner, openRequest{Name: name, Writable: writable})
}

func (s *Service) openWith(t registry.Tenant, owner string, req openRequest) (*share, error) {
	name, writable := req.Name, req.Writable
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "\x00\n\r") {
		return nil, errors.New("invalid display name")
	}
	persistent := req.Key != ""
	var id string
	if persistent {
		if !validDirectoryKey(req.Key) {
			return nil, errors.New("invalid directory key")
		}
		id = req.Key
	} else {
		var err error
		if id, err = randomID(); err != nil {
			return nil, err
		}
	}
	token, err := randomID()
	if err != nil {
		return nil, err
	}
	sh := &share{id: id, token: token, owner: owner, tenant: t, writable: writable, persistent: persistent, seen: time.Now(), queue: make(chan fs.Request, 64), pending: make(map[string]pendingCall), done: make(chan struct{}), abort: make(chan struct{})}
	sh.lifecycle.Lock()
	defer sh.lifecycle.Unlock()
	s.mu.Lock()
	count := 0
	for _, old := range s.shares {
		if old.tenant.Name != t.Name {
			continue
		}
		// The key is the identity of the LOCAL directory, so a share that still holds it —
		// serving, or waiting out its grace window — owns that path. Mounting the key again
		// would stack a second FUSE mount on the same directory.
		if old.id == id {
			s.mu.Unlock()
			return nil, errDuplicateDirectoryKey()
		}
		count++
	}
	if s.stopping || len(s.shares) >= 128 {
		s.mu.Unlock()
		return nil, errors.New("global mount limit reached or service stopping")
	}
	// The per-account limit is checked separately because it is a normal, explainable
	// condition for the person who already has this many directories open, not a gateway
	// failure. A mount waiting out its reconnect grace still counts: it holds a kernel mount
	// and a mount point until it is resumed or reaped.
	if count >= maxMountsPerTenant {
		s.mu.Unlock()
		return nil, errPerAccountMountLimit()
	}
	s.shares[token] = sh
	s.mu.Unlock()
	path, err := prepare(t.Workspace, id)
	sh.mu.Lock()
	sh.path = path
	sh.mu.Unlock()
	if err == nil {
		err = s.writeRecord(mountRecord{ID: id, Tenant: t.Name, Workspace: t.Workspace, Path: path, State: "preparing", Persistent: persistent})
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
				err = s.writeRecord(mountRecord{ID: id, Tenant: t.Name, Workspace: t.Workspace, Path: path, State: "ready", Persistent: persistent})
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
		return nil, errors.Join(err, s.cleanupLocked(sh, false))
	}
	return sh, nil
}
func (s *Service) get(token, tenant, owner string) (*share, error) {
	sh, err := s.capability(token, tenant, owner)
	if err != nil {
		return nil, err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if !sh.serving() {
		return nil, errors.New("directory disconnected")
	}
	sh.seen = time.Now()
	return sh, nil
}

// capability proves a token names this tenant's and this browser session's share, whatever
// state that share is in. It is the authorization half of get, split out because resume
// must accept a share that is not serving yet (that is the whole point of resuming) while
// staying exactly as strict about who is asking.
func (s *Service) capability(token, tenant, owner string) (*share, error) {
	s.mu.Lock()
	sh := s.shares[token]
	s.mu.Unlock()
	if sh == nil || sh.tenant.Name != tenant || sh.owner != owner {
		return nil, errors.New("unknown directory capability")
	}
	return sh, nil
}

// claimPoll takes the mount's single poll connection for this browser side. Requires sh.mu.
func (sh *share) claimPoll() bool {
	if sh.closed || sh.polling {
		return false
	}
	sh.polling = true
	sh.revoked = false
	return true
}

// releasePoll gives the connection back. Requires sh.mu.
func (sh *share) releasePoll() { sh.polling = false }

// resume hands a waiting mount back to the browser side that owns it. Only a page that
// already held this capability can call it: token, tenant and owner must all match, and the
// share must still be inside its grace window.
//
// A page that is still holding the poll connection is not asked to give the mount up. A page
// that is not holding one has no claim, however recently it was seen: its request may never
// have reached the gateway at all (a dropped connection, a blocked request, a navigation),
// and "the gateway has not noticed the death yet" must not make its own reconnection fail —
// which is exactly what happened when a page's transport broke and its own resume was
// refused as if another page owned the mount.
func (s *Service) resume(capabilityToken, tenant, owner string) (map[string]any, error) {
	sh, err := s.capability(capabilityToken, tenant, owner)
	if err != nil {
		return nil, err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.closed {
		return nil, errors.New("directory disconnected")
	}
	if sh.polling {
		return nil, errors.New("directory already served by another page")
	}
	if !sh.serving() && !sh.awaitingResume() {
		// Neither serving nor waiting for a page: the mount is on its way out and must not be
		// revived halfway through teardown.
		return nil, errors.New("directory disconnected")
	}
	// A poll connection that was still claimed belongs to a browser side that lost its
	// transport in a way the gateway has not seen die; it is about to be told so. On the
	// ordinary reload path there is no such connection (the poll died, which is what set the
	// grace window), and nothing is revoked.
	if sh.serving() {
		sh.disconnectedAt = time.Now()
		sh.revoked = true
	}
	sh.resumeLocked()
	return map[string]any{"id": sh.id, "mountpoint": sh.path, "resumed": sh.resumed}, nil
}

// resumeLocked takes a mount back for a browser side that has returned — the reloaded page,
// or a tab that replaced the one that had it. The caller has already proved the capability
// (token, tenant, owner), so this only restores the one field a disconnect cleared.
//
// The mount itself is untouched on purpose: the kernel mount, the mount point and the
// worker's binding are all still there, which is the whole point of the grace window. After
// a reload the page keeps the same path, so DSH's own workspace entry (persisted in
// storages/workspace.json) still points at something real.
func (sh *share) resumeLocked() {
	sh.disconnectedAt = time.Time{}
	sh.seen = time.Now()
	// A new browser side gets a new abort channel: anything still waiting on the old one
	// belonged to the page that left and must not be revived by this resume.
	sh.abort = make(chan struct{})
	sh.resumed++
}

// disconnect marks the browser side gone. It is called when the poll connection dies, which
// is the only honest signal that a page reloaded, closed, crashed or lost its network.
func (sh *share) disconnect() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.closed {
		return
	}
	// The first disconnect starts the grace window; a later one does not extend it, so a
	// page that flaps cannot keep a mount alive indefinitely.
	if sh.disconnectedAt.IsZero() {
		sh.disconnectedAt = time.Now()
	}
	select {
	case <-sh.abort:
		// Already aborted: this is a second report of the same departure.
	default:
		close(sh.abort)
	}
}

// MountsFor lists the mount points a worker profile must bind for one tenant.
// Only serveable mounts are advertised: see serveable for why binding a mount
// whose browser stopped polling fails the whole worker start.
// AttachedMounts lists the mount points this account currently has ATTACHED, whatever state
// their share is in — including one whose teardown failed. It is what tells an operator (and the
// logout audit) how many mounts a teardown really took, which MountsFor cannot: MountsFor answers
// "what may a starting worker bind", and a share mid-teardown is deliberately not in it.
func (s *Service) AttachedMounts(tenant string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for _, sh := range s.shares {
		sh.mu.Lock()
		if sh.tenant.Name == tenant && sh.mounted != nil && sh.path != "" {
			paths = append(paths, sh.path)
		}
		sh.mu.Unlock()
	}
	sort.Strings(paths)
	return paths
}

func (s *Service) MountsFor(tenant string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for _, sh := range s.shares {
		sh.mu.Lock()
		if sh.tenant.Name == tenant && sh.published && sh.path != "" && sh.mounted != nil && sh.serveable() {
			paths = append(paths, sh.path)
		}
		sh.mu.Unlock()
	}
	return paths
}

// purgeTombstoned releases the mount point a finished capability left behind. It only ever
// removes the private directory of a stable key inside this tenant's own browser container,
// and only while no live share owns it: reusing a key is normal (the same local directory
// mounted again), and a stale capability must never delete the directory a live mount serves.
func (s *Service) purgeTombstoned(t registry.Tenant, ts tombstone) error {
	if !ts.persistent || ts.path == "" {
		return nil
	}
	root := filepath.Join(t.Workspace, "browser")
	// The path must be a direct child of this tenant's own mount container: a tombstone is
	// internal state, but it is still not authority to remove an arbitrary directory.
	if filepath.Dir(ts.path) != root || filepath.Base(ts.path) == "" || len(ts.path) <= len(root) {
		return errors.New("refusing to purge a mount point outside the browser container")
	}
	s.mu.Lock()
	for _, sh := range s.shares {
		if sh.tenant.Name == t.Name && sh.id == filepath.Base(ts.path) {
			s.mu.Unlock()
			return nil
		}
	}
	s.mu.Unlock()
	return removeAbsentOK(ts.path)
}

// purgeByKey releases the mount point one local directory's key names. It is the delete path
// for a SAVED folder whose capability is long gone: the token died with the mount (a gateway
// restart, the reconnect grace window, a reaped lease), so the tombstone that would have
// authorized the purge is gone too, and without this the empty directory would sit in the
// account's container forever. Reusing a key is normal, so the name must survive exactly what
// a tombstone purge must survive: a live mount of it, and anything this service could not
// have created. A directory that is already absent is success, not an error.
func (s *Service) purgeByKey(t registry.Tenant, key string) (any, error) {
	if !validDirectoryKey(key) {
		return nil, errors.New("invalid directory key")
	}
	s.mu.Lock()
	for _, sh := range s.shares {
		if sh.tenant.Name == t.Name && sh.id == key {
			s.mu.Unlock()
			return nil, errors.New("directory is still mounted")
		}
	}
	s.mu.Unlock()
	root := filepath.Join(t.Workspace, "browser")
	path := filepath.Join(root, key)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		// No container at all: nothing this service created here, so there is nothing to
		// release. (A tenant that never mounted has no browser directory.)
		return s.forgetRecord(t, key)
	}
	// The key is one segment (validDirectoryKey), so the path cannot leave the container —
	// checked anyway, exactly as the tombstone purge does: this is a removal.
	if filepath.Dir(path) != root || filepath.Base(path) != key || !noSymlinkAncestors(root) {
		return nil, errors.New("refusing to release a path outside the browser container")
	}
	mounted, err := mountInfoPath(path)
	if err != nil {
		return nil, err
	}
	if mounted {
		// Unreachable while the live-share check above holds; a mount with no share would be a
		// leftover the startup cleanup owns, not something to pull out from under a worker.
		return nil, errors.New("directory is still mounted")
	}
	if _, err := os.Lstat(path); err == nil {
		if err := reusableMountPoint(path); err != nil {
			return nil, err
		}
		if err := removeAbsentOK(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s.forgetRecord(t, key)
}

// forgetRecord drops the record of a released mount. It belongs to the release, not to the
// directory removal: a record left behind would be replayed by the next startup cleanup.
func (s *Service) forgetRecord(t registry.Tenant, key string) (any, error) {
	if s.recordDir != "" {
		if err := removeAbsentOK(s.recordPath(t.Name, key)); err != nil {
			return nil, err
		}
	}
	return map[string]bool{"closed": true}, nil
}

type payload struct {
	Token    string      `json:"token"`
	Name     string      `json:"name"`
	Writable bool        `json:"writable"`
	ID       string      `json:"id"`
	Result   fs.Response `json:"result"`
	// Key is the client's stable identity of one LOCAL directory: the mount point becomes
	// <workspace>/browser/<key>, so the same folder always maps to the same virtual path and
	// DSH's workspace entry for it keeps its id, title and sessions. It is normally the local
	// directory's own name (see the `allocate` endpoint), and the legacy random id for a
	// folder saved before directory names. Absent means a fresh random id (an older client),
	// whose mount point is removed when the mount ends.
	Key string `json:"key"`
	// Purge is the operator removing the folder for good: the mount point and its record are
	// released instead of kept for the next mount of the same key. With no token it is a
	// release by key alone — the capability that proved the mount is gone with the mount, and
	// the empty directory it left behind must still be releasable.
	Purge bool `json:"purge"`
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
	if op == "allocate" {
		// The client's own step before the first mount of a saved folder: answer with the
		// mount directory name that folder may own. Read-only on purpose — nothing is created
		// and nothing is reserved; see allocateName.
		return map[string]string{"key": s.allocateName(t, p.Name)}, nil
	}
	if op == "open" {
		sh, err := s.openWith(t, owner, openRequest{Name: p.Name, Writable: p.Writable, Key: p.Key})
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
	if op == "close" && p.Token == "" && p.Purge && p.Key != "" {
		// A release by key alone: the operator is deleting a SAVED folder whose capability is
		// long gone, and the empty mount point it left behind must not stay forever (a stable
		// mount point is never removed by a disconnect).
		return s.purgeByKey(t, p.Key)
	}
	if op == "resume" {
		return s.resume(p.Token, t.Name, owner)
	}
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
				if p.Purge {
					if err := s.purgeTombstoned(t, ts); err != nil {
						return nil, err
					}
				}
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
		if !sh.claimPoll() {
			// Another browser side holds this mount's poll connection. That happens when a
			// stale tab (or a replaced document) tries to serve a capability that has moved
			// on, and it must not be able to interleave answers with the live page.
			return nil, errors.New("directory served by another page")
		}
		defer sh.releasePoll()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case req := <-sh.queue:
				sh.mu.Lock()
				pending, exists := sh.pending[req.ID]
				closed, revoked := sh.closed, sh.revoked
				sh.mu.Unlock()
				if revoked {
					// A resume took this mount over while this loop was parked. The capability
					// is not this page's any more, and saying so is what makes the stale page
					// stop instead of fighting the live one.
					return nil, errors.New("directory revoked")
				}
				if closed {
					return nil, errors.New("disconnected")
				}
				if !exists || pending.ctx.Err() != nil {
					continue
				}
				return map[string]any{"requests": []fs.Request{req}}, nil
			case <-ctx.Done():
				// The poll request IS the browser side of this mount: net/http
				// cancels its context when the page's connection goes away
				// (reload, closed tab, crash, or the client's own abort before a
				// close). Nothing else can answer this mount's FUSE requests, so it
				// stops being serveable here rather than at the end of the lease —
				// until then every worker start would block on it (see serveable).
				//
				// It is not torn down here: a reload cancels the poll of a page that
				// is coming back, so the mount waits out reconnectGrace for a resume,
				// and the reaper closes it when that window passes unused.
				sh.disconnect()
				return nil, ctx.Err()
			case <-sh.done:
				return nil, errors.New("disconnected")
			case <-timer.C:
				sh.mu.Lock()
				revoked := sh.revoked
				sh.mu.Unlock()
				if revoked {
					return nil, errors.New("directory revoked")
				}
				return map[string]any{"requests": []fs.Request{}}, nil
			}
		}
	case "respond":
		sh.mu.Lock()
		if sh.revoked {
			sh.mu.Unlock()
			return nil, errors.New("directory revoked")
		}
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
		if err := s.closeContext(ctx, sh, false, p.Purge); err != nil {
			return nil, err
		}
		return map[string]bool{"closed": true}, nil
	default:
		return nil, errors.New("unknown endpoint")
	}
}
