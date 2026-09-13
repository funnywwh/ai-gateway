package domain

import "testing"

func TestParseModelReasoning(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *ModelReasoning
		err  bool
	}{
		{"empty", "", nil, false},
		{"null", "null", nil, false},
		{"valid default", `{"mode":"default","effort":"medium"}`, &ModelReasoning{Mode: "default", Effort: "medium"}, false},
		{"valid force max", `{"mode":"force","effort":"max"}`, &ModelReasoning{Mode: "force", Effort: "max"}, false},
		{"unknown field", `{"mode":"force","effort":"high","other":true}`, nil, true},
		{"bad mode", `{"mode":"inherit","effort":"high"}`, nil, true},
		{"bad effort", `{"mode":"force","effort":"ultra"}`, nil, true},
		{"array", `[]`, nil, true},
		{"trailing document", `{"mode":"force","effort":"high"} {}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseModelReasoning(tc.raw)
			if tc.err {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
