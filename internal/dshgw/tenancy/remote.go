package tenancy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// remoteRollbackBudget bounds the compensating action after a failed remote create. It is short
// on purpose: the caller is waiting for an error, and a node that does not answer promptly is
// reported as "data retained on the node" instead of holding the request open.
const remoteRollbackBudget = 30 * time.Second

// This file is the control plane's half of multi-machine operation (M77): every lifecycle method
// whose tenant may live on another machine asks remoteFor first, and a tenant placed on a node
// becomes one RPC.
//
// The shape is deliberate. Every one of these methods is called from several entry points (the
// admin socket, the login hook, the CLI, the proxy's logout path), and a second copy of "which
// machine is this tenant on" is how a code path eventually ships that asks the wrong one. So the
// decision lives here, once per method, at the top of the local implementation.

// remoteFor resolves the node a tenant is placed on, or nil when the tenant runs in this process.
//
// A placement this process cannot resolve is an error, never a fallback to local execution: a
// tenant recorded on node-a whose worker actually runs on the control plane's host would be
// serving traffic from a machine nobody believes in.
func (m *Manager) remoteFor(t registry.Tenant) (*nodeclient.Client, error) {
	if m.Config.IsLocalNode(t.Node) {
		return nil, nil
	}
	if m.Nodes == nil || m.Nodes.Len() == 0 {
		return nil, fmt.Errorf("tenant %s is placed on node %q, but this process has no nodes configured", t.Name, t.Node)
	}
	client, ok := m.Nodes.Get(t.Node)
	if !ok {
		return nil, fmt.Errorf("tenant %s is placed on node %q, which this deployment does not define", t.Name, t.Node)
	}
	return client, nil
}

// nodeFor looks a node up by name for operations that are not about one tenant.
func (m *Manager) nodeFor(name string) (*nodeclient.Client, error) {
	if m.Config.IsLocalNode(name) {
		return nil, errors.New("the control plane's own machine is not a remote node")
	}
	if client, ok := m.Nodes.Get(name); ok {
		return client, nil
	}
	return nil, nodeclient.ErrNoNode(name)
}

// remoteTenantOp runs one argument-less tenant operation on the node that hosts it and folds the
// node's answer into an error the caller can read: the code stays stable, the text names the
// node, and an unreachable node is reported as such rather than as a failed operation.
func remoteTenantOp(ctx context.Context, client *nodeclient.Client, op, tenant string) error {
	var err error
	switch op {
	case "tenant-start":
		err = client.TenantStart(ctx, tenant)
	case "tenant-stop":
		err = client.TenantStop(ctx, tenant)
	case "tenant-restart":
		err = client.TenantRestart(ctx, tenant)
	default:
		return fmt.Errorf("internal: unknown remote tenant operation %q", op)
	}
	if err != nil {
		return fmt.Errorf("node %s: %s: %w", client.Name, op, err)
	}
	return nil
}

// applyState records what a node reported about one tenant.
//
// The node owns its worker port and the two paths inside its own data root, so the control plane
// copies them into its registry instead of deciding them. Suspension is *not* copied: that is the
// operator's intent, and the control plane is where it is decided.
func (m *Manager) applyState(state nodeproto.TenantState) error {
	if state.Name == "" {
		return errors.New("node reported a tenant without a name")
	}
	if state.WorkerPort == 0 || state.DshHome == "" || state.Workspace == "" {
		// A node that answers with a partial record is a protocol problem, not a tenant to
		// half-record: the worker port is exactly what the next handshake's Host depends on.
		return fmt.Errorf("node reported an incomplete record for tenant %s", state.Name)
	}
	return m.Registry.SetPlacement(state.Name, state.WorkerPort, state.DshHome, state.Workspace)
}

