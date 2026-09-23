package main

import (
	"context"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/activity"
)

// TenantRow is one tenant as the operator sees it: the record, plus the two things a record cannot
// answer on its own (is the worker process running right now, and when did somebody last log in).
//
// The console and `dshgw tenant list` render the same type on purpose: a field that only one of
// them shows is a field an operator will believe is missing.
type TenantRow struct {
	Name       string `json:"name"`
	PublicPort int    `json:"public_port"`
	WorkerPort int    `json:"worker_port"`
	KeyPrefix  string `json:"key_prefix"`
	// Node is where this tenant's worker runs (M77); empty means this machine.
	Node string `json:"node"`
	// Running is the worker *process* state; Suspended is the operator's durable intent. Both
	// matter: a tenant can be running while suspended (until the next start) and stopped without
	// being suspended (a crash).
	Running         bool   `json:"running"`
	Suspended       bool   `json:"suspended"`
	PID             int    `json:"pid"`
	Handshake       string `json:"handshake"`
	Account         string `json:"account,omitempty"`
	LastLogin       string `json:"last_login,omitempty"`
	DirectoryPicker string `json:"directory_picker"`
	BrowserFS       string `json:"browser_fs"`
	ModelsPending   bool   `json:"models_pending"`
	UID             int    `json:"uid"`
	User            string `json:"user"`
	Isolation       string `json:"isolation"`
	DshHome         string `json:"dsh_home"`
	Workspace       string `json:"workspace"`
	CreatedAt       string `json:"created_at"`
	PortalURL       string `json:"portal_url"`
	TenantURL       string `json:"tenant_url"`
}

// tenantRows builds the rows, optionally asking each worker whether it is running. withStatus is
// what makes it cheap enough for a console page that polls: the systemd query is the expensive part.
func tenantRows(ctx context.Context, deps *runtimeDeps, withStatus bool) []TenantRow {
	rows := make([]TenantRow, 0)
	for _, tenant := range deps.reg.List() {
		remote := !deps.cfg.IsLocalNode(tenant.Node)
		row := TenantRow{
			Name: tenant.Name, PublicPort: tenant.PublicPort, WorkerPort: tenant.WorkerPort,
			KeyPrefix: tenant.KeyPrefix, Node: tenant.Node, Suspended: tenant.Suspended,
			Handshake: string(tenant.Handshake), Account: tenant.Account,
			DirectoryPicker: tenant.DirectoryPicker, BrowserFS: tenant.PluginBrowserFS,
			ModelsPending: tenant.ModelsPending, UID: tenant.UID,
			User: deps.cfg.Deploy.WorkerUser, Isolation: tenant.EffectiveIsolation(),
			DshHome: tenant.DshHome, Workspace: tenant.Workspace,
			CreatedAt: tenant.CreatedAt.UTC().Format(time.RFC3339Nano),
			PortalURL: deps.cfg.WithTrailingSlash(deps.cfg.OriginForPort(deps.cfg.PortalPort)),
			TenantURL: deps.cfg.WithTrailingSlash(deps.cfg.OriginForPort(tenant.PublicPort)),
		}
		if remote {
			// A remote tenant's worker state belongs to that machine; a pid read here would be this
			// machine's, which is exactly the misleading answer the console must not show.
			row.PID = 0
		}
		if last, err := (&activity.Store{Path: deps.cfg.ActivityPath}).LastLogin(tenant.Name); err == nil && !last.IsZero() {
			row.LastLogin = last.Format(time.RFC3339)
		}
		if withStatus {
			status, _ := deps.manager.Status(ctx, tenant)
			row.Running, row.PID = status.Running, status.PID
		}
		rows = append(rows, row)
	}
	return rows
}
