// Package config loads and validates dshgw's standalone configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where a standalone `dshgw` looks for its configuration when no
// -config is given: the deployment root, next to aigw's config.yaml. It used to be
// /etc/dshgw/config.yaml, which belonged to the deleted root/systemd shape and put the
// one file an operator edits in a directory the running account cannot write (M63).
const DefaultPath = "./dshgw.yaml"

// defaultDataRoot is the single runtime data root for the standalone shape. It repeats
// internal/config's DefaultDataDir literally on purpose: the arch table forbids
// internal/dshgw/** from importing any other internal package, and a second data root
// hidden behind a convenience import would be worse than the literal (see
// docs/deployment-layout.md).
const defaultDataRoot = "./data"

// defaultImageRequestMaxBytes bounds the accumulated base64 image payload a tenant's dsh may
// put in one request to aigw (M68). 7 MiB of images inside aigw's 10 MiB
// `server.max_body_bytes` default leaves room for the same request's system prompt, history,
// tool definitions and JSON — the gateway reads a bounded body, so an oversized request is
// truncated into a parse error rather than refused with a message about its size.
const defaultImageRequestMaxBytes = 7 << 20

var tenantNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,25}[a-z0-9]$|^[a-z]$`)
var edgeHeaderRE = regexp.MustCompile(`(?i)^x-[a-z0-9]+(?:-[a-z0-9]+)*$`)
var accountNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
var publicHostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Duration is a YAML duration such as 5s or 168h.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration   { return time.Duration(d) }
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// RateLimit controls portal login attempts per source address.
type RateLimit struct {
	Requests int      `yaml:"requests" json:"requests"`
	Window   Duration `yaml:"window" json:"window"`
}

// DshRuntime describes an unpacked dsh release. BinJS is the public launcher
// contract; CurrentLink is switched by upgrade-dsh.
type DshRuntime struct {
	NodeBin     string `yaml:"node_bin" json:"node_bin"`
	BinJS       string `yaml:"bin_js" json:"bin_js"`
	CurrentLink string `yaml:"current_link" json:"current_link"`
}

// PublicBaseURL switches the deployment to single-domain path mode: every public
// URL dshgw generates (portal redirects, tenant redirects, CSP form-action) is
// built from this base plus a path prefix instead of from a host:port origin.
//
// It exists because subdomains are not always available: with one domain and no
// wildcard DNS, the only way to give each tenant its own URL space is a path.
// Empty keeps the port-based behaviour.
type PublicBaseURL string

// WorkerLimits are the per-worker resources, expressed the way cgroup v2 wants
// them. Zero means "no limit". They replace the old unit's MemoryHigh/MemoryMax/
// CPUQuota/TasksMax, which systemd enforced for us.
type WorkerLimits struct {
	MemoryHighBytes int64 `yaml:"memory_high_bytes" json:"memory_high_bytes"`
	MemoryMaxBytes  int64 `yaml:"memory_max_bytes" json:"memory_max_bytes"`
	TasksMax        int   `yaml:"tasks_max" json:"tasks_max"`
	CPUQuotaPercent int   `yaml:"cpu_quota_percent" json:"cpu_quota_percent"`
}

// TLSConfig is the certificate the edge serves directly. Empty values mean plain
// HTTP, which is only appropriate on a trusted network: there is no nginx in this
// shape to terminate TLS for us.
type TLSConfig struct {
	Certificate    string `yaml:"certificate" json:"certificate"`
	CertificateKey string `yaml:"certificate_key" json:"certificate_key"`
}

// DeployConfig holds the paths and identities a dshgw instance needs. Everything
// that described the deleted systemd/nginx/root shape (units, slices, nginx
// directories, per-tenant account prefixes, TLS for an external edge) is gone:
// dshgw now runs as aigw's child, starts tenant workers as its own processes, and
// owns no host-side service configuration.
type DeployConfig struct {
	PluginPath       string `yaml:"plugin_path" json:"plugin_path"`
	TemplateHome     string `yaml:"template_home" json:"template_home"`
	BackupDir        string `yaml:"backup_dir" json:"backup_dir"`
	TenantConfigRoot string `yaml:"tenant_config_root" json:"tenant_config_root"`
	ConfigPath       string `yaml:"config_path" json:"config_path"`
	// GatewayUser is the account the gateway runs as. It is the default for
	// WorkerUser so a deployment does not need a second service account.
	GatewayUser string `yaml:"gateway_user" json:"gateway_user"`
	// WorkerUser is the unprivileged account every tenant worker runs as. It must
	// not be root: a root caller could build a wider namespace instead of living
	// inside the tenant's profile.
	WorkerUser string `yaml:"worker_user" json:"worker_user"`
	// BwrapBin is the bubblewrap executable the sandbox profile and doctor use.
	BwrapBin string `yaml:"bwrap_bin" json:"bwrap_bin"`
	// PublicListen is the address the edge binds the portal and tenant public
	// ports on. Loopback is the safe default; exposing tenants to a network is an
	// explicit decision (and needs TLS, see tls:).
	PublicListen string `yaml:"public_listen" json:"public_listen"`
}

// Feishu is this gateway's side of the identity handoff (M61): where to send a person who
// wants to sign in with Feishu, and the key that proves the ticket aigw hands back is ours.
//
// Both values are normally injected by aigw when it generates this process's configuration
// (the supervised shape), so an operator has nothing to fill in; a standalone deployment
// sets them by hand to the same values as its aigw.
type Feishu struct {
	// Enabled opens the portal's Feishu login. While it is off the portal shows only the
	// key form and the login routes do not exist.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// AigwLoginURL is the browser-visible URL of aigw's /feishu/login: the page a person is
	// sent to before Feishu. It is empty while the feature is off.
	AigwLoginURL string `yaml:"aigw_login_url" json:"aigw_login_url"`
	// TicketSecret must equal aigw's feishu.ticket_secret (or the value aigw derives from
	// its credentials key and injects here).
	TicketSecret string `yaml:"ticket_secret" json:"ticket_secret"`
}

// SSHWorkspaces configures per-tenant SSH workspaces (M64): an account browses and creates
// directories on a remote host with its own key, and the gateway mounts the chosen remote
// directory inside that account's workspace so its dsh can register it as a workspace.
//
// The mount happens HERE, outside the tenant sandbox, because a tenant worker cannot mount
// at all: its bubblewrap profile gives it a minimal /dev (no /dev/fuse) and, since the host's
// root uid is unmapped inside its user namespace, no working setuid fusermount3 either, so
// mount(2) is refused whatever capabilities the profile grants (measured; see
// docs/design/m64-ssh-workspace.md §3). The tenant side keeps the ssh half — listing and
// creating remote directories — because that is what its own key is for.
//
// Off by default: enabling it means this gateway will ssh to remote hosts with a key an
// operator placed here, which is an explicit decision.
type SSHWorkspaces struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MountSubdir is the single path segment under each tenant workspace that holds mounts:
	// <workspace>/<mount_subdir>/<host>/<remote path>. It must not be hidden (a hidden
	// container would be invisible in the account's picker) and must not collide with a
	// workspace seed.
	MountSubdir string `yaml:"mount_subdir" json:"mount_subdir"`
	// SSHBin and SSHFSBin default to PATH lookups.
	SSHBin   string `yaml:"ssh_bin" json:"ssh_bin"`
	SSHFSBin string `yaml:"sshfs_bin" json:"sshfs_bin"`
	// IdentityDir holds per-account keys (<dir>/<account>), copied into an account that has no
	// key of its own yet.
	//
	// The key IS the boundary of what an account may reach: an account can read its own key,
	// so one shared key makes every account able to reach everything that key can. There is
	// therefore no shared source any more (see rejectRemovedIdentitySource): a deployment that
	// pointed one at the operator's own ~/.ssh/id_rsa handed every tenant the operator's
	// personal key, which is also the key that opens the gateway host itself. Accounts are
	// scoped differently only by holding different keys, and a directory inside the deployment
	// account's own ~/.ssh is refused below.
	IdentityDir string `yaml:"identity_dir" json:"identity_dir"`
	// SSHConfigDir holds one alias list per account (<dir>/<account>), copied to
	// <workspace>/.ssh/config for accounts that have none.
	//
	// There is deliberately no host-wide source: one shared file would hand every tenant the
	// same host inventory (the deployment account's own ~/.ssh/config names every machine the
	// operator knows) and make one edit decide for every tenant at once. Each account's list
	// belongs to that account — the tenant plugin adds and removes entries in it — and a
	// directory inside the deployment account's own ~/.ssh is refused below.
	SSHConfigDir string `yaml:"ssh_config_dir" json:"ssh_config_dir"`
	// Hosts is an optional allow-list for both halves. It is a guard rail, not a boundary:
	// the key an account holds is what really decides where it may go.
	Hosts []string `yaml:"hosts" json:"hosts"`
	// ConnectTimeout bounds every ssh round-trip; PollInterval is how often the gateway
	// services the tenant mailbox.
	ConnectTimeout Duration `yaml:"connect_timeout" json:"connect_timeout"`
	PollInterval   Duration `yaml:"poll_interval" json:"poll_interval"`
	MaxEntries     int      `yaml:"max_entries" json:"max_entries"`
	// SSHFSOptions are passed to sshfs -o. allow_other/allow_root are refused by the code:
	// every tenant worker shares one uid, so a shared mount would be a cross-account read.
	SSHFSOptions []string `yaml:"sshfs_options" json:"sshfs_options"`
	// DisableAutoRemount keeps the gateway from re-mounting recorded mounts at startup.
	DisableAutoRemount bool `yaml:"disable_auto_remount" json:"disable_auto_remount"`
}

// HostShares are operator-declared host directories bound straight into a tenant's workspace
// (M71). No ssh, no sshfs, no FUSE: the worker's bubblewrap profile binds the directory at
// <workspace>/<subdir>/<name>, so it behaves inside the sandbox like any other directory —
// local reads, working inotify, and none of the uninterruptible-wait failures a FUSE mount can
// produce (measured 2026-09-21: an sshfs mount of a directory that contains its own mount point
// hung every session of an account in a D state no signal could break).
//
// Why this exists next to ssh_workspaces: an sshfs mount is the right tool for another machine
// and the wrong one for this host. A host directory needs no network round trip, and the local
// case is exactly where sshfs is most dangerous — the workspace lives on that same host, so a
// mount of any ancestor of it contains itself.
//
// Operator-only by construction: there is no mailbox request for a share, because a host
// directory is not the tenant's to choose. Each share names the tenants that may see it, and is
// read-only unless the deployment says otherwise.
type HostShares struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Subdir is the single path segment under each tenant workspace that holds the binds. It
	// must be visible (a hidden container would be invisible in the account's picker), must not
	// collide with a workspace seed, and must differ from ssh_workspaces.mount_subdir.
	Subdir string `yaml:"subdir" json:"subdir"`
	// Shares is the declared list. Order is display order; names are unique.
	Shares []HostShare `yaml:"shares" json:"shares"`
}

// HostShare is one declared host directory.
type HostShare struct {
	// Name is the path segment the tenant sees: <workspace>/<subdir>/<name>.
	Name string `yaml:"name" json:"name"`
	// Path is the host directory. It is resolved through symlinks at load time, and it must be
	// disjoint from the state directory: a share that contains it would hand every tenant's
	// workspace, DSH home, key and session file to whoever sees the share.
	Path string `yaml:"path" json:"path"`
	// ReadOnly writes nothing back to the host. It is the default (a write grant is an explicit
	// `read_only: false`) because this is the host's own file system, not a scratch space.
	ReadOnly *bool `yaml:"read_only" json:"read_only"`
	// Tenants lists the accounts that may see the share. It must be non-empty when the feature
	// is enabled: "everyone" is not a safe default for a host directory.
	Tenants []string `yaml:"tenants" json:"tenants"`
}

// EffectiveReadOnly reports whether the share is read-only, defaulting to true.
func (s HostShare) EffectiveReadOnly() bool {
	return s.ReadOnly == nil || *s.ReadOnly
}

// Config is deliberately independent of aigw's internal configuration types.
type Config struct {
	PublicHost     string `yaml:"public_host" json:"public_host"`
	PortalPort     int    `yaml:"portal_port" json:"portal_port"`
	TenantPortLo   int    `yaml:"tenant_port_lo" json:"tenant_port_lo"`
	TenantPortHi   int    `yaml:"tenant_port_hi" json:"tenant_port_hi"`
	WorkerPortLo   int    `yaml:"worker_port_lo" json:"worker_port_lo"`
	WorkerPortHi   int    `yaml:"worker_port_hi" json:"worker_port_hi"`
	Listen         string `yaml:"listen" json:"listen"`
	EdgePortHeader string `yaml:"edge_port_header" json:"edge_port_header"`
	MaxHeaderBytes int    `yaml:"max_header_bytes" json:"max_header_bytes"`
	MaxSessions    int    `yaml:"max_sessions" json:"max_sessions"`
	AigwBaseURL    string `yaml:"aigw_base_url" json:"aigw_base_url"`
	// ImageRequestMaxBytes is the accumulated base64 image payload a tenant's dsh may put in
	// one request to aigw, rendered onto the aigw provider route when any of its models
	// accepts images (M68). It must stay below aigw's own `server.max_body_bytes` (10 MiB by
	// default): the gateway reads a bounded body, so an oversized request is truncated into a
	// parse error instead of being refused with a message about its size. dsh prunes the
	// oldest images to fit, which is why this is a bound it can act on rather than a limit.
	ImageRequestMaxBytes int      `yaml:"image_request_max_bytes" json:"image_request_max_bytes"`
	ValidateTimeout      Duration `yaml:"validate_timeout" json:"validate_timeout"`
	SessionTTL           Duration `yaml:"session_ttl" json:"session_ttl"`
	KeyRevalidate        string   `yaml:"key_revalidate" json:"key_revalidate"`
	DSHEnforce           string   `yaml:"dsh_enforce" json:"dsh_enforce"`
	// AdminSocket is the UNIX socket the root admin-serve listens on (M52 provisioning
	// channel). Empty disables the command: the daemon never starts by accident.
	AdminSocket string `yaml:"admin_socket" json:"admin_socket"`
	// AdminAllowedUIDs are the peer UIDs (typically the aigw runtime user) that may talk
	// to the admin socket. UID 0 is always allowed on the local machine.
	AdminAllowedUIDs []int      `yaml:"admin_allowed_uids" json:"admin_allowed_uids"`
	LoginRate        RateLimit  `yaml:"login_rate" json:"login_rate"`
	DirectoryPicker  string     `yaml:"directory_picker" json:"directory_picker"`
	PluginBrowserFS  string     `yaml:"plugin_browser_fs" json:"plugin_browser_fs"`
	WorkspaceSeed    []string   `yaml:"workspace_seed" json:"workspace_seed"`
	ReservedNames    []string   `yaml:"reserved_names" json:"reserved_names"`
	Dsh              DshRuntime `yaml:"dsh" json:"dsh"`
	// PublicBaseURL/scheme+host of the single public entry (e.g.
	// "https://chat.example"). Empty means tenants are addressed by port.
	PublicBaseURL    string `yaml:"public_base_url" json:"public_base_url"`
	TenantPathPrefix string `yaml:"tenant_path_prefix" json:"tenant_path_prefix"`
	PortalPathPrefix string `yaml:"portal_path_prefix" json:"portal_path_prefix"`
	// PublicScheme is how browsers actually reach the public surface: "auto"
	// (default) derives it from public_base_url in path mode and presumes https in
	// port mode, while "http"/"https" state it explicitly.
	//
	// Port mode used to hardcode https in every URL it generated. On a plain-HTTP
	// deployment that turns every redirect into a request to a port nobody serves,
	// which looks exactly like "the button does nothing".
	PublicScheme string `yaml:"public_scheme" json:"public_scheme"`
	// NoStoreAPIs forces "do not store" on every API response dshgw returns. It
	// defaults to true: dsh's API answers carry no cache directives at all, so a
	// shared cache in front (an nginx, a corporate proxy) or a browser is free to
	// reuse them — and every one of those answers belongs to one authenticated
	// tenant, which makes a reused answer a wrong answer.
	NoStoreAPIs *bool `yaml:"no_store_apis" json:"no_store_apis"`
	// SettingsUI decides whether a tenant's dsh settings/models panel is usable from
	// a page the browser does not consider loopback.
	//
	// dsh gates that panel on `transport?.ownsHost === true || isLoopback(page)`:
	// on any non-loopback page the settings scope stays "unavailable", the mirror
	// never loads, and the panel reports "settings are unavailable in this browser".
	// dshgw serves the tenant UI, so it can declare the transport it fronts — the
	// same patch the pre-existing deployment applied in its nginx config.
	//
	// "lan" (default) makes the panel work on the public host; "loopback" keeps
	// dsh's own behaviour. The confused-deputy cases the gate defends against
	// (DNS rebinding, cross-site requests) are already handled in front of it:
	// dshgw refuses any Host other than public_host and any unsafe request whose
	// Origin is not the tenant's own.
	SettingsUI string `yaml:"settings_ui" json:"settings_ui"`
	// SessionCookieSecure controls the session cookie's Secure attribute:
	// "auto" (default) sets it whenever the deployment is actually HTTPS, "always"
	// and "never" override that.
	//
	// It exists because a Secure cookie is *rejected* by browsers on a plain-HTTP
	// origin (localhost excepted): a deployment served over http:// would issue a
	// session the browser silently throws away, and the user would land back on the
	// portal after a successful login instead of inside dsh.
	SessionCookieSecure string `yaml:"session_cookie_secure" json:"session_cookie_secure"`
	// Feishu enables signing in with a Feishu identity instead of pasting an API key (M61).
	//
	// This gateway never talks to Feishu and holds no Feishu credential: aigw owns the
	// application, the secret and the registered redirect URL, and after it has identified
	// the person it hands over a short-lived signed ticket. Here we only verify that ticket
	// — which is why enabling this needs no app id, no secret and no second callback URL.
	Feishu            Feishu            `yaml:"feishu" json:"feishu"`
	WorkerLimits      WorkerLimits      `yaml:"worker_limits" json:"worker_limits"`
	TLS               TLSConfig         `yaml:"tls" json:"tls"`
	Deploy            DeployConfig      `yaml:"deploy" json:"deploy"`
	SSHWorkspaces     SSHWorkspaces     `yaml:"ssh_workspaces" json:"ssh_workspaces"`
	HostShares        HostShares        `yaml:"host_shares" json:"host_shares"`
	BrowserWorkspaces BrowserWorkspaces `yaml:"browser_workspaces" json:"browser_workspaces"`
	AccountCard       AccountCard       `yaml:"account_card" json:"account_card"`
	TenantRoot        string            `yaml:"tenant_root" json:"tenant_root"`
	WorkspaceRoot     string            `yaml:"workspace_root" json:"workspace_root"`
	HandshakeDir      string            `yaml:"handshake_dir" json:"handshake_dir"`
	StateDir          string            `yaml:"state_dir" json:"state_dir"`
	RegistryPath      string            `yaml:"registry_path" json:"registry_path"`
	KeyMapPath        string            `yaml:"key_map_path" json:"key_map_path"`
	SessionPath       string            `yaml:"session_path" json:"session_path"`
	AuditPath         string            `yaml:"audit_path" json:"audit_path"`
	ActivityPath      string            `yaml:"activity_path" json:"activity_path"`

	tenantMu    sync.RWMutex
	tenantPorts map[string]int
	portTenants map[int]string
}

// BrowserWorkspaces enables browser-backed mounts under the fixed browser subdirectory.
type BrowserWorkspaces struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// AccountCard renders the sidebar's identity row (M67): who is signed in, and a button that
// signs them out and returns them to the portal.
//
// It needs two things from the gateway rather than from the tenant: the account and Feishu
// names, which only aigw knows, and the logout, which only the session store can perform.
// That is why this is a gateway feature with a switch rather than something a plugin could
// decide on its own — and why flipping it off removes both the row and the routes it calls.
type AccountCard struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

func defaults() Config {
	return Config{
		PublicHost:           "chat.tirisen.hk",
		PortalPort:           32600,
		TenantPortLo:         32601,
		TenantPortHi:         32799,
		WorkerPortLo:         32100,
		WorkerPortHi:         32299,
		Listen:               "127.0.0.1:3099",
		EdgePortHeader:       "X-DSHGW-Port",
		MaxHeaderBytes:       128 << 10,
		MaxSessions:          10000,
		AigwBaseURL:          "http://192.168.190.86:8088",
		ImageRequestMaxBytes: defaultImageRequestMaxBytes,
		ValidateTimeout:      Duration(5 * time.Second),
		SessionTTL:           Duration(7 * 24 * time.Hour),
		KeyRevalidate:        "off",
		SessionCookieSecure:  "auto",
		SettingsUI:           "lan",
		PublicScheme:         "auto",
		DSHEnforce:           "login",
		LoginRate:            RateLimit{Requests: 10, Window: Duration(time.Minute)},
		DirectoryPicker:      "clamp",
		PluginBrowserFS:      "on",
		WorkspaceSeed:        []string{"work"},
		ReservedNames:        []string{"login", "dshgw"},
		SSHWorkspaces: SSHWorkspaces{
			MountSubdir:    "ssh",
			ConnectTimeout: Duration(10 * time.Second),
			PollInterval:   Duration(2 * time.Second),
			MaxEntries:     1000,
			// ServerAlive* keeps a dropped link from looking like a healthy mount; idmap=user
			// maps the remote account onto this one, which is what the tenant expects to see
			// for the files it creates. max_conns is not sshfs' default of 1: one mount serves
			// every session of an account, and a single sftp channel makes them queue behind
			// each other's slowest reader.
			SSHFSOptions: []string{"reconnect", "ServerAliveInterval=15", "ServerAliveCountMax=3", "idmap=user", "max_conns=4"},
		},
		// Off, and with a container name that does not collide with the ssh one. A share is a
		// grant over the host's own file system, so it is declared, never inferred.
		HostShares: HostShares{Subdir: "host"},
		Dsh: DshRuntime{
			// Empty means "ask the environment": DSHGW_NODE / DSHGW_DSH_ROOT, the same
			// rule aigw's supervised shape uses. The old defaults pointed at /opt/dsh,
			// the layout of the deleted root install, so an unconfigured host silently
			// got a path that only existed on the machine this repository grew up on.
			NodeBin:     "",
			BinJS:       "",
			CurrentLink: "",
		},
		// Every stateful path below is derived from the data root in applyDerivedDefaults.
		StateDir: defaultDataRoot + "/dshgw",
		Deploy: DeployConfig{
			// No default: the picker plugin ships with the repository
			// (cmd/dshgw/plugin/picker-clamp.js), so an operator names it — silently
			// inheriting /opt/dshgw/share/dsh-plugin/picker-clamp.js produced a
			// file:// URL pointing at a path that may not exist (M63).
			PluginPath:   "",
			ConfigPath:   DefaultPath,
			GatewayUser:  "dshgw",
			BwrapBin:     "/usr/bin/bwrap",
			PublicListen: "127.0.0.1",
		},
	}
}

// Load strictly decodes one YAML document, applies safe defaults, and validates it.
func Load(path string) (*Config, error) {
	f, err := openConfig(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("dshgw config %s exceeds 1 MiB", path)
	}

	cfg := defaults()
	// A removed key must be reported as a rename, not as "field not found" from the strict
	// decoder below: what it named (the deployment account's own ~/.ssh/config) is exactly
	// what this version forbids, so the error has to say what replaces it.
	if err := rejectRemovedSSHConfigSource(data); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	if err := rejectRemovedIdentitySource(data); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("dshgw config %s: multiple YAML documents are not allowed", path)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("dshgw config %s: trailing YAML: %w", path, err)
	}
	cfg.applyDerivedDefaults()
	if err := cfg.resolvePaths(); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	return &cfg, nil
}

// osOpen is split out only to keep the strict decoder easy to unit-test through Load.
var openConfig = func(path string) (file, error) { return openOSFile(path) }

// rejectRemovedSSHConfigSource reports the one key this feature removed.
//
// ssh_config_source was a single file copied into every account, and the deployment pointed
// it at the operator's own ~/.ssh/config — which gave every tenant the operator's whole host
// inventory. There is no host-wide source any more: ssh_config_dir holds one file per
// account. Malformed YAML is left to the strict decoder, which reports it better.
func rejectRemovedSSHConfigSource(data []byte) error {
	var legacy struct {
		SSHWorkspaces struct {
			SSHConfigSource string `yaml:"ssh_config_source"`
		} `yaml:"ssh_workspaces"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return nil
	}
	if strings.TrimSpace(legacy.SSHWorkspaces.SSHConfigSource) == "" {
		return nil
	}
	return errors.New("ssh_workspaces.ssh_config_source was removed: one host-wide file handed every account the same alias list, and the deployment account's ~/.ssh is never a tenant source. Use ssh_workspaces.ssh_config_dir with one file per account (<dir>/<account>) instead")
}

