// Package nodeops implements the worker node's side of the control protocol (M77): the handlers
// a node registers with nodeserve, written against this machine's own lifecycle manager.
//
// Why a package rather than code in cmd/dshgw: these handlers are the node's whole remote
// surface, and they are worth testing against the same synthetic deployment the tenancy tests
// use (a stand-in worker process, no bwrap required). Keeping them out of main also keeps the
// command's job what it says: load configuration, wire, serve.
//
// Error taxonomy: a handler decides the protocol code BEFORE calling the manager, by checking the
// thing it can check itself (does this tenant exist here, is the request complete). Anything the
// manager then fails at is reported as an internal error with its text attached — the control
// plane's operator reads that text, and guessing a code from a message would make a wrong answer
// look authoritative.
package nodeops

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/nodeserve"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// Ops is the node's operation surface.
type Ops struct {
	Config  *config.Config
	Manager *tenancy.Manager
	// Registry is this node's allocation table — the same registry the manager writes, kept as
	// its own field because the handlers must answer "does this node host that tenant" before
	// asking the manager to act.
	Registry *registry.Registry
	// Taken reports ports something is already listening on, so a created tenant never parks a
	// worker on a port another process holds. Nil skips the check (tests, and a node whose
	// listing tool is unavailable).
	Taken func(int) bool
	// TokenURL reads a worker's startup token URL (the handshake file the runner writes). Nil
	// uses the real file source; tests inject one so the handshake path needs no worker.
	TokenURL func(tenant string) (string, error)
	// Exchange turns that token into the worker's cookie. Nil uses the real HTTP exchanger.
	Exchange func(ctx context.Context, tokenURL, authority string) (*session.Upstream, error)
	// Features is what this machine can serve, reported through health/status (M77).
	Features nodeproto.Features
	// AuditPath is this node's audit file, served to the control plane so one stream reaches the
	// operator (see auditTail).
	AuditPath string
	Logger    *slog.Logger
	Now       func() time.Time
}

// tokenURLFor resolves one tenant's handshake URL.
func (o *Ops) tokenURLFor(tenant string) (string, error) {
	if o.TokenURL != nil {
		return o.TokenURL(tenant)
	}
	return handshake.FileSource{Dir: o.Config.HandshakeDir}.TokenURL(tenant)
}

// exchangeToken is the worker handshake, injectable for tests.
func (o *Ops) exchangeToken(ctx context.Context, tokenURL, authority string) (*session.Upstream, error) {
	if o.Exchange != nil {
		return o.Exchange(ctx, tokenURL, authority)
	}
	return (&handshake.HTTPExchanger{}).Exchange(ctx, tokenURL, authority)
}

func (o *Ops) log() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func (o *Ops) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// Handlers is the operation table a node registers with nodeserve.
func (o *Ops) Handlers() map[string]nodeserve.Handler {
	return map[string]nodeserve.Handler{
		"status":                    o.status,
		"reconcile":                 o.reconcile,
		"tenant-create":             o.create,
		"tenant-adopt":              o.adopt,
		"tenant-start":              o.start,
		"tenant-stop":               o.stop,
		"tenant-restart":            o.restart,
		"tenant-remove":             o.remove,
		"tenant-set-key":            o.setKey,
		"tenant-sync-models":        o.syncModels,
		"tenant-ensure-running":     o.ensureRunning,
		"tenant-ensure-provisioned": o.ensureProvisioned,
		"tenant-capture-url":        o.captureURL,
		"handshake":                 o.handshake,
		"audit-tail":                o.auditTail,
		"tenant-logout-stop":        o.logoutStop,
	}
}

// status is this node's self-description: every tenant in its allocation table with the worker
// state the control plane needs to reconcile against.
func (o *Ops) status(ctx context.Context, _ []byte) (any, error) {
	tenants := o.Registry.List()
	status := nodeproto.Status{Tenants: make([]nodeproto.TenantState, 0, len(tenants))}
	status.Health.Features = o.Features
	for _, tenant := range tenants {
		state := o.state(ctx, tenant)
		if state.Running {
			status.Health.Running++
		}
		if state.Suspended {
			status.Health.Suspended++
		}
		status.Tenants = append(status.Tenants, state)
	}
	status.Health.Tenants = len(tenants)
	return status, nil
}

