package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	// Test-only import: the console assets and the server's accepted values are two
	// halves of one contract, so the test needs the server's list. Production code in
	// this package still imports nothing from the module (guarded by internal/arch).
	"github.com/winger/ai-gateway/internal/config"
)

func TestConsoleAssetsAreEmbedded(t *testing.T) {
	if AssetCount() < 8 {
		t.Fatalf("expected the console assets to be embedded, got %d files", AssetCount())
	}
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	cases := []struct {
		path       string
		wantStatus int
		wantSubstr string
		wantType   string
	}{
		{"/", http.StatusOK, "<div id=\"app\">", "text/html"},
		{"/index.html", http.StatusOK, "AI Gateway 控制台", "text/html"},
		{"/app.css", http.StatusOK, "--accent", "text/css"},
		{"/js/app.js", http.StatusOK, "renderShell", "javascript"},
		{"/js/pages/keys.js", http.StatusOK, "record_output_text", "javascript"},
		{"/providers", http.StatusOK, "<div id=\"app\">", "text/html"},
		{"/js/pages/missing.js", http.StatusNotFound, "", ""},
	}
	for _, tc := range cases {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("GET %s status = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
			continue
		}
		if tc.wantSubstr != "" && !strings.Contains(string(body), tc.wantSubstr) {
			t.Errorf("GET %s body does not contain %q", tc.path, tc.wantSubstr)
		}
		if tc.wantType != "" && !strings.Contains(resp.Header.Get("Content-Type"), tc.wantType) {
			t.Errorf("GET %s content-type = %q, want %q", tc.path, resp.Header.Get("Content-Type"), tc.wantType)
		}
	}
}

func TestConsoleSendsCSP(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	policy := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "default-src 'self'") {
		t.Fatalf("missing content security policy: %q", policy)
	}
}

// TestConsoleRecordingModesMatchTheServer pins the per-key recording select to the values
// the server accepts. The console used to offer "meta" while the backend only understood
// "metadata", so the choice was stored as an unknown mode and silently ignored.
func TestConsoleRecordingModesMatchTheServer(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/js/pages/keys.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	source := string(body)

	want := append([]string{"inherit"}, config.RecordingInputModes...)
	for _, mode := range want {
		if !strings.Contains(source, "value: '"+mode+"'") {
			t.Errorf("the console must offer %q (accepted by the server)", mode)
		}
	}
	if strings.Contains(source, "value: 'meta'") {
		t.Error(`the console must not offer "meta": the server only understands "metadata"`)
	}
}

// TestConsoleDoesNotSuggestDeadSettingKeys keeps the settings page honest: a suggested key
// with no reader is a change that silently does nothing.
func TestConsoleDoesNotSuggestDeadSettingKeys(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/js/pages/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	source := string(body)
	if !strings.Contains(source, "'billing.fx_rates'") {
		t.Fatal("the FX table is a live setting and must stay reachable from the console")
	}
	for _, dead := range []string{"'recording.default'", "'recording.max_bytes'", "'mcp.max_query_rows'", "'backup.retention'"} {
		if strings.Contains(source, dead) {
			t.Errorf("%s has no reader; suggesting it makes a no-op look like configuration", dead)
		}
	}
}
