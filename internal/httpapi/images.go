package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/usage"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// The Images API surface (M84): POST /v1/images/generations and POST /v1/images/edits.
//
// It is a separate handler rather than a branch inside handleCreateResponse because almost
// nothing is shared: no input items, no assembler, no stored response, no continuation, no
// compaction. What *is* shared is the part that must not drift — authentication, rate
// limiting, balance admission, routing, provider capacity, metering, settlement, request
// logging — and each of those is a helper call here.
//
// See docs/api-images.md for the operator-facing spec and docs/design/m84-image-generation.md
// for the decisions behind it.

const (
	endpointImageGenerations = "/v1/images/generations"
	endpointImageEdits       = "/v1/images/edits"
	// defaultImagesBodyBytes is the body ceiling used when server.images_max_body_bytes is
	// not configured (a zero there would otherwise reject every request).
	defaultImagesBodyBytes int64 = 32 << 20
	// defaultImagesReserveTokens is the per-image output-token hold used when
	// billing.images_reserve_tokens is not configured.
	defaultImagesReserveTokens int64 = 8192
)

// handleImageGeneration implements POST /v1/images/generations.
func (s *Server) handleImageGeneration(w http.ResponseWriter, r *http.Request) {
	s.serveImage(w, r, pluginapi.ImageOpGenerate, endpointImageGenerations)
}

// handleImageEdit implements POST /v1/images/edits.
func (s *Server) handleImageEdit(w http.ResponseWriter, r *http.Request) {
	s.serveImage(w, r, pluginapi.ImageOpEdit, endpointImageEdits)
}

