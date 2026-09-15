package responses

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Codex remote compaction v2 support.
//
// Compaction v2 is not an endpoint: it is an ORDINARY POST /v1/responses whose input ends
// with a trigger item, after which the client demands exactly one `compaction` output item
// (codex-rs core/src/compact_remote_v2.rs, collect_compaction_output). Codex decides that a
// provider supports it from the provider's NAME ("OpenAI"), so every Codex pointed at this
// gateway sends trigger turns — including the routes served by upstreams that have no
// compaction of their own (deepseek and every other OpenAI-compatible endpoint). Those
// upstreams answer the turn like a normal chat, the client finds zero compaction items and
// fails the whole thread with
//
//	remote compaction v2 expected exactly one compaction output item, got 0 from N output items
//
// The gateway therefore acts as the compaction backend for them: it asks the upstream for a
// handoff summary, and mints the one output item the client wants itself. What it mints has
// to be readable again on the NEXT turn, because the client replays that item in the input
// of every later request — an item no upstream can read would silently delete the compacted
// history instead of compressing it.
//
// Hence the envelope: `gw1:` + base64(UTF-8 summary). The client treats encrypted_content as
// an opaque string it must replay verbatim, and the prefix makes the blob self-identifying, so
// only summaries this gateway wrote are decoded back into text (see decodeCompactionSummary).
// Blobs that are NOT ours are left untouched: a real OpenAI blob is unreadable here but
// readable by the upstream that minted it, so rewriting it would break the native path.
const (
	// ItemTypeCompactionTrigger is the trigger item Codex appends to the input
	// (openai/codex PR #22809).
	ItemTypeCompactionTrigger = "compaction_trigger"
	// ItemTypeCompaction is the output item the client counts; exactly one is required.
	ItemTypeCompaction = "compaction"
	// ItemTypeCompactionSummary is Codex's serde alias for ItemTypeCompaction.
	ItemTypeCompactionSummary = "compaction_summary"
	// ItemTypeContextCompaction is the trigger shape used before PR #22809 (a
	// context_compaction item WITHOUT encrypted_content), and also a native output item type.
	ItemTypeContextCompaction = "context_compaction"

	// CompactionEnvelopePrefix marks a summary this gateway minted.
	CompactionEnvelopePrefix = "gw1:"
)

