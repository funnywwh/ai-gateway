// Package nodeproto defines the control protocol between a dshgw control plane and its
// worker nodes (M77).
//
// It is deliberately tiny and dependency-free: one JSON envelope, one bearer token, one
// path prefix per major version, and a closed set of error codes. The control plane's
// tenant traffic rides the same listener under a second prefix, because that traffic has the
// same trust requirement (only the control plane may speak it) and one authenticated
// listener is easier to reason about than two.
//
// Wire shape:
//
//	GET  /node/v1/health                  -> Health
//	POST /node/v1/control/<op>            -> op-specific value
//	ANY  /node/v1/tenant/<path...>        -> the tenant's own response, forwarded verbatim
//
// Every request carries "Authorization: Bearer <token>" (the node's shared secret) and
// "X-Dshgw-Protocol: 1". Tenant traffic additionally carries "X-Dshgw-Tenant" naming which
// tenant to address; a node never infers the tenant from anything the browser sent.
package nodeproto

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
)

const (
	// Version is the protocol major version. It is part of every path, so two processes built
	// from different major versions cannot address each other's endpoints at all — the second
	// guard (HeaderProtocol, checked before any operation) exists so the failure is reported
	// as "your node speaks another protocol version, deploy again" instead of as a 404 that
	// looks like a broken deployment.
	Version = 1

	// PathPrefix is the root of every node endpoint.
	PathPrefix = "/node/v1"
	// HealthPath answers the node's self-description; it is what the control plane probes and
	// what an operator's curl sees.
	HealthPath = PathPrefix + "/health"
	// ControlPath is the prefix of control operations: ControlPath + <op>.
	ControlPath = PathPrefix + "/control/"
	// TenantPath is the prefix of forwarded tenant traffic: TenantPath + <the tenant's own path>.
	TenantPath = PathPrefix + "/tenant/"

	// HeaderProtocol carries the sender's protocol version.
	HeaderProtocol = "X-Dshgw-Protocol"
	// HeaderTenant names the tenant a forwarded request addresses.
	HeaderTenant = "X-Dshgw-Tenant"
	// HeaderBrowserSession carries the browser-session digest the control plane computed for
	// a browser-workspace long poll, so the node can serve the mount belonging to that
	// session. It is the node's only source for this value: the browser's own cookie never
	// reaches the node, exactly as it never reached the worker.
	HeaderBrowserSession = "X-Dshgw-Browser-Session"
	// HeaderError marks a tenant-path refusal the node produced. Tenant traffic is not
	// envelope-shaped — the answer goes to a browser, not to this protocol's client — so the
	// code travels in a header and the control plane turns it into its own message. Without it
	// the browser would read the node's internal wording ("worker is not running on node-a").
	HeaderError = "X-Dshgw-Error"

	// MaxControlBodyBytes bounds a control request body. Control bodies carry a tenant record
	// or a model list — kilobytes — so a megabyte is generous.
	MaxControlBodyBytes = 1 << 20
	// MaxTenantBodyBytes bounds a forwarded tenant request. It matches the control plane's own
	// replay buffer (proxy.maxReplayBody): past it, the request cannot be replayed after a
	// re-handshake anyway, so accepting more would only promise something we cannot keep.
	MaxTenantBodyBytes = 64 << 20
)

// ProtocolHeaderValue is what every request sends in HeaderProtocol.
const ProtocolHeaderValue = "1"

// Error codes. They are stable strings because they cross a process boundary and end up in
// audit lines, operator-facing messages and tests; the HTTP status that carries them is an
// implementation detail of this protocol.
const (
	// CodeUnreachable means the node could not be reached at all (dial, DNS, timeout).
	CodeUnreachable = "node_unreachable"
	// CodeAuthFailed means the node refused our token.
	CodeAuthFailed = "node_auth_failed"
	// CodeProtocolMismatch means the two sides disagree about the protocol version.
	CodeProtocolMismatch = "node_protocol_mismatch"
	// CodeTenantUnknown means the node does not host that tenant.
	CodeTenantUnknown = "tenant_unknown"
	// CodeWorkerNotRunning means the node hosts the tenant but its worker is not running.
	CodeWorkerNotRunning = "worker_not_running"
	// CodeBadRequest means the request was malformed (or the operation refused it).
	CodeBadRequest = "bad_request"
	// CodeNotImplemented means the operation exists in the protocol but not in this build.
	CodeNotImplemented = "not_implemented"
	// CodeInternal means the node failed while doing the work.
	CodeInternal = "internal"
)

// Error is a protocol error with a stable code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Errorf builds a protocol error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err (or anything it wraps) is a protocol error with this code.
func IsCode(err error, code string) bool {
	var proto *Error
	if errors.As(err, &proto) {
		return proto.Code == code
	}
	return false
}