// serveImage is the whole data path of one image request.
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, op, endpoint string) {
	ctx := r.Context()

	key, account, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	limit := s.imagesBodyLimit()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("failed to read the request body"))
		return
	}
	if int64(len(body)) > limit {
		writeAPIError(w, &domain.APIError{
			Status: http.StatusRequestEntityTooLarge, Type: domain.ErrTypeInvalidRequest,
			Code:    imageBodyTooLargeP,
			Message: fmt.Sprintf("the request body exceeds server.images_max_body_bytes (%d bytes)", limit),
		})
		return
	}

	var call *imageCall
	var apiErr *domain.APIError
	if op == pluginapi.ImageOpEdit {
		call, apiErr = parseImageEdit(r, body)
	} else {
		call, apiErr = parseImageGenerate(body)
	}
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	req, stream := call.req, call.stream

	// Admission before any upstream work, exactly like the Responses path: rate limits
	// first, then the balance hold, so a request nobody can pay for never reaches a provider.
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
		Features:    imageFeatures(op),
		ProviderPin: r.Header.Get("X-Gateway-Provider"),
		Strategy:    r.Header.Get("X-Gateway-Strategy"),
	})
	if err != nil {
		denial := toAPIError(err)
		writeAPIError(w, denial)
		s.recordImageDenied(ctx, key, account, endpoint, req, denial, len(body))
		return
	}

	// An image request only goes to a candidate that *declares* it serves images
	// (docs/api-images.md §6.3). This is deliberately stricter than the router's usual rule
	// that an unknown capability must not block a candidate: an endpoint that has never
	// advertised image support is not evidence that it can generate an image, and the failure
	// mode is expensive — the request lands on a chat-only provider, which answers with a
	// fatal error (no failover) after the client has already waited. The endpoint is new, so
	// requiring the declaration breaks no existing traffic.
	candidates, undeclared := imageCandidates(s.deps.Registry.Snapshot(), plan, op)
	if len(candidates) == 0 {
		denial := domain.ErrUnsupported(imageNoProviderMessage(plan, undeclared))
		writeAPIError(w, denial)
		s.recordImageDenied(ctx, key, account, endpoint, req, denial, len(body))
		return
	}

	startedAt := time.Now()
	requestID := requestIDFrom(ctx)

	if s.deps.Billing != nil && account != nil {
		cost, sale := s.ruleSetsFor(plan.Resolved.Canonical, candidates[0].ProviderID)
		estimate := billing.EstimateInput{
			Cost: cost, Sale: sale,
			// An image request has no max_output_tokens, so the usual formula would hold
			// only the prompt — the cheap part of an image. The image-side holds stand in
			// for it (billing.images_reserve_tokens) and are released at settlement.
			// The prompt is the only part of an image request that can be sized up front;
			// the image side of the hold comes from the two fields below.
			EstInputTokens:    estimateInputTokens([]byte(req.Prompt)),
			ImageOutputTokens: int64(req.N) * s.imagesReserveTokens(),
			ImageInputTokens:  int64(len(req.Input)) * s.imagesReserveTokens(),
			DefaultMarkupBP:   s.deps.Config.Billing.DefaultMarkupBP,
			Ledger:            s.ledgerCurrency(),
			FX:                s.fxTable(),
			// A rule set with no rate for the image dimensions falls back to the configured
			// default hold, the same way every other request does.
			DefaultReserveMicros: s.deps.Config.Billing.ReserveMicrosDefault,
		}
		decision, reservation := s.deps.Billing.Admit(account, requestID, estimate, s.inflightPolicy(account), startedAt)
		if !decision.Allowed {
			s.rejectImageQuota(w, r, key, account, endpoint, req, decision, len(body))
			return
		}
		if reservation != nil {
			defer s.deps.Billing.Release(reservation)
		}
	}

	maxAttempts := s.deps.Config.Routing.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	var (
		chosen    *domain.Candidate
		lastErr   error
		lastDims  map[string]int64
		lastUsage pluginapi.Usage
		// payload is the client-visible body of the winning non-streaming attempt.
		payload map[string]any
		// streamStarted reports that response frames (and therefore headers and a status
		// code) have already left the gateway, so a later failure must not try to write a
		// second, JSON-shaped answer.
		streamStarted bool
	)

	for i, cand := range candidates {
		if i >= maxAttempts {
			break
		}
		provReq := *req
		provReq.Model = cand.UpstreamModel
		attemptStarted := time.Now()

		var (
			attemptUsage pluginapi.Usage
			attemptErr   error
			attempt      runtime.Attempt
		)

		if stream {
			var (
				sse       *sseWriter
				completed *pluginapi.ImageEvent
			)
			var streamEnd *pluginapi.StreamEnd
			streamEnd, attempt, attemptErr = s.deps.Dispatcher.ImagesStream(ctx, cand.ProviderID, &provReq,
				func(ev pluginapi.Event) error {
					switch ev.Type {
					case pluginapi.EventImagePartial:
						if sse == nil {
							sse = newSSEWriter(w)
						}
						return s.writeImagePartial(sse, op, ev.Image)
					case pluginapi.EventImageCompleted:
						// Held back until the stream ends: the client-visible completed
						// frame carries the usage, and usage arrives after the image on the
						// canonical event stream.
						completed = ev.Image
					}
					return nil
				})
			streamStarted = sse != nil
			if attemptErr == nil && completed == nil {
				attemptErr = pluginapi.NewRetryableError("upstream_stream_incomplete",
					"the upstream stream ended without a finished image", 502)
			}
			if attemptErr == nil {
				if sse == nil {
					sse = newSSEWriter(w)
					streamStarted = true
				}
				if streamEnd != nil {
					attemptUsage = streamEnd.Usage
				}
				if writeErr := s.writeImageCompleted(sse, op, completed, attemptUsage); writeErr != nil {
					// The client is gone; there is nothing left to serve. The upstream work
					// is already paid for, so the attempt is recorded (as a failure caused by
					// the client, not by the upstream) and the audit row still has to exist:
					// a request nobody received is exactly the one an operator looks for.
					failed := contextError(writeErr)
					waitedMS := attempt.QueueWaitMS
					elapsed := int(time.Since(attemptStarted).Milliseconds()) - waitedMS
					if elapsed < 0 {
						elapsed = 0
					}
					s.recordImageAttempt(ctx, key, account, req.Model, plan.Resolved, cand, i+1,
						failed, elapsed, attemptStarted, attemptUsage)
					s.emitImageHook(ctx, key, account, plan.Resolved, "failed", attemptUsage)
					s.recordImageRequest(ctx, key, account, endpoint, req, plan.Resolved, "failed", len(body))
					ticket.Settle(totalTokensOf(attemptUsage.Dimensions))
					return
				}
			} else if sse != nil {
				s.writeImageStreamError(sse, attemptErr)
			}
		} else {
			var resp *pluginapi.ImageResponse
			resp, attempt, attemptErr = s.deps.Dispatcher.Images(ctx, cand.ProviderID, &provReq)
			if attemptErr == nil {
				attemptUsage = resp.Usage
				payload = imageResponsePayload(resp)
			}
		}

		latency := int(time.Since(attemptStarted).Milliseconds()) - attempt.QueueWaitMS
		if latency < 0 {
			latency = 0
		}
		s.logCapacityWait(requestID, cand, attempt.QueueWaitMS, attemptErr)
		s.recordImageAttempt(ctx, key, account, req.Model, plan.Resolved, cand, i+1, attemptErr,
			latency, attemptStarted, attemptUsage)

		if attemptErr == nil {
			chosen = &cand
			lastUsage = attemptUsage
			lastDims = attemptUsage.Dimensions
			s.deps.Router.NoteSuccess(plan.Affinity, cand.RouteID)
			break
		}
		lastErr = attemptErr

		// Once the client has seen image frames, failing over would contradict them.
		if streamStarted {
			break
		}
		if resetAt, isQuota := runtime.QuotaError(attemptErr); isQuota {
			s.deps.Dispatcher.CooldownForProvider(ctx, cand.RouteID, resetAt, "upstream quota exhausted")
		}
		if !runtime.Retryable(attemptErr) {
			break
		}
		if s.deps.Router.NoteFailure(plan.Affinity, cand.RouteID) && s.deps.Log != nil {
			s.deps.Log.Info("session affinity dropped after a retryable failure",
				"request_id", requestID, "route", cand.RouteID, "provider", cand.ProviderName)
		}
	}

	if chosen == nil {
		s.emitImageHook(ctx, key, account, plan.Resolved, "failed", lastUsage)
		if !streamStarted {
			apiErr := imageUpstreamError(lastErr)
			var busy *runtime.CapacityError
			if errors.As(lastErr, &busy) {
				apiErr = domain.ErrProviderBusy(busy.Error())
				w.Header().Set("Retry-After", fmt.Sprint(busy.RetryAfterSeconds()))
			}
			writeAPIError(w, apiErr)
			s.recordImageDenied(ctx, key, account, endpoint, req, apiErr, len(body))
		} else {
			// The failure already travelled in-band on the stream; only the audit row is
			// still owed.
			s.recordImageRequest(ctx, key, account, endpoint, req, plan.Resolved, "failed", len(body))
		}
		ticket.Settle(totalTokensOf(lastDims))
		return
	}

	if !stream {
		w.Header().Set("x-gateway-provider", chosen.ProviderName)
		w.Header().Set("x-gateway-model", plan.Resolved.Canonical)
		if len(chosen.Degraded) > 0 {
			w.Header().Set("x-gateway-degraded", strings.Join(chosen.Degraded, ","))
		}
		writeJSON(w, http.StatusOK, payload)
	}

	s.emitImageHook(ctx, key, account, plan.Resolved, "completed", lastUsage)
	s.recordImageRequest(ctx, key, account, endpoint, req, plan.Resolved, "completed", len(body))
	ticket.Settle(totalTokensOf(lastDims))
}

