// Package nodestore holds the control plane's knowledge of its worker nodes (M77): where each
// node is, how to reach it, how to install it over ssh, and what the last deployment did.
//
// Two sources of node identity exist and this package is where they meet:
//
//   - configuration (`nodes:` in dshgw.yaml / aigw's dshgw block) — the machines an operator
//     promises are always there, with their address and token written down in a file the
//     console cannot edit;
//   - this store (`<state_dir>/nodes.json`) — the machines added from the console's "DSH
//     node" page, plus the deployment metadata (ssh target, paths, overrides, last status) of
//     every node, including the configured ones.
//
// The precedence rule is one sentence: **configuration is authoritative for identity
// (address + token), the store contributes deployment metadata.** A name that exists in both
// therefore gets its URL and token from configuration and everything else from the store; a
// name that exists only in the store is fully console-managed. This is what lets an operator
// declare the nodes in configuration and still press "deploy" in the console.
package nodestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// Version is the on-disk format version.
const Version = 1

// Sources of a node record.
const (
	SourceConfig  = "config"
	SourceConsole = "console"
)

// Deployment states, as the console shows them.
const (
	StatePending     = "pending"     // registered, never deployed
	StateDeploying   = "deploying"   // a deploy job is running
	StateReady       = "ready"       // deployed and the last probe answered
	StateFailed      = "failed"      // the last deploy failed (phase + error say where)
	StateUnreachable = "unreachable" // deployed earlier, but the node does not answer now
)

// SSH is how to reach the node's machine to install it. Empty means the node was installed by
// hand and the console must not offer the deploy action.
type SSH struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
	// KeyFile is the private key on the **control plane** (mode 0600). It is a credential
	// equivalent to deployment rights on the target machine; the API never returns its
	// contents, only the fingerprint keyed from it.
	KeyFile string `json:"key_file,omitempty"`
	// KnownHostsFile pins the target's host key. It lives beside the key and is written on
	// first deploy after the operator confirms the fingerprint.
	KnownHostsFile string `json:"known_hosts_file,omitempty"`
	// HostKeyFingerprint is the SHA256 fingerprint the operator confirmed. While it is empty
	// the deploy stops at the fingerprint gate instead of trusting the network.
	HostKeyFingerprint string    `json:"host_key_fingerprint,omitempty"`
	FingerprintSetAt   time.Time `json:"fingerprint_set_at,omitempty"`
}

// Configured reports whether a deploy over ssh is possible at all.
func (s SSH) Configured() bool {
	return strings.TrimSpace(s.Host) != "" && strings.TrimSpace(s.User) != "" && strings.TrimSpace(s.KeyFile) != ""
}

