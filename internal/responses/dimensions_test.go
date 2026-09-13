package responses

import (
	"fmt"
	"strings"
	"testing"
)

// The bodies below are trimmed versions of requests captured from this deployment (M27's
// design document names the rows). They keep the exact prefixes, roles and field
// positions the extractor keys on, because those positions are the whole point: a rule
// that matched the same strings anywhere in the body would misread real traffic.

func TestDimensionsReadsDSHAgentRequest(t *testing.T) {
	req := parseBody(t, `{
		"model":"deepseek-flash",
		"prompt_cache_key":"session-ebf36761-3295-4881-bbec-73f80c9a4589",
		"input":[
			{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness.\n\nThe DeepSeek Harness implementation checkout is at /home/winger/.local/dsh-0.1.2-rc.1/."},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"重构请求日志"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nCurrent DSH file policy: workspace-write. Any available operation enforced by the DSH file sandbox may modify files under the session workspace: \"/home/winger/work/ai_gateway\". Some platform temporary areas may also be writable."}]}
		]
	}`)

	got := req.Dimensions("")
	if got.Client != ClientDSH || got.CallKind != CallKindAgent {
		t.Fatalf("client/call_kind = %q/%q, want dsh/agent", got.Client, got.CallKind)
	}
	if got.SessionID != "session-ebf36761-3295-4881-bbec-73f80c9a4589" {
		t.Fatalf("session_id = %q", got.SessionID)
	}
	if got.Workspace != "/home/winger/work/ai_gateway" {
		t.Fatalf("workspace = %q", got.Workspace)
	}
}

// DSH's sandbox policy only names the workspace in workspace-write mode, and the path is
// written with JSON.stringify — so it arrives quoted and must be unquoted, not trimmed.
func TestDimensionsReadOnlyDSHHasNoWorkspace(t *testing.T) {
	req := parseBody(t, `{
		"model":"deepseek-flash",
		"input":[
			{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness."},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Current runtime context. Current DSH file policy: read-only. Any available operation enforced by the DSH file sandbox cannot modify files in the standing mode."}]}
		]
	}`)

	got := req.Dimensions("")
	if got.Client != ClientDSH {
		t.Fatalf("client = %q, want dsh", got.Client)
	}
	if got.Workspace != "" {
		t.Fatalf("read-only policy carries no workspace, got %q", got.Workspace)
	}
}

// The title prompt travels with a system message in older DSH builds and a developer
// message in current ones; only the text is stable.
func TestDimensionsReadsDSHTitleCallInBothRoleVariants(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		req := parseBody(t, `{
			"model":"deepseek-flash",
			"max_output_tokens":64,
			"prompt_cache_key":"session-d680bd79-6be1-426b-a91b-fc232e0f80c3",
			"input":[
				{"role":"`+role+`","content":"Create a concise title for an AI coding-assistant session from the supplied human messages.\nReturn only the title on one line."},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"Generate the session title from this JSON array of human messages:\n[{\"seq\":7,\"text\":\"看看能不能从dsh,codex的请求里解析出workspace\"}]"}]}
			]
		}`)

		got := req.Dimensions("")
		if got.CallKind != CallKindTitle {
			t.Fatalf("role %s: call_kind = %q, want title", role, got.CallKind)
		}
		if got.Client != ClientDSH {
			t.Fatalf("role %s: client = %q, want dsh", role, got.Client)
		}
		if got.SessionID != "session-d680bd79-6be1-426b-a91b-fc232e0f80c3" {
			t.Fatalf("role %s: session_id = %q", role, got.SessionID)
		}
	}
}

func TestDimensionsReadsCodexRequest(t *testing.T) {
	req := parseBody(t, `{
		"model":"deepseek-flash",
		"prompt_cache_key":"01a08f27-cc56-7491-abf3-c5db92e442d9",
		"instructions":"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"<skills_instructions>\n## Skills\nA skill is a set of local instructions.\n</skills_instructions>"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/home/winger/work/ai_gateway</cwd>\n  <shell>bash</shell>\n  <filesystem><workspace_roots><root>/home/winger/work/ai_gateway</root></workspace_roots></filesystem>\n</environment_context>"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"用一句话回答：1+1 等于几？"}]}
		]
	}`)

	got := req.Dimensions("")
	if got.Client != ClientCodex || got.CallKind != CallKindAgent {
		t.Fatalf("client/call_kind = %q/%q, want codex/agent", got.Client, got.CallKind)
	}
	if got.Workspace != "/home/winger/work/ai_gateway" {
		t.Fatalf("workspace = %q", got.Workspace)
	}
	if got.SessionID != "01a08f27-cc56-7491-abf3-c5db92e442d9" {
		t.Fatalf("session_id = %q", got.SessionID)
	}
}

