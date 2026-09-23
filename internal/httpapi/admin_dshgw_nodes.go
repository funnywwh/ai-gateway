package httpapi

import (
	"net/http"
	"strconv"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/localdshgw"
)

// The DSH node management API (M77): the console's「DSH 节点」page drives every one of these.
//
// Two shapes are deliberate and worth naming, because they are what makes this page usable:
//
//   - `GET /admin/api/v1/dshgw/nodes` is cheap by default and only reports live state with
//     `probe=true`. Probing walks every node over the LAN, and a page that did it on every render
//     would make the console's own refresh the load on the fleet.
//   - a deploy is asynchronous. `POST .../nodes/{name}/deploy` returns as soon as the job has
//     started, and the page follows `GET .../nodes/{name}/deploy` for the phase list and the log
//     tail. An ssh install takes tens of seconds; holding a request open for it would only move
//     the timeout into the browser.
//
// The group is `dshgw` in the catalogue so an agent reading the MCP surface finds the whole
// multi-machine story — inventory, registration, deploy, migration — under one name.

// dshgwNodes returns the node half of the channel, or false when this deployment has none.
//
// Missing is reported as an internal error, matching how the tenant half reports its own absence:
// the console's own deployment is misconfigured, not the caller's request. A single-machine
// deployment genuinely has no nodes to manage, and its console page then says so plainly.
func (s *Server) dshgwNodes(w http.ResponseWriter) (DshgwNodeOps, bool) {
	if s.deps.DshgwNodes == nil {
		writeAPIError(w, domain.ErrInternal("dshgw node management channel is not configured"))
		return nil, false
	}
	return s.deps.DshgwNodes, true
}

// dshgwAdmin returns the tenant half of the channel, or false with the same shape of answer.
func (s *Server) dshgwAdmin(w http.ResponseWriter) (DshgwAdminOps, bool) {
	if s.deps.DshgwAdmin == nil {
		writeAPIError(w, domain.ErrInternal("dshgw provisioning channel is not configured"))
		return nil, false
	}
	return s.deps.DshgwAdmin, true
}

func (s *Server) handleAdminListDshgwNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	probe := r.URL.Query().Get("probe") == "true"
	nodes, err := ops.ListNodes(r.Context(), probe)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "probing": probe})
}

// dshgwNodeBody is the registration body an add or update carries. Every field is optional on an
// update (only what is present changes); add requires the ssh target and the listen address, and
// reports exactly which one is missing.
type dshgwNodeBody struct {
	Name         *string `json:"name"`
	Listen       *string `json:"listen"`
	URL          *string `json:"url"`
	SSHHost      *string `json:"ssh_host"`
	SSHPort      *int    `json:"ssh_port"`
	SSHUser      *string `json:"ssh_user"`
	SSHKeyFile   *string `json:"ssh_key_file"`
	DeployDir    *string `json:"deploy_dir"`
	NodeStateDir *string `json:"node_state_dir"`
	PluginDir    *string `json:"plugin_dir"`
	TemplateHome *string `json:"template_home"`
	TokenPath    *string `json:"token_path"`
	BwrapBin     *string `json:"bwrap_bin"`
	NodeBin      *string `json:"node_bin"`
	BinJS        *string `json:"bin_js"`
	CurrentLink  *string `json:"current_link"`
	WorkerPortLo *int    `json:"worker_port_lo"`
	WorkerPortHi *int    `json:"worker_port_hi"`
	Default      *bool   `json:"default"`
}

func (b dshgwNodeBody) spec(name string) localdshgw.NodeSpec {
	spec := localdshgw.NodeSpec{Name: name}
	derefString := func(target *string, value *string) {
		if value != nil {
			*target = *value
		}
	}
	derefString(&spec.Listen, b.Listen)
	derefString(&spec.URL, b.URL)
	derefString(&spec.SSHHost, b.SSHHost)
	derefString(&spec.SSHUser, b.SSHUser)
	derefString(&spec.SSHKeyFile, b.SSHKeyFile)
	derefString(&spec.DeployDir, b.DeployDir)
	derefString(&spec.NodeStateDir, b.NodeStateDir)
	derefString(&spec.PluginDir, b.PluginDir)
	derefString(&spec.TemplateHome, b.TemplateHome)
	derefString(&spec.TokenPath, b.TokenPath)
	derefString(&spec.BwrapBin, b.BwrapBin)
	derefString(&spec.NodeBin, b.NodeBin)
	derefString(&spec.BinJS, b.BinJS)
	derefString(&spec.CurrentLink, b.CurrentLink)
	for _, item := range []struct {
		source *int
		target *int
	}{
		{b.SSHPort, &spec.SSHPort}, {b.WorkerPortLo, &spec.WorkerPortLo}, {b.WorkerPortHi, &spec.WorkerPortHi},
	} {
		if item.source != nil {
			*item.target = *item.source
		}
	}
	if b.Default != nil {
		spec.Default = *b.Default
	}
	return spec
}

func (s *Server) handleAdminAddDshgwNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	var body dshgwNodeBody
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.Name == nil || *body.Name == "" {
		writeAPIError(w, domain.ErrInvalidRequest("name is required"))
		return
	}
	if body.Listen == nil || *body.Listen == "" {
		writeAPIError(w, domain.ErrInvalidRequest("listen is required: the node needs the address it binds (host:port)"))
		return
	}
	if body.SSHHost == nil || *body.SSHHost == "" {
		writeAPIError(w, domain.ErrInvalidRequest("ssh_host is required: a node is deployed over ssh"))
		return
	}
	if body.SSHUser == nil || *body.SSHUser == "" {
		writeAPIError(w, domain.ErrInvalidRequest("ssh_user is required: a node is deployed over ssh"))
		return
	}
	view, err := ops.AddNode(r.Context(), body.spec(*body.Name))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_node_add", "dshgw_node", *body.Name, nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"node": view})
}

func (s *Server) handleAdminUpdateDshgwNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	var body dshgwNodeBody
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name := r.PathValue("name")
	if body.Name != nil && *body.Name != "" {
		name = *body.Name
	}
	view, err := ops.UpdateNode(r.Context(), body.spec(name))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_node_update", "dshgw_node", name, nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"node": view})
}

func (s *Server) handleAdminRemoveDshgwNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	purge := r.URL.Query().Get("purge") == "true"
	if purge && r.URL.Query().Get("confirm") != name {
		writeAPIError(w, domain.ErrInvalidRequest(
			"purge deletes the node's tenants, workspaces and state on that machine; confirm it with confirm="+name))
		return
	}
	if err := ops.RemoveNode(r.Context(), name, purge); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_node_remove", "dshgw_node", name, map[string]any{"purged": purge}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"removed": name, "purged": purge})
}

// dshgwDeployBody are one deploy's switches.
type dshgwDeployBody struct {
	AcceptHostKey         *string `json:"accept_host_key"`
	PrepareTemplateOnNode *bool   `json:"prepare_template_on_node"`
	WithPackages          *bool   `json:"with_packages"`
	RotateToken           *bool   `json:"rotate_token"`
}

func (b dshgwDeployBody) options() localdshgw.DeployOptions {
	opts := localdshgw.DeployOptions{}
	if b.AcceptHostKey != nil {
		opts.AcceptHostKey = *b.AcceptHostKey
	}
	if b.PrepareTemplateOnNode != nil {
		opts.PrepareTemplateOnNode = *b.PrepareTemplateOnNode
	}
	if b.WithPackages != nil {
		opts.WithPackages = *b.WithPackages
	}
	if b.RotateToken != nil {
		opts.RotateToken = *b.RotateToken
	}
	return opts
}

func (s *Server) handleAdminDeployDshgwNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	var body dshgwDeployBody
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
			return
		}
	}
	name := r.PathValue("name")
	status, err := ops.DeployNode(r.Context(), name, body.options())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_node_deploy", "dshgw_node", name, map[string]any{"rotated_token": body.options().RotateToken}, "accepted")
	// 202: the deploy has started, not finished. The page polls the status route for the outcome.
	writeJSON(w, http.StatusAccepted, map[string]any{"deploy": status})
}

func (s *Server) handleAdminDshgwNodeDeployStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	status, err := ops.NodeDeployStatus(r.Context(), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deploy": status})
}

func (s *Server) handleAdminRotateDshgwNodeToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	status, err := ops.RotateNodeToken(r.Context(), name, localdshgw.DeployOptions{})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_node_rotate_token", "dshgw_node", name, nil, "accepted")
	writeJSON(w, http.StatusAccepted, map[string]any{"deploy": status})
}

func (s *Server) handleAdminReconcileDshgwNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	result, err := ops.ReconcileNode(r.Context(), name)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_node_reconcile", "dshgw_node", name,
		map[string]any{"started": len(result.Started), "stopped": len(result.Stopped)}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleAdminDshgwNodeAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	ops, ok := s.dshgwNodes(w)
	if !ok {
		return
	}
	lines := 100
	if raw := r.URL.Query().Get("lines"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			writeAPIError(w, domain.ErrInvalidRequest("lines must be between 1 and 500"))
			return
		}
		lines = value
	}
	events, err := ops.NodeAudit(r.Context(), r.PathValue("name"), lines)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": events, "count": len(events)})
}

// handleAdminRestartDshgwTenant restarts a tenant's worker without touching its credential. The
// page uses it after moving a tenant or when a worker is wedged; it is deliberately not a stop+start
// pair of calls, because a console that stops and is then interrupted leaves a tenant down.
func (s *Server) handleAdminRestartDshgwTenant(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwAdmin(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := ops.RestartTenant(r.Context(), name); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_tenant_restart", "dshgw_tenant", name, nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"restarted": name})
}

// handleAdminSetDshgwTenantNode records a tenant's placement. It refuses a running tenant: the
// tenant's data lives on the old node's disk, and pretending a pointer moves it would leave an
// operator with a tenant whose files are on a machine the console no longer mentions.
func (s *Server) handleAdminSetDshgwTenantNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	ops, ok := s.dshgwAdmin(w)
	if !ok {
		return
	}
	var body struct {
		Node *string `json:"node"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.Node == nil || *body.Node == "" {
		writeAPIError(w, domain.ErrInvalidRequest("node is required (use \"local\" to move a tenant back to the control plane)"))
		return
	}
	name := r.PathValue("name")
	if err := ops.SetTenantNode(r.Context(), name, *body.Node); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dshgw_tenant_set_node", "dshgw_tenant", name,
		map[string]any{"node": *body.Node}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"tenant": name, "node": *body.Node})
}