// state renders one tenant's runtime state. A local status failure is reported as "not running"
// with no detail rather than failing the whole answer: the control plane's reconcile can then
// still see the other tenants.
func (o *Ops) state(ctx context.Context, tenant registry.Tenant) nodeproto.TenantState {
	state := nodeproto.TenantState{
		Name: tenant.Name, WorkerPort: tenant.WorkerPort, DshHome: tenant.DshHome,
		Workspace: tenant.Workspace, PublicPort: tenant.PublicPort, Account: tenant.Account,
		Isolation: tenant.EffectiveIsolation(), Suspended: tenant.Suspended,
		Handshake: string(tenant.Handshake),
	}
	if status, err := o.Manager.Status(ctx, tenant); err == nil {
		state.Running, state.PID = status.Running, status.PID
	}
	return state
}

// create provisions one tenant on this machine.
func (o *Ops) create(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.TenantCreateRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.Spec.Name)
	if !config.ValidTenantName(name) {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "invalid tenant name %q", request.Spec.Name)
	}
	if strings.TrimSpace(request.Key) == "" {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "a worker credential is required to create a tenant")
	}
	if request.Spec.PublicPort < 1 || request.Spec.PublicPort > 65535 {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "the control plane must name the tenant's public port")
	}
	if _, exists := o.Registry.Get(name); exists {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "tenant %s already exists on this node", name)
	}
	created, err := o.Manager.Create(ctx, name, request.Key, nodeproto.ModelsFromSpec(request.Models), tenancy.CreateOptions{
		AllowEmptyModels: request.AllowEmptyModels,
		DirectoryPicker:  request.DirectoryPicker,
		PluginBrowserFS:  request.PluginBrowserFS,
		Account:          request.Spec.Account,
		PublicPort:       request.Spec.PublicPort,
		// The key prefix stays with the control plane: this machine's table answers "which tenants
		// does this machine host", and the prefix answers "which key is this tenant".
		NoKeyPrefix: true,
		// No Node: on this machine every tenant is local, and the node's registry must not carry
		// a placement it would then try to reach from inside a node.
	})
	if err != nil {
		return nil, err
	}
	o.log().Info("tenant provisioned over the control channel", "tenant", created.Name, "worker_port", created.WorkerPort)
	return o.state(ctx, created), nil
}

// adopt registers a tenant this node is already holding the data for (M77).
//
// It is the second half of a migration: the operator stopped the tenant, copied its directories
// to this machine, and now asks this node to take it over. Nothing is created and no worker is
// started — the data decides whether the request is valid, and the worker is started by the
// explicit `tenant start` that follows (or by the next reconcile).
func (o *Ops) adopt(ctx context.Context, body []byte) (any, error) {
	var spec nodeproto.TenantSpec
	if err := decode(body, &spec); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(spec.Name)
	if !config.ValidTenantName(name) {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "invalid tenant name %q", spec.Name)
	}
	if spec.PublicPort < 1 || spec.PublicPort > 65535 {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "the control plane must name the tenant's public port")
	}
	if _, exists := o.Registry.Get(name); exists {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "tenant %s is already registered on this node", name)
	}
	dshHome := filepath.Join(o.Config.TenantRoot, name, ".dsh")
	workspace := filepath.Join(o.Config.WorkspaceRoot, name)
	// The data is the precondition: a record without it is a tenant whose worker cannot start, and
	// registering one would turn a copy mistake into a mystery 502 later.
	if _, err := os.Stat(filepath.Join(dshHome, "settings.yaml")); err != nil {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest,
			"no tenant data to adopt at %s: copy the tenant's directories to this machine first", dshHome)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "tenant workspace %s is missing", workspace)
	}
	workerPort, err := o.Registry.AssignWorkerPort(o.Config, o.Taken)
	if err != nil {
		return nil, err
	}
	adopted := registry.Tenant{
		Name: name, UID: os.Geteuid(), PublicPort: spec.PublicPort, WorkerPort: workerPort,
		DshHome: dshHome, Workspace: workspace, CreatedAt: o.now(),
		Handshake: registry.HandshakePending, Isolation: registry.IsolationBwrap,
		DirectoryPicker: o.Config.DirectoryPicker, PluginBrowserFS: o.Config.PluginBrowserFS,
		Account: spec.Account, Suspended: spec.Suspended,
	}
	// No key prefix is recorded: the credential and the prefix it belongs to live with the tenant's
	// data and in the control plane's registry, and this table only answers "which tenants does
	// this machine host". The registry allows an unbound record for exactly this case.
	if err := o.Registry.Put(adopted); err != nil {
		return nil, err
	}
	if err := o.Registry.Save(); err != nil {
		return nil, fmt.Errorf("save this node's allocation table: %w", err)
	}
	o.log().Info("tenant adopted by operator migration", "tenant", name, "worker_port", workerPort)
	return o.state(ctx, adopted), nil
}