// imagesBodyLimit resolves the body ceiling for the two image endpoints.
func (s *Server) imagesBodyLimit() int64 {
	if s.deps.Config != nil && s.deps.Config.Server.ImagesMaxBodyBytes > 0 {
		return s.deps.Config.Server.ImagesMaxBodyBytes
	}
	return defaultImagesBodyBytes
}

// imagesReserveTokens is the per-image output-token hold used to reserve balance.
func (s *Server) imagesReserveTokens() int64 {
	if s.deps.Config != nil && s.deps.Config.Billing.ImagesReserveTokens > 0 {
		return s.deps.Config.Billing.ImagesReserveTokens
	}
	return defaultImagesReserveTokens
}

// imageFeatures derives the capability features an image request requires.
//
// image_generation is what routes an image request at all: without it the request would be
// routed exactly like a chat one and land on a model that cannot produce an image. An edit
// additionally requires `image` — reading reference images is a different ability from
// writing one.
func imageFeatures(op string) map[string]bool {
	features := map[string]bool{routing.CapabilityImageGeneration: true}
	if op == pluginapi.ImageOpEdit {
		features[routing.CapabilityImage] = true
	}
	return features
}

// imageCandidates keeps only the candidates whose provider-model row *declares* the
// capabilities this image request needs, and returns the names of the ones it dropped.
//
// Three kinds of candidate are dropped, and they are dropped for the same reason — none of
// them promised to produce an image:
//
//   - one that declares `image_generation` false (or declares a set without it);
//   - one that declares nothing at all: the router treats "unknown" as permissive so an
//     undeclared model stays usable for chat, but a request whose whole point is an image has
//     to land on a provider that says it can make one;
//   - one whose declaration was resolved to unknown by `capabilities_override: inherit`,
//     which is the documented "exempt from capability checks" escape hatch — for the image
//     endpoint that escape hatch has no meaning, because there is no default behaviour to
//     fall back to.
//
// The read is EffectiveCapabilities, i.e. exactly what routing and the model listing use, so
// a candidate can never be routed here on a set of facts that the listing did not disclose.
func imageCandidates(snap *registry.Snapshot, plan *routing.Result, op string) ([]domain.Candidate, []string) {
	needed := []string{routing.CapabilityImageGeneration}
	if op == pluginapi.ImageOpEdit {
		needed = append(needed, routing.CapabilityImage)
	}
	kept := make([]domain.Candidate, 0, len(plan.Candidates))
	var dropped []string
	for _, cand := range plan.Candidates {
		pm := snap.ProviderModel(cand.ProviderID, plan.Resolved.Canonical)
		caps := routing.EffectiveCapabilities(pm)
		declared := true
		for _, key := range needed {
			if !caps[key] {
				declared = false
				break
			}
		}
		if !declared {
			dropped = append(dropped, cand.ProviderName)
			continue
		}
		kept = append(kept, cand)
	}
	return kept, dropped
}

