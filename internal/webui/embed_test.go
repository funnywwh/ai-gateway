package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
