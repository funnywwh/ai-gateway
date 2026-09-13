package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/usage"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

const (
	maxAttemptsKey = "max_attempts"
)

// handleCreateResponse implements POST /v1/responses.
func (s *Server) handleCreateResponse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// admission holds this request's in-flight reservation, if billing is enabled.
	var admission *billing.Reservation

	key, account, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.deps.Config.Server.MaxBodyBytes))
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("failed to read the request body"))
		return
	}

	req, apiErr := responses.Parse(body)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	req.SetSessionHeaders(r.Header)

	// Continuation: prepend the stored input+output items of the previous response.
	var priorItems []pluginapi.Item
	if req.PreviousResponseID != "" {
		// A continuation reads a row the client was just handed, exactly like
		// GET /v1/responses/{id} does, and that row is written by the background
		// batcher: without this wait a client that continues a turn immediately —
		// which is what an agent loop does, and what the thinking-mode constraint
		// forces (the tool result has to travel back in the very next request) —
		// reads a 404 for a row that is microseconds behind.
		if waiter, ok := s.deps.LogRecorder.(responseWaiter); ok {
			if err := waiter.AwaitResponse(ctx, req.PreviousResponseID); err != nil {
				s.deps.Log.Warn("waiting for a stored response timed out",
					"err", err, "response_id", req.PreviousResponseID)
			}
		}
		prev, err := s.deps.Records.GetResponse(ctx, req.PreviousResponseID)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if prev.APIKeyID != key.ID {
			writeAPIError(w, domain.ErrNotFound("previous response "+req.PreviousResponseID))
			return
		}
		priorItems = decodeStoredItems(prev.OutputJSON)
		if req.Instructions == "" {
			req.Instructions = prev.Instructions
		}
	}

	// Admission: rate limits before any upstream work.
	limits := s.limitsFor(key)
	ticket, err := s.deps.Limiter.Reserve(ctx, scopeForKey(key.ID), limits)
	if err != nil {
		writeRateLimitError(w, err)
		return
	}
	defer ticket.Release()

	// The client's own session key: it makes consecutive requests of one session prefer
	// the upstream that last served them (docs/routing.md §4.4). It never affects what
	// the key is allowed to use — the candidate list is filtered for authorization
	// exactly as before. Log grouping independently uses explicit session metadata.
	sessionKey := req.SessionKey()
	plan, err := s.deps.Router.Plan(domain.RouteRequest{
		Model:       req.Model,
		Key:         key,
		Features:    featuresOf(req),
		ProviderPin: r.Header.Get("X-Gateway-Provider"),
		Strategy:    r.Header.Get("X-Gateway-Strategy"),
		SessionID:   sessionKey,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	canonical := plan.Resolved.Canonical
	startedAt := time.Now()
	requestID := requestIDFrom(ctx)

	// Balance admission happens before any upstream work: a request that cannot be paid
	// for must not reach a provider, and the reservation stops concurrent requests from
	// collectively overselling the account.
	if s.deps.Billing != nil && account != nil && len(plan.Candidates) > 0 {
		first := plan.Candidates[0]
		cost, sale := s.ruleSetsFor(canonical, first.ProviderID)
		estimate := billing.EstimateInput{
			Cost: cost, Sale: sale,
			MaxOutputTokens: s.effectiveMaxOutput(req, canonical),
			EstInputTokens:  estimateInputTokens(body),
			DefaultMarkupBP: s.deps.Config.Billing.DefaultMarkupBP,
			// The hold is taken on a ledger-denominated balance, so a model priced
			// in another currency is converted before the two sides are compared.
			Ledger:               s.ledgerCurrency(),
			FX:                   s.fxTable(),
			DefaultReserveMicros: s.deps.Config.Billing.ReserveMicrosDefault,
		}
		decision, reservation := s.deps.Billing.Admit(account, requestID, estimate, s.inflightPolicy(account), startedAt)
		if !decision.Allowed {
			s.rejectForQuota(w, r, key, account, req, decision)
			return
		}
		if reservation != nil {
			defer s.deps.Billing.Release(reservation)
			admission = reservation
		}
	}

	var sse *sseWriter
	var assembler *responses.Assembler
	if req.Stream {
		sse = newSSEWriter(w)
		assembler = responses.NewAssembler(canonical, sse.Send)
	} else {
		assembler = responses.NewAssembler(canonical, nil)
	}
	if err := assembler.Start(); err != nil {
		return // client already gone
	}

	var (
		chosen     *domain.Candidate
		providerID int64
		lastErr    error
		attemptNo  int
		usage      pluginapi.Usage
		ttftMS     int
		// finishReason is the chosen provider's own reason for stopping ("stop",
		// "length", "content_filter", ...). It is what separates "the model finished"
		// from "the answer was cut off", which the client cannot see otherwise.
		finishReason string
		// reasoningEffort is captured from the canonical provider request after
		// the model-level policy is applied, independently of content recording.
		reasoningEffort string
	)

	maxAttempts := s.deps.Config.Routing.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	for i := range plan.Candidates {
		if i >= maxAttempts {
			break
		}
		cand := plan.Candidates[i]
		attemptNo++

		provReq, apiErr := req.ToProviderRequest(cand.UpstreamModel)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		applyModelReasoning(provReq, plan.Reasoning)
		reasoningEffort = ""
		if provReq.Reasoning != nil {
			reasoningEffort = strings.TrimSpace(provReq.Reasoning.Effort)
		}
		if len(priorItems) > 0 {
			provReq.Input = append(append([]pluginapi.Item{}, priorItems...), provReq.Input...)
		}
		provReq.Stream = req.Stream

		attemptStarted := time.Now()
		var guard *inflightGuard
		if req.Stream {
			attemptCtx, cancelAttempt := context.WithCancel(ctx)
			costRules, saleRules := s.ruleSetsFor(plan.Resolved.Canonical, cand.ProviderID)
			guard = s.newInflightGuard(attemptCtx, cancelAttempt, account, requestID,
				plan.Resolved.Canonical, cand.UpstreamModel, cand.ProviderID, costRules, saleRules,
				s.resolveMarkup(key, account, saleRules), admission)
			var end *pluginapi.StreamEnd
			end, err = s.deps.Dispatcher.Stream(attemptCtx, cand.ProviderID, provReq, func(ev pluginapi.Event) error {
				return guard.Observe(ev, assembler.Add)
			})
			cancelAttempt()
			if err == nil && end != nil {
				finishReason = end.FinishReason
			}
		} else {
			var providerResp *pluginapi.Response
			providerResp, err = s.deps.Dispatcher.Complete(ctx, cand.ProviderID, provReq)
			if err == nil {
				if ferr := responses.FeedItems(assembler, providerResp.Items); ferr != nil {
					err = ferr
				} else {
					usage = providerResp.Usage
					finishReason = providerResp.FinishReason
				}
			}
		}

		latency := int(time.Since(attemptStarted).Milliseconds())
		if err == nil && ttftMS == 0 && assembler.Deltas() > 0 {
			ttftMS = latency
		}

		var outcome *inflightOutcome
		if guard != nil {
			outcome = guard.Outcome(assembler.Usage().Dimensions)
			if outcome != nil && outcome.aborted {
				// The upstream call usually fails with a cancellation; the reason the
				// client must see is the quota decision, not the transport error.
				err = errQuotaAborted
			}
		}
		// An attempt that served a fragment is metered like any other answer; the
		// reason it ended is recorded separately so the usage table can still tell
		// a truncated answer from a complete one.
		attemptTerminal := ""
		if _, truncated := pluginapi.IncompleteReason(finishReason); err == nil && truncated {
			attemptTerminal = "incomplete"
		}
		s.recordAttempt(ctx, key, account, req, plan.Resolved, cand, attemptNo, err, attemptTerminal, latency, ttftMS, attemptStarted, usage, outcome, assembler)

		if err == nil {
			chosen = &cand
			providerID = cand.ProviderID
			// Only a request that actually succeeded re-binds its session: a route that
			// merely got picked is not evidence that it works.
			s.deps.Router.NoteSuccess(plan.Affinity, cand.RouteID)
			// An in-process caller (the console chat) needs the routing facts the
			// non-streaming branch would have put in response headers, and this is the one
			// place that knows them.
			reportStep(ctx, stepOutcome{
				Provider:      cand.ProviderName,
				Canonical:     plan.Resolved.Canonical,
				UpstreamModel: cand.UpstreamModel,
				Degraded:      cand.Degraded,
			})
			break
		}
		lastErr = err

		// A stream that already produced output must not fail over: the client has
		// seen partial content and a retry would duplicate or contradict it.
		if assembler.Deltas() > 0 {
			break
		}
		if resetAt, isQuota := runtime.QuotaError(err); isQuota {
			s.deps.Dispatcher.CooldownForProvider(ctx, cand.RouteID, resetAt, "upstream quota exhausted")
		}
		if !runtime.Retryable(err) {
			break
		}
		// The attempt is about to move on to the next authorised candidate, so the
		// session must not be sent back here next time. Only a retryable failure says
		// that; a client error (400/404 from upstream) says nothing about the route.
		if s.deps.Router.NoteFailure(plan.Affinity, cand.RouteID) && s.deps.Log != nil {
			s.deps.Log.Info("session affinity dropped after a retryable failure",
				"request_id", requestID, "route", cand.RouteID, "provider", cand.ProviderName,
				"session", sessionKey, "attempt", attemptNo)
		}
	}

	if chosen == nil {
		payload := &responses.ErrorPayload{Code: "upstream_error", Message: errorMessage(lastErr)}
		if errors.Is(lastErr, errQuotaAborted) {
			payload.Code = "insufficient_quota"
			payload.Message = "the account ran out of quota while the response was streaming"
		}
		if apiErr, ok := domain.AsAPIError(lastErr); ok {
			payload.Code = apiErr.Code
			payload.Message = apiErr.Message
		}
		if req.Stream {
			_, _ = assembler.Fail(payload)
			s.persist(ctx, key, account, req, plan.Resolved.Canonical, 0, assembler, "failed", clientHintFromRequest(r), reasoningEffort)
		} else {
			writeAPIError(w, toAPIError(lastErr))
		}
		ticket.Settle(totalTokens(assembler.Usage()))
		return
	}

	// The answer the client is about to receive is a fragment when the provider said
	// so (token limit, content filter): finalise it as `response.incomplete` instead
	// of `response.completed`, otherwise a client that derives its own stop reason
	// from the terminal status (every Responses client does) reads a cut-off answer
	// as a finished one.
	incompleteReason, truncated := pluginapi.IncompleteReason(finishReason)
	status := "completed"
	if truncated {
		status = "incomplete"
	}

	if req.Stream {
		final := assembler.Usage()
		if len(usage.Dimensions) > 0 {
			final = usage
		}
		if truncated {
			if _, err := assembler.Incomplete(incompleteReason, final); err != nil {
				return
			}
		} else if _, err := assembler.Complete(final); err != nil {
			return
		}
	} else {
		final := usage
		if len(final.Dimensions) == 0 {
			final = assembler.Usage()
		}
		var (
			resp *responses.Response
			ferr error
		)
		if truncated {
			resp, ferr = assembler.Incomplete(incompleteReason, final)
		} else {
			resp, ferr = assembler.Complete(final)
		}
		if ferr != nil {
			writeAPIError(w, domain.ErrInternal(ferr.Error()))
			return
		}
		w.Header().Set("x-gateway-provider", chosen.ProviderName)
		w.Header().Set("x-gateway-model", canonical)
		if len(chosen.Degraded) > 0 {
			w.Header().Set("x-gateway-degraded", strings.Join(chosen.Degraded, ","))
		}
		writeJSON(w, http.StatusOK, resp)
	}

	s.persist(ctx, key, account, req, canonical, providerID, assembler, status, clientHintFromRequest(r), reasoningEffort)
	ticket.Settle(totalTokens(assembler.Usage()))
	_ = startedAt
}

// recordAttempt writes one usage_records row per upstream attempt.
func (s *Server) recordAttempt(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	req *responses.Request,
	resolved *domain.ResolvedModel,
	cand domain.Candidate,
	attemptNo int,
	attemptErr error,
	terminalReason string,
	latencyMS, ttftMS int,
	startedAt time.Time,
	u pluginapi.Usage,
	outcome *inflightOutcome,
	assembler *responses.Assembler,
) {
	if s.deps.Meter == nil {
		return
	}
	dims := u.Dimensions
	estimated := u.Estimated
	if len(dims) == 0 {
		dims = assembler.Usage().Dimensions
		estimated = assembler.Usage().Estimated
	}
	// An aborted attempt is charged only for what was metered at the decision point.
	if outcome != nil && outcome.aborted {
		if len(outcome.dims) > 0 {
			dims = outcome.dims
		}
	}
	status := "completed"
	errorCode := ""
	terminated := "completed"
	if attemptErr != nil {
		status = "failed"
		terminated = "upstream_error"
		if apiErr, ok := pluginapi.IsError(attemptErr); ok {
			errorCode = apiErr.Code
			if apiErr.Kind == pluginapi.KindQuotaExhausted {
				terminated = "aborted_quota"
			}
		} else if errors.Is(attemptErr, errQuotaAborted) {
			errorCode = "insufficient_quota"
			terminated = "aborted_quota"
		} else {
			errorCode = "upstream_error"
		}
	} else if terminalReason != "" {
		// The attempt succeeded and was metered: an answer cut short by the token
		// limit is still a served answer, but it must be distinguishable in the
		// record from one the model finished on its own.
		terminated = terminalReason
	}
	attempt := &usage.Attempt{
		RequestID:        requestIDFrom(ctx),
		AttemptNo:        attemptNo,
		AccountID:        account.ID,
		APIKeyID:         key.ID,
		Model:            req.Model,
		ResolvedModel:    resolved.Canonical,
		ProviderID:       cand.ProviderID,
		Dimensions:       dims,
		Estimated:        estimated,
		LatencyMS:        latencyMS,
		TTFTMS:           ttftMS,
		Status:           status,
		ErrorCode:        errorCode,
		DegradedFeatures: cand.Degraded,
		TerminatedReason: terminated,
	}
	if outcome != nil {
		attempt.OvershootCost = outcome.overshootCostMicros
		if outcome.terminatedReason != "" {
			attempt.TerminatedReason = outcome.terminatedReason
		}
	}

	// With billing enabled the usage row is written by the settlement transaction, so
	// that the metered attempt and the money it costs can never disagree.
	if s.deps.Billing != nil {
		s.settleAttempt(ctx, attempt, resolved, cand, attemptErr, startedAt, dims, outcome != nil && outcome.aborted, s.resolveMarkup(key, account, s.saleRulesFor(resolved.Canonical, cand.ProviderID)))
		return
	}
	if _, err := s.deps.Meter.Record(ctx, attempt); err != nil {
		s.deps.Log.Warn("recording usage failed", "err", err, "request_id", requestIDFrom(ctx))
	}
}

// auditWriteTimeout bounds the detached write of the audit trail (see persist).
const auditWriteTimeout = 5 * time.Second

// persist stores the response (when requested) and the request log.
//
// The audit trail must outlive the client. A request that fails hard — an upstream 400,
// a stream that was cut — usually ends with the client hanging up, which cancels the
// request context and with it every write that records what happened. Those are exactly
// the requests somebody later has to diagnose: during the M19d/M19e investigation the
// failing requests were the only ones missing from the request log, the write having
// died with the connection ("recording request content failed: context canceled"), and
// the request body had to be reconstructed from the client's own session.
// WithoutCancel keeps the context values (request id, and whatever else is read
// downstream) and drops only the cancellation; the timeout bounds the detached write.
func (s *Server) persist(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	req *responses.Request,
	canonical string,
	providerID int64,
	assembler *responses.Assembler,
	status string,
	clientHint string,
	reasoningEffort string,
) {
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()

	resp := assembler.Response()
	outputJSON, err := json.Marshal(resp.Output)
	if err != nil {
		s.deps.Log.Warn("marshalling response output failed", "err", err)
		outputJSON = []byte("[]")
	}
	usageJSON, _ := json.Marshal(resp.Usage)
	completed := time.Now().UTC()
	// A stored response lives exactly as long as the retention window says. When cleanup
	// is switched off there is no expiry at all (NULL), which is what "keep forever" has
	// to mean for a client that can still fetch the response by id.
	var expires *time.Time
	if window, ok := s.retentionWindow(); ok {
		at := completed.Add(window)
		expires = &at
	}

	// One computation feeds the stored response, the request log and the hook event, so
	// the three can never disagree about what was recorded under which policy.
	input := s.recordInput(auditCtx, key, req, clientHint)
	// The routed model is only known once routing succeeded; a locally rejected request
	// keeps the empty value, which is the honest answer for "it never reached a model".
	input.Resolved = canonical

	var storedResp *domain.ResponseRecord
	// Console steps already send store:false; the guard keeps a stored copy out of the
	// database even if that ever changes, because a stored response is readable by every
	// administrator while a conversation belongs to one.
	if req.Stored() && !isChatRecording(ctx) {
		storedResp = &domain.ResponseRecord{
			ID:           assembler.ID(),
			APIKeyID:     key.ID,
			AccountID:    account.ID,
			Model:        canonical,
			ProviderID:   providerID,
			Status:       status,
			RequestJSON:  input.Payload,
			OutputJSON:   string(outputJSON),
			UsageJSON:    string(usageJSON),
			Instructions: req.Instructions,
			CreatedAt:    completed,
			CompletedAt:  &completed,
			ExpiresAt:    expires,
		}
		// The response has already been written to the client, so this row is not on
		// anyone's critical path: batch it with the request log instead of taking the
		// writer lock twice per request.
		if s.deps.LogRecorder == nil {
			if err := s.deps.Records.PutResponse(auditCtx, storedResp); err != nil {
				s.deps.Log.Warn("storing response failed", "err", err, "response_id", assembler.ID())
				storedResp = nil
			}
		}
	}

	s.recordContent(auditCtx, key, account, assembler, status, input, storedResp, reasoningEffort)

	if s.deps.Hooks != nil {
		event := "response.completed"
		switch status {
		case "incomplete":
			event = "response.incomplete"
		case "completed":
		default:
			event = "response.failed"
		}
		accountName := ""
		if account != nil {
			accountName = account.Name
		}
		payload := map[string]any{
			"request_id":  requestIDFrom(ctx),
			"response_id": assembler.ID(),
			"account":     accountName,
			"api_key":     key.Name,
			"model":       canonical,
			"status":      status,
			"usage":       assembler.Usage().Dimensions,
		}
		if isChatRecording(ctx) {
			// A hook delivers to an external sink; console content must not ride along.
			payload["client"] = responses.ClientConsole
			payload["content_withheld"] = true
			s.deps.Hooks.Emit(ctx, &domain.Event{Name: event, Timestamp: time.Now().UTC(), Payload: payload})
			return
		}
		s.deps.Hooks.Emit(ctx, &domain.Event{
			Name:      event,
			Timestamp: time.Now().UTC(),
			Payload: map[string]any{
				"request_id":  requestIDFrom(ctx),
				"response_id": assembler.ID(),
				"account":     accountName,
				"api_key":     key.Name,
				"model":       canonical,
				"status":      status,
				"usage":       assembler.Usage().Dimensions,
				// The input is the same document the request log stores, under the same
				// policy. It used to be filled with the model's answer (assembler.Text()),
				// which made "input" a second copy of "output" and leaked content into a
				// field the recording switches were supposed to govern.
				"input":  input.Payload,
				"output": assembler.Text(),
			},
		})
	}
}

// inputRecord is one request's recorded input under the key's effective policy, plus the
// identity dimensions the row carries.
//
// The dimensions are deliberately not part of the policy: they are facts about which
// client, model and workspace the request came from, not content, so they are recorded
// even when record_input is "off" (which keeps a row with no body at all).
type inputRecord struct {
	TitleFingerprint string
	StartedAt        time.Time
	Mode             string // full|user|metadata|off
	Payload          string // the stored document ("" for metadata and off)
	Bytes            int    // serialized size of the whole request body
	Truncated        bool

	Dims     responses.Dimensions // client / workspace / session / call_kind
	Model    string               // the model the client asked for (billed dimension)
	Resolved string               // the model routing picked; set by persist
}

// recordInput applies the input channel of the recording policy to one request.
//
// Three channels are recorded independently: this one decides what happens to the
// client's request, and recordContent decides what happens to the model's thinking and
// final text. The default here is "user": only the user's own input is stored, with a
// tally of what was left out. "full" keeps the whole body for the times when an upstream
// 400 has to be diagnosed against the exact bytes the client sent.
func (s *Server) recordInput(ctx context.Context, key *domain.APIKey, req *responses.Request, clientHint string) inputRecord {
	cfg := s.deps.Config.Recording
	rec := inputRecord{Mode: cfg.InputModeFor(key.RecordInputMode), StartedAt: time.Now().UTC()}
	if isChatRecording(ctx) {
		// A console conversation carries private material: the operator's own skills, the
		// questions they asked about their gateway, and whatever the management tools
		// returned. The request log is global, so this traffic is recorded as metadata
		// only regardless of what the key's policy says — while quota, metering,
		// settlement and the row itself stay exactly as they are for every other request.
		rec.Mode = "off"
	}
	// Identity first: it is recorded under every input policy, including "off".
	rec.Dims = s.redactDimensions(req.Dimensions(clientHint))
	rec.Model = s.redactDimension("model", strings.TrimSpace(req.Model))
	if rec.Mode == "off" {
		return rec
	}

	// Do not derive content fingerprints when input recording/redaction forbids it.
	if (rec.Mode == "user" || rec.Mode == "full") && len(cfg.RedactPaths) == 0 {
		rec.TitleFingerprint = req.TitlePromptFingerprint(rec.Dims)
	}
	raw := mustJSON(req)
	rec.Bytes = len(raw)
	switch rec.Mode {
	case "full":
		rec.Payload = string(raw)
	case "user":
		doc, err := req.UserInputDocument(len(raw))
		if err != nil {
			// The request parsed, so this cannot normally happen; if it ever does, a row
			// with the envelope and no body still beats no row at all — the operator has
			// to be able to see that the request happened.
			if s.deps.Log != nil {
				s.deps.Log.Warn("building the recorded input document failed",
					"err", err, "request_id", requestIDFrom(ctx))
			}
			rec.Payload = string(mustJSON(map[string]any{"model": req.Model, "request_bytes": len(raw)}))
		} else {
			rec.Payload = string(mustJSON(doc))
		}
	}

	rec.Payload = redact(rec.Payload, cfg.RedactPaths)
	if limit := s.recordingLimit(); len(rec.Payload) > limit {
		rec.Payload = truncate(rec.Payload, limit)
		rec.Truncated = true
	}
	return rec
}

// recordingLimit is recording.max_bytes with the same default the config ships.
func (s *Server) recordingLimit() int {
	if limit := s.deps.Config.Recording.MaxBytes; limit > 0 {
		return limit
	}
	return 1 << 20
}

// recordContent applies the three-channel recording policy: input text is recorded
// according to recordInput's policy, thinking text and final output text only when the
// key opts in.
func (s *Server) recordContent(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	assembler *responses.Assembler,
	status string,
	input inputRecord,
	storedResp *domain.ResponseRecord,
	reasoningEffort string,
) {
	cfg := s.deps.Config.Recording
	recordReasoning := key.RecordReasoning || cfg.RecordReasoning
	recordOutput := key.RecordOutputText || cfg.RecordOutputText
	if isChatRecording(ctx) {
		// Same reasoning as recordInput: the answer to a private question does not belong
		// in a log every administrator can read.
		recordReasoning = false
		recordOutput = false
	}
	limit := s.recordingLimit()

	rec := &domain.RequestLogRecord{
		TitleFingerprint: input.TitleFingerprint,
		StartedAt:        input.StartedAt,
		RequestID:        requestIDFrom(ctx),
		APIKeyID:         key.ID,
		AccountID:        account.ID,
		Endpoint:         "/v1/responses",
		Status:           status,
		Client:           input.Dims.Client,
		Model:            input.Model,
		ResolvedModel:    input.Resolved,
		ReasoningEffort:  s.redactDimension("reasoning_effort", s.redactDimension("reasoning.effort", reasoningEffort)),
		Workspace:        input.Dims.Workspace,
		SessionID:        input.Dims.SessionID,
		CallKind:         input.Dims.CallKind,
		RecordInputMode:  input.Mode,
		RecordReasoning:  recordReasoning,
		RecordOutputText: recordOutput,
		RequestJSON:      input.Payload,
		RequestBytes:     input.Bytes,
		Truncated:        input.Truncated,
		CreatedAt:        time.Now().UTC(),
	}
	if recordReasoning {
		text := assembler.Reasoning()
		rec.ReasoningRecorded = text != ""
		if len(text) > limit {
			text = text[:limit]
			rec.Truncated = true
		}
		rec.ResponseReasoning = text
	}
	if recordOutput {
		text := assembler.Text()
		rec.OutputTextRecorded = text != ""
		if len(text) > limit {
			text = text[:limit]
			rec.Truncated = true
		}
		rec.ResponseText = text
	}
	// A session title is metadata about the session, not the answer to a user's question:
	// it is kept even when final-output recording is off, under its own switch. It is only
	// ever written on the row of the title call itself (call_kind=title) — no other row
	// produced it, and copying it around would be inventing provenance.
	if input.Dims.CallKind == responses.CallKindTitle && cfg.RecordTitle {
		rec.Title = responses.TitleOf(assembler.Text())
		if input.Dims.Client == responses.ClientCodex {
			rec.Title = responses.CodexTitleOf(assembler.Text())
		}
	}
	rec.ResponseBytes = len(rec.ResponseReasoning) + len(rec.ResponseText)

	// Batched mode: hand both rows to the recorder together, so they land in one
	// transaction and a stored response is never missing while its request log exists.
	if s.deps.LogRecorder != nil {
		s.deps.LogRecorder.EnqueueRecording(storedResp, rec)
		return
	}
	s.storeRequestLog(ctx, rec)
}

// ---------------------------------------------------------------------------
// GET / DELETE /v1/responses/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	// The stored response is written by the background batcher, so a client that POSTs and
	// immediately GETs the id it was just handed could otherwise read a 404 for a row that
	// is only microseconds behind. Wait for that one id instead of making every write
	// synchronous again.
	if waiter, ok := s.deps.LogRecorder.(responseWaiter); ok {
		if err := waiter.AwaitResponse(r.Context(), id); err != nil {
			s.deps.Log.Warn("waiting for a stored response timed out", "err", err, "response_id", id)
		}
	}
	rec, err := s.deps.Records.GetResponse(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if rec.APIKeyID != key.ID {
		writeAPIError(w, domain.ErrNotFound("response "+r.PathValue("id")))
		return
	}
	out := map[string]any{
		"id":         rec.ID,
		"object":     "response",
		"created_at": rec.CreatedAt.Unix(),
		"status":     rec.Status,
		"model":      rec.Model,
		"output":     json.RawMessage(rec.OutputJSON),
	}
	if rec.UsageJSON != "" {
		out["usage"] = json.RawMessage(rec.UsageJSON)
	}
	if rec.Instructions != "" {
		out["instructions"] = rec.Instructions
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteResponse(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	rec, err := s.deps.Records.GetResponse(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if rec.APIKeyID != key.ID {
		writeAPIError(w, domain.ErrNotFound("response "+r.PathValue("id")))
		return
	}
	if err := s.deps.Records.DeleteResponse(r.Context(), rec.ID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": rec.ID, "object": "response.deleted", "deleted": true,
	})
}

// ---------------------------------------------------------------------------
// GET /v1/models
// ---------------------------------------------------------------------------

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	snap := s.deps.Registry.Snapshot()
	grant := s.deps.Router.Authorize(key, s.deps.Router.ResolveTags(snap, key))

	list := responses.ModelList{Object: "list", Data: []responses.Model{}}
	for _, model := range snap.Models {
		if !model.Enabled {
			continue
		}
		if !granted(grant.Models, model.PublicName) {
			continue
		}
		cands, err := s.deps.Router.Candidates(domain.RouteRequest{Model: model.PublicName, Key: key, Grant: grant})
		if err != nil || len(cands) == 0 {
			continue
		}
		entry := responses.Model{
			ID: model.PublicName, Object: "model", Created: model.CreatedAt.Unix(), OwnedBy: "aigw",
		}
		if pricing := salePricing(model.SalePricingJSON, s.deps.Config.Billing); pricing != nil {
			entry.Pricing = pricing
		}
		list.Data = append(list.Data, entry)
	}
	writeJSON(w, http.StatusOK, list)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func scopeForKey(keyID int64) string { return fmt.Sprintf("key:%d", keyID) }

// salePricing renders the sale price advertised on GET /v1/models. It deliberately
// never exposes cost prices or upstream discounts.
func salePricing(raw string, cfg config.Billing) *responses.ModelPricing {
	out := &responses.ModelPricing{Currency: cfg.Currency}
	if strings.TrimSpace(raw) == "" {
		out.Basis = cfg.BasisDefault
		out.MarkupBP = cfg.DefaultMarkupBP
		return out
	}
	var wire struct {
		Basis               string `json:"basis"`
		MarkupBP            int    `json:"markup_bp"`
		Currency            string `json:"currency"`
		InputMicrosPerMTok  int64  `json:"input_micros_per_mtok"`
		OutputMicrosPerMTok int64  `json:"output_micros_per_mtok"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil
	}
	// A model may be priced in its own currency (M22), and this advertisement has
	// to say which one: the client multiplies the number by its own token counts.
	if code, err := pricing.NormalizeCurrency(wire.Currency); err == nil && code != "" {
		out.Currency = code
	}
	out.Basis = wire.Basis
	if out.Basis == "" {
		out.Basis = cfg.BasisDefault
	}
	out.MarkupBP = wire.MarkupBP
	if out.MarkupBP == 0 {
		out.MarkupBP = cfg.DefaultMarkupBP
	}
	out.InputMicrosPerMTok = wire.InputMicrosPerMTok
	out.OutputMicrosPerMTok = wire.OutputMicrosPerMTok
	return out
}

// sensitiveKeys are always removed from recorded request bodies.
var sensitiveKeys = map[string]bool{
	"api_key": true, "apikey": true, "authorization": true, "password": true,
	"secret": true, "token": true, "access_token": true, "refresh_token": true,
	"credentials": true,
}

// redactDimensions applies recording.redact_paths to the identity columns. A workspace
// path is exactly the kind of value an operator lists there, and a column that ignored the
// list would quietly defeat the setting the stored body honours.
func (s *Server) redactDimensions(d responses.Dimensions) responses.Dimensions {
	d.Client = s.redactDimension("client", d.Client)
	d.Workspace = s.redactDimension("workspace", d.Workspace)
	d.SessionID = s.redactDimension("session_id", d.SessionID)
	d.CallKind = s.redactDimension("call_kind", d.CallKind)
	return d
}

// redactDimension blanks one identity value when its column name is named in
// recording.redact_paths (or is a prefix of a listed path).
func (s *Server) redactDimension(column, value string) string {
	if value == "" {
		return ""
	}
	if matchesPath(column, s.deps.Config.Recording.RedactPaths) {
		return ""
	}
	return value
}

// clientHintFromRequest returns the User-Agent the identity extractor may fall back on.
// It is never stored: it only maps onto the client vocabulary, and a client that sends
// nothing is still identified by what it actually put in the request body.
func clientHintFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	hint := strings.TrimSpace(r.Header.Get("User-Agent"))
	if len(hint) > 64 {
		hint = hint[:64]
	}
	return hint
}

// redact removes credentials from a recorded payload, then applies operator-configured
// JSON paths (dot notation, e.g. "metadata.internal_id").
func redact(raw string, paths []string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return raw // not JSON: leave untouched rather than corrupt it
	}
	redactValue(doc, "", paths)
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return string(out)
}

func redactValue(node any, path string, paths []string) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			child := key
			if path != "" {
				child = path + "." + key
			}
			if sensitiveKeys[strings.ToLower(key)] || matchesPath(child, paths) {
				typed[key] = "[redacted]"
				continue
			}
			redactValue(value, child, paths)
		}
	case []any:
		for i, value := range typed {
			redactValue(value, fmt.Sprintf("%s[%d]", path, i), paths)
		}
	}
}

func matchesPath(path string, paths []string) bool {
	for _, want := range paths {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if path == want || strings.HasPrefix(path, want+".") || strings.HasPrefix(path, want+"[") {
			return true
		}
	}
	return false
}

// limitsFor merges the key policy with every tag policy (strictest wins).
func (s *Server) limitsFor(key *domain.APIKey) quota.Limits {
	merged := quota.LimitsFromPolicy(key.PolicyJSON)
	snap := s.deps.Registry.Snapshot()
	for _, tag := range s.deps.Router.ResolveTags(snap, key) {
		merged = quota.Merge(merged, quota.LimitsFromPolicy(tag.PolicyJSON))
	}
	return merged
}

func writeRateLimitError(w http.ResponseWriter, err error) {
	exceeded, ok := err.(*quota.ExceededError)
	if !ok {
		writeAPIError(w, toAPIError(err))
		return
	}
	now := time.Now().UTC()
	w.Header().Set("Retry-After", fmt.Sprintf("%d", exceeded.RetryAfter(now)))
	limitKinds := map[quota.LimitKind]string{
		quota.LimitRequests:    "requests",
		quota.LimitTokens:      "tokens",
		quota.LimitConcurrency: "requests",
	}
	kind := limitKinds[exceeded.Kind]
	w.Header().Set("x-ratelimit-limit-"+kind, fmt.Sprintf("%d", exceeded.Limit))
	w.Header().Set("x-ratelimit-remaining-"+kind, fmt.Sprintf("%d", exceeded.Remaining))
	w.Header().Set("x-ratelimit-reset-"+kind, exceeded.ResetAt.UTC().Format(time.RFC3339))
	writeAPIError(w, exceeded.ToAPIError())
}

// featuresOf derives the capability features a request requires.
func featuresOf(req *responses.Request) map[string]bool {
	features := map[string]bool{}
	if req.Stream {
		features["stream"] = true
	}
	if req.HasFunctionTools() {
		features["tools"] = true
	}
	if req.ParallelToolCalls != nil && *req.ParallelToolCalls {
		features["parallel_tools"] = true
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		features["reasoning"] = true
	}
	if req.Text != nil && len(req.Text.Format) > 0 {
		// The level decides which capability is required. A provider that serves
		// json_object (DeepSeek's /chat/completions, for one) must not be excluded from a
		// request it handles perfectly well, and a provider that only declares
		// json_schema must not be handed a schema it cannot enforce.
		level, err := responses.TextFormatLevel(req.Text.Format)
		switch {
		case err != nil:
			// Parsing rejects an unreadable level before it gets here, so this branch is
			// about validation drift rather than client input: fall back to the strictest
			// reading instead of quietly widening the candidate set.
			features["json_schema"] = true
		case level == responses.TextFormatText || level == "":
			// Plain text is what every provider does by default; demanding a "text"
			// capability would exclude every one of them, since nobody declares it.
		default:
			features[level] = true
		}
	}
	return features
}

func granted(set map[string]bool, name string) bool {
	if set == nil {
		return false
	}
	return set["*"] || set[name]
}

func totalTokens(u pluginapi.Usage) int64 {
	var total int64
	for _, v := range u.Dimensions {
		total += v
	}
	return total
}

func errorMessage(err error) string {
	if err == nil {
		return "no provider could serve the request"
	}
	return err.Error()
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// truncate cuts a string to at most limit bytes, never in the middle of a rune: the
// recorded documents are mostly Chinese text, and a cut inside a multi-byte rune would
// store invalid UTF-8 that no console or JSON reader can display.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// decodeStoredItems rebuilds the canonical items of a stored response so a
// previous_response_id continuation can replay them. A reasoning item carries its
// chain of thought in content (see responses.OutputItem): without it the item would
// be an empty husk, and an upstream that requires its own reasoning back (DeepSeek,
// with tools) rejects the continuation.
func decodeStoredItems(outputJSON string) []pluginapi.Item {
	if strings.TrimSpace(outputJSON) == "" {
		return nil
	}
	var items []responses.OutputItem
	if err := json.Unmarshal([]byte(outputJSON), &items); err != nil {
		return nil
	}
	out := make([]pluginapi.Item, 0, len(items))
	for _, item := range items {
		if item.Raw != nil {
			out = append(out, *item.Raw)
			continue
		}
		switch item.Type {
		case "message":
			parts := make([]map[string]string, 0, len(item.Content))
			for _, part := range item.Content {
				parts = append(parts, map[string]string{"type": part.Type, "text": part.Text})
			}
			content, _ := json.Marshal(parts)
			out = append(out, pluginapi.Item{Type: "message", Role: item.Role, Content: content})
		case "function_call":
			out = append(out, pluginapi.Item{
				Type: "function_call", ID: item.ID, CallID: item.CallID,
				Name: item.Name, Arguments: item.Arguments,
			})
		case "reasoning":
			out = append(out, pluginapi.Item{
				Type: "reasoning", ID: item.ID, Summary: reasoningSummary(item),
			})
		}
	}
	return out
}

// reasoningSummary carries a stored chain of thought into the canonical summary
// parts, where the translation layer looks for replayable reasoning text.
func reasoningSummary(item responses.OutputItem) []pluginapi.SummaryPart {
	parts := make([]pluginapi.SummaryPart, 0, len(item.Summary)+len(item.Content))
	for _, part := range item.Summary {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, pluginapi.SummaryPart{Type: part.Type, Text: part.Text})
		}
	}
	if len(parts) > 0 {
		return parts
	}
	for _, part := range item.Content {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, pluginapi.SummaryPart{Type: part.Type, Text: part.Text})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}