// imageNoProviderMessage explains a request no candidate can serve, naming the capability
// that was missing and the candidates that did not declare it. An operator reading it should
// not have to guess whether the model has no route or the route forgot its declaration.
func imageNoProviderMessage(plan *routing.Result, undeclared []string) string {
	model := ""
	if plan != nil && plan.Resolved != nil {
		model = plan.Resolved.Canonical
	}
	if len(undeclared) > 0 {
		return "no provider declares " + routing.CapabilityImageGeneration + " for model " + model +
			" (candidates that do not declare it: " + strings.Join(undeclared, ", ") +
			"; add the capability to the provider-model row that should serve image requests)"
	}
	return "no provider can serve images for model " + model
}

// imageResponsePayload renders the canonical image response as the OpenAI-shaped body.
//
// The usage object is rebuilt from the metered dimensions rather than forwarded verbatim: the
// dimensions are what the gateway metered and billed, so what the client sees and what the
// invoice says are the same numbers by construction. An estimated usage is omitted rather
// than dressed up as the upstream's own report.
func imageResponsePayload(resp *pluginapi.ImageResponse) map[string]any {
	if resp == nil {
		return map[string]any{}
	}
	data := make([]map[string]any, 0, len(resp.Data))
	for _, img := range resp.Data {
		entry := map[string]any{}
		if img.B64JSON != "" {
			entry["b64_json"] = img.B64JSON
		}
		if img.URL != "" {
			entry["url"] = img.URL
		}
		if img.RevisedPrompt != "" {
			entry["revised_prompt"] = img.RevisedPrompt
		}
		data = append(data, entry)
	}
	payload := map[string]any{"created": resp.Created, "data": data}
	if usage := imageUsagePayload(resp.Usage); usage != nil {
		payload["usage"] = usage
	}
	for name, value := range map[string]string{
		"size": resp.Size, "quality": resp.Quality,
		"background": resp.Background, "output_format": resp.OutputFormat,
	} {
		if value != "" {
			payload[name] = value
		}
	}
	return payload
}

