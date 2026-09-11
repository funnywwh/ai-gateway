package responses

import (
	"testing"
)

// A client can send tool types this gateway does not model: the Codex CLI ships a
// web_search tool and a multi-agent namespace by default, and it cannot be asked to
// change. The client-facing surface must keep such shapes intact for the provider layer
// to translate or drop — rejecting the request fails a call the model could serve, and
// folding the tool into the function fields produces a nameless tool.
func TestUnknownToolTypesSurviveParsing(t *testing.T) {
	const (
		webSearch = `{"type":"web_search","external_web_access":false}`
		namespace = `{"type":"namespace","name":"multi_agent_v1","description":"sub-agents","tools":[{"type":"function","name":"close_agent","parameters":{"type":"object"}}]}`
		function  = `{"type":"function","name":"bash","description":"run a command","parameters":{"type":"object"}}`
	)
	body := `{"model":"echo-model","input":"hi","tools":[` + webSearch + `,` + namespace + `,` + function + `]}`

	req, apiErr := Parse([]byte(body))
	if apiErr != nil {
		t.Fatalf("Parse rejected a shape the Responses surface allows: %v", apiErr)
	}
	out, apiErr := req.ToProviderRequest("upstream-model")
	if apiErr != nil {
		t.Fatalf("ToProviderRequest: %v", apiErr)
	}
	if len(out.Tools) != 3 {
		t.Fatalf("tools = %d, want 3: nothing dropped, nothing invented", len(out.Tools))
	}
	for i, want := range []string{webSearch, namespace} {
		if got := string(out.Tools[i].Raw); got != want {
			t.Fatalf("tools[%d].Raw = %s, want the client's own bytes %s", i, got, want)
		}
		if out.Tools[i].Name != "" {
			t.Fatalf("tools[%d] was folded into a function tool (name %q)", i, out.Tools[i].Name)
		}
	}
	if out.Tools[2].Name != "bash" {
		t.Fatalf("function tool name = %q, want bash", out.Tools[2].Name)
	}
	if len(out.Tools[2].Raw) != 0 {
		t.Fatalf("a function tool uses the structured form, got Raw %s", out.Tools[2].Raw)
	}
}

// The function-specific rule still applies: a function tool without a name is a client
// mistake the gateway can name precisely.
func TestFunctionToolsStillRequireAName(t *testing.T) {
	if _, apiErr := Parse([]byte(`{"model":"echo-model","input":"hi","tools":[{"type":"function"}]}`)); apiErr == nil {
		t.Fatal("a function tool without a name must still be rejected")
	}
}

// HasFunctionTools drives the routing capability check, so it must ignore the tool types
// no provider behind this gateway can execute.
func TestHasFunctionToolsIgnoresUnmodelledTypes(t *testing.T) {
	unknown, apiErr := Parse([]byte(`{"model":"echo-model","input":"hi","tools":[{"type":"web_search"}]}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if unknown.HasFunctionTools() {
		t.Fatal("web_search alone must not claim the tools capability")
	}
	mixed, apiErr := Parse([]byte(`{"model":"echo-model","input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"bash"}]}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !mixed.HasFunctionTools() {
		t.Fatal("a function tool must claim the tools capability")
	}
}
