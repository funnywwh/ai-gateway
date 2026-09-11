package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// expectedAdminPatterns is the management surface as it was before the table
// existed, plus the one endpoint this milestone added (PATCH /mcp-tokens/{id}).
// Comparing this literal list against the table is what turns "every backend API"
// into a checkable claim: dropping an endpoint, renaming a path or forgetting to
// declare a new one fails the test.
var expectedAdminPatterns = []string{
	"DELETE /admin/api/v1/backups/{id}",
	"DELETE /admin/api/v1/hooks/{id}",
	"DELETE /admin/api/v1/mcp-tokens/{id}",
	"DELETE /admin/api/v1/model-mappings/{id}",
	"DELETE /admin/api/v1/portal-users/{id}",
	"DELETE /admin/api/v1/provider-models/{id}",
	"DELETE /admin/api/v1/providers/{id}",
	"DELETE /admin/api/v1/routes/{id}",
	"DELETE /admin/api/v1/tags/{id}",
	"GET /admin/api/v1/accounts",
	"GET /admin/api/v1/accounts/{id}/balance",
	"GET /admin/api/v1/accounts/{id}/credits",
	"GET /admin/api/v1/accounts/{id}/invoices",
	"GET /admin/api/v1/accounts/{id}/ledger",
	"GET /admin/api/v1/accounts/{id}/portal-users",
	"GET /admin/api/v1/audit-logs",
	"GET /admin/api/v1/auth/me",
	"GET /admin/api/v1/backups",
	"GET /admin/api/v1/backups/{id}/download",
	"GET /admin/api/v1/billing/currency",
	"GET /admin/api/v1/billing/invariants",
	"GET /admin/api/v1/billing/reconciliations",
	"GET /admin/api/v1/billing/status",
	"GET /admin/api/v1/hooks",
	"GET /admin/api/v1/invoices",
	"GET /admin/api/v1/invoices/{id}",
	"GET /admin/api/v1/keys",
	"GET /admin/api/v1/mcp-tokens",
	"GET /admin/api/v1/model-mappings",
	"GET /admin/api/v1/models",
	"GET /admin/api/v1/portal-users",
	"GET /admin/api/v1/pricing/targets",
	"GET /admin/api/v1/provider-kinds",
	"GET /admin/api/v1/provider-models",
	"GET /admin/api/v1/providers",
	"GET /admin/api/v1/providers/{id}",
	"GET /admin/api/v1/providers/{id}/actions",
	"GET /admin/api/v1/providers/{id}/logs",
	"GET /admin/api/v1/providers/{id}/models",
	"GET /admin/api/v1/redemption-codes",
	"GET /admin/api/v1/requests",
	"GET /admin/api/v1/requests/{id}",
	"GET /admin/api/v1/router/explain",
	"GET /admin/api/v1/routes",
	"GET /admin/api/v1/settings",
	"GET /admin/api/v1/stats",
	"GET /admin/api/v1/tags",
	"PATCH /admin/api/v1/accounts/{id}",
	"PATCH /admin/api/v1/keys/{id}",
	"PATCH /admin/api/v1/mcp-tokens/{id}",
	"PATCH /admin/api/v1/models/{name}",
	"PATCH /admin/api/v1/pricing/markup",
	"PATCH /admin/api/v1/providers/{id}",
	"PATCH /admin/api/v1/routes/{id}",
	"POST /admin/api/v1/accounts",
	"POST /admin/api/v1/accounts/{id}/credits",
	"POST /admin/api/v1/accounts/{id}/invoices",
	"POST /admin/api/v1/accounts/{id}/portal-users",
	"POST /admin/api/v1/auth/login",
	"POST /admin/api/v1/auth/logout",
	"POST /admin/api/v1/backups",
	"POST /admin/api/v1/backups/prune",
	"POST /admin/api/v1/backups/{id}/restore",
	"POST /admin/api/v1/billing/expire-credit",
	"POST /admin/api/v1/billing/failures/replay",
	"POST /admin/api/v1/billing/rebuild-ledger",
	"POST /admin/api/v1/billing/reconcile",
	"POST /admin/api/v1/hooks",
	"POST /admin/api/v1/invoices/{id}/{action}",
	"POST /admin/api/v1/keys",
	"POST /admin/api/v1/mcp-tokens",
	"POST /admin/api/v1/model-mappings",
	"POST /admin/api/v1/models",
	"POST /admin/api/v1/portal-users/{id}/password",
	"POST /admin/api/v1/pricing/simulate",
	"POST /admin/api/v1/pricing/validate",
	"POST /admin/api/v1/providers",
	"POST /admin/api/v1/providers/{id}/actions/{name}",
	"POST /admin/api/v1/providers/{id}/models",
	"POST /admin/api/v1/providers/{id}/models/refresh",
	"POST /admin/api/v1/providers/{id}/restart",
	"POST /admin/api/v1/providers/{id}/test",
	"POST /admin/api/v1/requests/prune",
	"POST /admin/api/v1/redemption-codes",
	"POST /admin/api/v1/redemption-codes/redeem",
	"POST /admin/api/v1/routes",
	"POST /admin/api/v1/tags",
	"PUT /admin/api/v1/settings/{key}",
}

