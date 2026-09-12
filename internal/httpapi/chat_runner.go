package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// This file is the console chat's model transport. A chat step is not a bespoke model call:
// it is a real POST /v1/responses that this process sends to itself, through the very
// handler every other client uses. That is what makes a conversation cost exactly what the
// same request would cost from any agent — quota, admission, retries, metering, settlement,
// request log and hooks all run once, in one place, with no second implementation to drift.
//
// Three things are private to this path:
//
//   - the identity: there is no bearer token to send, so the server installs the key and
//     account it resolved itself (VerifyID) and authenticate() accepts that;
//   - the recording: content is forced to metadata-only, because a skill the operator wrote
//     is private to them while the global request log is readable by every administrator;
//   - the routing facts: provider/canonical/degraded come from an observer callback instead
//     of response headers, which the streaming branch never sets.
//
// Every step gets its own request id. usage_records is keyed by request id, so reusing one
// across steps would collide and lose money.

// consoleUserAgent marks console traffic in the request log's client dimension. It is set
// server-side, so unlike a client's own User-Agent it cannot be spoofed from outside.
const consoleUserAgent = "aigw-console/1"

// maxStepBufferBytes bounds what one step may buffer. Text is forwarded to the browser as
// it arrives, so this guards against a runaway provider rather than limiting display.
const maxStepBufferBytes = 8 << 20

// chatRunner implements chat.Runner.
type chatRunner struct{ s *Server }

