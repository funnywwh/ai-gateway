package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/domain"
)

// The interactive preview bridge is the one place where a model-authored document is allowed
// to send something back, so its tests are grouped around three questions: what makes a
// document interactive, what the injected client is allowed to be, and what a submission
// costs. The expensive half — a real model building a real form — cannot be exercised here
// (the built-in testecho provider never emits HTML) and is covered by the manual walkthrough
// recorded in docs/design/m34-ui-bridge.md.

// putArtifact uploads one preview payload and returns the response payload. The key is the
// caller's: uploading the same key twice replaces the stored row (that is how re-previewing a
// code block stays bounded), so tests that need two live previews must use two keys.
//
// An interactive upload carries the handshake token the console generated for it; tests mint one
// per call, exactly as the console does.
func (f *chatFixture) putArtifact(t *testing.T, cookie, sessionID, key, format, body string, bridge bool) map[string]any {
	t.Helper()
	token := ""
	if bridge {
		token = "tok_" + strings.ReplaceAll(key, ":", "_")
	}
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/artifacts",
		fmt.Sprintf(`{"key":%q,"format":%q,"title":"表单","bridge":%t,"bridge_token":%q,"body":%q}`,
			key, format, bridge, token, body), cookie)
	payload := decodeChatJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload preview status = %d payload=%v", resp.StatusCode, payload)
	}
	if bridge {
		payload["_token"] = token
	}
	return payload
}