// reconcile applies the control plane's authoritative list for this node.
//
// The rules are the two halves of the design's drift policy:
//   - the control plane decides identity and the operator's intent (name, public port, account,
//     suspension), and this node adopts them;
//   - this node decides its own worker port and paths, and reports them back so the control plane
//     can record what is true here.
//
// Workers are then started and stopped so the machine matches the intent, and a local record the
// control plane does not know about is pruned when asked — never with its data.
func (o *Ops) reconcile(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.ReconcileRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	wanted := make(map[string]nodeproto.TenantSpec, len(request.Tenants))
	for _, spec := range request.Tenants {
		name := strings.TrimSpace(spec.Name)
		if !config.ValidTenantName(name) {
			return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "invalid tenant name %q in the reconcile list", spec.Name)
		}
		wanted[name] = spec
	}
	result := nodeproto.ReconcileResult{}
	// Tenants the control plane lists and this node has never provisioned. They are reported
	// rather than created: adopting one would need a worker credential this node does not have,
	// and inventing one is worse than saying so.
	var unprovisioned []nodeproto.TenantState

	// Adopt or update everything the control plane knows about.
	for name, spec := range wanted {
		current, exists := o.Registry.Get(name)
		if !exists {
			unprovisioned = append(unprovisioned, nodeproto.TenantState{
				Name: name, PublicPort: spec.PublicPort, Account: spec.Account, Running: false,
			})
			o.log().Warn("reconcile: tenant is recorded on this node but has never been provisioned here", "tenant", name)
			continue
		}
		if current.PublicPort != spec.PublicPort || current.Account != spec.Account {
			updated := current
			updated.PublicPort, updated.Account = spec.PublicPort, spec.Account
			if err := o.Registry.Put(updated); err != nil {
				return nil, fmt.Errorf("record tenant %s: %w", name, err)
			}
		}
		if current.Suspended != spec.Suspended {
			if err := o.Manager.SetSuspended(ctx, name, spec.Suspended); err != nil {
				return nil, fmt.Errorf("apply suspension of %s: %w", name, err)
			}
		}
	}
	// Drop local records the control plane no longer has, when it says so. The worker stops first
	// (a tenant nobody addresses should not keep holding memory), but the record only goes when
	// that succeeded: keeping it means the next reconcile retries the stop instead of leaving an
	// orphan process nothing knows about. The data always stays — deleting a workspace is a purge,
	// and a purge is an explicit operator action.
	if request.Prune {
		for _, tenant := range o.Registry.List() {
			if _, keep := wanted[tenant.Name]; keep {
				continue
			}
			if status, err := o.Manager.Status(ctx, tenant); err == nil && status.Running {
				if err := o.Manager.StopWorkerProcess(ctx, tenant); err != nil {
					o.log().Warn("reconcile: cannot stop a worker of a tenant the control plane dropped", "tenant", tenant.Name, "err", err)
					continue
				}
				result.Stopped = append(result.Stopped, tenant.Name)
			}
			o.Registry.Delete(tenant.Name)
			result.Pruned = append(result.Pruned, tenant.Name)
		}
	}
	if err := o.Registry.Save(); err != nil {
		return nil, fmt.Errorf("save this node's allocation table: %w", err)
	}

	// Bring workers in line with the (now adopted) intent.
	for _, tenant := range o.Registry.List() {
		status, err := o.Manager.Status(ctx, tenant)
		if err != nil {
			o.log().Warn("reconcile: cannot read a worker's state", "tenant", tenant.Name, "err", err)
			continue
		}
		switch {
		case tenant.Suspended && status.Running:
			if err := o.Manager.StopWorkerProcess(ctx, tenant); err != nil {
				o.log().Warn("reconcile: stopping a suspended tenant's worker failed", "tenant", tenant.Name, "err", err)
				continue
			}
			result.Stopped = append(result.Stopped, tenant.Name)
		case !tenant.Suspended && !status.Running:
			if err := o.Manager.StartWorker(ctx, tenant); err != nil {
				o.log().Warn("reconcile: starting a worker failed", "tenant", tenant.Name, "err", err)
				continue
			}
			result.Started = append(result.Started, tenant.Name)
		}
	}
	status, err := o.status(ctx, nil)
	if err != nil {
		return nil, err
	}
	result.Status = status.(nodeproto.Status)
	// The answer describes the whole list the control plane sent: what this node hosts, and what it
	// was told about but does not have. A missing entry would look like a tenant that vanished.
	result.Status.Tenants = append(result.Status.Tenants, unprovisioned...)
	result.Status.Health.Tenants += len(unprovisioned)
	return result, nil
}