// imageUsagePayload rebuilds the upstream's usage object from the metered dimensions.
func imageUsagePayload(u pluginapi.Usage) map[string]any {
	if u.Estimated || len(u.Dimensions) == 0 {
		return nil
	}
	textTokens := u.Dimensions["input"]
	imageTokens := u.Dimensions["image_input"]
	outputTokens := u.Dimensions["image_output"]
	if outputTokens == 0 {
		outputTokens = u.Dimensions["output"]
	}
	if textTokens == 0 && imageTokens == 0 && outputTokens == 0 {
		return nil
	}
	return map[string]any{
		"input_tokens": textTokens + imageTokens,
		"input_tokens_details": map[string]any{
			"text_tokens": textTokens, "image_tokens": imageTokens,
		},
		"output_tokens": outputTokens,
		"total_tokens":  textTokens + imageTokens + outputTokens,
	}
}

// imageStreamPrefix is the upstream's event-name family for one operation.
func imageStreamPrefix(op string) string {
	if op == pluginapi.ImageOpEdit {
		return "image_edit"
	}
	return "image_generation"
}

// writeImagePartial forwards one partial image as the client-visible SSE frame.
func (s *Server) writeImagePartial(sse *sseWriter, op string, image *pluginapi.ImageEvent) error {
	if sse == nil || image == nil {
		return nil
	}
	name := imageStreamPrefix(op) + ".partial_image"
	return sse.sendRaw(name, map[string]any{
		"type": name, "b64_json": image.B64JSON, "partial_image_index": image.PartialIndex,
		"created_at": image.Created, "size": image.Size, "quality": image.Quality,
		"background": image.Background, "output_format": image.OutputFormat,
	})
}

// writeImageCompleted forwards the finished image as the client-visible SSE frame.
//
// The event names and payload keys are the Images API's own (docs/api-images.md §5), so a
// client using the official SDK parses them without knowing this gateway exists. Usage travels
// in this frame, which is where the SDK expects it; the gateway's own metering uses the same
// numbers from the stream's usage event.
func (s *Server) writeImageCompleted(sse *sseWriter, op string, image *pluginapi.ImageEvent, u pluginapi.Usage) error {
	if sse == nil || image == nil {
		return nil
	}
	name := imageStreamPrefix(op) + ".completed"
	payload := map[string]any{
		"type": name, "b64_json": image.B64JSON, "created_at": image.Created,
		"size": image.Size, "quality": image.Quality, "background": image.Background,
		"output_format": image.OutputFormat,
	}
	if usage := imageUsagePayload(u); usage != nil {
		payload["usage"] = usage
	}
	return sse.sendRaw(name, payload)
}

// writeImageStreamError ends an already-started stream with an error frame. The status code is
// gone by then (headers went out with the first frame), so the error travels in-band — the same
// shape the Responses stream uses for a mid-stream failure.
func (s *Server) writeImageStreamError(sse *sseWriter, err error) {
	if sse == nil {
		return
	}
	apiErr := imageUpstreamError(err)
	_ = sse.sendRaw("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type": string(apiErr.Type), "code": apiErr.Code, "message": apiErr.Message,
		},
	})
}