// getPreview fetches one preview document and returns its status, headers and body.
func (f *chatFixture) getPreview(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	resp := f.call(t, http.MethodGet, path, "", "")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// previewPath builds the URL the console loads for one artifact. `bridge` is the query
// parameter the console adds when it opened an interactive preview; it is *not* what grants
// interactivity (the ticket's audience is), which is why the tests request every combination.
func previewPath(payload map[string]any, bridge bool) string {
	url := payload["url"].(string) + "?ticket=" + payload["ticket"].(string)
	if bridge {
		url += "&bridge=1"
	}
	return url
}

var (
	injectedToken = regexp.MustCompile(`data-token="([^"]+)"`)
	injectedNonce = regexp.MustCompile(`nonce="([^"]+)"`)
	// bridgeTagRE matches the whole injected element, so a test can remove it and compare the
	// rest of the document with what it uploaded.
	bridgeTagRE = regexp.MustCompile(`(?s)<script id="` + uiBridgeScriptID + `".*?</script>`)
)

func bridgeTokenFromBody(t *testing.T, body string) string {
	t.Helper()
	match := injectedToken.FindStringSubmatch(body)
	if match == nil {
		t.Fatal("the served document carries no handshake token")
	}
	return match[1]
}

func nonceFromBody(t *testing.T, body string) string {
	t.Helper()
	match := injectedNonce.FindStringSubmatch(body)
	if match == nil {
		t.Fatal("the served document carries no nonce")
	}
	return match[1]
}

// TestUIPreviewInjectsTheBridgeOnlyForAnInteractiveTicket pins the upgrade path: the mode is
// inside the ticket signature, so a query parameter alone can never turn a read-only preview
// into one that can reach the conversation.
func TestUIPreviewInjectsTheBridgeOnlyForAnInteractiveTicket(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	const page = "<!doctype html><html><head><title>表单</title></head><body>" +
		"<form><input name=region></form></body></html>"

	readOnly := f.putArtifact(t, cookie, sessionID, "read:0", "html", page, false)
	interactive := f.putArtifact(t, cookie, sessionID, "interactive:0", "html", page, true)
	if interactive["bridge"] != true {
		t.Fatalf("the upload did not report an interactive ticket: %v", interactive)
	}

	// Read-only: byte-for-byte the uploaded payload, no script, no nonce, no bridge header.
	status, header, body := f.getPreview(t, previewPath(readOnly, false))
	if status != http.StatusOK {
		t.Fatalf("read-only preview = %d", status)
	}
	if body != page {
		t.Fatalf("a read-only preview must serve the payload verbatim:\n%s", body)
	}
	if header.Get("X-Aigw-Bridge") != "" {
		t.Fatal("a read-only preview answered as interactive")
	}
	if strings.Contains(header.Get("Content-Security-Policy"), "nonce-") {
		t.Fatalf("a read-only preview needs no nonce: %s", header.Get("Content-Security-Policy"))
	}

	// A read-only ticket with ?bridge=1 is still read-only: the audience decides, not the URL.
	status, header, body = f.getPreview(t, previewPath(readOnly, true))
	if status != http.StatusOK {
		t.Fatalf("read-only ticket with bridge=1 = %d", status)
	}
	if strings.Contains(body, uiBridgeScriptID) || header.Get("X-Aigw-Bridge") != "" {
		t.Fatal("appending ?bridge=1 upgraded a read-only ticket")
	}

	// Interactive: the script is injected with a per-response token, and the policy carries
	// the matching nonce — the page's own inline scripts keep working because
	// 'unsafe-inline' stays, and the sandbox is unchanged.
	status, header, body = f.getPreview(t, previewPath(interactive, true))
	if status != http.StatusOK {
		t.Fatalf("interactive preview = %d", status)
	}
	if header.Get("X-Aigw-Bridge") != "1" {
		t.Fatalf("interactive preview header = %q", header.Get("X-Aigw-Bridge"))
	}
	if !strings.Contains(body, `id="`+uiBridgeScriptID+`"`) || !strings.Contains(body, uiBridgeScript()) {
		t.Fatal("the bridge script was not injected")
	}
	if !strings.Contains(body, "<form><input name=region></form>") {
		t.Fatal("the model's own document was not preserved")
	}
	if strings.Index(body, uiBridgeScriptID) > strings.Index(body, "<form>") {
		t.Fatal("injection must land in the head, before the page's own content")
	}
	token := bridgeTokenFromBody(t, body)
	// The token the page answers with is the one the console generated and sent: it has to
	// survive the round trip, because the console keeps its own copy to check the handshake
	// against (it cannot read it back out of the frame — that document is an opaque origin).
	if token != interactive["_token"] {
		t.Fatalf("the injected token %q is not the one the upload supplied (%q)", token, interactive["_token"])
	}
	if got := interactive["bridge_token"]; got != interactive["_token"] {
		t.Fatalf("the upload did not echo the token: %v", got)
	}
	csp := header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "'nonce-"+nonceFromBody(t, body)+"'") {
		t.Fatalf("the policy does not authorize the injected script: %s", csp)
	}
	for _, want := range []string{"sandbox allow-scripts", "'unsafe-inline'", "default-src 'none'", "connect-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("interactive CSP lost %q: %s", want, csp)
		}
	}

	// Each response gets its own nonce, while the handshake token is stable for one preview
	// (it belongs to the artifact, not to a response) — which is what lets a reloaded frame
	// answer with the token the console still holds.
	_, _, second := f.getPreview(t, previewPath(interactive, true))
	if nonceFromBody(t, second) == nonceFromBody(t, body) {
		t.Fatal("two responses shared a nonce")
	}
	if bridgeTokenFromBody(t, second) != token {
		t.Fatal("reloading a preview changed its handshake token")
	}
	// A different preview gets a different token: it identifies one page, not a deployment.
	third := f.putArtifact(t, cookie, sessionID, "interactive:2", "html", page, true)
	if third["_token"] == interactive["_token"] {
		t.Fatal("two previews were issued the same handshake token")
	}
	_, _, thirdBody := f.getPreview(t, previewPath(third, true))
	if bridgeTokenFromBody(t, thirdBody) != third["_token"] {
		t.Fatal("the third preview did not get its own token")
	}

	// The ticket names one artifact: this session's other block is a different payload, and a
	// ticket signed for this one must not open it.
	other := f.putArtifact(t, cookie, sessionID, "interactive:1", "html",
		"<html><head></head><body><form><input name=other></form></body></html>", true)
	if other["id"] == interactive["id"] {
		t.Fatal("the two uploads were expected to be distinct blocks")
	}
	mismatched := "/admin/chat-artifact/" + other["id"].(string) +
		"?ticket=" + interactive["ticket"].(string) + "&bridge=1"
	if status, _, _ := f.getPreview(t, mismatched); status != http.StatusNotFound {
		t.Fatalf("a ticket for another artifact was honoured: %d", status)
	}
	// The other block's own ticket does work, so the refusal above is about the audience and
	// not about that block being unreachable.
	if status, _, _ := f.getPreview(t, previewPath(other, true)); status != http.StatusOK {
		t.Fatalf("the second block's own ticket did not work: %d", status)
	}
}