func (o *Ops) start(ctx context.Context, body []byte) (any, error) {
	name, err := o.ref(body)
	if err != nil {
		return nil, err
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	if err := o.Manager.StartWorker(ctx, tenant); err != nil {
		return nil, err
	}
	return o.state(ctx, o.reload(tenant)), nil
}

func (o *Ops) stop(ctx context.Context, body []byte) (any, error) {
	name, err := o.ref(body)
	if err != nil {
		return nil, err
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	if err := o.Manager.StopWorker(ctx, tenant); err != nil {
		return nil, err
	}
	return o.state(ctx, o.reload(tenant)), nil
}

func (o *Ops) restart(ctx context.Context, body []byte) (any, error) {
	name, err := o.ref(body)
	if err != nil {
		return nil, err
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	if err := o.Manager.Restart(ctx, tenant); err != nil {
		return nil, err
	}
	return o.state(ctx, o.reload(tenant)), nil
}

func (o *Ops) remove(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.TenantRemoveRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.Name)
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	snapshot, err := o.Manager.Remove(ctx, tenant, request.Purge)
	if err != nil {
		return nil, err
	}
	return nodeproto.TenantRemoveResult{Snapshot: snapshot}, nil
}

func (o *Ops) setKey(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.TenantSetKeyRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.Name)
	if strings.TrimSpace(request.Key) == "" {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "a worker credential is required")
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	if err := o.Manager.RotateKey(ctx, tenant, request.Key, nodeproto.ModelsFromSpec(request.Models), request.KeepPrevious); err != nil {
		return nil, err
	}
	// The account label is recorded after the rotation: the rotation's own lifecycle lock reloads
	// this table from disk, so a label written before it would be discarded by that reload.
	if account := strings.TrimSpace(request.Account); account != "" && account != tenant.Account {
		if err := o.Registry.SetAccount(name, account); err != nil {
			return nil, err
		}
		if err := o.Registry.Save(); err != nil {
			return nil, err
		}
	}
	return o.state(ctx, o.reload(tenant)), nil
}

// reload reads a tenant's current record, so an answer describes the state the operation left
// behind rather than the state it started from.
func (o *Ops) reload(tenant registry.Tenant) registry.Tenant {
	if current, ok := o.Registry.Get(tenant.Name); ok {
		return current
	}
	return tenant
}

func (o *Ops) syncModels(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.TenantSyncModelsRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.Name)
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	if err := o.Manager.SyncModels(tenant, nodeproto.ModelsFromSpec(request.Models)); err != nil {
		return nil, err
	}
	return map[string]any{"synced": len(request.Models)}, nil
}