// CodeOf returns the protocol error code of err, or "" when it is not a protocol error.
func CodeOf(err error) string {
	var proto *Error
	if errors.As(err, &proto) {
		return proto.Code
	}
	return ""
}

// Envelope is the one response shape of the control surface. Tenant traffic bypasses it: a
// tenant response is the worker's own bytes and is forwarded as they are.
type Envelope struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

// WriteValue answers with a value.
func WriteValue(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "cannot encode response")
		return
	}
	writeEnvelope(w, status, Envelope{OK: true, Value: raw})
}

// WriteTenantError answers a forwarded tenant request that the node refuses, in the shape that
// belongs to that path: a plain HTTP error carrying the protocol code in HeaderError. The control
// plane reads the code, logs it, and answers the browser with its own wording.
func WriteTenantError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set(HeaderError, code)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, message, status)
}

// WriteError answers with an error.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeEnvelope(w, status, Envelope{OK: false, Error: &Error{Code: code, Message: message}})
}

func writeEnvelope(w http.ResponseWriter, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// DecodeEnvelope reads one envelope from a response body.
func DecodeEnvelope(r io.Reader) (Envelope, error) {
	var env Envelope
	dec := json.NewDecoder(io.LimitReader(r, MaxControlBodyBytes))
	if err := dec.Decode(&env); err != nil {
		return env, fmt.Errorf("decode node response: %w", err)
	}
	return env, nil
}

// Health is a node's self-description. The control plane compares Name, Protocol and
// Revision against what it expects: a node answering with another name is a misconfiguration
// (two records pointing at one machine), and a different revision is what the console shows
// as "can be upgraded".
type Health struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Revision string `json:"revision"`
	Protocol int    `json:"protocol"`
	// StartedAt is when this node agent process started; it restarts a clock for operators
	// wondering whether the node has been up since their last deploy.
	StartedAt time.Time `json:"started_at"`
	// Tenants/Running/Suspended describe the node's own allocation table: what it has been
	// told to host, how many workers are actually running, and how many are deliberately
	// stopped. The control plane compares Running against its own view to detect drift.
	Tenants   int `json:"tenants"`
	Running   int `json:"running"`
	Suspended int `json:"suspended"`
	// Features is what this machine can serve (M77). The control plane cannot know it from its own
	// configuration — the capabilities belong to the node's machine — so the node reports them and
	// the console shows them, instead of an operator discovering by failure that one node has no
	// FUSE runtime.
	Features Features `json:"features,omitempty"`
}

// Features is a worker node's capability report.
type Features struct {
	// SSHWorkspaces and BrowserWorkspaces say whether the feature is enabled *and its runtime is
	// expected* on that machine; the node's own doctor verifies the binaries.
	SSHWorkspaces     bool `json:"ssh_workspaces,omitempty"`
	BrowserWorkspaces bool `json:"browser_workspaces,omitempty"`
	// SSHMounts is how many ssh workspace mounts this node currently has attached.
	SSHMounts int `json:"ssh_mounts,omitempty"`
	// HostShares is how many host directories this node may bind into tenant sandboxes.
	HostShares int `json:"host_shares,omitempty"`
	// TenantPlugins lists the tenant-side web plugins this node renders.
	TenantPlugins []string `json:"tenant_plugins,omitempty"`
	// DirectoryPicker and PluginBrowserFS are the per-tenant choices this node applies.
	DirectoryPicker string `json:"directory_picker,omitempty"`
	PluginBrowserFS string `json:"plugin_browser_fs,omitempty"`
}

// Status is the richer answer to the "status" control operation: the health fields plus the
// per-tenant state a reconciliation needs.
type Status struct {
	Health  Health        `json:"health"`
	Tenants []TenantState `json:"tenants"`
}

