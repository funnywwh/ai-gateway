// Package registry stores dshgw tenants and the public key-prefix index.
package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

const Version = 1

type HandshakeState string

const (
	HandshakePending HandshakeState = "pending"
	HandshakeOK      HandshakeState = "ok"
	HandshakeFailed  HandshakeState = "failed"
)

type Tenant struct {
	Name             string         `json:"name"`
	UID              int            `json:"uid"`
	PublicPort       int            `json:"public_port"`
	WorkerPort       int            `json:"worker_port"`
	KeyPrefix        string         `json:"key_prefix"`
	PreviousPrefixes []string       `json:"previous_prefixes,omitempty"`
	DshHome          string         `json:"dsh_home"`
	Workspace        string         `json:"workspace"`
	CreatedAt        time.Time      `json:"created_at"`
	Handshake        HandshakeState `json:"handshake"`
	DirectoryPicker  string         `json:"directory_picker,omitempty"`
	PluginBrowserFS  string         `json:"plugin_browser_fs,omitempty"`
	ModelsPending    bool           `json:"models_pending,omitempty"`
}

func (t Tenant) Prefixes() []string {
	out := make([]string, 0, 1+len(t.PreviousPrefixes))
	if t.KeyPrefix != "" {
		out = append(out, t.KeyPrefix)
	}
	out = append(out, t.PreviousPrefixes...)
	return out
}

type diskRegistry struct {
	Version int      `json:"version"`
	Tenants []Tenant `json:"tenants"`
}

type Registry struct {
	mu         sync.RWMutex
	path       string
	keyMapPath string
	tenants    map[string]Tenant
}

func New(path, keyMapPath string) *Registry {
	return &Registry{path: path, keyMapPath: keyMapPath, tenants: make(map[string]Tenant)}
}

func LoadRegistry(path string) (*Registry, error) {
	return Load(path, strings.TrimSuffix(path, ".json")+".keys.map")
}

// Reload atomically replaces the in-memory snapshot from disk. A missing file
// is an empty registry, matching first-install behavior.
func (r *Registry) Reload() error {
	next, err := Load(r.path, r.keyMapPath)
	if err != nil {
		return err
	}
	next.mu.RLock()
	tenants := clone(next.tenants)
	next.mu.RUnlock()
	r.mu.Lock()
	r.tenants = tenants
	r.mu.Unlock()
	return nil
}

func Load(path, keyMapPath string) (*Registry, error) {
	r := New(path, keyMapPath)
	data, err := securefile.ReadLimitedRegular(path, 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := securefile.CheckPermissions(path, 0o600); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var disk diskRegistry
	if err := dec.Decode(&disk); err != nil {
		return nil, fmt.Errorf("decode registry: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return nil, err
	}
	if disk.Version != Version {
		return nil, fmt.Errorf("unsupported registry version %d", disk.Version)
	}
	for _, t := range disk.Tenants {
		if err := validateTenant(t); err != nil {
			return nil, err
		}
		if _, exists := r.tenants[t.Name]; exists {
			return nil, fmt.Errorf("duplicate tenant %q", t.Name)
		}
		r.tenants[t.Name] = t
	}
	if err := validateUnique(r.tenants); err != nil {
		return nil, err
	}
	return r, nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("registry has trailing JSON")
		}
		return err
	}
	return nil
}

func validateTenant(t Tenant) error {
	if !config.ValidTenantName(t.Name) {
		return fmt.Errorf("invalid tenant name %q", t.Name)
	}
	if t.UID <= 0 || t.PublicPort < 1 || t.PublicPort > 65535 || t.WorkerPort < 1 || t.WorkerPort > 65535 || t.PublicPort == t.WorkerPort {
		return fmt.Errorf("tenant %s has invalid uid or ports", t.Name)
	}
	if !filepath.IsAbs(t.DshHome) || filepath.Clean(t.DshHome) != t.DshHome || !filepath.IsAbs(t.Workspace) || filepath.Clean(t.Workspace) != t.Workspace || t.CreatedAt.IsZero() {
		return fmt.Errorf("tenant %s has incomplete absolute paths or creation time", t.Name)
	}
	if t.Handshake != HandshakePending && t.Handshake != HandshakeOK && t.Handshake != HandshakeFailed {
		return fmt.Errorf("tenant %s has invalid handshake state", t.Name)
	}
	if !validPrefix(t.KeyPrefix) {
		return fmt.Errorf("tenant %s key prefix must be 12 printable non-space ASCII characters", t.Name)
	}
	seen := map[string]bool{t.KeyPrefix: true}
	for _, prefix := range t.PreviousPrefixes {
		if !validPrefix(prefix) || seen[prefix] {
			return fmt.Errorf("tenant %s has invalid or duplicate previous prefix", t.Name)
		}
		seen[prefix] = true
	}
	return nil
}