// applyNodeStatus folds a whole status answer into the registry and reports which tenants differ
// from what the node says (a drift an operator should see, even after it has been repaired).
func (m *Manager) applyNodeStatus(node string, status nodeproto.Status) []string {
	var drifted []string
	for _, state := range status.Tenants {
		current, ok := m.Registry.Get(state.Name)
		if !ok {
			// A tenant the node hosts and the control plane does not know: it is not adopted
			// silently (that would invent a tenant), but it is reported so reconcile can prune it.
			drifted = append(drifted, state.Name+":unknown-to-control-plane")
			continue
		}
		if !m.Config.IsLocalNode(current.Node) && current.Node != node {
			drifted = append(drifted, fmt.Sprintf("%s:recorded-on-%s", state.Name, current.Node))
			continue
		}
		if current.WorkerPort != state.WorkerPort || current.DshHome != state.DshHome || current.Workspace != state.Workspace {
			if err := m.applyState(state); err != nil {
				drifted = append(drifted, fmt.Sprintf("%s:unrecorded:%v", state.Name, err))
				continue
			}
			drifted = append(drifted, fmt.Sprintf("%s:port-or-paths", state.Name))
		}
	}
	return drifted
}

// ReconcileNode pushes the control plane's authoritative tenant list for one node and records
// what the node reports back.
//
// It is the repair for every disagreement the two sides can have: a node that restarted and
// re-allocated ports, a control plane that missed a lifecycle event while it was down, and a
// tenant left on a node after its record was dropped here. It starts and stops workers as the
// list requires and never deletes tenant data.
func (m *Manager) ReconcileNode(ctx context.Context, name string) (nodeproto.ReconcileResult, error) {
	client, err := m.nodeFor(name)
	if err != nil {
		return nodeproto.ReconcileResult{}, err
	}
	tenants := m.Registry.TenantsForNode(name)
	specs := make([]nodeproto.TenantSpec, 0, len(tenants))
	for _, tenant := range tenants {
		specs = append(specs, nodeproto.TenantSpec{
			Name: tenant.Name, PublicPort: tenant.PublicPort,
			Account: tenant.Account, Suspended: tenant.Suspended,
		})
	}
	request := nodeproto.ReconcileRequest{Tenants: specs, Prune: true}
	var result nodeproto.ReconcileResult
	if err := client.Reconcile(ctx, request, &result); err != nil {
		return result, fmt.Errorf("node %s: reconcile: %w", name, err)
	}
	drifted := m.applyNodeStatus(name, result.Status)
	if err := m.Registry.Save(); err != nil {
		return result, fmt.Errorf("save registry after reconciling node %s: %w", name, err)
	}
	if len(drifted) > 0 {
		m.log().Warn("node registry drift repaired", "node", name, "tenants", drifted)
	}
	return result, nil
}

// ReconcileAll reconciles every configured node, and reports the failures instead of stopping at
// the first one: one broken node must not keep the others' state stale.
func (m *Manager) ReconcileAll(ctx context.Context) error {
	if m.Nodes == nil {
		return nil
	}
	var failures []error
	names := append([]string(nil), m.Nodes.Names()...)
	sort.Strings(names)
	for _, name := range names {
		if _, err := m.ReconcileNode(ctx, name); err != nil {
			failures = append(failures, err)
			continue
		}
	}
	return errors.Join(failures...)
}

// NodeHealth probes one node and records the outcome on its node store record, so the console
// page and `dshgw node status` show what was true the last time anybody looked.
func (m *Manager) NodeHealth(ctx context.Context, name string) (nodeproto.Health, error) {
	client, err := m.nodeFor(name)
	if err != nil {
		return nodeproto.Health{}, err
	}
	return client.Probe(ctx)
}