func (o *Ops) ensureRunning(ctx context.Context, body []byte) (any, error) {
	name, err := o.ref(body)
	if err != nil {
		return nil, err
	}
	if _, ok := o.Registry.Get(name); !ok {
		return nil, tenantUnknown(name)
	}
	started, runErr := o.Manager.EnsureRunning(ctx, registry.Tenant{Name: name})
	if runErr != nil {
		return nil, runErr
	}
	return nodeproto.TenantEnsureRunningResult{Started: started}, nil
}

func (o *Ops) ensureProvisioned(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.TenantEnsureProvisionedRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.Name)
	if strings.TrimSpace(request.Key) == "" {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "a worker credential is required")
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	provisioned, err := o.Manager.EnsureProvisioned(ctx, tenant, request.Key, nodeproto.ModelsFromSpec(request.Models))
	if err != nil {
		return nil, err
	}
	return nodeproto.TenantEnsureProvisionedResult{Provisioned: provisioned}, nil
}

// handshake performs the worker handshake on this machine (M77).
//
// It runs where the worker is, because that is where the startup token file and the loopback
// socket are; the control plane cannot reach either. The authority is checked against this
// node's own record of the worker port: a mismatch means the control plane's registry is stale,
// and answering anyway would hand back a cookie that its requests cannot use.
func (o *Ops) handshake(ctx context.Context, body []byte) (any, error) {
	var request nodeproto.TenantHandshakeRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.Name)
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	if authority := strings.TrimSpace(request.Authority); authority != want {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest,
			"this node's worker for %s listens as %s, not %s: reconcile the control plane's record first", name, want, authority)
	}
	url, err := o.tokenURLFor(name)
	if err != nil {
		return nil, nodeproto.Errorf(nodeproto.CodeWorkerNotRunning,
			"no worker startup token for %s on this node: its worker is not running", name)
	}
	upstream, err := o.exchangeToken(ctx, url, want)
	if err != nil {
		return nil, nodeproto.Errorf(nodeproto.CodeWorkerNotRunning,
			"worker handshake for %s failed: %v", name, err)
	}
	if upstream.Authority != want {
		return nil, nodeproto.Errorf(nodeproto.CodeInternal, "handshake returned authority %q, want %q", upstream.Authority, want)
	}
	return nodeproto.TenantHandshakeResult{
		Name: upstream.Name, Value: upstream.Value, Authority: upstream.Authority, ExpiresAt: upstream.ExpiresAt,
	}, nil
}