// Deploy describes where things live on the node's machine. Every path is absolute and is
// interpreted **on the node**; the control plane only carries them around.
type Deploy struct {
	Dir      string `json:"dir,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
	// TokenPath is where the node's shared secret is installed ON THE TARGET. It is not
	// TokenFile: that one names a file on the control plane, which is a different machine and a
	// different purpose (an operator who keeps the secret in a file rather than in the record).
	TokenPath    string `json:"token_path,omitempty"`
	PluginPath   string `json:"plugin_path,omitempty"`
	TemplateHome string `json:"template_home,omitempty"`
	BwrapBin     string `json:"bwrap_bin,omitempty"`
	NodeBin      string `json:"node_bin,omitempty"`
	BinJS        string `json:"bin_js,omitempty"`
	CurrentLink  string `json:"current_link,omitempty"`
	WorkerPortLo int    `json:"worker_port_lo,omitempty"`
	WorkerPortHi int    `json:"worker_port_hi,omitempty"`
}

// FeatureSwitch is one on/off feature the node's configuration carries. A nil pointer means
// "inherit the control plane's value", which is the common case: a deployment that offers ssh
// workspaces on one machine offers them on all of them unless the operator says otherwise.
type FeatureSwitch struct {
	Enabled bool `json:"enabled"`
}

// Overrides are the per-node deviations from the control plane's own tenant-side
// configuration. They exist because some of those values are machine-local by nature: which
// host directories are shared, how much memory a worker may take, and whether this machine
// has the runtime for FUSE workspaces at all.
type Overrides struct {
	HostShares        *config.HostShares   `json:"host_shares,omitempty"`
	SSHWorkspaces     *FeatureSwitch       `json:"ssh_workspaces,omitempty"`
	BrowserWorkspaces *FeatureSwitch       `json:"browser_workspaces,omitempty"`
	WorkerLimits      *config.WorkerLimits `json:"worker_limits,omitempty"`
	WorkspaceSeed     []string             `json:"workspace_seed,omitempty"`
	DirectoryPicker   string               `json:"directory_picker,omitempty"`
	PluginBrowserFS   string               `json:"plugin_browser_fs,omitempty"`
}

// Status is the last thing we know about a node: its deployment state and the node's own
// self-description as of the last probe. It is persisted so an operator opening the console
// the morning after a failed deploy sees the failure instead of an empty page.
type Status struct {
	State  string `json:"state"`
	Phase  string `json:"phase,omitempty"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	// LogPath names the deployment log file on the control plane
	// (<state_dir>/node-deploy/<node>.log). The API serves its tail; the file is bounded by
	// the deploy runner.
	LogPath string `json:"log_path,omitempty"`

	Version  string    `json:"version,omitempty"`
	Revision string    `json:"revision,omitempty"`
	Protocol int       `json:"protocol,omitempty"`
	ReadyAt  time.Time `json:"ready_at,omitempty"`
	// ProbedAt is when the health check last ran, and Reachable its outcome. They are the
	// difference between "deployed" and "answering right now".
	ProbedAt  time.Time `json:"probed_at,omitempty"`
	Reachable bool      `json:"reachable,omitempty"`

	// AuditCursor is how far this node's audit has been read into the control plane's stream
	// (M77). Persisted so a gateway restart resumes instead of re-importing or skipping history.
	AuditCursor string `json:"audit_cursor,omitempty"`

	// Features is what the node reported it can serve at the last probe (M77): the console shows
	// it, so an operator learns which machine offers ssh workspaces or browser mounts without
	// discovering it by failure.
	Features nodeproto.Features `json:"features,omitempty"`

	// StartedAt/FinishedAt frame the last deploy or probe; they are what the console shows as
	// "deployed 3 minutes ago".
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	DeployStartedAt  time.Time `json:"deploy_started_at,omitempty"`
	DeployFinishedAt time.Time `json:"deploy_finished_at,omitempty"`
}

