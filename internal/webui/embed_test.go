package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	// Test-only import: the console assets and the server's accepted values are two
	// halves of one contract, so the test needs the server's list. Production code in
	// this package still imports nothing from the module (guarded by internal/arch).
	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/store"
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
		{"/js/pages/chat.js", http.StatusOK, "chat/sessions", "javascript"},
		{"/js/pages/skills.js", http.StatusOK, "chat/skills", "javascript"},
		{"/js/pages/chat_artifact.js", http.StatusOK, "allow-scripts", "javascript"},
		{"/js/markdown.js", http.StatusOK, "renderMarkdown", "javascript"},
		{"/js/chart.js", http.StatusOK, "renderChart", "javascript"},
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

// TestConsoleDimensionOptionsMatchTheServer pins the request log's grouping select to the
// server's whitelist. The same shape as the recording-mode test above: a dimension the
// console offers but the server rejects is not a cosmetic mismatch, it is a control that
// answers 400, and one the server gained but the console never offers is invisible.
func TestConsoleDimensionOptionsMatchTheServer(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/js/pages/requests.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	source := string(body)

	if len(store.RequestLogDimensionNames) == 0 {
		t.Fatal("the store's dimension whitelist is empty; the console reads nothing to offer")
	}
	for _, name := range store.RequestLogDimensionNames {
		if !strings.Contains(source, "['"+name+"',") {
			t.Errorf("the console must offer the %q grouping (accepted by the server)", name)
		}
	}
	// The two credential dimensions are the ones this milestone added; naming them here
	// keeps the test honest if the whitelist is ever emptied by accident.
	for _, name := range []string{"account", "api_key"} {
		if !strings.Contains(source, "['"+name+"',") {
			t.Errorf("the console must offer the %q grouping", name)
		}
	}
}

// TestConsoleSortOptionsMatchTheServer pins the request log's statistics sort select to the
// server's whitelist and to its order. It is the same contract as the dimension test above,
// with one extra: the console's first entry is what the select opens on, so it has to be
// the server's default — a list with the right members in the wrong order would open the
// page on a sort nobody chose.
func TestConsoleSortOptionsMatchTheServer(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/js/pages/requests.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	source := string(body)

	if len(store.RequestLogDimensionSorts) == 0 {
		t.Fatal("the store's sort whitelist is empty; the console reads nothing to offer")
	}
	if store.RequestLogDimensionSorts[0] != store.RequestLogDimensionDefaultSort {
		t.Fatalf("the store's whitelist must list its default first: %v", store.RequestLogDimensionSorts)
	}
	const marker = "const STATS_SORTS = ["
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatal("the console must declare the sorts it offers")
	}
	block := source[start+len(marker):]
	if end := strings.Index(block, "];"); end > 0 {
		block = block[:end]
	}
	var offered []string
	for _, match := range regexp.MustCompile(`\['([a-z_]+)',`).FindAllStringSubmatch(block, -1) {
		offered = append(offered, match[1])
	}
	if !reflect.DeepEqual(offered, store.RequestLogDimensionSorts) {
		t.Fatalf("the console offers %v, the server accepts %v", offered, store.RequestLogDimensionSorts)
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

// TestConsoleChartBoundsMatchTheServer pins the chart renderer's two limits to the numbers
// the server's instructions promise the model. They are the same contract as the enum tests
// above, with a sharper failure mode: if the renderer's cap were lower than the prompt's, a
// model that followed the instructions would produce charts the console refuses to draw, and
// the answer would silently fall back to a code block.
func TestConsoleChartBoundsMatchTheServer(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/js/chart.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	source := string(body)

	if !strings.Contains(source, "export const MAX_SERIES = "+itoa(chat.MaxChartSeries)) {
		t.Errorf("the console must cap series at the server's %d", chat.MaxChartSeries)
	}
	if !strings.Contains(source, "export const MAX_POINTS = "+itoa(chat.MaxChartPoints)) {
		t.Errorf("the console must cap points at the server's %d", chat.MaxChartPoints)
	}
	// The prompt itself has to state those numbers, otherwise the contract is only half
	// written down.
	if prompts := chat.DefaultSystemPromptForTest(); !strings.Contains(prompts, itoa(chat.MaxChartSeries)) ||
		!strings.Contains(prompts, itoa(chat.MaxChartPoints)) {
		t.Error("the chat instructions must state the chart bounds the renderer enforces")
	}
}

// TestConsoleChatRoutesMatchTheServer keeps the console's API paths in step with the route
// table: a page that calls a path the server does not register renders an error the
// operator cannot act on, and it is invisible until someone opens that page.
func TestConsoleChatRoutesMatchTheServer(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	source := func(path string) string {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}
	chatSource := source("/js/pages/chat.js")
	skillsSource := source("/js/pages/skills.js")
	for _, want := range []string{"/chat/sessions", "/chat/models", "/skill-draft", "/turns"} {
		if !strings.Contains(chatSource, want) {
			t.Errorf("the chat page no longer calls %s", want)
		}
	}
	for _, want := range []string{"/chat/skills"} {
		if !strings.Contains(skillsSource, want) {
			t.Errorf("the skills page no longer calls %s", want)
		}
	}
	// The preview upload and the ticket endpoint are a pair: uploading without a ticket
	// would leave the frame unable to load anything.
	artifactSource := source("/js/pages/chat_artifact.js")
	if !strings.Contains(artifactSource, "/artifacts") || !strings.Contains(artifactSource, "ticket") {
		t.Error("the preview helper must upload the payload and then use the ticket it gets back")
	}
	// A route the router knows but nobody registered is a 404 the operator sees as a blank
	// page; this asserts the two new pages are both reachable by hash.
	routerSource := source("/js/router.js")
	for _, path := range []string{"'/chat'", "'/skills'"} {
		if !strings.Contains(routerSource, "path: "+path) {
			t.Errorf("the router does not offer %s", path)
		}
	}
}

func itoa(v int) string { return strconv.Itoa(v) }
