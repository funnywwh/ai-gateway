package httpapi

import (
	"context"
	"net/http"
	"sync"

	"github.com/winger/ai-gateway/internal/domain"
)

// The console chat is an in-process client of the gateway's own data plane. Two pieces of
// private context make that possible without opening a hole in the public API:
//
//	verifiedIdentity — an already-authenticated key/account pair, so a chat step can be
//	                   dispatched into the real Responses handler without a bearer token.
//	chatRecording    — the recording override that keeps conversation content out of the
//	                   global request log while leaving quota, metering and settlement
//	                   exactly as they are for every other request.
//
// Both keys are unexported struct types, so only this package can install them: an HTTP
// request that merely claims to be the console cannot forge either one. Neither of them is
// derived from a header, so no client input can turn them on.

type verifiedIdentityKey struct{}

// verifiedIdentity is an API key and account that were resolved by the server itself
// (internal/apikey.VerifyID) rather than by a bearer token on the wire.
type verifiedIdentity struct {
	key     *domain.APIKey
	account *domain.Account
}

func withVerifiedIdentity(r *http.Request, id verifiedIdentity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), verifiedIdentityKey{}, id))
}

func verifiedIdentityFrom(ctx context.Context) (verifiedIdentity, bool) {
	id, ok := ctx.Value(verifiedIdentityKey{}).(verifiedIdentity)
	return id, ok
}

type chatRecordingKey struct{}

// withChatRecording marks a request as console-chat traffic. Content channels (request
// body, reasoning, output text, stored responses and hook payloads) are forced to metadata
// only; accounting channels are untouched.
func withChatRecording(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), chatRecordingKey{}, true))
}

func isChatRecording(ctx context.Context) bool {
	v, ok := ctx.Value(chatRecordingKey{}).(bool)
	return ok && v
}

type stepObserverKey struct{}

// stepOutcome carries the facts only the data plane knows about a step it just served:
// which provider actually answered, which canonical model it was routed to, and whether
// the routing had to degrade the request.
//
// The console cannot read these from response headers: only the non-streaming branch sets
// them, and a chat step is always streaming. The alternative — re-running routing to guess
// — would be a second implementation that could disagree with the first one.
type stepOutcome struct {
	Provider      string
	Canonical     string
	UpstreamModel string
	Degraded      []string
}

// stepObserver is the callback the data plane invokes once a candidate has served a step.
// It is a pointer so the chat runner can install it before the request is dispatched and
// read the result afterwards.
type stepObserver struct {
	mu       sync.Mutex
	recorded stepOutcome
	seen     bool
}

// observe records the routing outcome of a served step. It runs inside the data plane, so
// it only writes to memory the chat runner reads back after the handler returns.
func (o *stepObserver) observe(outcome stepOutcome) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.recorded = outcome
	o.seen = true
	o.mu.Unlock()
}

func withStepObserver(r *http.Request, o *stepObserver) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), stepObserverKey{}, o))
}

func stepObserverFrom(ctx context.Context) (*stepObserver, bool) {
	o, ok := ctx.Value(stepObserverKey{}).(*stepObserver)
	return o, ok
}

// snapshot returns the recorded outcome once a step has been served.
func (o *stepObserver) snapshot() (stepOutcome, bool) {
	if o == nil {
		return stepOutcome{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.recorded, o.seen
}

// reportStep hands one step's routing outcome to the observer, if a request installed one.
func reportStep(ctx context.Context, outcome stepOutcome) {
	if o, ok := stepObserverFrom(ctx); ok {
		o.observe(outcome)
	}
}
