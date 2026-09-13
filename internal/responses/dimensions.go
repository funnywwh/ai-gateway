package responses

import (
	"encoding/json"
	"strings"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// The identity vocabulary recorded on every request log row. Client says which coding
// agent sent the request; CallKind says whether the request is the agent's own turn or
// one of its auxiliary calls.
const (
	ClientDSH   = "dsh"
	ClientCodex = "codex"
	// ClientConsole is the gateway's own management console (the smart-chat page). It is a
	// first-class client because its traffic is billed like any other: an operator asking
	// "which client spent this money" must be able to see the console next to the agents.
	ClientConsole = "console"
	ClientUnknown = "unknown"

	CallKindAgent = "agent"
	CallKindTitle = "title"
)

// The markers below were read off real captured traffic from this deployment (see
// docs/design/m27-request-dimensions.md §2), which is why each one is matched only in the
// position it actually appears in. Matching them against the whole body would misclassify
// requests: 259 of the 265 locally recorded rows that mention "Codex CLI" are DSH requests
// whose *tool output* happened to contain the phrase.
const (
	// Codex puts its system prompt in the top-level instructions field, never in a message.
	codexInstructionPrefix = "You are a coding agent running in the Codex CLI"
	// Captured from gpt001's Codex task-title request (#966). This is a user
	// message, not the agent's instructions, and can request JSON title/description.
	codexTitleUserPrefix = "You are a helpful assistant. You will be presented with a user prompt, and your job is to provide a short title for a task that will be created from that prompt."
	// Codex opens its first user message with the environment context block.
	codexEnvContextTag         = "<environment_context>"
	codexCwdElement            = "cwd"
	codexWorkspaceRootsElement = "workspace_roots"

	// DSH sends its system prompt as the first developer (once: system) message.
	dshDeveloperPrefix = "You are an AI agent powered by DeepSeek Harness."
	// DSH's session-title call: the instruction prompt plus the request that carries the
	// human messages. Only the text prefix is stable — the role changed from system to
	// developer between DSH versions.
	dshTitleSystemPrefix = "Create a concise title for an AI coding-assistant session"
	dshTitleUserPrefix   = "Generate the session title from this JSON array of human messages:"
	// Older DSH exposes the workspace in sandbox snapshots; newer versions also
	// include it in developer instructions, independently of the sandbox mode.
	dshWorkspaceMarker        = "session workspace: "
	dshWorkingDirectoryMarker = "Your working directory is "
	dshRuntimePrefix          = "Current runtime context."
)

// Bounds on what a single row may carry. These are metadata, not content: they only have
// to be long enough to identify the thing they name.
const (
	maxWorkspaceBytes = 512
	maxSessionIDBytes = 128
	maxTitleRunes     = 200
)

// Dimensions is the identity a request log row records about its request, extracted from
// the parsed request rather than from the stored body: the body is truncated at 1 MiB and
// pruned by retention, so reading it back later is not a reliable source.
//
// Every field degrades to "" / ClientUnknown rather than failing: a request that cannot be
// identified is still a request somebody has to be able to see.
type Dimensions struct {
	Client    string // dsh | codex | unknown
	Workspace string // absolute workspace root, "" when the client did not send one
	SessionID string // explicit root session identity, falling back to prompt_cache_key
	CallKind  string // agent | title
}

// SessionKey is the bounded prompt cache key used by sticky routing. LogSessionKey
// may identify a different conversation; neither method changes the upstream cache key.
func (r *Request) SessionKey() string {
	return clampBytes(r.PromptCacheKey, maxSessionIDBytes)
}

// Dimensions identifies the caller behind one request. clientHint is the User-Agent,
// used only as a fallback: the body rules above are the ones verified against real
// traffic, and a client that lies about its User-Agent still gets identified by what it
// actually sent.
func (r *Request) Dimensions(clientHint string) Dimensions {
	sessionID, titleHint := r.logSession()
	out := Dimensions{
		Client:    ClientUnknown,
		CallKind:  CallKindAgent,
		SessionID: sessionID,
	}

	items, apiErr := r.Items()
	if apiErr != nil {
		items = nil
	}

	var firstInstruction, envContext, runtimeContext, workingDirectory string
	titleCall := false
	codexTitleCall := false
	for _, item := range items {
		if item.Type != "message" {
			continue
		}
		text := itemText(item)
		if text == "" {
			continue
		}
		if item.Role == "developer" || item.Role == "system" {
			if firstInstruction == "" {
				firstInstruction = text
			}
			if path := dshWorkingDirectory(text); path != "" {
				workingDirectory = path
			}
		}
		if envContext == "" && strings.HasPrefix(text, codexEnvContextTag) {
			envContext = text
		}
		// A snapshot supersedes earlier snapshots, even when its policy carries
		// no path. Never read quoted policy text from assistant messages.
		if item.Role == "user" && strings.HasPrefix(text, dshRuntimePrefix) {
			runtimeContext = text
		}
		if strings.HasPrefix(text, dshTitleUserPrefix) {
			titleCall = true
		}
		if item.Role == "user" && strings.HasPrefix(text, codexTitleUserPrefix) {
			codexTitleCall = true
		}
	}
	if titleCall || codexTitleCall || titleHint {
		out.CallKind = CallKindTitle
	}

	switch {
	case codexTitleCall || titleHint:
		out.Client = ClientCodex
	case strings.HasPrefix(strings.TrimSpace(r.Instructions), codexInstructionPrefix):
		out.Client = ClientCodex
	case envContext != "":
		out.Client = ClientCodex
	case strings.HasPrefix(firstInstruction, dshDeveloperPrefix):
		out.Client = ClientDSH
	case out.CallKind == CallKindTitle:
		// The title prompts are DSH's own; a title call is a DSH call.
		out.Client = ClientDSH
	default:
		out.Client = clientFromHint(clientHint)
	}

	switch out.Client {
	case ClientCodex:
		out.Workspace = clampBytes(codexWorkspace(envContext), maxWorkspaceBytes)
	case ClientDSH:
		path := dshWorkspace(runtimeContext)
		if path == "" {
			path = workingDirectory
		}
		out.Workspace = clampBytes(path, maxWorkspaceBytes)
	}
	return out
}

// clientFromHint maps a User-Agent onto the client vocabulary. DSH sends
// "deepseek-harness/<version> (+https://github.com/deepseek-ai/deepseek-harness)"; Codex
// sends originator/version headers instead on its built-in provider, and a user-defined
// provider entry may send nothing at all — which is exactly why this is only a fallback.
//
// The console's own requests set "aigw-console/<version>" server-side (they never come
// from a browser), so this marker is reliable for them in a way a client-supplied header
// would not be; nothing security-relevant is decided from it either way.
func clientFromHint(hint string) string {
	hint = strings.ToLower(strings.TrimSpace(hint))
	switch {
	case hint == "":
		return ClientUnknown
	case strings.Contains(hint, "deepseek-harness"), strings.Contains(hint, "dsh/"):
		return ClientDSH
	case strings.Contains(hint, "codex"):
		return ClientCodex
	case strings.Contains(hint, "aigw-console"):
		return ClientConsole
	default:
		return ClientUnknown
	}
}

// codexWorkspace reads the cwd out of a Codex environment context block, falling back to
// the first workspace root.
func codexWorkspace(envContext string) string {
	if path := elementText(envContext, codexCwdElement); path != "" {
		return path
	}
	if roots := elementText(envContext, codexWorkspaceRootsElement); roots != "" {
		if path := elementText(roots, "root"); path != "" {
			return path
		}
	}
	return ""
}

// dshWorkspace reads the path out of the sandbox policy line. The value is written with
// JSON.stringify, so it arrives as a quoted JSON string and is unquoted rather than
// trimmed.
func dshWorkspace(runtimeContext string) string {
	idx := strings.Index(runtimeContext, dshWorkspaceMarker)
	if idx < 0 {
		return ""
	}
	rest := runtimeContext[idx+len(dshWorkspaceMarker):]
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	var path string
	if err := json.NewDecoder(strings.NewReader(rest)).Decode(&path); err != nil {
		return ""
	}
	return path
}

// dshWorkingDirectory reads the dedicated developer instruction, not the
// implementation checkout path (which can be unrelated to the session workspace).
func dshWorkingDirectory(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, dshWorkingDirectoryMarker) {
			return strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, dshWorkingDirectoryMarker)), ".")
		}
	}
	return ""
}