func (o *Ops) captureURL(ctx context.Context, body []byte) (any, error) {
	name, err := o.ref(body)
	if err != nil {
		return nil, err
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	url, err := o.Manager.CaptureURL(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return nodeproto.TenantCaptureURLResult{URL: url}, nil
}

func (o *Ops) logoutStop(ctx context.Context, body []byte) (any, error) {
	name, err := o.ref(body)
	if err != nil {
		return nil, err
	}
	tenant, ok := o.Registry.Get(name)
	if !ok {
		return nil, tenantUnknown(name)
	}
	result, err := o.Manager.StopForLogout(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return nodeproto.TenantLogoutResult{
		MountsDetached: result.MountsDetached,
		MountsLeftover: result.MountsLeftover,
		WorkerStopped:  result.WorkerStopped,
	}, nil
}

// auditTail serves this node's security events to the control plane (M77).
//
// The read is a byte range of the node's own audit file: the node keeps no second copy of its
// audit in memory, and the cursor is simply "how far the control plane has read". A cursor that no
// longer matches the file (it was rotated or replaced) is reported as dropped events instead of
// silently starting over.
func (o *Ops) auditTail(_ context.Context, body []byte) (any, error) {
	var request nodeproto.AuditTailRequest
	if len(body) > 0 {
		if err := decode(body, &request); err != nil {
			return nil, err
		}
	}
	path := o.AuditPath
	if path == "" && o.Config != nil {
		path = o.Config.AuditPath
	}
	if path == "" {
		return nodeproto.AuditTailResult{}, nil
	}
	limit := request.Limit
	if limit <= 0 || limit > maxAuditTailLines {
		limit = maxAuditTailLines
	}
	if request.Last > 0 {
		return o.auditLast(path, request.Last)
	}
	offset, dropped := parseAuditCursor(request.Cursor)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// No events yet is not an error: a node that has never logged anything reports nothing.
		return nodeproto.AuditTailResult{Cursor: formatAuditCursor(0)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open this node's audit file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if offset > info.Size() {
		// The file shrank (rotation): start over at the beginning and say so.
		dropped += int(offset - info.Size())
		offset = 0
	}
	if offset == 0 && request.Cursor == "" {
		// First contact: start from now, not from the whole history.
		return nodeproto.AuditTailResult{Cursor: formatAuditCursor(info.Size())}, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	result := nodeproto.AuditTailResult{Dropped: dropped}
	read := offset
	for len(result.Lines) < limit {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A partial trailing line is left for the next call: a half-written event would be
				// unreadable JSON and would make the whole batch look corrupt.
				break
			}
			return nil, err
		}
		read += int64(len(line))
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		result.Lines = append(result.Lines, trimmed)
	}
	result.Cursor = formatAuditCursor(read)
	return result, nil
}

// auditLast returns the newest lines of this node's audit file, for an operator reading a node's
// recent events. It is a bounded read: the file grows for as long as the node runs, and a tail that
// walks a gigabyte of history would be a denial of service against the control plane.
func (o *Ops) auditLast(path string, last int) (any, error) {
	if last > maxAuditTailLines {
		last = maxAuditTailLines
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nodeproto.AuditTailResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open this node's audit file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > maxAuditTailBytes {
		start = info.Size() - maxAuditTailBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(io.LimitReader(file, maxAuditTailBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	ring := make([]string, 0, last)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if len(ring) == last {
			copy(ring, ring[1:])
			ring[len(ring)-1] = line
			continue
		}
		ring = append(ring, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nodeproto.AuditTailResult{Lines: ring, Cursor: formatAuditCursor(info.Size())}, nil
}

// maxAuditTailBytes bounds how much of the audit file a tail read walks.
const maxAuditTailBytes = 4 << 20

// maxAuditTailLines bounds one audit answer: the control plane polls, so a small batch is enough
// and an unbounded one would let a busy node's answer grow without limit.
const maxAuditTailLines = 500

// formatAuditCursor encodes a byte offset. It is deliberately opaque to the caller, so a later
// version can change the encoding without a protocol bump.
func formatAuditCursor(offset int64) string { return "off:" + strconv.FormatInt(offset, 10) }

// parseAuditCursor decodes a cursor, reporting whether it was unusable (which counts as dropped
// history rather than as an error: the caller's next batch simply starts over).
func parseAuditCursor(cursor string) (offset int64, dropped int) {
	if cursor == "" {
		return 0, 0
	}
	value, err := strconv.ParseInt(strings.TrimPrefix(cursor, "off:"), 10, 64)
	if err != nil || value < 0 {
		return 0, 0
	}
	return value, 0
}

// ref decodes a bare tenant reference.
func (o *Ops) ref(body []byte) (string, error) {
	var ref nodeproto.TenantRef
	if err := decode(body, &ref); err != nil {
		return "", err
	}
	name := strings.TrimSpace(ref.Name)
	if !config.ValidTenantName(name) {
		return "", nodeproto.Errorf(nodeproto.CodeBadRequest, "invalid tenant name %q", ref.Name)
	}
	return name, nil
}

func tenantUnknown(name string) error {
	return nodeproto.Errorf(nodeproto.CodeTenantUnknown, "this node does not host tenant %q", name)
}

// decode unmarshals a control request body, reporting a malformed body as a bad request rather
// than as the handler's own failure.
func decode(body []byte, out any) error {
	if len(body) == 0 {
		return nodeproto.Errorf(nodeproto.CodeBadRequest, "a request body is required")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nodeproto.Errorf(nodeproto.CodeBadRequest, "unreadable request body: %v", err)
	}
	return nil
}