// TestUIPreviewBridgeCanBeTurnedOffDeploymentWide pins the operator's kill switch: with
// chat.ui_bridge_enabled=false there is no way to obtain an interactive ticket, an
// already-issued one stops being honoured, and the model is not taught the contract.
func TestUIPreviewBridgeCanBeTurnedOffDeploymentWide(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	plain := f.putArtifact(t, cookie, sessionID, "read:0", "html", "<h1>hi</h1>", false)
	interactive := f.putArtifact(t, cookie, sessionID, "interactive:0", "html", "<form></form>", true)

	off := *f.cfg
	off.Chat.UIBridgeEnabled = false
	f.api.deps.Config = &off

	refused := f.call(t, http.MethodPost,
		"/admin/api/v1/chat/sessions/"+sessionID+"/artifacts/"+plain["id"].(string)+"/ticket",
		`{"bridge":true}`, cookie)
	refused.Body.Close()
	if refused.StatusCode != http.StatusBadRequest {
		t.Fatalf("interactive ticket with the bridge off = %d, want 400", refused.StatusCode)
	}
	upload := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/artifacts",
		`{"key":"b:0","format":"html","bridge":true,"body":"<form></form>"}`, cookie)
	upload.Body.Close()
	if upload.StatusCode != http.StatusBadRequest {
		t.Fatalf("interactive upload with the bridge off = %d, want 400", upload.StatusCode)
	}

	// A ticket issued while the bridge was on stops being interactive: the serving side
	// checks too, so switching the feature off does not leave live capabilities behind.
	status, header, body := f.getPreview(t, previewPath(interactive, true))
	if status != http.StatusOK {
		t.Fatalf("read-only fallback = %d", status)
	}
	if strings.Contains(body, uiBridgeScriptID) || header.Get("X-Aigw-Bridge") != "" {
		t.Fatal("an interactive ticket was still honoured after the bridge was switched off")
	}
}

// TestUIPreviewBridgeScriptIsNotANetworkClient pins what the injected client may do. It has
// one job — talk to the console over a MessagePort — and any line that could fetch, evaluate
// or write markup would be a new attack surface inside a model-authored document.
func TestUIPreviewBridgeScriptIsNotANetworkClient(t *testing.T) {
	script := uiBridgeScript()
	for _, forbidden := range []string{
		"fetch(", "XMLHttpRequest", "eval(", "new Function", "innerHTML", "outerHTML",
		"insertAdjacentHTML", "document.write", "localStorage", "sessionStorage", "document.cookie",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the bridge script must not use %s", forbidden)
		}
	}
	for _, required := range []string{
		"'hello'", "'port'", "ev.ports", "addEventListener('submit'", "data-aigw-send",
		"FormData", "postMessage",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("the bridge script is missing %s", required)
		}
	}
	// It is concatenated into arbitrary documents, so it must not depend on syntax an odd
	// parser might refuse.
	for _, forbidden := range []string{"=>", "${", "let ", "const "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the bridge script must stay ES5 (found %s)", forbidden)
		}
	}
}

func TestInjectUIBridgeIsIdempotentAndPlacesItselfEarly(t *testing.T) {
	const page = "<script>var a=1</script>"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"head", "<!doctype html><html><head><title>t</title></head><body>" + page + "</body></html>", "<title>"},
		{"html without head", "<html><body>" + page + "</body></html>", "<body>"},
		{"fragment", "<div>" + page + "</div>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := injectUIBridge(tc.body, "tok", "non")
			if !strings.Contains(out, `data-token="tok"`) || !strings.Contains(out, `nonce="non"`) {
				t.Fatalf("the injected tag is missing its attributes: %s", out)
			}
			if stripped := bridgeTagRE.ReplaceAllString(out, ""); stripped != tc.body {
				t.Fatalf("injection rewrote the document:\n got %s\nwant %s", stripped, tc.body)
			}
			if tc.want != "" && strings.Index(out, uiBridgeScriptID) > strings.Index(out, tc.want) {
				t.Fatalf("the bridge was injected after %s", tc.want)
			}
			if again := injectUIBridge(out, "tok2", "non2"); again != out {
				t.Fatal("injecting twice must be a no-op")
			}
		})
	}
}