// CompactionPrompt is codex-rs prompts/templates/compact/prompt.md, verbatim. Codex uses it
// for LOCAL compaction, i.e. this is the instruction the model producing the handoff summary
// is trained to answer; sending our own wording would produce a differently shaped summary.
const CompactionPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`

// CompactionSummaryPrefix is codex-rs prompts/templates/compact/summary_prefix.md, verbatim.
// Codex writes its own local summary into history as a user message with this prefix; a
// summary this gateway replays has to look the same for the model to read it as history.
const CompactionSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

// IsCompactionRequest reports whether this input is a Codex remote-compaction turn.
//
// Both trigger shapes count: `{"type":"compaction_trigger"}` (current Codex) and
// `{"type":"context_compaction"}` with no encrypted_content (the shape before PR #22809).
// A `context_compaction` item that CARRIES content is history, not a trigger.
func IsCompactionRequest(items []pluginapi.Item) bool {
	for i := range items {
		if isCompactionTrigger(items[i]) {
			return true
		}
	}
	return false
}

func isCompactionTrigger(item pluginapi.Item) bool {
	switch item.Type {
	case ItemTypeCompactionTrigger:
		return true
	case ItemTypeContextCompaction:
		return encryptedContent(item) == ""
	default:
		return false
	}
}

// PrepareCompactionRequest rewrites a provider request for a compaction turn.
//
// Three changes, all of them about making the upstream answer a summarization request with
// prose instead of continuing the task:
//   - the trigger item is dropped (it is an instruction to the CLIENT's compact task, not
//     something any upstream models);
//   - tools are removed, mirroring what Codex sends for local compaction (Prompt::default()
//     carries no tools). Leaving them in hands the model a way to answer this turn with a
//     tool call — which produces no summary text at all;
//   - the summarization prompt is appended as the last user message, again mirroring Codex.
func PrepareCompactionRequest(req *pluginapi.Request) {
	if req == nil {
		return
	}
	input := make([]pluginapi.Item, 0, len(req.Input)+1)
	for _, item := range req.Input {
		if isCompactionTrigger(item) {
			continue
		}
		input = append(input, item)
	}
	input = append(input, userMessage(CompactionPrompt))
	req.Input = input
	req.Tools = nil
	req.ToolChoice = nil
	req.ParallelToolCalls = nil
}

// LocalizeCompactionItems replaces the compaction items an upstream cannot read with the
// text they stand for, and leaves everything else exactly as it is.
//
// Only this gateway's own envelope (`gw1:`) is decoded: it is the summary of a history the
// client no longer sends in full, so dropping it would delete that history from the model's
// view. A foreign blob (a real OpenAI encrypted_content) is passed through untouched — the
// upstream that minted it may still understand it, and a note saying "history was compacted"
// would be strictly worse than the blob for that upstream while being no better for ours.
func LocalizeCompactionItems(items []pluginapi.Item) []pluginapi.Item {
	var out []pluginapi.Item
	for i, item := range items {
		if !isCompactionFamily(item.Type) {
			continue
		}
		summary, ok := DecodeCompactionSummary(encryptedContent(item))
		if !ok {
			continue
		}
		if out == nil {
			out = append(make([]pluginapi.Item, 0, len(items)), items...)
		}
		out[i] = userMessage(CompactionSummaryPrefix + "\n" + summary)
	}
	if out == nil {
		return items
	}
	return out
}

// isCompactionFamily reports whether an item type belongs to the compaction family: the
// client's own compact protocol, all of whose members carry an opaque blob it replays.
func isCompactionFamily(itemType string) bool {
	switch itemType {
	case ItemTypeCompaction, ItemTypeCompactionSummary, ItemTypeContextCompaction:
		return true
	default:
		return false
	}
}

// IsNativeCompactionItem reports whether the upstream produced the item the client counts.
//
// It gates synthesis: a client that receives two compaction items fails exactly like one that
// receives none ("got 2 from N"), so an upstream that already answered natively must be
// passed through untouched. `context_compaction` alone does not count — current Codex only
// counts `compaction` — so an upstream speaking the older shape still gets a synthesized one.
func IsNativeCompactionItem(item pluginapi.Item) bool {
	return IsNativeCompactionType(item.Type)
}

// IsNativeCompactionType is IsNativeCompactionItem for an already-rendered output item type.
func IsNativeCompactionType(itemType string) bool {
	return itemType == ItemTypeCompaction || itemType == ItemTypeCompactionSummary
}

// EncodeCompactionSummary wraps a summary in the gateway envelope.
func EncodeCompactionSummary(summary string) string {
	return CompactionEnvelopePrefix + base64.StdEncoding.EncodeToString([]byte(summary))
}

// DecodeCompactionSummary unwraps a summary this gateway minted. It reports false for a
// foreign blob, for a missing prefix, and for a payload that is not valid base64/UTF-8 or
// carries no text: a corrupted or empty envelope must travel as an opaque blob rather than be
// guessed at, and an empty summary would tell the model that the history it replaced was empty.
func DecodeCompactionSummary(encryptedContent string) (string, bool) {
	encoded, ok := strings.CutPrefix(encryptedContent, CompactionEnvelopePrefix)
	if !ok {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !utf8.Valid(raw) {
		return "", false
	}
	summary := string(raw)
	if strings.TrimSpace(summary) == "" {
		return "", false
	}
	return summary, true
}

// CompactionItem builds the single output item a Codex compaction turn must receive.
// The shape is minimal on purpose: the client deserializes it as ResponseItem::Compaction and
// ignores everything but type and encrypted_content, so nothing else is asserted about it.
func CompactionItem(summary string) pluginapi.Item {
	return pluginapi.Item{
		Type:  ItemTypeCompaction,
		Extra: map[string]json.RawMessage{"encrypted_content": quoteJSON(EncodeCompactionSummary(summary))},
	}
}

// CompactionObserver wraps an SSE emitter for a compaction turn.
//
// The client asked for a checkpoint, not for an answer: the summary belongs in the single
// compaction item, so the model's own text and reasoning are kept out of the stream while
// still reaching the assembler (usage, billing, request logs and GET /v1/responses/{id} are
// unchanged — the emitter filters what the CLIENT sees, not what the gateway records).
func CompactionObserver(send func(*Event) error) func(*Event) error {
	return func(event *Event) error {
		if event == nil {
			return nil
		}
		if !compactionVisibleEvent(event) {
			return nil
		}
		return send(event)
	}
}

// compactionVisibleEvent decides what a compaction turn may show the client: the response
// envelope, and the compaction items themselves (an upstream that answers natively must reach
// the client, and the item this gateway synthesizes at the end has to get through).
func compactionVisibleEvent(event *Event) bool {
	switch event.Type {
	case EventOutputItemAdded, EventOutputItemDone:
		return event.Item != nil && isCompactionFamily(event.Item.Type)
	case EventContentPartAdded, EventContentPartDone,
		EventOutputTextDelta, EventOutputTextDone,
		EventRefusalDelta, EventRefusalDone,
		EventFunctionArgsDelta, EventFunctionArgsDone,
		EventReasoningSummaryAdded, EventReasoningSummaryDelta,
		EventReasoningSummaryDone, EventReasoningSummaryClosed:
		return false
	default:
		return true
	}
}

// encryptedContent reads the opaque blob of a compaction-family item. It is an unmodelled
// field everywhere else, so it travels in Extra.
func encryptedContent(item pluginapi.Item) string {
	raw, ok := item.Extra["encrypted_content"]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// userMessage builds the input item shape Codex uses for its own local summaries.
func userMessage(text string) pluginapi.Item {
	content, err := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
	if err != nil {
		// A string cannot fail to marshal; keep the shape valid rather than panic.
		content = []byte("[]")
	}
	return pluginapi.Item{Type: "message", Role: "user", Content: content}
}

// quoteJSON renders a string as a JSON value (used for Extra payloads, which are raw).
func quoteJSON(value string) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}