// imageUpstreamError maps a provider failure onto the client-visible error.
//
// It exists because the generic mapping (toAPIError) turns every unrecognised error into a 500:
// for a chat turn that is defensible, but an image request rejected for a bad size is the
// client's own mistake, and hiding it behind "internal_error" leaves the caller with nothing to
// fix. The upstream's status is therefore preserved for 4xx, and its message always travels —
// a 5xx still reads as a gateway-visible upstream failure (502), because that is what it is
// from the client's side.
func imageUpstreamError(err error) *domain.APIError {
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		return toAPIError(err)
	}
	// A provider that cannot serve images at all is a configuration statement, not an
	// upstream failure: the operator declared image_generation on a provider whose kind has
	// no images implementation. Reporting it as a 400 with the provider's own words beats a
	// 500 that reads like the gateway broke.
	if apiErr.Code == "unsupported_method" {
		return domain.ErrInvalidRequest(apiErr.Message)
	}
	if apiErr.HTTPStatus < 400 {
		return toAPIError(err)
	}
	out := &domain.APIError{Status: apiErr.HTTPStatus, Code: apiErr.Code, Message: apiErr.Message}
	switch {
	case apiErr.HTTPStatus == http.StatusUnauthorized:
		out.Type = domain.ErrTypeAuthentication
	case apiErr.HTTPStatus == http.StatusForbidden:
		out.Type = domain.ErrTypePermission
	case apiErr.HTTPStatus == http.StatusTooManyRequests:
		out.Type = domain.ErrTypeRateLimit
	case apiErr.HTTPStatus == http.StatusNotFound:
		out.Type = domain.ErrTypeNotFound
	case apiErr.HTTPStatus < 500:
		out.Type = domain.ErrTypeInvalidRequest
	default:
		out.Type = domain.ErrTypeAPI
		out.Status = http.StatusBadGateway
	}
	return out
}

// contextError marks a failure caused by the client going away, so the audit row can say so
// instead of blaming the upstream.
func contextError(err error) error {
	if err == nil {
		return nil
	}
	return pluginapi.NewError("client_disconnected", err.Error())
}

// recordImageAttempt writes one usage_records row per upstream attempt (and, with billing
// enabled, settles it in the same transaction).
//
// It is the image-shaped sibling of recordAttempt: the chat version takes a Responses request
// and an assembler, and neither exists here. The fields that decide what an operator sees —
// provider, upstream model, route, status, error code, degraded features — are identical.
func (s *Server) recordImageAttempt(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	model string,
	resolved *domain.ResolvedModel,
	cand domain.Candidate,
	attemptNo int,
	attemptErr error,
	latencyMS int,
	startedAt time.Time,
	u pluginapi.Usage,
) {
	if s.deps.Meter == nil {
		return
	}
	dims := u.Dimensions
	if dims == nil {
		dims = map[string]int64{}
	}
	status := "completed"
	errorCode := ""
	terminated := "completed"
	if attemptErr != nil {
		status = "failed"
		terminated = "upstream_error"
		var busy *runtime.CapacityError
		switch {
		case errors.As(attemptErr, &busy):
			errorCode = "provider_busy"
			terminated = "provider_capacity"
		default:
			if apiErr, ok := pluginapi.IsError(attemptErr); ok {
				errorCode = apiErr.Code
				if apiErr.Kind == pluginapi.KindQuotaExhausted {
					terminated = "aborted_quota"
				}
			} else {
				errorCode = "upstream_error"
			}
		}
	}

	attempt := &usage.Attempt{
		RequestID:        requestIDFrom(ctx),
		AttemptNo:        attemptNo,
		AccountID:        account.ID,
		APIKeyID:         key.ID,
		Model:            model,
		ResolvedModel:    resolved.Canonical,
		ProviderID:       cand.ProviderID,
		RouteID:          cand.RouteID,
		UpstreamModel:    cand.UpstreamModel,
		Dimensions:       dims,
		Estimated:        u.Estimated,
		LatencyMS:        latencyMS,
		Status:           status,
		ErrorCode:        errorCode,
		DegradedFeatures: cand.Degraded,
		TerminatedReason: terminated,
	}
	if s.deps.Billing != nil {
		s.settleAttempt(ctx, attempt, resolved, cand, attemptErr, startedAt, dims, false,
			s.resolveMarkup(key, account, s.saleRulesFor(resolved.Canonical, cand.ProviderID)))
		return
	}
	if _, err := s.deps.Meter.Record(ctx, attempt); err != nil {
		s.deps.Log.Warn("recording usage failed", "err", err, "request_id", requestIDFrom(ctx))
	}
}

