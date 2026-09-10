package ids

import (
	"strings"
	"testing"
)

func TestPrefixAndLength(t *testing.T) {
	got := Response()
	if !strings.HasPrefix(got, "resp_") {
		t.Fatalf("prefix missing: %q", got)
	}
	if len(got) != len("resp_")+24 {
		t.Fatalf("unexpected length %d for %q", len(got), got)
	}
}

func TestUniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := Request()
		if seen[id] {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = true
	}
}

func TestKnownPrefixes(t *testing.T) {
	cases := map[string]string{
		APIKey():         "sk-gw_",
		MCPToken():       "aigw_mcp_",
		FunctionCall():   "fc_",
		Reasoning():      "rs_",
		Session():        "sess_",
		RedemptionCode(): "gwrc_",
	}
	for got, want := range cases {
		if !strings.HasPrefix(got, want) {
			t.Errorf("id %q does not start with %q", got, want)
		}
	}
}