// createRemote provisions a tenant on another machine.
//
// The control plane decides the identity and the public port (it owns the public surface), then
// the node provisions the tenant with its own paths and worker port and reports the record it
// created. Everything after that — the registry entry, the credential copy the control plane
// keeps for revalidation, the handshake state — is written here, so a remote tenant is a tenant
// in every respect that other code can see.
//
// On any failure after the node has created the tenant, the node is asked to remove it again: a
// half-created tenant whose registry entry never landed would keep a worker running that nobody
// can address.
func (m *Manager) createRemote(ctx context.Context, name, key string, models []aigw.Model, opt CreateOptions) (created registry.Tenant, err error) {
	client, err := m.nodeFor(opt.Node)
	if err != nil {
		return created, err
	}
	publisher := opt.PublicPort
	if publisher == 0 {
		pub, _, err := m.Registry.AssignPorts(m.Config, m.Taken)
		if err != nil {
			return created, err
		}
		publisher = pub
	}
	request := nodeproto.TenantCreateRequest{
		Spec: nodeproto.TenantSpec{
			Name: name, PublicPort: publisher, Account: opt.Account,
		},
		Key:              key,
		Models:           nodeproto.ModelsToSpec(models),
		DirectoryPicker:  opt.DirectoryPicker,
		PluginBrowserFS:  opt.PluginBrowserFS,
		AllowEmptyModels: opt.AllowEmptyModels,
	}
	state, err := client.TenantCreate(ctx, request)
	if err != nil {
		return created, fmt.Errorf("node %s: create tenant: %w", client.Name, err)
	}
	rollback := func(cause error) error {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), remoteRollbackBudget)
		defer cancel()
		if _, removeErr := client.TenantRemove(rollbackCtx, nodeproto.TenantRemoveRequest{Name: name, Purge: true}); removeErr != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback remote tenant (data retained on node %s): %w", client.Name, removeErr))
		}
		return cause
	}
	created = registry.Tenant{
		// UID is this process's account: it owns the control-plane side of the tenant (the
		// credential copy). The worker's UID on the node is that machine's business.
		Name: state.Name, UID: os.Geteuid(), PublicPort: publisher, WorkerPort: state.WorkerPort,
		KeyPrefix: key[:12], DshHome: state.DshHome, Workspace: state.Workspace,
		CreatedAt: m.now(), Handshake: registry.HandshakeOK, DirectoryPicker: opt.DirectoryPicker,
		PluginBrowserFS: opt.PluginBrowserFS, ModelsPending: len(models) == 0,
		Isolation: registry.IsolationBwrap, Account: opt.Account, Node: client.Name,
	}
	if created.Name == "" {
		created.Name = name
	}
	if state.Handshake != "" && registry.HandshakeState(state.Handshake) != registry.HandshakeOK {
		created.Handshake = registry.HandshakeState(state.Handshake)
	}
	if err := m.Registry.Put(created); err != nil {
		return created, rollback(err)
	}
	if err := m.Registry.Save(); err != nil {
		m.Registry.Delete(name)
		_ = m.Registry.Save()
		return created, rollback(err)
	}
	// The control plane keeps its own copy of the worker credential: revalidation, the key
	// picker and the sidebar's identity all read it here, while the node's copy belongs to the
	// node. Both are written from the same value, so they cannot disagree.
	if err := m.writeGatewayKey(name, key); err != nil {
		m.Registry.Delete(name)
		_ = m.Registry.Save()
		return created, rollback(err)
	}
	return created, nil
}