// TestUIPreviewUploadUsesTheKeyAsItsIdentity documents the rule one of these tests first
// tripped over: (session_id, key) is the identity of a stored preview, so uploading the same
// block again replaces the body in place — the row keeps its id, the answer keeps naming that
// id, and the URL a previously opened tab holds keeps working (and now shows the new bytes).
// A silent divergence here is expensive: the client builds the preview URL out of the id it
// was answered with, so an id the row does not have is a preview that 404s.
func TestUIPreviewUploadUsesTheKeyAsItsIdentity(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)

	first := f.putArtifact(t, cookie, sessionID, "msg_1:0", "html", "<h1>one</h1>", false)
	second := f.putArtifact(t, cookie, sessionID, "msg_1:0", "html", "<h1>two</h1>", false)
	if first["id"] != second["id"] {
		t.Fatalf("re-uploading a block changed its id: %v then %v", first["id"], second["id"])
	}
	status, _, body := f.getPreview(t, previewPath(second, false))
	if status != http.StatusOK || !strings.Contains(body, "two") {
		t.Fatalf("the re-uploaded preview is not the live one: %d %s", status, body)
	}
	if status, _, body := f.getPreview(t, previewPath(first, false)); status != http.StatusOK || !strings.Contains(body, "two") {
		t.Fatalf("the URL from the first upload must keep resolving: %d %s", status, body)
	}

	// A different key is a different block, with its own row and its own live URL.
	third := f.putArtifact(t, cookie, sessionID, "msg_1:1", "html", "<h1>three</h1>", false)
	if third["id"] == first["id"] {
		t.Fatal("two blocks shared a row")
	}
	if status, _, body := f.getPreview(t, previewPath(third, false)); status != http.StatusOK || !strings.Contains(body, "three") {
		t.Fatalf("a second block did not keep its own preview: %d %s", status, body)
	}
}

// TestUIPreviewTicketCarriesAnAudience pins the ticket shape: four fields, the last one being
// the audience. The pre-M34 three-field payload has no audience, so honouring it would mean
// guessing whether a preview may talk back.
func TestUIPreviewTicketCarriesAnAudience(t *testing.T) {
	f := newChatFixture(t)
	now := time.Now().UTC()

	signed := f.api.chatSigner.sign("art_1", "sess_1", "art_1", now.Add(time.Minute))
	session, scope, ok := f.api.chatSigner.verify(signed, "art_1", now)
	if !ok || session != "sess_1" || scope != "art_1" {
		t.Fatalf("verify = (%q, %q, %v)", session, scope, ok)
	}

	// A three-field payload (what M32 signed) does not verify any more.
	legacy := legacyTicket(t, f.api.chatSigner.key, "art_1", "sess_1", now.Add(time.Minute))
	if _, _, ok := f.api.chatSigner.verify(legacy, "art_1", now); ok {
		t.Fatal("a pre-M34 ticket without an audience was accepted")
	}
	// Neither does a wrong audience for the artifact being read.
	if _, _, ok := f.api.chatSigner.verify(signed, "art_2", now); ok {
		t.Fatal("a ticket verified against another artifact")
	}
}