// RunStep performs one billed model call and returns what it produced.
func (r *chatRunner) RunStep(ctx context.Context, step chat.Step, emit func(chat.StepEvent) error) (*chat.StepResult, error) {
	key, account, err := r.verify(step)
	if err != nil {
		return nil, err
	}
	body, err := stepBody(step)
	if err != nil {
		return nil, err
	}

	requestID := ids.Request()
	stepCtx := context.WithValue(ctx, ctxRequestID, requestID)

	observer := &stepObserver{}
	recorder := newStepRecorder(emit)

	req, err := http.NewRequestWithContext(stepCtx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, domain.ErrInternal("building the console request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", consoleUserAgent)
	req = withVerifiedIdentity(req, verifiedIdentity{key: key, account: account})
	req = withChatRecording(req)
	req = withStepObserver(req, observer)

	// The real handler: it authenticates from the injected identity, admits the request
	// against the account's balance and quota, routes it, streams the answer and settles
	// what it consumed — exactly as it would for any other client.
	r.s.handleCreateResponse(recorder, req)

	result, failure := recorder.result(requestID)
	if failure != nil {
		return nil, &chat.StepFailure{Status: failure.status, Code: failure.code, Message: failure.message}
	}
	if observed, ok := observer.snapshot(); ok {
		result.Provider = firstNonEmpty(observed.Provider, result.Provider)
		result.ResolvedModel = firstNonEmpty(observed.Canonical, result.ResolvedModel)
		result.Degraded = observed.Degraded
	}
	return result, nil
}

// verify resolves the key the conversation is bound to. A key that was revoked or expired,
// or whose account was suspended, fails here — before any money is reserved.
func (r *chatRunner) verify(step chat.Step) (*domain.APIKey, *domain.Account, error) {
	if r.s.deps.Verifier == nil {
		return nil, nil, domain.ErrUnsupported("the data plane is disabled on this deployment")
	}
	if step.APIKeyID <= 0 || step.AccountID <= 0 {
		return nil, nil, domain.ErrInvalidRequest("this conversation has no API key bound to it")
	}
	// The verification deliberately runs on a background context: it is an authorization
	// check about the conversation's binding, and cancelling the browser's fetch must not
	// turn "the key is still valid" into an ambiguous answer.
	key, account, err := r.s.deps.Verifier.VerifyID(context.WithoutCancel(context.Background()), step.APIKeyID)
	if err != nil {
		return nil, nil, err
	}
	if key.AccountID != step.AccountID {
		// The conversation remembers which account it was opened against; a key that has
		// since been moved elsewhere must not silently start spending the new account.
		return nil, nil, domain.ErrForbidden("the API key bound to this conversation now belongs to another account; bind it again")
	}
	return key, account, nil
}

// stepBody renders the Responses request for one model step.
func stepBody(step chat.Step) ([]byte, error) {
	payload := map[string]any{
		"model":  step.Model,
		"input":  step.Items,
		"stream": true,
		// No stored response object: the conversation itself is the record, and a stored
		// response would be a second copy readable by every administrator.
		"store":            false,
		"prompt_cache_key": step.PromptCacheKey,
	}
	if strings.TrimSpace(step.Instructions) != "" {
		payload["instructions"] = step.Instructions
	}
	if len(step.Tools) > 0 {
		tools := make([]map[string]any, 0, len(step.Tools))
		for _, tool := range step.Tools {
			entry := map[string]any{"type": "function", "name": tool.Name, "description": tool.Description}
			if len(tool.Schema) > 0 {
				entry["parameters"] = json.RawMessage(tool.Schema)
			}
			tools = append(tools, entry)
		}
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	if step.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = step.MaxOutputTokens
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the console request failed: %w", err)
	}
	return body, nil
}

// stepRecorder is the ResponseWriter the data plane writes into. It is a buffered sink, not
// a live connection: the chat's own SSE stream already carries deltas to the browser, and
// the recorder's job is to collect the canonical end state of the step.
type stepRecorder struct {
	header  http.Header
	status  int
	events  []stepFrame
	body    bytes.Buffer
	emit    func(chat.StepEvent) error
	emitErr error
	// callSeen tracks the function calls already announced, so the deltas that follow are
	// attributed to the right call even when a provider omits the item id.
	callSeen map[string]bool
	lastCall string
	overflow bool
}

// stepFrame is one parsed SSE frame, reduced to what the chat needs.
type stepFrame struct {
	name string
	data []byte
}

func newStepRecorder(emit func(chat.StepEvent) error) *stepRecorder {
	return &stepRecorder{header: http.Header{}, status: http.StatusOK, emit: emit, callSeen: map[string]bool{}}
}

func (r *stepRecorder) Header() http.Header { return r.header }

func (r *stepRecorder) WriteHeader(status int) { r.status = status }

func (r *stepRecorder) Write(p []byte) (int, error) {
	if r.body.Len()+len(p) > maxStepBufferBytes {
		r.overflow = true
		return len(p), nil
	}
	r.body.Write(p)
	r.consume()
	return len(p), nil
}

func (r *stepRecorder) Flush() {}

// consume parses complete SSE frames out of the buffer as they arrive, so text reaches the
// browser while the model is still talking.
func (r *stepRecorder) consume() {
	for {
		data := r.body.Bytes()
		idx := bytes.Index(data, []byte("\n\n"))
		if idx < 0 {
			return
		}
		frame := string(data[:idx])
		rest := append([]byte{}, data[idx+2:]...)
		r.body.Reset()
		r.body.Write(rest)
		r.handleFrame(frame)
	}
}

func (r *stepRecorder) handleFrame(frame string) {
	name, payload := parseSSEFrame(frame)
	if name == "" && len(payload) == 0 {
		return
	}
	r.events = append(r.events, stepFrame{name: name, data: payload})
	if r.emit == nil || r.emitErr != nil {
		return
	}
	switch name {
	case "response.output_text.delta":
		r.emitEvent(chat.StepEvent{Kind: "text", Text: deltaOf(payload)})
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		r.emitEvent(chat.StepEvent{Kind: "reasoning", Text: deltaOf(payload)})
	case "response.output_item.added":
		item, ok := objectField(payload, "item")
		if !ok || mapString(item, "type") != "function_call" {
			return
		}
		callID := mapString(item, "call_id")
		if callID != "" {
			r.callSeen[callID] = true
			r.lastCall = callID
		}
		r.emitEvent(chat.StepEvent{Kind: "tool_call", CallID: callID, Name: mapString(item, "name")})
	case "response.function_call_arguments.delta":
		callID := stringField(payload, "item_id")
		if callID == "" {
			callID = r.lastCall
		}
		r.emitEvent(chat.StepEvent{Kind: "tool_call", CallID: callID, Text: deltaOf(payload)})
	default:
	}
}

func (r *stepRecorder) emitEvent(ev chat.StepEvent) {
	if r.emit == nil || r.emitErr != nil {
		return
	}
	if err := r.emit(ev); err != nil {
		r.emitErr = err
	}
}

// stepFailure is a data-plane rejection: a JSON error response rather than a stream.
type stepFailure struct {
	status  int
	code    string
	message string
}

// result reduces the recorded stream to the step's outcome.
func (r *stepRecorder) result(requestID string) (*chat.StepResult, *stepFailure) {
	if r.status >= 400 || strings.Contains(r.header.Get("Content-Type"), "application/json") {
		return nil, r.apiFailure()
	}

	result := &chat.StepResult{RequestID: requestID, Outcome: chat.OutcomeFailed}
	var (
		text      strings.Builder
		reasoning strings.Builder
		callArgs  = map[string]*strings.Builder{}
		callNames = map[string]string{}
		callOrder []string
	)
	for _, frame := range r.events {
		switch frame.name {
		case "response.output_text.delta":
			text.WriteString(deltaOf(frame.data))
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			reasoning.WriteString(deltaOf(frame.data))
		case "response.output_item.added":
			item, ok := objectField(frame.data, "item")
			if !ok || mapString(item, "type") != "function_call" {
				continue
			}
			callID := mapString(item, "call_id")
			if callID == "" {
				continue
			}
			if _, seen := callArgs[callID]; !seen {
				callArgs[callID] = &strings.Builder{}
				callOrder = append(callOrder, callID)
			}
			callNames[callID] = mapString(item, "name")
		case "response.function_call_arguments.delta":
			callID := stringField(frame.data, "item_id")
			if callID == "" {
				callID = r.lastCall
			}
			if builder, ok := callArgs[callID]; ok {
				builder.WriteString(deltaOf(frame.data))
			}
		case "response.output_item.done":
			item, ok := objectField(frame.data, "item")
			if !ok || mapString(item, "type") != "function_call" {
				continue
			}
			callID := mapString(item, "call_id")
			if callID == "" {
				continue
			}
			if _, seen := callArgs[callID]; !seen {
				callArgs[callID] = &strings.Builder{}
				callOrder = append(callOrder, callID)
			}
			callNames[callID] = mapString(item, "name")
			// The completed item carries the authoritative argument string, so a provider
			// that never sent argument deltas still produces an executable call.
			if args := mapString(item, "arguments"); args != "" {
				callArgs[callID].Reset()
				callArgs[callID].WriteString(args)
			}
		case "response.completed", "response.incomplete", "response.failed":
			result.Outcome = outcomeOf(frame.name)
			result.Items = itemsOf(frame.data)
			result.Usage = usageOf(frame.data)
		case "error":
			result.Outcome = chat.OutcomeFailed
			if message := stringField(frame.data, "message"); message != "" {
				result.FinishReason = message
			}
		}
	}

	// Tool calls are assembled in the order the provider announced them, and only when
	// every one of them has complete arguments and a name: a half-written call must never
	// reach the executor.
	result.ToolCallsComplete = true
	for _, callID := range callOrder {
		args := ""
		if builder := callArgs[callID]; builder != nil {
			args = builder.String()
		}
		name := callNames[callID]
		if strings.TrimSpace(args) == "" || strings.TrimSpace(name) == "" {
			result.ToolCallsComplete = false
			continue
		}
		if hasItemCall(result.Items, callID) {
			continue
		}
		result.Items = append(result.Items, pluginapi.Item{
			Type: "function_call", CallID: callID, Name: name, Arguments: args, Status: "completed",
		})
	}
	result.Text = text.String()
	result.Reasoning = reasoning.String()
	if len(r.events) == 0 {
		result.Outcome = chat.OutcomeFailed
		if result.FinishReason == "" {
			result.FinishReason = "the model produced no output"
		}
	}
	return result, nil
}

// apiFailure decodes the JSON error the data plane returned instead of a stream.
func (r *stepRecorder) apiFailure() *stepFailure {
	payload := struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}{}
	_ = json.Unmarshal(r.body.Bytes(), &payload)
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = fmt.Sprintf("the gateway rejected this step with status %d", r.status)
	}
	code := payload.Error.Code
	if code == "" {
		code = "request_rejected"
	}
	return &stepFailure{status: r.status, code: code, message: message}
}

