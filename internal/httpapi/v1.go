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

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
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

	// Continuation: prepend the stored input+output items of the previous response.
	var priorItems []pluginapi.Item
	if req.PreviousResponseID != "" {
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

	plan, err := s.deps.Router.Plan(domain.RouteRequest{
		Model:       req.Model,
		Key:         key,
		Features:    featuresOf(req),
		ProviderPin: r.Header.Get("X-Gateway-Provider"),
		Strategy:    r.Header.Get("X-Gateway-Strategy"),
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
			s.persist(ctx, key, account, req, plan.Resolved.Canonical, 0, assembler, "failed")
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

	s.persist(ctx, key, account, req, canonical, providerID, assembler, status)
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
	expires := completed.Add(30 * 24 * time.Hour)

	if req.Stored() {
		rec := &domain.ResponseRecord{
			ID:           assembler.ID(),
			APIKeyID:     key.ID,
			AccountID:    account.ID,
			Model:        canonical,
			ProviderID:   providerID,
			Status:       status,
			RequestJSON:  truncate(string(mustJSON(req)), s.deps.Config.Recording.MaxBytes),
			OutputJSON:   string(outputJSON),
			UsageJSON:    string(usageJSON),
			Instructions: req.Instructions,
			CreatedAt:    completed,
			CompletedAt:  &completed,
			ExpiresAt:    &expires,
		}
		if err := s.deps.Records.PutResponse(auditCtx, rec); err != nil {
			s.deps.Log.Warn("storing response failed", "err", err, "response_id", assembler.ID())
		}
	}

	s.recordContent(auditCtx, key, account, req, assembler, status)

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
				"input":       truncate(assembler.Text(), 0),
				"output":      assembler.Text(),
			},
		})
	}
}

// recordContent applies the three-channel recording policy: input text is recorded by
// default, thinking text and final output text only when the key opts in.
func (s *Server) recordContent(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	req *responses.Request,
	assembler *responses.Assembler,
	status string,
) {
	cfg := s.deps.Config.Recording
	inputMode := key.RecordInputMode
	if inputMode == "" || inputMode == "inherit" {
		inputMode = cfg.RecordInput
	}
	recordReasoning := key.RecordReasoning || cfg.RecordReasoning
	recordOutput := key.RecordOutputText || cfg.RecordOutputText

	limit := cfg.MaxBytes
	if limit <= 0 {
		limit = 1 << 20
	}

	rec := &domain.RequestLogRecord{
		RequestID:        requestIDFrom(ctx),
		APIKeyID:         key.ID,
		AccountID:        account.ID,
		Endpoint:         "/v1/responses",
		Status:           status,
		RecordInputMode:  inputMode,
		RecordReasoning:  recordReasoning,
		RecordOutputText: recordOutput,
		CreatedAt:        time.Now().UTC(),
	}
	if inputMode != "off" {
		payload := redact(string(mustJSON(req)), s.deps.Config.Recording.RedactPaths)
		rec.RequestBytes = len(payload)
		if len(payload) > limit {
			payload = payload[:limit]
			rec.Truncated = true
		}
		rec.RequestJSON = payload
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
	rec.ResponseBytes = len(rec.ResponseReasoning) + len(rec.ResponseText)

	if err := s.deps.Records.PutRequestLog(ctx, rec); err != nil {
		s.deps.Log.Warn("recording request content failed", "err", err, "request_id", rec.RequestID)
	}
}

// ---------------------------------------------------------------------------
// GET / DELETE /v1/responses/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
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
		InputMicrosPerMTok  int64  `json:"input_micros_per_mtok"`
		OutputMicrosPerMTok int64  `json:"output_micros_per_mtok"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil
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
		features["json_schema"] = true
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

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit]
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