// legacyTicket builds the exact payload M32 signed: artifact|session|expiryNanos.
func legacyTicket(t *testing.T, key []byte, artifactID, sessionID string, expires time.Time) string {
	t.Helper()
	payload := artifactID + "|" + sessionID + "|" + fmt.Sprintf("%d", expires.UnixNano())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// TestUIEventIsBilledLikeAnyOtherQuestion pins the money path: a submission from a generated
// interface is a normal turn. It is not a new kind of request, and it is not free.
func TestUIEventIsBilledLikeAnyOtherQuestion(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)

	// Exactly what the console sends for a form submission.
	content := "表单提交：目标设备信息\n\n```json\n" +
		`{"source":"ui_event","event":"submit","data":{"region":"cn-north-1","tier":"pro"}}` + "\n```"
	status, body := f.runTurn(t, cookie, sessionID, "turn_uiev", content)
	if status != http.StatusOK {
		t.Fatalf("ui_event turn status = %d body=%s", status, body)
	}
	if !strings.Contains(body, "event: done") {
		t.Fatalf("the turn did not finish: %s", body)
	}

	deadline := time.Now().Add(5 * time.Second)
	var logged []*domain.RequestLogRecord
	for time.Now().Before(deadline) {
		logged = f.requestLogs(t)
		if len(logged) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(logged) == 0 {
		t.Fatal("a ui_event turn wrote no request log row")
	}
	row := logged[0]
	if row.Client != "console" || row.SessionID != sessionID {
		t.Fatalf("ui_event row = %+v", row)
	}
	// The submitted values stay out of the global log, exactly like a typed question.
	if row.RequestJSON != "" || row.ResponseText != "" || row.ResponseReasoning != "" {
		t.Fatalf("the submission was recorded globally: %+v", row)
	}

	// The conversation keeps the submission verbatim: the next turn's context has to contain
	// what the user actually entered.
	detail := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions/"+sessionID, "", cookie))
	messages, _ := detail["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d", len(messages))
	}
	user, _ := messages[0].(map[string]any)
	if got, _ := user["content"].(string); !strings.Contains(got, `"region":"cn-north-1"`) {
		t.Fatalf("the form values were not kept: %q", got)
	}
	if assistant, ok := messages[1].(map[string]any); !ok {
		t.Fatal("no assistant message")
	} else if _, ok := assistant["charge_micros"]; !ok {
		t.Fatal("the ui_event step was not costed")
	}

	usage, err := f.db.RequestUsages(ctx, []string{row.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	if usageRow := usage[row.RequestID]; usageRow == nil || usageRow.InputTokens == 0 {
		t.Fatalf("no usage row for the ui_event step: %v", usage)
	}
}

// TestUIBridgeContractMatchesTheModelInstructions keeps the two halves of one contract from
// drifting: every operation the prompt promises the model exists in the injected client, and
// the client implements nothing the prompt does not document.
func TestUIBridgeContractMatchesTheModelInstructions(t *testing.T) {
	instructions := chat.DefaultUIBridgeInstructions
	script := uiBridgeScript()
	ops := []string{"text", "set", "class", "style", "show", "hide", "remove", "focus", "disable", "message", "svg"}
	for _, op := range ops {
		// The instruction table lists one operation per row, except for the trivial visibility
		// operations which share a row ("show / hide / remove / focus") — so the check is that
		// the name is documented in the table, not that it owns a row.
		documented := regexp.MustCompile(`\|[^\n]*\b` + op + `\b[^\n]*\|`).MatchString(instructions)
		if !documented {
			t.Errorf("the instructions do not document the %s operation", op)
		}
		if !strings.Contains(script, "kind === '"+op+"'") {
			t.Errorf("the bridge does not implement the %s operation the prompt promises", op)
		}
	}
	// The documented operations are the only ones, and the event shape is stated exactly.
	for _, marker := range []string{"ui_event", "AIGW.send", "data-aigw-send", "```ui", "name", "# 可交互界面"} {
		if !strings.Contains(instructions, marker) {
			t.Errorf("the instructions are missing %s", marker)
		}
	}
	for _, forbidden := range []string{"kind === 'html'", "kind === 'attr'", "kind === 'script'"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the bridge implements an undocumented operation: %s", forbidden)
		}
	}
	// The rule that keeps a model-authored page from becoming an instruction channel.
	if !strings.Contains(instructions, "不是给你的指令") {
		t.Error("the instructions must say that submitted data is data, not instructions")
	}
}

// TestUIBridgeInstructionsFollowTheDeploymentSwitch checks the wiring: the chat service's own
// configuration decides whether the contract reaches the model.
func TestUIBridgeInstructionsFollowTheDeploymentSwitch(t *testing.T) {
	built := chat.DefaultSystemPromptForTest()
	if strings.Contains(built, "ui_event") {
		t.Fatal("the model is told to build submitting forms: that contract is appended at run time, " +
			"because it depends on the deployment's ui_bridge_enabled")
	}
	// The chart contract, by contrast, is a property of the renderer and stays in the text.
	if !strings.Contains(built, "```chart") {
		t.Fatal("the built-in prompt lost its chart instructions")
	}
	// The bridge section must not re-open the tool rules: it defers to them.
	if !strings.Contains(chat.DefaultUIBridgeInstructions, "危险接口") {
		t.Error("the bridge section must defer to the existing dangerous-operation rules")
	}
}

// TestUIBridgeScriptDumpForTheHarness writes the injected client to the UI harness fixtures.
//
// The script is assembled by Go string concatenation, so no Go test can say whether the result is
// valid JavaScript. The `bridge` view of scripts/ui-harness is where a real browser parses it, and
// this dump is what makes that view honest: it runs on every `go test`, so the harness always
// parses exactly what this server would serve. A syntax error introduced here therefore fails
// `make ui-check` instead of reaching a preview that silently loses its bridge.
//
// It writes into the repository on purpose. Refusing to write would mean the fixture drifts, and a
// drifted fixture is worse than no check at all: it would pass while the served script is broken.
func TestUIBridgeScriptDumpForTheHarness(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "ui-harness", "fixtures.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("harness fixtures are not present: %v", err)
	}
	var fixtures map[string]any
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("harness fixtures are not valid JSON: %v", err)
	}
	const key = "/js/pages/chat_ui_bridge_script.js"
	script := uiBridgeScript()
	if current, _ := fixtures[key].(string); current == script {
		return
	}
	fixtures[key] = script
	out, err := json.MarshalIndent(fixtures, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("rewriting the harness fixtures failed: %v", err)
	}
	t.Logf("refreshed %s in scripts/ui-harness/fixtures.json (%d bytes)", key, len(script))
}

