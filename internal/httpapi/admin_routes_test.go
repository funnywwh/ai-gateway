package httpapi

import (
	"encoding/json"
	"fmt"
	"slices"
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
	"GET /admin/api/v1/requests/dimensions",
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
	"PATCH /admin/api/v1/tags/{id}",
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
	"POST /admin/api/v1/keys/import",
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
	"PUT /admin/api/v1/org/nodes/{id}/accounts",

	// Console chat (M32). Every one of these is NoTool: a conversation and a skill library
	// belong to a logged-in administrator account, which an MCP token does not have.
	"GET /admin/api/v1/chat/sessions",
	"POST /admin/api/v1/chat/sessions",
	"GET /admin/api/v1/chat/sessions/{id}",
	"PATCH /admin/api/v1/chat/sessions/{id}",
	"DELETE /admin/api/v1/chat/sessions/{id}",
	"POST /admin/api/v1/chat/sessions/{id}/turns",
	"POST /admin/api/v1/chat/sessions/{id}/skill-draft",
	"POST /admin/api/v1/chat/sessions/{id}/artifacts",
	"POST /admin/api/v1/chat/sessions/{id}/artifacts/{art}/ticket",
	"GET /admin/api/v1/chat/models",
	"GET /admin/api/v1/chat/skills",
	"GET /admin/api/v1/org/nodes",
	"GET /admin/api/v1/org/nodes/{id}/accounts",
	"POST /admin/api/v1/chat/skills",
	"POST /admin/api/v1/org/nodes",
	"PATCH /admin/api/v1/chat/skills/{id}",
	"PATCH /admin/api/v1/org/nodes/{id}",
	"DELETE /admin/api/v1/chat/skills/{id}",
	"DELETE /admin/api/v1/org/nodes/{id}",
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
	// Public routes: /v1 (5), health+ready+metrics+version (4) and the sandboxed preview
	// document (1). pprof is off in this fixture, so public patterns are 10 plus the
	// admin table.
	if len(s.registered) != len(s.admin)+10 {
		t.Errorf("registered %d patterns, expected %d management entries plus 10 public routes",
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

// TestGeneratedExamplesSatisfyTheirSchema is the other half of "a description is the interface":
// the example admin_describe hands an agent is meant to be copied, so an example that violates
// the schema next to it teaches a request that cannot succeed.
//
// It also keeps the bounds honest. A minimum the handler does not enforce is a bound a model
// will trust for nothing; here every number in a body example has to sit inside the range the
// route declares, and every declared bound is checked against the placeholder the generator
// produces.
func TestGeneratedExamplesSatisfyTheirSchema(t *testing.T) {
	s := testServer(t)
	for _, route := range s.admin {
		for _, field := range route.Body {
			if field.Schema == nil {
				continue
			}
			value := sampleValue(field)
			if problems := checkExampleValue(field.Schema, value, route.pattern()+"."+field.Name); len(problems) > 0 {
				t.Errorf("%s: the generated example does not satisfy its own schema:\n  %s\n"+
					"an agent copies this example verbatim (standard: docs/mcp.md §4.5)",
					route.pattern()+"."+field.Name, strings.Join(problems, "\n  "))
			}
		}
		// The body the describe payload renders has to satisfy the schema that same payload
		// publishes: a field-level check alone would miss a body assembled wrongly (an empty
		// object where a number belongs, a declared shape replaced by {}).
		if declared := route.bodySchema(); declared != nil && len(route.Body) > 0 {
			if problems := checkExampleValue(declared, route.sampleBody(), route.pattern()); len(problems) > 0 {
				t.Errorf("%s: the example body does not satisfy the body schema admin_describe publishes:\n  %s",
					route.pattern(), strings.Join(problems, "\n  "))
			}
		}
	}
}

// checkValue reports every way value violates schema. It implements the subset the route table
// actually uses (type/minimum/maximum/enum/items/properties/required) so the assertion stays
// readable; it is not a general JSON Schema validator and does not need to be.
func checkExampleValue(schema map[string]any, value any, where string) []string {
	var problems []string
	if schema == nil {
		return nil
	}
	if enum, ok := schema["enum"].([]string); ok && len(enum) > 0 {
		text, _ := value.(string)
		if !slices.Contains(enum, text) {
			problems = append(problems, fmt.Sprintf("%s: %q is not one of %v", where, text, enum))
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		if !slices.Contains(enum, value) {
			problems = append(problems, fmt.Sprintf("%s: %v is not one of %v", where, value, enum))
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: expected an object, the example is %T", where, value))
			return problems
		}
		if schema["additionalProperties"] == false {
			properties, _ := schema["properties"].(map[string]any)
			for key := range object {
				if _, declared := properties[key]; !declared && len(properties) > 0 {
					problems = append(problems, fmt.Sprintf("%s.%s: the example sets a field the schema does not "+
						"declare, and the endpoint rejects unknown fields", where, key))
				}
			}
		}
		if required, ok := schema["required"].([]string); ok {
			// "Required unless <condition>" is the one condition this table needs and JSON
			// Schema's flat `required` cannot express; the schema declares it explicitly (see
			// pricingRuleSetSchema) instead of leaving the checker to guess from prose.
			exempt := map[string]bool{}
			if unless, ok := schema["x-rules-required-unless"].([]string); ok {
				for _, condition := range unless {
					key, want, found := strings.Cut(condition, "=")
					if found && valueString(object[key]) == want {
						exempt["rules"] = true
					}
				}
			}
			for _, key := range required {
				if _, present := object[key]; present || exempt[key] {
					continue
				}
				problems = append(problems, fmt.Sprintf("%s: the example omits required field %q", where, key))
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, raw := range properties {
			sub, _ := raw.(map[string]any)
			if sub == nil {
				continue
			}
			if inner, present := object[key]; present {
				problems = append(problems, checkExampleValue(sub, inner, where+"."+key)...)
			}
		}
	case "array":
		items, _ := value.([]any)
		if _, ok := value.([]any); !ok {
			problems = append(problems, fmt.Sprintf("%s: expected an array, the example is %T", where, value))
			return problems
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for i, item := range items {
			problems = append(problems, checkExampleValue(itemSchema, item, fmt.Sprintf("%s[%d]", where, i))...)
		}
	case "integer", "number":
		number, ok := numeric(value)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: expected a number, the example is %T", where, value))
			return problems
		}
		if typ == "integer" && number != float64(int(number)) {
			problems = append(problems, fmt.Sprintf("%s: expected an integer, the example is %v", where, number))
		}
		if minimum, ok := numeric(schema["minimum"]); ok && number < minimum {
			problems = append(problems, fmt.Sprintf("%s: the example is %v, below the declared minimum %v", where, number, minimum))
		}
		if maximum, ok := numeric(schema["maximum"]); ok && number > maximum {
			problems = append(problems, fmt.Sprintf("%s: the example is %v, above the declared maximum %v", where, number, maximum))
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: expected a string, the example is %T", where, value))
			return problems
		}
		if minimum, ok := numeric(schema["minLength"]); ok && float64(len(text)) < minimum {
			problems = append(problems, fmt.Sprintf("%s: the example is shorter than the declared minLength %v", where, minimum))
		}
	}
	return problems
}

// valueString renders an example value for comparison against a declared condition.
func valueString(value any) string {
	text, _ := value.(string)
	return text
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

// TestStructuredBodyFieldsCarryTheirShape is the mechanical version of the rule in
// docs/mcp.md §4.5: a body field an agent cannot read the shape of is a field the agent
// cannot write.
//
// It exists because of a real failure. admin_upsert_provider_model declared pricing_rules as
// {"type":"object"} with a description of "成本侧计价规则" and an example of {}, and an
// operator's request to configure a cost price ended with the model refusing to write
// anything: pricing.ParseRuleSet rejects unknown fields, so guessing a field name is a 400,
// and there was nothing to read instead of guessing. The model was right; the description was
// not. bodySchema now panics on this shape, and this test is what makes the panic a
// *deliberate* one rather than a surprise at runtime.
func TestStructuredBodyFieldsCarryTheirShape(t *testing.T) {
	s := testServer(t)
	for _, route := range s.admin {
		for _, field := range route.Body {
			where := route.pattern() + "." + field.Name
			if strings.TrimSpace(field.Desc) == "" {
				t.Errorf("%s: no description; write what the value means, its unit and its default "+
					"(standard: docs/mcp.md §4.5)", where)
			}
			if field.Type == "object" && field.Schema == nil {
				t.Errorf("%s: declared as an object with no Schema, so an agent sees {\"type\":\"object\"} "+
					"and cannot write it; give it a shape with schemaField(...) or declare the whole body "+
					"with RawBody (standard: docs/mcp.md §4.5)", where)
			}
			// A structured field with an invented example is the other half of the same defect:
			// sampleBody renders {} for an object, and copying {} is not a call anyone can make.
			if (field.Type == "object" || field.Type == "array") && field.Example == nil && field.Schema == nil {
				t.Errorf("%s: no example and no schema, so describe can only offer a placeholder "+
					"(standard: docs/mcp.md §4.5)", where)
			}
		}
	}
}

// TestBodyFieldsAreDocumentedInTheCatalogue checks the compact view an agent sees first: a
// body field that admin_endpoints does not name costs the agent an admin_describe call just to
// learn whether this endpoint could be the one it needs.
func TestBodyFieldsAreDocumentedInTheCatalogue(t *testing.T) {
	s := testServer(t)
	for _, route := range s.admin {
		row := route.summaryRow()
		listed, _ := row["body_fields"].([]string)
		if len(listed) != len(route.Body) {
			t.Errorf("%s: body_fields lists %d of %d declared body fields",
				route.pattern(), len(listed), len(route.Body))
		}
	}
}