// writeGatewayKey replaces the control plane's copy of one tenant's worker credential. It is the
// same file the local create and rotation write (mode 0640, next to the tenant record), so the
// key source, the key picker and revalidation need no idea that the tenant is remote.
func (m *Manager) writeGatewayKey(name, key string) error {
	dir := filepath.Join(m.Config.Deploy.TenantConfigRoot, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return securefile.WriteAtomic(filepath.Join(dir, "gateway.key"), []byte(key+"\n"), 0o640)
}

// rotateRemote rotates a tenant's worker credential on its node.
func (m *Manager) rotateRemote(ctx context.Context, client *nodeclient.Client, t registry.Tenant, key string, models []aigw.Model, keepPrevious bool) error {
	request := nodeproto.TenantSetKeyRequest{
		Name: t.Name, Account: t.Account, Key: key,
		Models: nodeproto.ModelsToSpec(models), KeepPrevious: keepPrevious,
	}
	if err := client.TenantSetKey(ctx, request); err != nil {
		return fmt.Errorf("node %s: rotate key: %w", client.Name, err)
	}
	// The control plane's copy follows the node's, in the same order as the local rotation: the
	// node has the new credential and the registry the new prefix, then this copy is replaced.
	if err := m.Registry.RotatePrefix(t.Name, key[:12], keepPrevious); err != nil {
		return err
	}
	if err := m.Registry.Save(); err != nil {
		return err
	}
	return m.writeGatewayKey(t.Name, key)
}

// forgetRemoteTenant drops everything the control plane keeps about a tenant whose node has
// removed it: the registry entry and the copy of its worker credential.
//
// The data itself belongs to the node and was already dealt with there. What is left here is a
// credential for a tenant that no longer exists — leaving it behind would mean a key that still
// validates against aigw and a registry row that promises a worker nobody hosts.
func (m *Manager) forgetRemoteTenant(name string) error {
	m.Registry.Delete(name)
	if err := m.Registry.Save(); err != nil {
		return err
	}
	dir := filepath.Join(m.Config.Deploy.TenantConfigRoot, name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove recorded credential for %s: %w", name, err)
	}
	return nil
}

// AdoptOnNode records a new placement for a tenant whose data an operator has already copied to
// that machine (M77).
//
// The two halves mirror the two sides of every other operation: a remote target registers the
// tenant in its own allocation table and answers with the paths and worker port it chose; a local
// target (moving a tenant back to the control plane's own machine) is the same procedure without
// the round trip.
//
// Nothing is created and no worker is started: the data is the precondition, and the registration
// is followed by an explicit start. The caller has already refused a running tenant — a worker
// still serving from the old machine while the registry says otherwise is exactly the state this
// command exists to prevent.
func (m *Manager) AdoptOnNode(ctx context.Context, t registry.Tenant, node string) error {
	if m.Config.IsLocalNode(node) {
		return m.adoptLocal(t)
	}
	client, err := m.nodeFor(node)
	if err != nil {
		return err
	}
	spec := nodeproto.TenantSpec{
		Name: t.Name, PublicPort: t.PublicPort, Account: t.Account, Suspended: t.Suspended,
	}
	state, err := client.TenantAdopt(ctx, spec)
	if err != nil {
		return fmt.Errorf("node %s: adopt tenant: %w", client.Name, err)
	}
	if err := m.Registry.SetNode(t.Name, client.Name); err != nil {
		return err
	}
	if err := m.applyState(state); err != nil {
		return err
	}
	return m.Registry.Save()
}

// adoptLocal is the same registration for a tenant that comes back to the control plane's own
// machine: the data must be at this machine's paths, and this machine allocates the worker port.
func (m *Manager) adoptLocal(t registry.Tenant) error {
	settings := filepath.Join(m.Config.TenantRoot, t.Name, ".dsh", "settings.yaml")
	if _, err := os.Stat(settings); err != nil {
		return fmt.Errorf("no tenant data to adopt at %s: copy the tenant's directories here first", settings)
	}
	workspace := filepath.Join(m.Config.WorkspaceRoot, t.Name)
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return fmt.Errorf("tenant workspace %s is missing", workspace)
	}
	port, err := m.Registry.AssignWorkerPort(m.Config, m.Taken)
	if err != nil {
		return err
	}
	if err := m.Registry.SetNode(t.Name, config.LocalNodeName); err != nil {
		return err
	}
	if err := m.Registry.SetPlacement(t.Name, port, filepath.Join(m.Config.TenantRoot, t.Name, ".dsh"), workspace); err != nil {
		return err
	}
	return m.Registry.Save()
}