// rejectRemovedIdentitySource reports the other key this feature removed.
//
// identity_source was one private key copied into every account that had none. In this
// deployment it was pointed at the deployment account's own ~/.ssh/id_rsa, so every tenant
// held a byte-identical copy of the operator's personal key — a key that is authorised on the
// gateway host itself, which made "a tenant can read its own key" a way out of the tenant
// sandbox and into the deployment account. Nothing replaces "one key for everyone": the source
// of a tenant identity is either the account's own upload or identity_dir/<account>, one key
// per account. Malformed YAML is left to the strict decoder, which reports it better.
func rejectRemovedIdentitySource(data []byte) error {
	var legacy struct {
		SSHWorkspaces struct {
			IdentitySource string `yaml:"identity_source"`
		} `yaml:"ssh_workspaces"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return nil
	}
	if strings.TrimSpace(legacy.SSHWorkspaces.IdentitySource) == "" {
		return nil
	}
	return errors.New("ssh_workspaces.identity_source was removed: one shared key made every account reach everything that key could reach, and a deployment that pointed it at the operator's own ~/.ssh/id_rsa gave every tenant the operator's personal key. Use ssh_workspaces.identity_dir with one key per account (<dir>/<account>), or leave both empty and let each account upload its own identity")
}

type file interface {
	Read([]byte) (int, error)
	Close() error
}

func (c *Config) applyDerivedDefaults() {
	if c.PublicBaseURL != "" {
		c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
		if c.TenantPathPrefix == "" {
			c.TenantPathPrefix = "/t"
		}
		if c.PortalPathPrefix == "" {
			c.PortalPathPrefix = "/dshgw"
		}
	}
	// The bwrap mode's shared worker account is optional in configuration: the
	// documented practice is to reuse the gateway account rather than create a
	// second service account, and the installed worker unit names that account
	// literally. Deriving it here keeps one source of truth for the identity.
	if c.Deploy.WorkerUser == "" {
		c.Deploy.WorkerUser = c.Deploy.GatewayUser
	}
	// The per-tenant configuration (today: the gateway's copy of the tenant key) lives
	// under the state directory. It cannot stay at /etc/dshgw/tenants: that path is
	// root-owned in a host installation, and this shape runs as an ordinary account.
	// It is still a separate directory from the tenant's own .dsh so the sandbox never
	// mounts it (see internal/dshgw/sandbox) — which is why it must NOT share the tenant
	// root either: gateway.key inside <tenant_root>/<t> would be inside the tree the
	// worker binds.
	if c.Deploy.TenantConfigRoot == "" {
		c.Deploy.TenantConfigRoot = filepath.Join(c.StateDir, "tenant-config")
	}
	if c.RegistryPath == "" {
		c.RegistryPath = filepath.Join(c.StateDir, "registry.json")
	}
	if c.KeyMapPath == "" {
		c.KeyMapPath = filepath.Join(c.StateDir, "keys.map")
	}
	if c.SessionPath == "" {
		c.SessionPath = filepath.Join(c.StateDir, "gateway", "sessions.json")
	}
	if c.AuditPath == "" {
		c.AuditPath = filepath.Join(c.StateDir, "gateway", "audit.jsonl")
	}
	if c.ActivityPath == "" {
		c.ActivityPath = filepath.Join(c.StateDir, "gateway", "activity.json")
	}
	if c.HandshakeDir == "" {
		c.HandshakeDir = filepath.Join(c.StateDir, "handshake")
	}
	// The provisioning channel's socket sits beside the state it provisions, at the same
	// derived location the supervised shape uses (cmd/aigw derives <state>/admin.sock too),
	// so a standalone deployment and a console talking to it agree without configuration.
	// Leaving it empty silently disabled the channel: `serve` only binds a socket when one
	// is configured.
	if c.AdminSocket == "" {
		c.AdminSocket = filepath.Join(c.StateDir, "admin.sock")
	}
	if c.TenantRoot == "" {
		c.TenantRoot = filepath.Join(c.StateDir, "tenants")
	}
	// The remaining state roots hang off StateDir too, so one directory holds the whole
	// standalone deployment (M63). TemplateHome is a state directory rather than an
	// installed asset: prepare-template.sh builds it (with npm) into the data root, and
	// a default under /opt/dshgw pointed at a layout that no longer exists.
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = filepath.Join(c.StateDir, "workspaces")
	}
	if c.Deploy.TemplateHome == "" {
		c.Deploy.TemplateHome = filepath.Join(c.StateDir, "template-home")
	}
	if c.Deploy.BackupDir == "" {
		c.Deploy.BackupDir = filepath.Join(c.StateDir, "backups")
	}
	// The dsh runtime is an installation, not data: take it from the environment when
	// the file does not name it, exactly as aigw's supervised shape does through
	// cmd/aigw/dshgw_child.go. bin_js follows a named release directory.
	if c.Dsh.NodeBin == "" {
		c.Dsh.NodeBin = strings.TrimSpace(os.Getenv("DSHGW_NODE"))
	}
	if c.Dsh.CurrentLink == "" {
		c.Dsh.CurrentLink = strings.TrimSpace(os.Getenv("DSHGW_DSH_ROOT"))
	}
	if c.Dsh.BinJS == "" && c.Dsh.CurrentLink != "" {
		c.Dsh.BinJS = filepath.Join(c.Dsh.CurrentLink, "lib", "bin.js")
	}
}

// resolvePaths turns every configured path into the absolute form the rest of dshgw
// works with.
//
// Relative paths are resolved against the process working directory — the deployment
// root, pinned by the user unit's WorkingDirectory and by scripts/local-run.sh's cd —
// so a configuration may say ./data/dshgw and mean the directory the documentation
// describes. Validation still rejects unclean paths afterwards: this step must not
// become a way to smuggle ".." or a newline past the checks below.
func (c *Config) resolvePaths() error {
	targets := []struct {
		label  string
		target *string
	}{
		{"state_dir", &c.StateDir},
		{"admin_socket", &c.AdminSocket},
		{"tenant_root", &c.TenantRoot},
		{"workspace_root", &c.WorkspaceRoot},
		{"handshake_dir", &c.HandshakeDir},
		{"registry_path", &c.RegistryPath},
		{"key_map_path", &c.KeyMapPath},
		{"session_path", &c.SessionPath},
		{"audit_path", &c.AuditPath},
		{"activity_path", &c.ActivityPath},
		{"dsh.node_bin", &c.Dsh.NodeBin},
		{"dsh.bin_js", &c.Dsh.BinJS},
		{"dsh.current_link", &c.Dsh.CurrentLink},
		{"deploy.plugin_path", &c.Deploy.PluginPath},
		{"deploy.template_home", &c.Deploy.TemplateHome},
		{"deploy.backup_dir", &c.Deploy.BackupDir},
		{"deploy.tenant_config_root", &c.Deploy.TenantConfigRoot},
		{"deploy.config_path", &c.Deploy.ConfigPath},
		{"deploy.bwrap_bin", &c.Deploy.BwrapBin},
		{"ssh_workspaces.ssh_bin", &c.SSHWorkspaces.SSHBin},
		{"ssh_workspaces.sshfs_bin", &c.SSHWorkspaces.SSHFSBin},
		{"ssh_workspaces.identity_dir", &c.SSHWorkspaces.IdentityDir},
		{"ssh_workspaces.ssh_config_dir", &c.SSHWorkspaces.SSHConfigDir},
	}
	for _, item := range targets {
		if *item.target == "" || filepath.IsAbs(*item.target) {
			continue
		}
		// A relative path may point deeper into the deployment, never back out of it:
		// silently cleaning ".." would let ./data/../elsewhere land outside the data root
		// that the rest of this file promises. Writing an absolute path is the escape hatch.
		if hasParentElement(*item.target) {
			return fmt.Errorf("%s %q must not contain \"..\"; use an absolute path to leave the deployment root", item.label, *item.target)
		}
		abs, err := filepath.Abs(*item.target)
		if err != nil {
			return fmt.Errorf("%s %q is not a usable relative path: %w", item.label, *item.target, err)
		}
		*item.target = abs
	}
	return nil
}

// hasParentElement reports whether any element of a relative path is "..".
func hasParentElement(path string) bool {
	for _, elem := range strings.Split(filepath.ToSlash(path), "/") {
		if elem == ".." {
			return true
		}
	}
	return false
}

// Validate rejects ambiguous, externally exposed, or overlapping configurations.
func (c *Config) Validate() error {
	if len(c.PublicHost) > 253 || !publicHostRE.MatchString(c.PublicHost) {
		return errors.New("public_host must be one lowercase DNS hostname without a scheme, port, or path")
	}
	for label, p := range map[string]int{"portal_port": c.PortalPort, "tenant_port_lo": c.TenantPortLo, "tenant_port_hi": c.TenantPortHi, "worker_port_lo": c.WorkerPortLo, "worker_port_hi": c.WorkerPortHi} {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%s must be between 1 and 65535", label)
		}
	}
	if c.TenantPortLo > c.TenantPortHi || c.WorkerPortLo > c.WorkerPortHi {
		return errors.New("port range low bound exceeds high bound")
	}
	if c.PortalPort >= c.TenantPortLo && c.PortalPort <= c.TenantPortHi {
		return errors.New("portal_port overlaps the tenant public port range")
	}
	if rangesOverlap(c.TenantPortLo, c.TenantPortHi, c.WorkerPortLo, c.WorkerPortHi) || c.PortalPort >= c.WorkerPortLo && c.PortalPort <= c.WorkerPortHi {
		return errors.New("public and worker port ranges must not overlap")
	}
	host, rawListenPort, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen must use a loopback IP address")
	}
	listenPort, err := strconv.Atoi(rawListenPort)
	if err != nil || listenPort < 1 || listenPort > 65535 || strconv.Itoa(listenPort) != rawListenPort {
		return errors.New("listen must contain a numeric port between 1 and 65535")
	}
	if listenPort == c.PortalPort || listenPort >= c.TenantPortLo && listenPort <= c.TenantPortHi || listenPort >= c.WorkerPortLo && listenPort <= c.WorkerPortHi {
		return errors.New("listen port overlaps a public or worker port")
	}
	header := strings.ToLower(c.EdgePortHeader)
	if !edgeHeaderRE.MatchString(header) || strings.HasPrefix(header, "x-forwarded-") || header == "x-real-ip" || header == "x-api-key" {
		return errors.New("edge_port_header must be a distinct custom X- header using letters, digits and hyphens")
	}
	if c.MaxHeaderBytes < 8192 || c.MaxHeaderBytes > 1<<20 {
		return errors.New("max_header_bytes must be between 8192 and 1048576")
	}
	if c.MaxSessions < 1 || c.MaxSessions > 1_000_000 {
		return errors.New("max_sessions must be between 1 and 1000000")
	}
	u, err := url.Parse(c.AigwBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("aigw_base_url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	c.AigwBaseURL = strings.TrimRight(c.AigwBaseURL, "/")
	if c.ImageRequestMaxBytes == 0 {
		c.ImageRequestMaxBytes = defaultImageRequestMaxBytes
	}
	if c.ImageRequestMaxBytes < 1<<20 {
		return errors.New("image_request_max_bytes must be at least 1048576 (1 MiB)")
	}
	if c.ValidateTimeout.Duration() <= 0 || c.SessionTTL.Duration() <= 0 {
		return errors.New("validate_timeout and session_ttl must be positive")
	}
	if _, err := ParseRevalidate(c.KeyRevalidate); err != nil {
		return err
	}
	if _, err := ParseDSHEnforce(c.DSHEnforce); err != nil {
		return err
	}
	if c.LoginRate.Requests < 1 || c.LoginRate.Window.Duration() <= 0 {
		return errors.New("login_rate requests and window must be positive")
	}
	if c.DirectoryPicker != "clamp" && c.DirectoryPicker != "browse" {
		return errors.New(`directory_picker must be "clamp" or "browse"`)
	}
	// The clamp picker is a plugin the worker imports by absolute file URL. Without a
	// path the rendered tenant profile would carry a file:// URL pointing nowhere, and
	// the failure would surface as a broken directory picker inside a tenant session
	// instead of a configuration error on the host (M63).
	if c.DirectoryPicker == "clamp" && strings.TrimSpace(c.Deploy.PluginPath) == "" {
		return errors.New(`deploy.plugin_path is required when directory_picker is "clamp" (name the picker plugin, e.g. ./cmd/dshgw/plugin/picker-clamp.js)`)
	}
	if c.PluginBrowserFS != "on" && c.PluginBrowserFS != "off" {
		return errors.New(`plugin_browser_fs must be "on" or "off"`)
	}
	for _, name := range c.ReservedNames {
		if !ValidTenantName(name) {
			return fmt.Errorf("reserved tenant name %q is invalid", name)
		}
	}
	for _, seed := range c.WorkspaceSeed {
		if seed == "" || filepath.IsAbs(seed) || filepath.Clean(seed) != seed || seed == "." || strings.HasPrefix(seed, ".."+string(filepath.Separator)) || seed == ".." {
			return fmt.Errorf("workspace_seed %q must be a clean relative path inside the tenant root", seed)
		}
	}
	if c.WorkerLimits.MemoryHighBytes < 0 || c.WorkerLimits.MemoryMaxBytes < 0 || c.WorkerLimits.TasksMax < 0 || c.WorkerLimits.CPUQuotaPercent < 0 {
		return errors.New("worker_limits values must not be negative")
	}
	if c.WorkerLimits.MemoryHighBytes > 0 && c.WorkerLimits.MemoryMaxBytes > 0 && c.WorkerLimits.MemoryHighBytes > c.WorkerLimits.MemoryMaxBytes {
		return errors.New("worker_limits.memory_high_bytes must not exceed memory_max_bytes")
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	switch c.PublicScheme {
	case "", "auto", "http", "https":
	default:
		return fmt.Errorf("public_scheme must be auto, http or https (got %q)", c.PublicScheme)
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	switch c.SessionCookieSecure {
	case "", "auto", "always", "never":
	default:
		return fmt.Errorf("session_cookie_secure must be auto, always or never (got %q)", c.SessionCookieSecure)
	}
	if err := c.validateFeishu(); err != nil {
		return err
	}
	if c.BrowserWorkspaces.Enabled {
		if strings.TrimSpace(c.Deploy.PluginPath) == "" {
			return errors.New("browser_workspaces.enabled requires deploy.plugin_path")
		}
		for _, seed := range c.WorkspaceSeed {
			if seed == "browser" {
				return errors.New("browser workspace directory collides with workspace_seed")
			}
		}
		if c.SSHWorkspaces.Enabled && c.SSHWorkspaces.MountSubdir == "browser" {
			return errors.New("browser workspace directory collides with ssh_workspaces.mount_subdir")
		}
	}
	if err := c.validateSSHWorkspaces(); err != nil {
		return err
	}
	if err := c.validateHostShares(); err != nil {
		return err
	}
	if c.AccountCard.Enabled && strings.TrimSpace(c.Deploy.PluginPath) == "" {
		// The row is a client plugin shipped beside the picker (deploy.plugin_path names the
		// plugin directory's sibling), so without it the switch would turn on two routes and
		// no visible row — a silent half-configuration.
		return errors.New("account_card.enabled requires deploy.plugin_path")
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	switch c.PublicScheme {
	case "", "auto", "http", "https":
	default:
		return fmt.Errorf("public_scheme must be auto, http or https (got %q)", c.PublicScheme)
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	if c.PublicBaseURL != "" {
		parsed, err := url.Parse(c.PublicBaseURL)
		if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return errors.New("public_base_url must be a scheme://host URL without a path")
		}
		tenantPrefix, portalPrefix := normalizePathPrefix(c.TenantPathPrefix), normalizePathPrefix(c.PortalPathPrefix)
		if tenantPrefix == "/" || portalPrefix == "/" {
			return errors.New("tenant_path_prefix and portal_path_prefix must not be the root path")
		}
		if tenantPrefix == portalPrefix || strings.HasPrefix(tenantPrefix, portalPrefix+"/") || strings.HasPrefix(portalPrefix, tenantPrefix+"/") {
			return errors.New("tenant_path_prefix and portal_path_prefix must be distinct and must not overlap")
		}
	}
	if net.ParseIP(c.Deploy.PublicListen) == nil {
		return errors.New("deploy.public_listen must be one IP address")
	}
	if (c.TLS.Certificate == "") != (c.TLS.CertificateKey == "") {
		return errors.New("tls.certificate and tls.certificate_key must be set together")
	}
	for label, value := range map[string]string{"tls.certificate": c.TLS.Certificate, "tls.certificate_key": c.TLS.CertificateKey} {
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be a clean absolute path", label)
		}
	}
	paths := map[string]string{
		"tenant_root": c.TenantRoot, "workspace_root": c.WorkspaceRoot, "handshake_dir": c.HandshakeDir,
		"state_dir": c.StateDir, "registry_path": c.RegistryPath, "key_map_path": c.KeyMapPath,
		"session_path": c.SessionPath, "audit_path": c.AuditPath, "activity_path": c.ActivityPath,
		"admin_socket":              c.AdminSocket,
		"deploy.template_home":      c.Deploy.TemplateHome,
		"deploy.backup_dir":         c.Deploy.BackupDir,
		"deploy.tenant_config_root": c.Deploy.TenantConfigRoot,
		"deploy.config_path":        c.Deploy.ConfigPath,
		"deploy.bwrap_bin":          c.Deploy.BwrapBin,
	}
	for label, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%s must be a clean absolute path", label)
		}
		if strings.ContainsAny(p, "\r\n;{}") {
			return fmt.Errorf("%s contains unsafe configuration characters", label)
		}
	}
	// These four may legitimately be empty, and each has an owner for that case:
	//   * the dsh runtime trio — `dshgw doctor` reports dsh-node/dsh-bin-js/dsh-current-symlink
	//     as failing preconditions, and the contract check refuses an empty runtime. Failing
	//     at config load instead would turn a diagnosable "not installed yet" into a config
	//     error, and `doctor` — the command whose job is that diagnosis — could not even run.
	//   * deploy.plugin_path — only the clamp picker imports it; the check below requires it
	//     in exactly that case.
	// A value that IS given must still be a clean absolute path.
	optional := map[string]string{
		"dsh.node_bin": c.Dsh.NodeBin, "dsh.bin_js": c.Dsh.BinJS, "dsh.current_link": c.Dsh.CurrentLink,
		"deploy.plugin_path": c.Deploy.PluginPath,
	}
	for label, p := range optional {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%s must be a clean absolute path", label)
		}
		if strings.ContainsAny(p, "\r\n;{}") {
			return fmt.Errorf("%s contains unsafe configuration characters", label)
		}
	}
	for label, root := range map[string]string{"tenant_root": c.TenantRoot, "workspace_root": c.WorkspaceRoot, "state_dir": c.StateDir} {
		if root == "/" {
			return fmt.Errorf("%s must not be filesystem root", label)
		}
	}
	for label, account := range map[string]string{"deploy.gateway_user": c.Deploy.GatewayUser, "deploy.worker_user": c.Deploy.WorkerUser} {
		if !accountNameRE.MatchString(account) || len(account) > 32 {
			return fmt.Errorf("%s must be a valid OS account name", label)
		}
	}
	return nil
}

func rangesOverlap(aLo, aHi, bLo, bHi int) bool { return aLo <= bHi && bLo <= aHi }

// validateFeishu checks the identity handoff block. As everywhere else in this file, the
// checks run only while the feature is on: a deployment that does not use Feishu must not be
// stopped from starting by values it never reads.
func (c *Config) validateFeishu() error {
	if !c.Feishu.Enabled {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(c.Feishu.AigwLoginURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("feishu.aigw_login_url must be the absolute URL of aigw's /feishu/login")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("feishu.aigw_login_url scheme %q must be http or https", parsed.Scheme)
	}
	if !strings.HasSuffix(parsed.Path, "/feishu/login") {
		return fmt.Errorf("feishu.aigw_login_url must point at /feishu/login (got %q)", parsed.Path)
	}
	if strings.TrimSpace(c.Feishu.TicketSecret) == "" {
		return errors.New("feishu.ticket_secret must be set: it is what proves a login ticket came from this deployment's aigw")
	}
	// A portal that is served over plain HTTP cannot issue a Secure cookie: the browser drops
	// it and the person lands back on the portal with no error. Saying so here turns a
	// confusing runtime symptom into a startup failure.
	if !strings.HasPrefix(strings.TrimSpace(c.Feishu.AigwLoginURL), "https://") && c.SecureSessionCookie() {
		return errors.New("feishu is enabled on a plain-HTTP deployment but the session cookie would be Secure: set public_scheme: http (or session_cookie_secure: never)")
	}
	return nil
}

// sshMountSubdirRE is the one shape the mount container may take: a visible single segment.
var sshMountSubdirRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// sshHostSpecRE mirrors the tenant-facing plugin's rule: an ssh alias or user@host, never
// something that could be read as an option or a path.
var sshHostSpecRE = regexp.MustCompile(`^[A-Za-z0-9._@][A-Za-z0-9._@:-]{0,254}$`)

// validateSSHWorkspaces checks the SSH-workspace surface. The mount container is validated
// even while the feature is off, so a configuration that would break the account's picker is
// rejected when it is written rather than on the day the feature is switched on.
func (c *Config) validateSSHWorkspaces() error {
	ssh := &c.SSHWorkspaces
	if !sshMountSubdirRE.MatchString(ssh.MountSubdir) {
		return fmt.Errorf("ssh_workspaces.mount_subdir %q must be one visible path segment", ssh.MountSubdir)
	}
	for _, seed := range c.WorkspaceSeed {
		if seed == ssh.MountSubdir {
			return fmt.Errorf("ssh_workspaces.mount_subdir %q collides with a workspace_seed name", ssh.MountSubdir)
		}
	}
	if !ssh.Enabled {
		return nil
	}
	if ssh.ConnectTimeout.Duration() <= 0 {
		return errors.New("ssh_workspaces.connect_timeout must be positive")
	}
	if ssh.PollInterval.Duration() <= 0 {
		return errors.New("ssh_workspaces.poll_interval must be positive")
	}
	if ssh.MaxEntries < 1 || ssh.MaxEntries > 100000 {
		return errors.New("ssh_workspaces.max_entries must be between 1 and 100000")
	}
	// Key sources are optional: accounts may upload their own identities in the UI.
	if ssh.IdentityDir != "" {
		info, err := os.Stat(ssh.IdentityDir)
		if err != nil {
			return fmt.Errorf("ssh_workspaces.identity_dir: %w", err)
		}
		if !info.IsDir() {
			return errors.New("ssh_workspaces.identity_dir must be a directory holding one key per account")
		}
		if err := checkOutsideDeploymentSSH("ssh_workspaces.identity_dir", ssh.IdentityDir); err != nil {
			return err
		}
	}
	if ssh.SSHConfigDir != "" {
		info, err := os.Stat(ssh.SSHConfigDir)
		if err != nil {
			return fmt.Errorf("ssh_workspaces.ssh_config_dir: %w", err)
		}
		if !info.IsDir() {
			return errors.New("ssh_workspaces.ssh_config_dir must be a directory holding one alias list per account")
		}
		if info.Mode().Perm()&0o002 != 0 {
			return fmt.Errorf("ssh_workspaces.ssh_config_dir mode %04o is world writable", info.Mode().Perm())
		}
		if err := checkOutsideDeploymentSSH("ssh_workspaces.ssh_config_dir", ssh.SSHConfigDir); err != nil {
			return err
		}
	}
	for label, bin := range map[string]string{"ssh_bin": ssh.SSHBin, "sshfs_bin": ssh.SSHFSBin} {
		if bin == "" {
			continue
		}
		info, err := os.Stat(bin)
		if err != nil {
			return fmt.Errorf("ssh_workspaces.%s: %w", label, err)
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("ssh_workspaces.%s %s is not executable", label, bin)
		}
	}
	for _, host := range ssh.Hosts {
		if !sshHostSpecRE.MatchString(strings.TrimSpace(host)) {
			return fmt.Errorf("ssh_workspaces.hosts entry %q is not an ssh alias or user@host", host)
		}
	}
	return nil
}

// hostShareNameRE is the one shape a share name may take: a visible single path segment. A
// hidden one would be invisible in the account's own picker, and a name with a separator would
// escape the container.
var hostShareNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateHostShares checks the operator-declared host directories (M71).
//
// Two rules carry the weight here. The container must not collide with the ssh one, because
// both live directly under the tenant workspace and the account's picker treats every level as a
// directory. And a share must be disjoint from the state directory: that directory holds every
// tenant's workspace, DSH home, ssh key and session record, so a share that contains it (or sits
// inside it) would hand one tenant another tenant's data through a bind the sandbox is meant to
// make private. Everything else — which host directory, read-only or not, which accounts — is
// the operator's decision, stated explicitly.
func (c *Config) validateHostShares() error {
	shares := &c.HostShares
	if !sshMountSubdirRE.MatchString(shares.Subdir) {
		return fmt.Errorf("host_shares.subdir %q must be one visible path segment", shares.Subdir)
	}
	for _, seed := range c.WorkspaceSeed {
		if seed == shares.Subdir {
			return fmt.Errorf("host_shares.subdir %q collides with a workspace_seed name", shares.Subdir)
		}
	}
	if shares.Subdir == c.SSHWorkspaces.MountSubdir {
		return fmt.Errorf("host_shares.subdir %q collides with ssh_workspaces.mount_subdir", shares.Subdir)
	}
	if !shares.Enabled {
		if len(shares.Shares) > 0 {
			return errors.New("host_shares.shares is set but host_shares.enabled is not")
		}
		return nil
	}
	if len(shares.Shares) == 0 {
		return errors.New("host_shares.enabled requires at least one entry in host_shares.shares")
	}
	stateDir, err := filepath.Abs(c.StateDir)
	if err != nil {
		return fmt.Errorf("host_shares: resolving state_dir: %w", err)
	}
	seen := map[string]bool{}
	for _, share := range shares.Shares {
		label := "host_shares.shares[" + share.Name + "]"
		if !hostShareNameRE.MatchString(share.Name) {
			return fmt.Errorf("%s: name %q must be one visible path segment", label, share.Name)
		}
		if seen[share.Name] {
			return fmt.Errorf("%s: duplicate share name %q", label, share.Name)
		}
		seen[share.Name] = true
		if len(share.Tenants) == 0 {
			return fmt.Errorf("%s: tenants must name the accounts that may see it (an empty list is not \"everyone\")", label)
		}
		for _, tenant := range share.Tenants {
			if !ValidTenantName(tenant) {
				return fmt.Errorf("%s: tenants entry %q is not an account name", label, tenant)
			}
		}
		if strings.TrimSpace(share.Path) == "" || !filepath.IsAbs(share.Path) {
			return fmt.Errorf("%s: path must be an absolute host directory", label)
		}
		// Symlinks are resolved, so the two checks below cannot be walked around with one.
		resolved, err := filepath.EvalSymlinks(share.Path)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: %s is not a directory", label, resolved)
		}
		if resolved == string(filepath.Separator) {
			return fmt.Errorf("%s: refusing to share the file system root", label)
		}
		if withinPath(resolved, stateDir) || withinPath(stateDir, resolved) {
			return fmt.Errorf("%s: %s overlaps the state directory %s, which holds every account's workspace, DSH home and keys", label, resolved, stateDir)
		}
	}
	return nil
}

// withinPath reports whether child is root or sits inside it. Both paths are compared as given:
// callers resolve symlinks first, because a share that reaches the state directory through one
// is the same exposure.
func withinPath(root, child string) bool {
	root = filepath.Clean(root)
	child = filepath.Clean(child)
	if root == child {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkOutsideDeploymentSSH refuses a tenant source inside the deployment account's own ~/.ssh.
//
// Both keys it guards decide what every tenant gets from the operator: ssh_config_dir decides
// each account's alias list, identity_dir decides its key. Pointing either at the operator's
// personal ssh directory would hand every account the operator's own configuration or key
// material — the coupling these keys exist to remove, and the exact shape of the incident that
// removed identity_source. A directory that merely looks like it lives elsewhere does not
// qualify: symlinks are resolved before the comparison. Per-account files need no separate
// check here: they are read through securefile, which refuses a leaf or ancestor symlink
// outright.
func checkOutsideDeploymentSSH(label, dir string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Nothing to compare against. The remaining checks still apply.
		return nil
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolvedHome = home
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	sshDir := filepath.Join(resolvedHome, ".ssh")
	rel, err := filepath.Rel(sshDir, resolvedDir)
	if err != nil {
		return nil
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return fmt.Errorf("%s %s is inside the deployment account's own %s; the operator's ssh directory is never a tenant source", label, dir, sshDir)
	}
	return nil
}

// RevalidateMode is the parsed key-revalidation policy.
type RevalidateMode struct {
	PerRequest bool
	Interval   time.Duration
}

func ParseRevalidate(raw string) (RevalidateMode, error) {
	switch raw {
	case "off":
		return RevalidateMode{}, nil
	case "per-request":
		return RevalidateMode{PerRequest: true}, nil
	}
	const prefix = "interval:"
	if strings.HasPrefix(raw, prefix) {
		n, err := strconv.ParseInt(strings.TrimPrefix(raw, prefix), 10, 64)
		if err != nil || n < 1 || n > (1<<63-1)/int64(time.Second) {
			return RevalidateMode{}, errors.New("key_revalidate interval must be a positive number of seconds")
		}
		return RevalidateMode{Interval: time.Duration(n) * time.Second}, nil
	}
	return RevalidateMode{}, errors.New(`key_revalidate must be "off", "per-request", or "interval:<seconds>"`)
}

// DSHEnforceMode is the parsed dsh_enforce setting (M52): when the dshgw proxy consults
// aigw's /v1/dshgw/authorize beyond the mandatory login-time check. Zero value = login only.
type DSHEnforceMode struct {
	PerRequest bool
	Interval   time.Duration
}

// ParseDSHEnforce mirrors ParseRevalidate: "login" (default), "per-request", or
// "interval:<seconds>" for a cached near-immediate revocation. Anything else is a
// configuration error, so a typo can never silently disable the entitlement check.
func ParseDSHEnforce(raw string) (DSHEnforceMode, error) {
	switch raw {
	case "", "login":
		return DSHEnforceMode{}, nil
	case "per-request":
		return DSHEnforceMode{PerRequest: true}, nil
	}
	const prefix = "interval:"
	if strings.HasPrefix(raw, prefix) {
		n, err := strconv.ParseInt(strings.TrimPrefix(raw, prefix), 10, 64)
		if err != nil || n < 1 || n > (1<<63-1)/int64(time.Second) {
			return DSHEnforceMode{}, errors.New("dsh_enforce interval must be a positive number of seconds")
		}
		return DSHEnforceMode{Interval: time.Duration(n) * time.Second}, nil
	}
	return DSHEnforceMode{}, errors.New(`dsh_enforce must be "login", "per-request", or "interval:<seconds>"`)
}

func ValidTenantName(name string) bool { return tenantNameRE.MatchString(name) }

func (c *Config) IsReservedTenant(name string) bool {
	for _, v := range c.ReservedNames {
		if name == v {
			return true
		}
	}
	return false
}

// SetTenantPorts installs an immutable registry snapshot used by origin helpers.
func (c *Config) SetTenantPorts(ports map[string]int) {
	names := make(map[string]int, len(ports))
	reverse := make(map[int]string, len(ports))
	for name, port := range ports {
		names[name] = port
		reverse[port] = name
	}
	c.tenantMu.Lock()
	c.tenantPorts = names
	c.portTenants = reverse
	c.tenantMu.Unlock()
}

func (c *Config) TenantOrigin(tenant string) string {
	if c.PathMode() {
		return c.PublicBaseURL + normalizePathPrefix(c.TenantPathPrefix) + "/" + tenant + "/"
	}
	c.tenantMu.RLock()
	port, ok := c.tenantPorts[tenant]
	c.tenantMu.RUnlock()
	if !ok {
		return ""
	}
	return c.OriginForPort(port)
}

// StoreAPIData reports whether API responses may be stored by a cache. False is the
// default and means every API answer leaves with "no-store".
func (c *Config) StoreAPIData() bool { return c.NoStoreAPIs != nil && !*c.NoStoreAPIs }

// LANSettingsUI reports whether the tenant settings panel is enabled for pages the
// browser does not treat as loopback.
func (c *Config) LANSettingsUI() bool { return c.SettingsUI != "loopback" }

// EffectiveImageRequestMaxBytes is the image payload bound a rendered tenant profile
// carries (M68). Validate resolves it as well, but the renderer must not depend on
// validation having run: a config built in a test, or one whose field was left out, would
// otherwise render no bound at all and silently keep dsh's own 20 MiB default — the very
// value this deployment-wide setting exists to lower.
func (c *Config) EffectiveImageRequestMaxBytes() int {
	if c.ImageRequestMaxBytes > 0 {
		return c.ImageRequestMaxBytes
	}
	return defaultImageRequestMaxBytes
}

// Scheme is the scheme browsers use to reach this deployment.
func (c *Config) Scheme() string {
	switch c.PublicScheme {
	case "http", "https":
		return c.PublicScheme
	}
	if c.PathMode() && strings.HasPrefix(c.PublicBaseURL, "http://") {
		return "http"
	}
	return "https"
}

// SecureSessionCookie reports whether the session cookie should carry Secure. A
// browser refuses to store such a cookie on a plain-HTTP origin, so this must match
// how clients actually reach the gateway — not how it hopes to be reached.
func (c *Config) SecureSessionCookie() bool {
	switch c.SessionCookieSecure {
	case "always":
		return true
	case "never":
		return false
	}
	// The cookie has to match how the browser reaches the site, in both modes:
	// modern browsers refuse to store a Secure cookie on a plain-HTTP origin, so a
	// mismatch silently discards the session right after a successful login.
	return c.Scheme() == "https"
}

// PathMode reports whether public URLs are path-based instead of port-based.
func (c *Config) PathMode() bool { return c.PublicBaseURL != "" }

// PublicOrigin is the origin a browser sends in the Origin header in path mode:
// an origin never carries a path, so every surface of the single domain shares it.
func (c *Config) PublicOrigin() string { return c.PublicBaseURL }

// ExpectedOrigin is the Origin value a browser sends for one surface. In path mode
// that is always the base origin (browsers drop the path); in port mode it is the
// surface's own origin.
func (c *Config) ExpectedOrigin(port int) string {
	if c.PathMode() {
		return c.PublicOrigin()
	}
	return c.OriginForPort(port)
}

// PortalPath is the public path of the login page: the portal prefix in path mode,
// the root otherwise.
func (c *Config) PortalPath() string {
	if !c.PathMode() {
		return "/"
	}
	return normalizePathPrefix(c.PortalPathPrefix) + "/"
}

// SessionCookiePath binds a tenant's session cookie to that tenant's path. In path
// mode every tenant shares one origin, so without this a browser would happily
// attach alice's cookie to a request for /t/bob/: the cookie's path is the only
// thing that keeps two tenants' sessions apart inside one origin.
func (c *Config) SessionCookiePath(tenant string) string {
	if !c.PathMode() {
		return "/"
	}
	return normalizePathPrefix(c.TenantPathPrefix) + "/" + tenant + "/"
}

func (c *Config) OriginForPort(port int) string {
	if !c.PathMode() {
		return fmt.Sprintf("%s://%s:%d", c.Scheme(), c.PublicHost, port)
	}
	if port == c.PortalPort {
		return c.PublicBaseURL + normalizePathPrefix(c.PortalPathPrefix) + "/"
	}
	c.tenantMu.RLock()
	name, known := c.portTenants[port]
	c.tenantMu.RUnlock()
	if known {
		return c.TenantOrigin(name)
	}
	return c.PublicBaseURL + "/"
}

// normalizePathPrefix returns a leading-slash, no-trailing-slash prefix.
func normalizePathPrefix(prefix string) string {
	trimmed := "/" + strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed == "/" {
		return "/"
	}
	return trimmed
}

func (c *Config) TenantFromPort(port int) (string, bool) {
	c.tenantMu.RLock()
	defer c.tenantMu.RUnlock()
	name, ok := c.portTenants[port]
	return name, ok
}

func (c *Config) SessionCookieName(tenant string) string { return "dshgw_s_" + tenant }
func (c *Config) WithTrailingSlash(raw string) string    { return strings.TrimRight(raw, "/") + "/" }