func outcomeOf(event string) string {
	switch event {
	case "response.completed":
		return chat.OutcomeCompleted
	case "response.incomplete":
		return chat.OutcomeIncomplete
	default:
		return chat.OutcomeFailed
	}
}

// itemsOf reads the terminal response's output items: the canonical conversation state
// (messages, reasoning items and function calls) in the order the provider produced them.
func itemsOf(payload []byte) []pluginapi.Item {
	response, ok := objectField(payload, "response")
	if !ok {
		return nil
	}
	raw, ok := response["output"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var items []pluginapi.Item
	if err := json.Unmarshal(encoded, &items); err != nil {
		return nil
	}
	return items
}

func hasItemCall(items []pluginapi.Item, callID string) bool {
	for _, item := range items {
		if item.Type == "function_call" && item.CallID == callID {
			return true
		}
	}
	return false
}

// usageOf reads the terminal usage block.
func usageOf(payload []byte) chat.Usage {
	response, ok := objectField(payload, "response")
	if !ok {
		return chat.Usage{}
	}
	raw, err := json.Marshal(response["usage"])
	if err != nil {
		return chat.Usage{}
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		return chat.Usage{}
	}
	out := chat.Usage{
		InputTokens:  intField(usage, "input_tokens"),
		OutputTokens: intField(usage, "output_tokens"),
	}
	if details, ok := objectField(raw, "output_tokens_details"); ok {
		out.ReasoningTokens = intField(details, "reasoning_tokens")
	}
	out.Metered = out.InputTokens > 0 || out.OutputTokens > 0
	return out
}

// parseSSEFrame splits one "event: …\ndata: …" block.
func parseSSEFrame(frame string) (string, []byte) {
	var (
		name string
		data []string
	)
	for _, line := range strings.Split(frame, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if name == "" && len(data) == 0 {
		return "", nil
	}
	return name, []byte(strings.Join(data, "\n"))
}

func deltaOf(payload []byte) string { return stringField(payload, "delta") }

// stringField reads a top-level string field from a JSON object.
func stringField(payload []byte, key string) string {
	fields, ok := objectField(payload, "")
	if !ok {
		return ""
	}
	return mapString(fields, key)
}

// mapString reads a string field from an already-decoded object.
func mapString(fields map[string]any, key string) string {
	text, _ := fields[key].(string)
	return text
}

// objectField decodes a JSON object and returns one nested object (or the whole object when
// key is empty).
func objectField(payload []byte, key string) (map[string]any, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, false
	}
	if key == "" {
		return decoded, true
	}
	nested, ok := decoded[key].(map[string]any)
	if !ok {
		return nil, false
	}
	return nested, true
}

// intField reads a numeric field that may arrive as a JSON number or a string.
func intField(fields map[string]any, key string) int {
	switch value := fields[key].(type) {
	case float64:
		return int(value)
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// readBodyLimited reads a small JSON request body.
func readBodyLimited(r *http.Request, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, max))
}