func testServer(t *testing.T) *Server {
	t.Helper()
	return New(Deps{})
}

func TestAdminRouteTableCoversEveryEndpoint(t *testing.T) {
	s := testServer(t)
	got := map[string]bool{}
	for _, route := range s.admin {
		if got[route.pattern()] {
			t.Errorf("pattern %q is declared twice", route.pattern())
		}
		got[route.pattern()] = true
	}
	for _, want := range expectedAdminPatterns {
		if !got[want] {
			t.Errorf("management endpoint %q is missing from the route table", want)
		}
	}
	if len(s.admin) != len(expectedAdminPatterns) {
		t.Errorf("route table has %d entries, expected %d", len(s.admin), len(expectedAdminPatterns))
	}
}

func TestAdminRoutesAreRegisteredFromTheTable(t *testing.T) {
	s := testServer(t)
	registered := map[string]bool{}
	for _, pattern := range s.registered {
		registered[pattern] = true
	}
	for _, route := range s.admin {
		if !registered[route.pattern()] {
			t.Errorf("route %q was not registered on the mux", route.pattern())
		}
	}
	// 13 public routes: /v1 (5), health+ready+metrics (3), pprof block is off here,
	// so public patterns are 8 plus the admin table.
	if len(s.registered) != len(s.admin)+8 {
		t.Errorf("registered %d patterns, expected %d management entries plus 8 public routes",
			len(s.registered), len(s.admin))
	}
}

func TestAdminRouteMetadataIsComplete(t *testing.T) {
	s := testServer(t)
	names := map[string]bool{}
	for _, route := range s.admin {
		where := route.pattern()
		if route.Name == "" || !strings.HasPrefix(route.Name, "admin_") {
			t.Errorf("%s: tool name %q must be set and prefixed with admin_", where, route.Name)
		}
		if names[route.Name] {
			t.Errorf("%s: tool name %q is used twice", where, route.Name)
		}
		names[route.Name] = true
		if route.Summary == "" {
			t.Errorf("%s: summary is required (it is what the agent sees first)", where)
		}
		if route.Group == "" {
			t.Errorf("%s: group is required", where)
		}
		if route.Role != roleViewer && route.Role != roleAdmin {
			t.Errorf("%s: role %q must be viewer or admin", where, route.Role)
		}
		if route.Handler == nil {
			t.Errorf("%s: handler is required", where)
		}
		if route.Dangerous && route.ConfirmReason == "" {
			t.Errorf("%s: a dangerous endpoint must explain why it needs confirmation", where)
		}
		if route.NoTool != "" && !strings.Contains(route.NoTool, "MCP") {
			t.Errorf("%s: a hidden endpoint must name MCP in its reason", where)
		}
		assertPathParamsDeclared(t, route)
	}
}

// assertPathParamsDeclared keeps the declared parameters and the ServeMux pattern
// in sync in both directions: an undeclared {id} could not be filled by an agent,
// and a declared parameter missing from the path would silently never be used.
func assertPathParamsDeclared(t *testing.T, route adminRoute) {
	t.Helper()
	declared := map[string]bool{}
	for _, param := range route.Params {
		declared[param.Name] = true
	}
	inPath := map[string]bool{}
	for _, segment := range strings.Split(route.Path, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			inPath[strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")] = true
		}
	}
	for name := range inPath {
		if !declared[name] {
			t.Errorf("%s: path parameter %q has no declaration", route.pattern(), name)
		}
	}
	for name := range declared {
		if !inPath[name] {
			t.Errorf("%s: declared parameter %q does not appear in the path", route.pattern(), name)
		}
	}
}

func TestAdminRouteDescriptionsAreUsable(t *testing.T) {
	s := testServer(t)
	for _, route := range s.admin {
		if !route.exposed() {
			continue
		}
		detail := route.detail()
		if detail["example"] == nil {
			t.Errorf("%s: describe payload has no example", route.pattern())
		}
		if route.hasBody() && detail["body_schema"] == nil {
			t.Errorf("%s: endpoint takes a body but describe has no schema", route.pattern())
		}
		if _, err := json.Marshal(detail); err != nil {
			t.Errorf("%s: describe payload does not marshal: %v", route.pattern(), err)
		}
	}
}
