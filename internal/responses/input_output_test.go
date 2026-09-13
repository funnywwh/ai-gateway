package responses

import (
	"encoding/json"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestToolOutputContentSurvivesParsingAndPluginProtocol(t *testing.T) {
	for _, kind := range []string{"custom_tool_call_output", "function_call_output"} {
		for _, output := range []string{`"gateway_input_check"`, `[{"type":"input_text","text":"Script completed"},{"type":"input_text","text":"gateway_input_check"}]`, `[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8=","detail":"auto"}]`, `[]`} {
			t.Run(kind+output, func(t *testing.T) {
				req, apiErr := Parse([]byte(`{"model":"codex","input":[{"type":"` + kind + `","call_id":"call_1","output":` + output + `}]}`))
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				canonical, apiErr := req.ToProviderRequest("codex")
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				wire, err := json.Marshal(canonical)
				if err != nil {
					t.Fatal(err)
				}
				var restored pluginapi.Request
				if err = json.Unmarshal(wire, &restored); err != nil {
					t.Fatal(err)
				}
				wire, err = json.Marshal(restored.Input[0])
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err = json.Unmarshal(wire, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["output"]) != output {
					t.Fatalf("output changed: %s", wire)
				}
			})
		}
	}
}

func TestInvalidToolOutputTypesRejected(t *testing.T) {
	for _, output := range []string{`42`, `true`, `{"text":"wrong shape"}`} {
		if _, err := Parse([]byte(`{"model":"codex","input":[{"type":"function_call_output","call_id":"call_1","output":` + output + `}]}`)); err == nil {
			t.Fatalf("accepted %s", output)
		}
	}
}
