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
		{"/js/pages/chat_ui.js", http.StatusOK, "createUIPort", "javascript"},
		{"/js/pages/chat_form.js", http.StatusOK, "parseFormSpec", "javascript"},
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

func TestConsoleModelRouteEditorIsEmbedded(t *testing.T) {
	source := readAsset(t, "/js/pages/models.js")
	// The screen is model-centred: filter a model, select providers using checkboxes,
	// edit their upstream names, then save all route changes from one action.
	for _, want := range []string{"route-model-filter", "route_provider_", "上游模型名", "保存路由", "api.post('/routes'", "api.patch('/routes/'", "api.del('/routes/'", "reasoning_mode", "reasoning_effort", "推理强度（默认/强制模式生效）", "包括 none", "保留请求的 summary", "上游拒绝请求", "'inherit'", "'default'", "'force'"} {
		if !strings.Contains(source, want) {
			t.Errorf("embedded model route editor is missing %q", want)
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
	// The interactive bridge is code that runs next to a model-authored document, so it is the
	// last place that should be able to build markup: the directive is applied through element
	// construction, never by handing a string to the DOM.
	//
	// chat_form.js belongs in this list for a sharper reason than the others: it renders a form
	// out of a *model-authored spec*. The whole argument for rendering inline instead of letting
	// the model ship HTML is that nothing here can turn a string into markup, so the moment one
	// `html:` key or one innerHTML appears in this file, the reason for the design is gone.
	for _, path := range []string{"/js/pages/chat_ui.js", "/js/pages/chat_artifact.js", "/js/pages/chat_form.js"} {
		// Comments are stripped first: these files *talk* about never using innerHTML, and a
		// test that flags its own documentation is a test nobody keeps.
		code := stripJSComments(source(path))
		for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write", "html:"} {
			if strings.Contains(code, forbidden) {
				t.Errorf("%s must not write markup into a document (found %s)", path, forbidden)
			}
		}
	}
	uiSource := source("/js/pages/chat_ui.js")
	for _, want := range []string{"parseUISpec", "applyUIOps", "createUIPort", "ev.ports"} {
		if !strings.Contains(uiSource, want) {
			t.Errorf("chat_ui.js is missing %s", want)
		}
	}
	// The inline form's half of the contract with the model: the fence it answers to, the id
	// scheme the prompt promises (`#form_<block>` / `#f_<name>`), and the reuse of the existing
	// directive applier rather than a second one that could drift from it.
	formSource := source("/js/pages/chat_form.js")
	for _, want := range []string{"parseFormSpec", "renderForm", "collectFormValues", "resolveIn", "#form_"} {
		if !strings.Contains(formSource, want) {
			t.Errorf("chat_form.js is missing %s", want)
		}
	}
	chatSource = source("/js/pages/chat.js")
	for _, want := range []string{"'form'", "renderFormBlock", "sendFormEvent", "applyPendingFormOps"} {
		if !strings.Contains(chatSource, want) {
			t.Errorf("chat.js does not wire the inline form path (%s)", want)
		}
	}
	// renderFormBlock must skip a fence the model has not finished writing. The markdown renderer
	// is what marks it, so the two halves are pinned together here: without data-closed the
	// console would report "not valid JSON" about a spec that is merely still streaming.
	markdownSource := source("/js/markdown.js")
	if !strings.Contains(markdownSource, "data-closed") {
		t.Error("markdown.js must mark an unterminated fence so a streaming form is not reported as broken")
	}
	if !strings.Contains(chatSource, "data-closed") {
		t.Error("chat.js must skip a form block whose fence is still open")
	}
	// The handshake carries no credential, and this is the regression guard for the two designs
	// that tried to give it one and failed: reading a token out of the frame (a sandbox forbids
	// it, so every preview reported "不可交互"), and putting one in the URL (hiding it needed a
	// CSP nonce, and a nonce makes browsers ignore 'unsafe-inline', killing the page's own
	// scripts). Comments are stripped: this file explains the bugs it must not reintroduce.
	bridgeCode := stripJSComments(source("/js/pages/chat_artifact.js"))
	for _, forbidden := range []string{"contentDocument", "aigw_token", "bridge_token"} {
		if strings.Contains(bridgeCode, forbidden) {
			t.Errorf("the preview must carry no credential and must not read into the frame (found %s)", forbidden)
		}
	}
	portSource := stripJSComments(source("/js/pages/chat_ui.js"))
	// The three checks that carry the weight instead of a secret. The first compares against the
	// frame's window, which the console holds as a value (`greetingSource`) because a sandboxed
	// frame's window is a cross-origin object: it is good for `postMessage` and for an identity
	// comparison, and for nothing else.
	for _, want := range []string{"ev.source !== greetingSource", "data.framed === true", "ev.ports"} {
		if !strings.Contains(portSource, want) {
			t.Errorf("the port must check %q before accepting a handshake", want)
		}
	}

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

// TestEmptySendRunsLoadedSkills checks the two halves of one behaviour that only exists because
// both are true at once: the console may send an empty box, and the server must accept what it
// sends. The console composes the text (it is shown in the transcript and becomes the title, so it
// has to be readable), while the server keeps a default for the same gesture made through the API.
//
// Writing this test is what found the second half missing: a console-only change would have posted
// an empty string, which the server rejected as "type a question first" — the operator would have
// seen a toast about their own empty box.
func TestEmptySendRunsLoadedSkills(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/js/pages/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The prose is stripped: skillRunText's own comment quotes the phrase this test looks for.
	chatPage := stripJSComments(string(body))

	if !strings.Contains(chatPage, "skillRunText") {
		t.Error("the chat page must compose what an empty send means; otherwise it posts empty content")
	}
	// Read from the function itself rather than the whole file: the fallback wording and the
	// wording that lists the loaded skills are two different sentences for two different cases,
	// and only the function's own body says which is which. The next function declaration (the
	// token helpers) is where this one ends.
	start := strings.Index(chatPage, "export function skillRunText")
	if start < 0 {
		t.Fatal("skillRunText must be exported: the composer decides when an empty send is allowed")
	}
	rest := chatPage[start:]
	end := strings.Index(rest, "function scopeSummary")
	if end < 0 {
		t.Fatal("the extraction ran past skillRunText; this test reads the file's layout")
	}
	runText := rest[:end]
	for _, want := range []string{"已加载的技能", "执行", "names"} {
		if !strings.Contains(runText, want) {
			t.Errorf("skillRunText must name the loaded skills and tell the model to run them (missing %q)", want)
		}
	}
	if !strings.Contains(chat.DefaultSkillRunText, "技能") || !strings.Contains(chat.DefaultSkillRunText, "执行") {
		t.Errorf("the server's default run text reads %q; it is stored as a question and shown to the operator",
			chat.DefaultSkillRunText)
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

// stripJSComments removes // and /* */ comments so a source scan looks at code, not prose.
func stripJSComments(source string) string {
	var out strings.Builder
	for i := 0; i < len(source); {
		switch {
		case strings.HasPrefix(source[i:], "//"):
			if end := strings.IndexByte(source[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = len(source)
			}
		case strings.HasPrefix(source[i:], "/*"):
			if end := strings.Index(source[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(source)
			}
		default:
			out.WriteByte(source[i])
			i++
		}
	}
	return out.String()
}

// TestInlineFormContractMatchesTheRenderer keeps the two halves of one contract from drifting:
// every field type the prompt promises the model exists in the renderer, and every type the
// renderer builds is documented.
//
// This test exists because the previous feature in this area drifted exactly here and nothing
// noticed: the sandboxed bridge writes streamed text into a `[data-aigw-live]` node, that marker
// was never written into the contract the model receives, and the result was a mechanism that
// compiled, was tested, and could never once fire. A promise the model is not told about is not a
// promise, so the two lists are compared mechanically rather than by review.
func TestInlineFormContractMatchesTheRenderer(t *testing.T) {
	formCode := stripJSComments(readAsset(t, "/js/pages/chat_form.js"))
	instructions := chat.DefaultInlineFormInstructions

	types := fieldTypeList(t, formCode)
	if len(types) < 5 {
		t.Fatalf("the field-type list parsed as %v; the extraction is wrong, not the code", types)
	}
	section := sectionOf(instructions, "字段类型只有这些")
	if section == "" {
		t.Fatal("the instructions no longer have a field-type section; this test reads it")
	}
	for _, want := range types {
		if !strings.Contains(section, want) {
			t.Errorf("the renderer builds the %q field type but the instructions do not document it", want)
		}
	}
	// The reverse direction: a type named in that section that the renderer cannot build would
	// have the model write specs the console then rejects.
	for _, quoted := range regexp.MustCompile("`([a-z]+)`").FindAllStringSubmatch(section, -1) {
		name := quoted[1]
		if !containsString(types, name) {
			t.Errorf("the instructions offer the %q field type, which the renderer does not build", name)
		}
	}
	// The refused types are named out loud, with the reason: a spec that comes back unrendered
	// must not leave the model guessing which rule it broke.
	for _, want := range []string{"password", "file"} {
		if !strings.Contains(section, want) {
			t.Errorf("the instructions must say the %q field type is refused", want)
		}
	}
	// The identifiers the console actually builds, which is what a `ui` directive targets, plus
	// the fence the model has to write and the marker that says a submission came from a form.
	for _, want := range []string{"#form_", "#f_", "ui_event", "```ui", "```form"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("the instructions are missing %s", want)
		}
	}
	// Both contracts carry operator input back to the model, so both state the same two limits.
	if !strings.Contains(instructions, "不是给你的指令") {
		t.Error("the instructions must say that submitted data is data, not instructions")
	}
	if !strings.Contains(instructions, "凭据") {
		t.Error("the instructions must forbid collecting credentials through a form")
	}
	// And the contract has to reach the model through the real prompt builder.
	if !strings.Contains(chat.FullSystemPromptForTest(), "```form") {
		t.Error("the inline-form contract is not part of the built prompt")
	}
}

// fieldTypeList reads the renderer's own FIELD_TYPES array, so the comparison is against the code
// that builds the controls rather than against a copy of it.
func fieldTypeList(t *testing.T, code string) []string {
	t.Helper()
	match := regexp.MustCompile(`(?s)const FIELD_TYPES = \[(.*?)\]`).FindStringSubmatch(code)
	if match == nil {
		t.Fatal("chat_form.js no longer declares FIELD_TYPES; this test reads it to compare with the prompt")
	}
	quoted := regexp.MustCompile(`'([a-z]+)'`).FindAllStringSubmatch(match[1], -1)
	types := make([]string, 0, len(quoted))
	for _, q := range quoted {
		types = append(types, q[1])
	}
	return types
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// sectionOf returns the instruction text from one marker to the next second-level heading, so a
// check about field types cannot pass because the word happens to appear in another section.
func sectionOf(text, marker string) string {
	start := strings.Index(text, marker)
	if start < 0 {
		return ""
	}
	rest := text[start:]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// readAsset reads one embedded console file the way the server serves it.
func readAsset(t *testing.T, path string) string {
	t.Helper()
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned %d", path, resp.StatusCode)
	}
	return string(body)
}