// TenantState is one tenant as the node actually has it.
//
// It is the node's authoritative answer, which is why it carries the three values the node
// owns — the worker port, and the two paths inside its own data root — alongside the runtime
// state. The control plane records what it reads here instead of deciding those values itself:
// a free port and a data root are properties of the machine, not of the placement.
type TenantState struct {
	Name       string `json:"name"`
	WorkerPort int    `json:"worker_port"`
	DshHome    string `json:"dsh_home,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	PublicPort int    `json:"public_port,omitempty"`
	Account    string `json:"account,omitempty"`
	Isolation  string `json:"isolation,omitempty"`
	// Running is the worker *process* state on this node; Suspended is the operator's durable
	// intent (registry.Tenant.Suspended). Both matter for reconciliation: a tenant that should
	// be running and is not is started, one that is suspended and running is stopped.
	Running   bool `json:"running"`
	PID       int  `json:"pid,omitempty"`
	Suspended bool `json:"suspended,omitempty"`
	// Handshake is the node's handshake state for this tenant ("ok"|"pending"|"failed").
	Handshake string `json:"handshake,omitempty"`
}

// TenantSpec is what the control plane tells a node about one tenant: identity and the
// operator's intent, and nothing about the machine. The node derives its own paths and worker
// port, so those can never drift between the two sides' opinions.
type TenantSpec struct {
	Name       string `json:"name"`
	PublicPort int    `json:"public_port"`
	Account    string `json:"account,omitempty"`
	// Suspended is the operator's durable intent. A node that restarts honours its own stored
	// copy of this flag, so a control plane that is down cannot resurrect a disabled tenant.
	Suspended bool `json:"suspended,omitempty"`
}

// TenantHandshakeRequest asks a node to exchange its worker's startup token for the cookie the
// control plane will present on that worker's behalf.
type TenantHandshakeRequest struct {
	Name string `json:"name"`
	// Authority is the authority the control plane will use on every forwarded request
	// (Host: 127.0.0.1:<worker_port>). The node must handshake against exactly it: dsh binds the
	// cookie's name and signature to the authority, so a cookie obtained under another one would
	// be refused by the very requests it is meant to authorize.
	Authority string `json:"authority"`
}

// TenantHandshakeResult is the worker credential the node obtained. Value is a bearer
// credential: it crosses the control channel by design and is only ever stored server-side.
type TenantHandshakeResult struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	Authority string    `json:"authority"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// TenantRef names one tenant for an operation that has no other arguments.
type TenantRef struct {
	Name string `json:"name"`
}

// ModelSpec is one model a tenant's worker may use, as the control plane learned it from aigw.
// It mirrors aigw.Model on the wire; the converters below are the only place that mapping
// lives, so a new capability field cannot be carried in one direction only.
type ModelSpec struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	Images          bool   `json:"images,omitempty"`
	// ReasoningSupported nil means "aigw disclosed no capability set", which is a different
	// fact from "this model does not reason" (false) — see M68.
	ReasoningSupported *bool `json:"reasoning_supported,omitempty"`
	ReasoningForced    bool  `json:"reasoning_forced,omitempty"`
}

// ModelsToSpec converts the control plane's model list for the wire.
func ModelsToSpec(models []aigw.Model) []ModelSpec {
	out := make([]ModelSpec, 0, len(models))
	for _, model := range models {
		out = append(out, ModelSpec{
			ID: model.ID, Name: model.Name, ContextWindow: model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens, Images: model.Images,
			ReasoningSupported: model.ReasoningSupported, ReasoningForced: model.ReasoningForced,
		})
	}
	return out
}

// ModelsFromSpec converts a node's received list back into the shape the tenant renderer uses.
func ModelsFromSpec(specs []ModelSpec) []aigw.Model {
	out := make([]aigw.Model, 0, len(specs))
	for _, spec := range specs {
		out = append(out, aigw.Model{
			ID: spec.ID, Name: spec.Name, ContextWindow: spec.ContextWindow,
			MaxOutputTokens: spec.MaxOutputTokens, Images: spec.Images,
			ReasoningSupported: spec.ReasoningSupported, ReasoningForced: spec.ReasoningForced,
		})
	}
	return out
}

// TenantCreateRequest provisions one tenant on a node.
type TenantCreateRequest struct {
	Spec TenantSpec `json:"spec"`
	// Key is the tenant's worker credential for aigw. It crosses the control channel because the
	// node's worker calls aigw directly; the channel is therefore credential-bearing by design
	// (see the deployment manual's security section).
	Key    string      `json:"key"`
	Models []ModelSpec `json:"models,omitempty"`
	// DirectoryPicker and PluginBrowserFS are the per-tenant profile choices the control plane
	// holds (they are recorded on the tenant record).
	DirectoryPicker  string `json:"directory_picker,omitempty"`
	PluginBrowserFS  string `json:"plugin_browser_fs,omitempty"`
	AllowEmptyModels bool   `json:"allow_empty_models,omitempty"`
}

// TenantSetKeyRequest rotates one tenant's worker credential on a node.
type TenantSetKeyRequest struct {
	Name    string      `json:"name"`
	Account string      `json:"account,omitempty"`
	Key     string      `json:"key"`
	Models  []ModelSpec `json:"models,omitempty"`
	// KeepPrevious keeps the outgoing key prefix valid for the same reason the local rotation
	// does: requests signed with the old key must not fail during the changeover.
	KeepPrevious bool `json:"keep_previous,omitempty"`
}