// Newer Codex builds can send workspace roots without a cwd element.
func TestDimensionsFallsBackToCodexWorkspaceRoots(t *testing.T) {
	req := parseBody(t, `{
		"model":"m",
		"instructions":"You are a coding agent running in the Codex CLI.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<filesystem><workspace_roots><root>/srv/app</root></workspace_roots></filesystem>\n</environment_context>"}]}]
	}`)

	if got := req.Dimensions(""); got.Workspace != "/srv/app" {
		t.Fatalf("workspace = %q, want the first workspace root", got.Workspace)
	}
}

// The measured false positive this design exists to avoid: a DSH request whose tool
// output quoted another project's Codex prompt and an environment context block. Nothing
// outside the structural positions may be read.
func TestDimensionsIgnoresMarkersInsideToolOutput(t *testing.T) {
	req := parseBody(t, `{
		"model":"deepseek-flash",
		"input":[
			{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness."},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"把 codex 的请求格式整理一下"}]},
			{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"file_path\":\"docs/codex.md\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"You are a coding agent running in the Codex CLI\n<environment_context><cwd>/etc/leaked</cwd></environment_context>"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"读到了"}]}
		]
	}`)

	got := req.Dimensions("")
	if got.Client != ClientDSH {
		t.Fatalf("client = %q, want dsh: a tool output must not identify the client", got.Client)
	}
	if got.Workspace != "" {
		t.Fatalf("workspace = %q, want empty: a tool output must not supply the workspace", got.Workspace)
	}
	if got.CallKind != CallKindAgent {
		t.Fatalf("call_kind = %q, want agent", got.CallKind)
	}
}

// An agent request must never be classified as a title call just because it quoted the
// title prompt inside a message body rather than starting with it.
func TestDimensionsTitlePrefixMustBeAtTheStart(t *testing.T) {
	req := parseBody(t, `{
		"model":"m",
		"input":[
			{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness."},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"解释这段代码：Generate the session title from this JSON array of human messages: [...]"}]}
		]
	}`)

	if got := req.Dimensions(""); got.CallKind != CallKindAgent {
		t.Fatalf("call_kind = %q, want agent", got.CallKind)
	}
}

func TestDimensionsFallBackToUserAgentHint(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":"ping"}`)

	if got := req.Dimensions("deepseek-harness/0.1.2 (+https://github.com/deepseek-ai/deepseek-harness)"); got.Client != ClientDSH {
		t.Fatalf("client = %q, want dsh from the User-Agent hint", got.Client)
	}
	if got := req.Dimensions("codex_cli_rs/0.50.0"); got.Client != ClientCodex {
		t.Fatalf("client = %q, want codex from the User-Agent hint", got.Client)
	}
	// The console's own steps set this agent server-side, so "console" is how an operator
	// finds the conversations they paid for in the request log next to the agents.
	if got := req.Dimensions("aigw-console/1"); got.Client != ClientConsole {
		t.Fatalf("client = %q, want console from the User-Agent hint", got.Client)
	}
	if got := req.Dimensions("curl/8.5.0"); got.Client != ClientUnknown {
		t.Fatalf("client = %q, want unknown", got.Client)
	}
	if got := req.Dimensions(""); got.Client != ClientUnknown {
		t.Fatalf("client = %q, want unknown", got.Client)
	}
}

// The string shorthand is a normal request shape; it must degrade cleanly.
func TestDimensionsOnStringInputAndUnparseableBody(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":"ping"}`)
	got := req.Dimensions("")
	if got.Client != ClientUnknown || got.CallKind != CallKindAgent || got.Workspace != "" {
		t.Fatalf("dimensions = %+v", got)
	}

	broken := &Request{Model: "m", Input: []byte(`{"not":"an item array"}`)}
	if got := broken.Dimensions(""); got.Client != ClientUnknown || got.SessionID != "" {
		t.Fatalf("unparseable input must not invent identity: %+v", got)
	}
}