// recordImageRequest writes the request-log row of a served image request.
//
// What is recorded is the prompt and the parameters; the images themselves are never stored.
// A "full" policy stores the canonical request document with each reference image replaced by
// its name, media type and size, which is the honest maximum: the base64 payload of one edit
// can be tens of megabytes, and a log that stores it stops being readable long before it stops
// being a privacy problem.
func (s *Server) recordImageRequest(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	endpoint string,
	req *pluginapi.ImageRequest,
	resolved *domain.ResolvedModel,
	status string,
	bodyBytes int,
) {
	if s.deps.Records == nil {
		return
	}
	mode, payload, truncated := s.imageInputRecord(ctx, key, req)
	rec := &domain.RequestLogRecord{
		RequestID:       requestIDFrom(ctx),
		APIKeyID:        key.ID,
		AccountID:       account.ID,
		Endpoint:        endpoint,
		Model:           req.Model,
		ResolvedModel:   resolved.Canonical,
		RequestJSON:     payload,
		RequestBytes:    bodyBytes,
		Truncated:       truncated,
		RecordInputMode: mode,
		Status:          status,
		CreatedAt:       time.Now().UTC(),
	}
	s.storeRequestLog(ctx, rec)
}

// imageInputRecord applies the input channel of the recording policy to an image request and
// reports the payload, its mode and whether it was cut.
func (s *Server) imageInputRecord(ctx context.Context, key *domain.APIKey, req *pluginapi.ImageRequest) (string, string, bool) {
	cfg := s.deps.Config.Recording
	mode := cfg.InputModeFor(key.RecordInputMode)
	if isChatRecording(ctx) {
		mode = "off"
	}
	payload := ""
	truncated := false
	switch mode {
	case "full":
		payload = redact(string(mustJSON(imageLogDocument(req))), cfg.RedactPaths)
	case "user":
		payload = redactInputText(strings.TrimSpace(req.Prompt), cfg.RedactPaths)
	}
	if limit := s.recordingLimit(); len(payload) > limit {
		payload = truncate(payload, limit)
		truncated = true
	}
	return mode, payload, truncated
}

// imageLogDocument renders what the request log stores for an image request: the parameters
// and, for each reference image, its identity rather than its bytes.
func imageLogDocument(req *pluginapi.ImageRequest) map[string]any {
	doc := map[string]any{
		"op": req.Op, "model": req.Model, "prompt": req.Prompt, "n": req.N,
	}
	for name, value := range map[string]string{
		"size": req.Size, "quality": req.Quality, "background": req.Background,
		"output_format": req.OutputFormat, "moderation": req.Moderation,
		"input_fidelity": req.InputFidelity, "response_format": req.ResponseFormat,
		"user": req.User,
	} {
		if strings.TrimSpace(value) != "" {
			doc[name] = value
		}
	}
	if req.OutputCompression != nil {
		doc["output_compression"] = *req.OutputCompression
	}
	if req.PartialImages > 0 {
		doc["partial_images"] = req.PartialImages
	}
	if len(req.Input) > 0 {
		images := make([]map[string]any, 0, len(req.Input))
		for _, img := range req.Input {
			images = append(images, map[string]any{"name": img.Name, "mime": img.MIME, "bytes": len(img.Data)})
		}
		doc["input_images"] = images
	}
	if req.Mask != nil {
		doc["mask"] = map[string]any{"name": req.Mask.Name, "mime": req.Mask.MIME, "bytes": len(req.Mask.Data)}
	}
	if len(req.Extra) > 0 {
		doc["extra"] = req.Extra
	}
	return doc
}