// TenantEnsureProvisionedRequest writes the files a tenant's dsh reads at startup when they are
// missing (the settings slice, the credential reference, the profile patch). It is idempotent:
// an existing settings.yaml belongs to SyncModels and is left alone.
type TenantEnsureProvisionedRequest struct {
	Name   string      `json:"name"`
	Key    string      `json:"key"`
	Models []ModelSpec `json:"models,omitempty"`
}

// TenantEnsureProvisionedResult says whether files had to be written.
type TenantEnsureProvisionedResult struct {
	Provisioned bool `json:"provisioned"`
}

// TenantSyncModelsRequest replaces one tenant's model list.
type TenantSyncModelsRequest struct {
	Name   string      `json:"name"`
	Models []ModelSpec `json:"models,omitempty"`
}

// TenantRemoveRequest removes one tenant from a node. Purge deletes its data; without it the
// workspace and DSH home stay exactly as they were, which is what "停用" means everywhere else.
type TenantRemoveRequest struct {
	Name  string `json:"name"`
	Purge bool   `json:"purge,omitempty"`
}

// TenantRemoveResult reports the snapshot a removal wrote before deleting anything.
type TenantRemoveResult struct {
	Snapshot string `json:"snapshot,omitempty"`
}

// TenantEnsureRunningResult says whether a worker had to be started.
type TenantEnsureRunningResult struct {
	Started bool `json:"started"`
}

// TenantCaptureURLResult carries the worker's startup URL. It embeds a bearer token, so the
// control plane treats it as a credential (prints it only when an operator asks).
type TenantCaptureURLResult struct {
	URL string `json:"url"`
}

// TenantLogoutResult mirrors the local logout teardown's outcome (M76) so the control plane can
// audit what a remote sign-out actually detached and stopped.
type TenantLogoutResult struct {
	MountsDetached int      `json:"mounts_detached"`
	MountsLeftover []string `json:"mounts_leftover,omitempty"`
	WorkerStopped  bool     `json:"worker_stopped"`
}

// AuditTailRequest asks a node for the security events it has written since a cursor.
//
// Node-side events (a refused ssh mount, a logout that had to break a mount) happen where the
// mount lives, but the operator reads one audit stream in one place. The control plane pulls them
// with the cursor it last recorded, so a restart resumes instead of re-importing history.
type AuditTailRequest struct {
	// Cursor is the value of a previous AuditTailResult. Empty means "start from the current end":
	// importing a node's whole history on first contact would fill the gateway's audit with events
	// from before it ever knew about that node.
	Cursor string `json:"cursor,omitempty"`
	// Limit bounds the lines returned in one answer (0 = the node's default).
	Limit int `json:"limit,omitempty"`
	// Last asks for the newest N lines regardless of the cursor, for an operator reading a node's
	// recent events (the cursor is not advanced by such a read).
	Last int `json:"last,omitempty"`
}

// AuditTailResult is a bounded batch of a node's audit lines, verbatim.
type AuditTailResult struct {
	// Lines are the JSONL records the node wrote, newest last.
	Lines []string `json:"lines,omitempty"`
	// Cursor is what to pass next time. It is opaque to the caller.
	Cursor string `json:"cursor"`
	// Dropped counts events the cursor no longer reaches (the file was rotated or truncated): the
	// gap is reported rather than hidden.
	Dropped int `json:"dropped,omitempty"`
}

// ReconcileRequest is the control plane's authoritative tenant list for one node.
type ReconcileRequest struct {
	Tenants []TenantSpec `json:"tenants"`
	// Prune removes node-local records that the control plane does not know about. It is a
	// separate switch because a wrong control plane should not be able to erase a node's
	// allocation table by being empty.
	Prune bool `json:"prune,omitempty"`
}

// ReconcileResult reports the node's state after applying the list, so the control plane can
// record the ports and paths the node actually uses.
type ReconcileResult struct {
	Status Status `json:"status"`
	// Started and Stopped name the tenants whose worker changed state, so an operator reading the
	// audit line can see what the reconciliation did rather than only what it intended.
	Started []string `json:"started,omitempty"`
	Stopped []string `json:"stopped,omitempty"`
	// Pruned names node-local records that were dropped because the control plane no longer
	// knows the tenant. Their data is never deleted.
	Pruned []string `json:"pruned,omitempty"`
}

// TokenMatches compares the configured node token with the one a request presented, in
// constant time. Both sides are hashed first so the comparison is also independent of the
// length of the presented value.
func TokenMatches(expected, presented string) bool {
	if expected == "" {
		return false
	}
	want := sha256.Sum256([]byte(expected))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// BearerToken extracts the token from an Authorization header value.
func BearerToken(header string) (string, bool) {
	const prefix = "bearer "
	trimmed := strings.TrimSpace(header)
	if len(trimmed) <= len(prefix) || !strings.EqualFold(trimmed[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(trimmed[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