// TestUIPreviewBridgeTokenRoundTrip pins the fix for the bug this feature shipped with: the
// console used to read the handshake token out of the frame's document, which a sandbox forbids
// (a document without allow-same-origin is an opaque origin, so `frame.contentDocument` is null
// for the parent). Every interactive preview therefore reported "不可交互".
//
// The token now travels the other way — the console generates it, the upload carries it, the
// server injects it — so this test asserts all three ends agree, and that an interactive upload
// without one is refused rather than served with a channel nobody can authenticate.
func TestUIPreviewBridgeTokenRoundTrip(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	const page = "<html><head></head><body><form><input name=x></form></body></html>"

	// No token: refused, and for a reason the operator can act on.
	noToken := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/artifacts",
		`{"key":"notoken:0","format":"html","bridge":true,"body":"<form></form>"}`, cookie)
	payload := decodeChatJSON(t, noToken)
	if noToken.StatusCode != http.StatusBadRequest {
		t.Fatalf("an interactive upload without a token = %d, payload=%v", noToken.StatusCode, payload)
	}
	if !strings.Contains(fmt.Sprint(payload), "bridge_token") {
		t.Fatalf("the refusal does not name the missing field: %v", payload)
	}

	// With a token: echoed to the console and injected into the document.
	up := f.putArtifact(t, cookie, sessionID, "roundtrip:0", "html", page, true)
	status, _, body := f.getPreview(t, previewPath(up, true))
	if status != http.StatusOK {
		t.Fatalf("interactive preview = %d", status)
	}
	if got := bridgeTokenFromBody(t, body); got != up["_token"] {
		t.Fatalf("served token = %q, console holds %q", got, up["_token"])
	}
	// The injected client reads the token from its own location, which is the one place another
	// document cannot look — so the URL must carry it under the parameter the script looks for.
	if !strings.Contains(body, "aigw_token") {
		t.Fatal("the injected client no longer looks for the token in its own location")
	}
	if !strings.Contains(uiBridgeScript(), uiBridgeTokenParam) {
		t.Fatalf("the injected client must read %q out of its own URL", uiBridgeTokenParam)
	}

	// Re-opening through the ticket endpoint hands back the same token, so a reloaded frame and
	// the console still agree without re-uploading the payload.
	fresh := f.call(t, http.MethodPost,
		"/admin/api/v1/chat/sessions/"+sessionID+"/artifacts/"+up["id"].(string)+"/ticket",
		`{"bridge":true}`, cookie)
	freshPayload := decodeChatJSON(t, fresh)
	if freshPayload["bridge_token"] != up["_token"] {
		t.Fatalf("re-ticketing changed the token: %v", freshPayload["bridge_token"])
	}
}