// Node is one worker node as the control plane knows it.
type Node struct {
	Name string `json:"name"`
	// Listen is the address the node agent binds on its own machine (node.listen). The control
	// plane needs it to generate a node's configuration: the node cannot guess which of its
	// interfaces the control plane reaches it on.
	Listen string `json:"listen,omitempty"`
	// URL is the node agent's base address; Token/TokenFile are the shared secret. These three
	// are identity: for a configuration-declared node they come from configuration.
	URL       string `json:"url"`
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	// Source is where the identity came from ("config" | "console"). It is recomputed by View
	// and never trusted from disk.
	Source string `json:"-"`
	// Default marks the node new tenants land on when nobody names one. At most one node has
	// it, and the control plane's own machine ("local") may hold it too without being in this
	// store at all.
	Default bool `json:"default,omitempty"`

	SSH        SSH        `json:"ssh,omitempty"`
	Deploy     Deploy     `json:"deploy,omitempty"`
	Overrides  Overrides  `json:"overrides,omitempty"`
	Status     Status     `json:"status,omitempty"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// diskStore is the on-disk document.
type diskStore struct {
	Version int    `json:"version"`
	Nodes   []Node `json:"nodes"`
}

// Store is the in-memory snapshot plus its file. Every mutation saves; the file is small (a
// handful of nodes) and a control-plane change is a rare, operator-driven event.
type Store struct {
	mu    sync.RWMutex
	path  string
	nodes map[string]Node
}

// New returns an empty store writing to path.
func New(path string) *Store {
	return &Store{path: path, nodes: make(map[string]Node)}
}

// Load reads the store. A missing file is an empty store, exactly like the registry: the
// single-machine deployment never creates one.
func Load(path string) (*Store, error) {
	s := New(path)
	data, err := securefile.ReadLimitedRegular(path, 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := securefile.CheckPermissions(path, 0o600); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var disk diskStore
	if err := dec.Decode(&disk); err != nil {
		return nil, fmt.Errorf("decode node store: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("node store has trailing JSON")
	}
	if disk.Version != Version {
		return nil, fmt.Errorf("unsupported node store version %d", disk.Version)
	}
	for _, n := range disk.Nodes {
		if err := validate(n); err != nil {
			return nil, err
		}
		if _, exists := s.nodes[n.Name]; exists {
			return nil, fmt.Errorf("duplicate node %q", n.Name)
		}
		s.nodes[n.Name] = n
	}
	return s, nil
}

// Path is where this store persists itself.
func (s *Store) Path() string { return s.path }

// List returns the stored records in name order.
func (s *Store) List() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get looks one record up.
func (s *Store) Get(name string) (Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[name]
	return n, ok
}

// Put inserts or replaces a record and saves.
func (s *Store) Put(n Node) error {
	if err := validate(n); err != nil {
		return err
	}
	s.mu.Lock()
	previous, existed := s.nodes[n.Name]
	n.CreatedAt = previous.CreatedAt
	n.UpdatedAt = time.Now().UTC()
	if !existed || n.CreatedAt.IsZero() {
		n.CreatedAt = n.UpdatedAt
	}
	s.nodes[n.Name] = n
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

// SetStatus records the outcome of a probe or a deploy. A missing record is not an error: a
// probe may finish after the operator deleted the node, and resurrecting it would be worse
// than dropping the result.
func (s *Store) SetStatus(name string, status Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[name]
	if !ok {
		return nil
	}
	n.Status = status
	n.UpdatedAt = time.Now().UTC()
	s.nodes[name] = n
	return s.saveLocked()
}

// AuditCursor reports how far one node's audit has been merged into this gateway's stream.
func (s *Store) AuditCursor(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[name].Status.AuditCursor
}

// SetAuditCursor records that position.
//
// A node declared in configuration has no record of its own in this store, so one is created empty
// for the cursor: the cursor still has to live somewhere, or every gateway start would re-import a
// node's whole history (or, with the node's "start from now" rule, silently skip everything written
// while the gateway was down). The empty record does not take over that node's identity — View
// still resolves it from configuration.
func (s *Store) SetAuditCursor(name, cursor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[name]
	if !ok {
		node = Node{Name: name}
	}
	if node.Status.AuditCursor == cursor {
		return nil
	}
	node.Status.AuditCursor = cursor
	node.UpdatedAt = time.Now().UTC()
	s.nodes[name] = node
	return s.saveLocked()
}

// Delete removes a record. It never touches the node's machine: removing the machine's data is
// a separate, explicit operation.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[name]; !ok {
		return fmt.Errorf("node %q not found", name)
	}
	delete(s.nodes, name)
	return s.saveLocked()
}

// SetDefault marks exactly one node as the default placement, or clears the flag when name is
// empty or "local" (the control plane's own machine, which is not in this store).
func (s *Store) SetDefault(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != "" && !config.IsLocalNodeName(name) {
		if _, ok := s.nodes[name]; !ok {
			return fmt.Errorf("node %q not found", name)
		}
	}
	for key, n := range s.nodes {
		want := key == name
		if n.Default == want {
			continue
		}
		n.Default = want
		n.UpdatedAt = time.Now().UTC()
		s.nodes[key] = n
	}
	return s.saveLocked()
}

// View is the runtime node list: the static configuration entries (authoritative for identity)
// each enriched with the stored record of the same name, plus the console-managed nodes.
//
// A name that is in both is not an error: configuration decides the address and the token, the
// store decides everything else. It is the shape that lets an operator declare nodes in a file
// and still press "deploy" in the console.
func (s *Store) View(static []config.Node) ([]Node, error) {
	s.mu.RLock()
	stored := make(map[string]Node, len(s.nodes))
	for k, v := range s.nodes {
		stored[k] = v
	}
	s.mu.RUnlock()

	out := make([]Node, 0, len(static)+len(stored))
	seen := make(map[string]bool, len(static))
	for _, cfgNode := range static {
		name := strings.TrimSpace(cfgNode.Name)
		record, ok := stored[name]
		if !ok {
			record = Node{Name: name}
		}
		record.Name = name
		record.URL = strings.TrimSpace(cfgNode.URL)
		record.Token = cfgNode.Token
		record.TokenFile = cfgNode.TokenFile
		record.Source = SourceConfig
		out = append(out, record)
		seen[name] = true
	}
	for name, record := range stored {
		if seen[name] {
			continue
		}
		record.Source = SourceConsole
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Find looks one node up in a view produced by View.
func Find(nodes []Node, name string) (Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// DefaultNode returns the node new tenants land on: the one flagged default, else the control
// plane's own machine when the list does not exist or names no default.
func DefaultNode(nodes []Node, configuredDefault string) string {
	if trimmed := strings.TrimSpace(configuredDefault); trimmed != "" {
		return trimmed
	}
	for _, n := range nodes {
		if n.Default {
			return n.Name
		}
	}
	return config.LocalNodeName
}

// NamesKnown is the check registry.ValidateNodes wants: does this deployment define that node?
func NamesKnown(nodes []Node) func(string) bool {
	index := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		index[n.Name] = true
	}
	return func(name string) bool { return index[name] }
}

// Resolve looks a node up by reference: the empty name and the literal "local" resolve to the
// control plane's own machine, which is deliberately not a record in this store (it is not a
// machine the console can deploy to).
func Resolve(nodes []Node, name string) (Node, bool) {
	if config.IsLocalNodeName(name) {
		return Node{Name: config.LocalNodeName, Source: config.LocalNodeName}, true
	}
	return Find(nodes, name)
}

// validate enforces the identity rules shared with configuration loading, plus the store's own
// rule that a record must be reachable some way: either an address, or an ssh target so the
// console can deploy it.
func validate(n Node) error {
	name := strings.TrimSpace(n.Name)
	if !config.ValidNodeName(name) {
		return fmt.Errorf("node name %q must match ^[a-z][a-z0-9-]{0,25}$ and must not be %q", n.Name, config.LocalNodeName)
	}
	if strings.TrimSpace(n.URL) != "" {
		if err := config.ValidateNodeURL("node "+name+" url", n.URL); err != nil {
			return err
		}
	}
	if err := config.ValidateNodeTokenSource("node "+name, n.Token, n.TokenFile, false); err != nil {
		return err
	}
	// A url without a secret is legal: that is exactly what a registered-but-not-yet-deployed node
	// looks like. The deploy installs the token on the target and records it here; until then every
	// call to the node is refused by the node, which is the honest state (and `node list` reports it
	// as "not deployed yet"). A node whose identity came from configuration always has one, because
	// the gateway cannot call it otherwise.
	if n.SSH.Port < 0 || n.SSH.Port > 65535 {
		return fmt.Errorf("node %s has an invalid ssh port", name)
	}
	if n.Deploy.WorkerPortLo != 0 && n.Deploy.WorkerPortHi != 0 && n.Deploy.WorkerPortLo > n.Deploy.WorkerPortHi {
		return fmt.Errorf("node %s worker port range low bound exceeds high bound", name)
	}
	for label, path := range map[string]string{
		"deploy.dir": n.Deploy.Dir, "deploy.state_dir": n.Deploy.StateDir,
		"deploy.plugin_path": n.Deploy.PluginPath, "deploy.template_home": n.Deploy.TemplateHome,
		"deploy.token_path": n.Deploy.TokenPath,
		"ssh.key_file":      n.SSH.KeyFile, "ssh.known_hosts_file": n.SSH.KnownHostsFile,
	} {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("node %s %s must be a clean absolute path", name, label)
		}
	}
	if n.Status.State != "" && !validState(n.Status.State) {
		return fmt.Errorf("node %s has an unknown status state %q", name, n.Status.State)
	}
	return nil
}

func validState(state string) bool {
	switch state {
	case StatePending, StateDeploying, StateReady, StateFailed, StateUnreachable:
		return true
	}
	return false
}

func (s *Store) saveLocked() error {
	nodes := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		n.Source = ""
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	data, err := json.MarshalIndent(diskStore{Version: Version, Nodes: nodes}, "", "  ")
	if err != nil {
		return err
	}
	// WriteAtomic creates the parent directory (0750) and pins it against symlinks; the state
	// directory itself is created by whoever owns the deployment root.
	return securefile.WriteAtomic(s.path, append(data, '\n'), 0o600)
}