func TestDimensionsBoundsEveryField(t *testing.T) {
	long := strings.Repeat("a", 4096)
	req := &Request{
		Model:          "m",
		Instructions:   codexInstructionPrefix,
		PromptCacheKey: long,
		Input: []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/` +
			long + `</cwd></environment_context>"}]}]`),
	}

	got := req.Dimensions("")
	if len(got.SessionID) != maxSessionIDBytes {
		t.Fatalf("session_id length = %d, want %d", len(got.SessionID), maxSessionIDBytes)
	}
	if len(got.Workspace) != maxWorkspaceBytes {
		t.Fatalf("workspace length = %d, want %d", len(got.Workspace), maxWorkspaceBytes)
	}
}

// SessionKey is what session stickiness indexes by, while SessionID is what the request log
// records. Without explicit conversation metadata both fall back to the same bounded
// cache key; explicit root identity is tested separately in session_test.go.
func TestSessionKeyMatchesRecordedDimensionWithoutExplicitMetadata(t *testing.T) {
	long := strings.Repeat("a", 4096)
	for name, raw := range map[string]string{
		"plain":      "session-ebf36761",
		"padded":     "  session-ebf36761  ",
		"empty":      "",
		"whitespace": "   ",
		"overlong":   long,
	} {
		req := &Request{Model: "m", PromptCacheKey: raw}
		if got, want := req.SessionKey(), req.Dimensions("").SessionID; got != want {
			t.Errorf("%s: SessionKey() = %q, Dimensions().SessionID = %q", name, got, want)
		}
		if len(req.SessionKey()) > maxSessionIDBytes {
			t.Errorf("%s: session key length = %d, want <= %d", name, len(req.SessionKey()), maxSessionIDBytes)
		}
	}
	if got := (&Request{Model: "m", PromptCacheKey: "  session-ebf36761  "}).SessionKey(); got != "session-ebf36761" {
		t.Fatalf("padding must not become part of the key: %q", got)
	}
}

func TestTitleOfCollapsesLinesAndBoundsRunes(t *testing.T) {
	if got := TitleOf("  从 dsh 和 codex 请求解析 workspace  \n"); got != "从 dsh 和 codex 请求解析 workspace" {
		t.Fatalf("title = %q", got)
	}
	if got := TitleOf("first line\nsecond line"); got != "first line second line" {
		t.Fatalf("title = %q", got)
	}
	if got := TitleOf(""); got != "" {
		t.Fatalf("title = %q, want empty", got)
	}
	got := TitleOf(strings.Repeat("字", 500))
	if runes := []rune(got); len(runes) != maxTitleRunes {
		t.Fatalf("title runes = %d, want %d", len(runes), maxTitleRunes)
	}
	// Clamping must not split a multi-byte rune into invalid UTF-8.
	if !strings.ContainsRune(got, '字') || strings.Contains(got, "\uFFFD") {
		t.Fatalf("title was cut mid-rune: %q", got)
	}
}

// The title helper has its own session key; never replace it with a guessed parent.
func TestDimensionsCodexTitle(t *testing.T) {
	for _, tc := range []struct {
		name, role, text string
		title            bool
	}{
		{"user", "user", codexTitleUserPrefix + "\nGenerate a concise UI title (up to 36 characters) for this task.", true},
		{"quoted", "user", "Explain this prompt: " + codexTitleUserPrefix, false},
		{"assistant", "assistant", codexTitleUserPrefix, false},
		{"developer", "developer", codexTitleUserPrefix, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"m","prompt_cache_key":"title-helper-session","input":[{"role":%q,"content":%q}]}`, tc.role, tc.text)
			got := parseBody(t, body).Dimensions("")
			if (got.CallKind == CallKindTitle) != tc.title {
				t.Fatalf("dimensions = %+v", got)
			}
			if tc.title && got.Client != ClientCodex {
				t.Fatalf("client = %q", got.Client)
			}
			if got.SessionID != "title-helper-session" {
				t.Fatalf("session = %q", got.SessionID)
			}
		})
	}
}

func TestCodexTitleOf(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"title":" 修复 Codex 标题识别 ","description":"不要记录这个描述"}`, "修复 Codex 标题识别"},
		{"  普通标题\n", "普通标题"},
		{`{"description":"missing title"}`, ""},
		{`{"title":123}`, ""},
		{`{"title":"incomplete`, ""},
		{`{"title":null}`, ""},
		{`{"title":"` + strings.Repeat("字", 201) + `"}`, strings.Repeat("字", 200)},
	} {
		if got := CodexTitleOf(tc.input); got != tc.want {
			t.Errorf("CodexTitleOf(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