func validPrefix(prefix string) bool {
	if len(prefix) != 12 {
		return false
	}
	for _, b := range []byte(prefix) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

func validateUnique(ts map[string]Tenant) error {
	ports := map[int]string{}
	workers := map[int]string{}
	prefixes := map[string]string{}
	for name, t := range ts {
		if prior := ports[t.PublicPort]; prior != "" {
			return fmt.Errorf("public port %d shared by %s and %s", t.PublicPort, prior, name)
		}
		ports[t.PublicPort] = name
		if prior := workers[t.WorkerPort]; prior != "" {
			return fmt.Errorf("worker port %d shared by %s and %s", t.WorkerPort, prior, name)
		}
		workers[t.WorkerPort] = name
		for _, p := range t.Prefixes() {
			if prior := prefixes[p]; prior != "" {
				return fmt.Errorf("key prefix %s shared by %s and %s", p, prior, name)
			}
			prefixes[p] = name
		}
	}
	return nil
}

func (r *Registry) Path() string       { return r.path }
func (r *Registry) KeyMapPath() string { return r.keyMapPath }

func (r *Registry) List() []Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		out = append(out, copyTenant(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Get(name string) (Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tenants[name]
	return copyTenant(t), ok
}
func (r *Registry) ByPublicPort(port int) (Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tenants {
		if t.PublicPort == port {
			return copyTenant(t), true
		}
	}
	return Tenant{}, false
}
func (r *Registry) ByPrefix(prefix string) (Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tenants {
		for _, p := range t.Prefixes() {
			if p == prefix {
				return copyTenant(t), true
			}
		}
	}
	return Tenant{}, false
}

func (r *Registry) Put(t Tenant) error {
	if err := validateTenant(t); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := clone(r.tenants)
	next[t.Name] = copyTenant(t)
	if err := validateUnique(next); err != nil {
		return err
	}
	r.tenants = next
	return nil
}

func (r *Registry) Delete(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := clone(r.tenants)
	delete(next, name)
	r.tenants = next
}

func copyTenant(t Tenant) Tenant {
	t.PreviousPrefixes = append([]string(nil), t.PreviousPrefixes...)
	return t
}

func clone(in map[string]Tenant) map[string]Tenant {
	out := make(map[string]Tenant, len(in))
	for k, v := range in {
		v.PreviousPrefixes = append([]string(nil), v.PreviousPrefixes...)
		out[k] = v
	}
	return out
}

func (r *Registry) RotatePrefix(name, prefix string, keepPrevious bool) error {
	if !validPrefix(prefix) {
		return errors.New("key prefix must contain exactly 12 printable non-space ASCII characters")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[name]
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	next := clone(r.tenants)
	if prefix == t.KeyPrefix {
		if !keepPrevious {
			t.PreviousPrefixes = nil
			next[name] = t
			r.tenants = next
		}
		return nil
	}
	var previous []string
	if keepPrevious {
		previous = append(previous, t.KeyPrefix)
		for _, old := range t.PreviousPrefixes {
			if old != prefix && old != t.KeyPrefix {
				previous = append(previous, old)
			}
		}
	}
	t.PreviousPrefixes = previous
	t.KeyPrefix = prefix
	next[name] = t
	if err := validateUnique(next); err != nil {
		return err
	}
	r.tenants = next
	return nil
}

// AddPrefix binds an additional login prefix without changing the credential
// currently injected into the tenant worker.
func (r *Registry) AddPrefix(name, prefix string) error {
	if !validPrefix(prefix) {
		return errors.New("key prefix must contain exactly 12 printable non-space ASCII characters")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[name]
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	for _, current := range t.Prefixes() {
		if current == prefix {
			return nil
		}
	}
	next := clone(r.tenants)
	t.PreviousPrefixes = append(t.PreviousPrefixes, prefix)
	next[name] = t
	if err := validateUnique(next); err != nil {
		return err
	}
	r.tenants = next
	return nil
}

func (r *Registry) SetHandshake(name string, state HandshakeState) error {
	if state != HandshakePending && state != HandshakeOK && state != HandshakeFailed {
		return errors.New("invalid handshake state")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[name]
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	t.Handshake = state
	r.tenants[name] = t
	return nil
}

// AssignPorts chooses one free public and worker port. taken must report
// listeners outside this registry (normally a snapshot of ss -ltn).
func (r *Registry) AssignPorts(cfg *config.Config, taken func(int) bool) (int, int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	used := make(map[int]bool, len(r.tenants)*2)
	for _, t := range r.tenants {
		used[t.PublicPort] = true
		used[t.WorkerPort] = true
	}
	choose := func(lo, hi int) (int, error) {
		for p := lo; p <= hi; p++ {
			if !used[p] && (taken == nil || !taken(p)) {
				return p, nil
			}
		}
		return 0, fmt.Errorf("port range %d-%d is exhausted", lo, hi)
	}
	public, err := choose(cfg.TenantPortLo, cfg.TenantPortHi)
	if err != nil {
		return 0, 0, err
	}
	worker, err := choose(cfg.WorkerPortLo, cfg.WorkerPortHi)
	if err != nil {
		return 0, 0, err
	}
	return public, worker, nil
}

func (r *Registry) TenantPorts() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.tenants))
	for n, t := range r.tenants {
		out[n] = t.PublicPort
	}
	return out
}

// Save atomically replaces canonical registry.json (0600) and its derived,
// operator-readable keys.map index (0640) under one flock. Authentication
// always uses canonical registry.json, so a crash while updating the derived
// map cannot grant or revoke access incorrectly; the next Save repairs it.
func (r *Registry) Save() error {
	data, keyData, err := r.encoded()
	if err != nil {
		return err
	}
	return securefile.WithLock(r.path+".lock", func() error {
		if err := securefile.WriteAtomic(r.path, data, 0o600); err != nil {
			return err
		}
		return securefile.WriteAtomic(r.keyMapPath, keyData, 0o640)
	})
}

func (r *Registry) encoded() ([]byte, []byte, error) {
	r.mu.RLock()
	tenants := make([]Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		tenants = append(tenants, copyTenant(t))
	}
	r.mu.RUnlock()
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].Name < tenants[j].Name })
	data, err := json.MarshalIndent(diskRegistry{Version: Version, Tenants: tenants}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	data = append(data, '\n')
	keyData, err := encodeKeyMap(tenants)
	return data, keyData, err
}

func encodeKeyMap(tenants []Tenant) ([]byte, error) {
	var lines []string
	seen := map[string]string{}
	for _, t := range tenants {
		for _, p := range t.Prefixes() {
			if prior := seen[p]; prior != "" && prior != t.Name {
				return nil, fmt.Errorf("prefix %s is duplicated", p)
			}
			seen[p] = t.Name
			lines = append(lines, p+" "+t.Name)
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return []byte{}, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func LoadKeyMap(path string) (map[string]string, error) {
	data, err := securefile.ReadLimitedRegular(path, 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := securefile.CheckPermissions(path, 0o640); err != nil {
		return nil, err
	}
	out := map[string]string{}
	scan := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for scan.Scan() {
		line++
		fields := strings.Fields(scan.Text())
		if len(fields) != 2 || !validPrefix(fields[0]) || !config.ValidTenantName(fields[1]) {
			return nil, fmt.Errorf("invalid keys.map line %d", line)
		}
		if _, ok := out[fields[0]]; ok {
			return nil, fmt.Errorf("duplicate key prefix on line %d", line)
		}
		out[fields[0]] = fields[1]
	}
	return out, scan.Err()
}

// ListeningPorts parses `ss -H -ltn` output. The command execution remains in
// cmd/dshgw so this package is deterministic and easily tested.
func ListeningPorts(output []byte) map[int]bool {
	out := map[int]bool{}
	scan := bufio.NewScanner(bytes.NewReader(output))
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 4 {
			continue
		}
		addr := fields[3]
		idx := strings.LastIndexByte(addr, ':')
		if idx < 0 {
			continue
		}
		p, err := strconv.Atoi(strings.Trim(addr[idx+1:], "[]"))
		if err == nil {
			out[p] = true
		}
	}
	return out
}