// recordImageDenied logs a locally rejected image request. No usage row is written: the request
// never reached an upstream, so it must not appear in billing at all.
func (s *Server) recordImageDenied(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	endpoint string,
	req *pluginapi.ImageRequest,
	apiErr *domain.APIError,
	bodyBytes int,
) {
	if s.deps.Records == nil {
		return
	}
	mode, payload, truncated := s.imageInputRecord(ctx, key, req)
	rec := &domain.RequestLogRecord{
		RequestID:       requestIDFrom(ctx),
		APIKeyID:        key.ID,
		AccountID:       account.ID,
		Endpoint:        endpoint,
		Model:           req.Model,
		RequestJSON:     payload,
		RequestBytes:    bodyBytes,
		Truncated:       truncated,
		RecordInputMode: mode,
		Status:          fmt.Sprint(apiErr.Status),
		CreatedAt:       time.Now().UTC(),
	}
	s.storeRequestLog(ctx, rec)
}

// rejectImageQuota writes the 402 (or 429) that ends an image request the account cannot pay
// for. It mirrors rejectForQuota, with a message that names the right knob — there is no
// max_output_tokens to lower here.
func (s *Server) rejectImageQuota(
	w http.ResponseWriter,
	r *http.Request,
	key *domain.APIKey,
	account *domain.Account,
	endpoint string,
	req *pluginapi.ImageRequest,
	decision billing.Admission,
	bodyBytes int,
) {
	reason := decision.Reason
	if reason == "" {
		reason = "insufficient_quota"
	}
	message := "insufficient balance for this image request; top up the account"
	if reason == "account_suspended" || reason == "account_closed" {
		message = "account is " + account.Status
	}
	apiErr := domain.ErrInsufficientQuota(message)
	if s.deps.Config.Billing.RejectAs429 {
		apiErr = domain.ErrRateLimited(message)
	}
	if s.deps.Log != nil {
		s.deps.Log.Info("image request rejected for quota",
			"request_id", requestIDFrom(r.Context()), "key", key.Name, "account", account.Name,
			"reason", reason, "available_micros", decision.AvailableMicros,
			"reserve_micros", decision.ReserveMicros)
	}
	if s.deps.Hooks != nil {
		s.deps.Hooks.Emit(r.Context(), &domain.Event{
			Name:      "request.denied",
			Timestamp: time.Now().UTC(),
			Payload: map[string]any{
				"request_id": requestIDFrom(r.Context()),
				"account":    account.Name,
				"api_key":    key.Name,
				"model":      req.Model,
				"endpoint":   endpoint,
				"reason":     reason,
			},
		})
	}
	s.recordImageDenied(r.Context(), key, account, endpoint, req, apiErr, bodyBytes)
	writeAPIError(w, apiErr)
}

// emitImageHook publishes the image analogue of the response.completed hook events. The payload
// carries the meter and the routing facts, never the image itself.
func (s *Server) emitImageHook(
	ctx context.Context,
	key *domain.APIKey,
	account *domain.Account,
	resolved *domain.ResolvedModel,
	status string,
	u pluginapi.Usage,
) {
	if s.deps.Hooks == nil {
		return
	}
	event := "image.completed"
	if status != "completed" {
		event = "image.failed"
	}
	accountName := ""
	if account != nil {
		accountName = account.Name
	}
	model := ""
	if resolved != nil {
		model = resolved.Canonical
	}
	s.deps.Hooks.Emit(ctx, &domain.Event{
		Name: event, Timestamp: time.Now().UTC(),
		Payload: map[string]any{
			"request_id": requestIDFrom(ctx),
			"account":    accountName,
			"api_key":    key.Name,
			"model":      model,
			"status":     status,
			"usage":      u.Dimensions,
			"estimated":  u.Estimated,
		},
	})
}
