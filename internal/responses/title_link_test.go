package responses

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestTitlePromptFingerprint(t *testing.T) {
	prompt := "fix this issue"
	hash := func(content string, kind string) string {
		t.Helper()
		req := parseBody(t, fmt.Sprintf(`{"model":"m","prompt_cache_key":"session","input":[{"role":"user","content":%s}]}`, content))
		return req.TitlePromptFingerprint(Dimensions{Client: ClientCodex, Workspace: "/repo", SessionID: "session", CallKind: kind})
	}
	quoted := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	want := hash(quoted(prompt), CallKindAgent)
	if want == "" {
		t.Fatal("missing fingerprint")
	}
	if got := hash(quoted(codexTitleUserPrefix+"\n\nUser prompt:\n"+prompt), CallKindTitle); got != want {
		t.Fatalf("title = %q want %q", got, want)
	}
	if got := hash(quoted(prompt+"!"), CallKindAgent); got == want {
		t.Fatal("different prompt matched")
	}
	for _, content := range []string{`[{"type":"input_text","text":"fix this issue"},{"type":"input_image","image_url":"https://example.com/a.png"}]`, `""`} {
		if got := hash(content, CallKindAgent); got != "" {
			t.Fatalf("unsafe input fingerprinted: %s", got)
		}
	}
	if got := hash(quoted(codexTitleUserPrefix), CallKindTitle); got != "" {
		t.Fatal("template without prompt matched")
	}
}