// TitleOf turns the model's answer to a title call into a title: one line, no surrounding
// whitespace, bounded. A title that cannot be read this way stays empty rather than being
// stored as something it is not.
func TitleOf(text string) string {
	return clampRunes(strings.Join(strings.Fields(text), " "), maxTitleRunes)
}

// CodexTitleOf accepts both plain titles and Codex's structured title/description
// output. Malformed objects must not become visible titles containing raw JSON.
func CodexTitleOf(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "{") {
		var result struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			return ""
		}
		return TitleOf(result.Title)
	}
	return TitleOf(text)
}

// elementText returns the text between <name> and </name>, or "" when the element is
// absent or unclosed. name is the bare element name ("cwd", not "<cwd>").
func elementText(body, name string) string {
	open := "<" + name + ">"
	close := "</" + name + ">"
	start := strings.Index(body, open)
	if start < 0 {
		return ""
	}
	rest := body[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// itemText flattens one message item to text. Content is either a plain string (the shape
// DSH uses for its developer message) or an array of typed parts (both clients, for user
// messages).
func itemText(item pluginapi.Item) string {
	raw := strings.TrimSpace(string(item.Content))
	if raw == "" || raw == "null" {
		return ""
	}
	if strings.HasPrefix(raw, "[") {
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &parts); err != nil {
			return ""
		}
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		return ""
	}
	return text
}

// clampBytes bounds a byte string without splitting a rune.
func clampBytes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8Start(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

// clampRunes bounds a string by characters: a title is prose, and cutting it mid-rune
// would put invalid UTF-8 in the database.
func clampRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit]))
}

// utf8Start reports whether b can start a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
