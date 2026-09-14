package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/runtime"
)

// The provider concurrency ceiling is an end-to-end property: `providers.max_inflight` is
// read from the registry snapshot, an excess attempt waits in the gate instead of failing,
// the wait is gateway time rather than upstream latency, and a request that cannot be
// admitted at all comes back as 429 provider_busy. These tests drive the real HTTP surface,
// so all four are checked against what a client actually receives.

// streamOnce posts one streaming request and drains its event stream. It returns errors
// instead of failing the test because the concurrency tests call it from goroutines, where
// t.Fatal must not be used.
func streamOnce(f *fixture, body string) (status int, events []string, err error) {
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
			events = append(events, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return resp.StatusCode, events, err
	}
	return resp.StatusCode, events, nil
}

type streamOutcome struct {
	status int
	events []string
	err    error
}

// startStream launches one streaming request and reports its outcome on the channel.
func startStream(f *fixture, body string) <-chan streamOutcome {
	out := make(chan streamOutcome, 1)
	go func() {
		status, events, err := streamOnce(f, body)
		out <- streamOutcome{status: status, events: events, err: err}
	}()
	return out
}

func awaitStream(t *testing.T, ch <-chan streamOutcome, what string) streamOutcome {
	t.Helper()
	select {
	case result := <-ch:
		if result.err != nil {
			t.Fatalf("%s failed: %v", what, result.err)
		}
		if result.status != http.StatusOK {
			t.Fatalf("%s got HTTP %d, want 200", what, result.status)
		}
		if len(result.events) == 0 || result.events[len(result.events)-1] != "response.completed" {
			t.Fatalf("%s did not complete: %v", what, result.events)
		}
		return result
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never finished", what)
		return streamOutcome{}
	}
}

func TestProviderCeilingQueuesARequestInsteadOfFailingIt(t *testing.T) {
	// One attempt costs about 200ms of upstream time (two chunks 100ms apart).
	f := newFixture(t,
		withProviderCeiling(1, `{"chunks":2,"delay_ms":100}`),
		withCapacityQueue(5*time.Second, 0),
	)
	provider := f.providerID(t)
	body := `{"model":"echo-model","input":"ping","stream":true}`

	first := startStream(f, body)
	waitForCondition(t, func() bool {
		return f.srv.deps.Dispatcher.CapacityStats()[provider].Inflight == 1
	}, "the first request to hold the provider slot")

	started := time.Now()
	second := startStream(f, body)
	waitForCondition(t, func() bool {
		stats, ok := f.srv.deps.Dispatcher.CapacityStats()[provider]
		return ok && stats.Waiting == 1
	}, "the second request to queue")

	// The queued request must be admitted without the operator doing anything: the ceiling
	// releases the slot when the first attempt finishes.
	awaitStream(t, first, "the first request")
	awaitStream(t, second, "the queued request")
	elapsed := time.Since(started)

	// A ceiling of 1 serialises them, so the second needs a second upstream call's worth of
	// time after the first one ends.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("the queued request finished %v after queueing: it did not wait for the slot", elapsed)
	}
	if stats := f.srv.deps.Dispatcher.CapacityStats()[provider]; stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("the gate must be idle once both requests finished: %+v", stats)
	}

	// The wait is gateway time, not upstream latency: latency_ms must stay near one upstream
	// call even for the request that queued behind the other.
	rows := f.usageRows(t)
	if len(rows) != 2 {
		t.Fatalf("expected one usage row per attempt, got %d", len(rows))
	}
	for _, row := range rows {
		if row.latencyMS <= 0 {
			t.Fatalf("latency_ms is missing on %+v", row)
		}
		if row.latencyMS >= 300 {
			t.Fatalf("latency_ms = %d contains the queue wait: it must measure the upstream call only", row.latencyMS)
		}
	}
}

func TestProviderCeilingRefusesWithProviderBusyWhenQueueingIsOff(t *testing.T) {
	f := newFixture(t,
		withProviderCeiling(1, `{"chunks":4,"delay_ms":100}`),
		withCapacityQueue(0, 0), // queueing disabled: an excess attempt fails at once
	)
	provider := f.providerID(t)

	// Hold the single slot with a stream that stays open for ~400ms.
	holder := startStream(f, `{"model":"echo-model","input":"hold","stream":true}`)
	waitForCondition(t, func() bool {
		return f.srv.deps.Dispatcher.CapacityStats()[provider].Inflight == 1
	}, "the first request to hold the provider slot")

	resp := f.do(t, "POST", "/v1/responses", `{"model":"echo-model","input":"rejected","stream":false}`, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once every candidate is at its ceiling", resp.StatusCode)
	}
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter == "" {
		t.Fatal("a provider-capacity refusal must carry Retry-After")
	}
	// A capacity refusal is not the account's own quota, and its headers must not say so.
	if limit := resp.Header.Get("x-ratelimit-limit-requests"); limit != "" {
		t.Fatalf("a provider-capacity refusal must not carry x-ratelimit-*: %q", limit)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding the error payload: %v", err)
	}
	if payload.Error.Code != "provider_busy" || payload.Error.Type != "rate_limit_error" {
		t.Fatalf("error = %s/%s, want rate_limit_error/provider_busy", payload.Error.Type, payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "concurrency limit") {
		t.Fatalf("the message must explain the refusal, got %q", payload.Error.Message)
	}

	awaitStream(t, holder, "the holding request")

	// The refused attempt is recorded under its own code, and it is never charged.
	rows := f.usageRows(t)
	refused := false
	for _, row := range rows {
		if row.errorCode != "provider_busy" {
			continue
		}
		refused = true
		if row.terminatedReason != "provider_capacity" {
			t.Fatalf("terminated_reason = %q, want provider_capacity", row.terminatedReason)
		}
		if row.chargeMicros != 0 {
			t.Fatalf("a refused attempt must not be charged, got %d micros", row.chargeMicros)
		}
	}
	if !refused {
		t.Fatalf("no usage row carries error_code=provider_busy: %+v", rows)
	}
}

func TestProviderCeilingIsInvisibleWhileUnset(t *testing.T) {
	// The default deployment: no ceiling, no queue, no gate state.
	f := newFixture(t)

	body := `{"model":"echo-model","input":"ping","stream":true}`
	first := startStream(f, body)
	second := startStream(f, body)
	awaitStream(t, first, "the first request")
	awaitStream(t, second, "the second request")

	if stats := f.srv.deps.Dispatcher.CapacityStats(); len(stats) != 0 {
		t.Fatalf("an unset ceiling must not be tracked: %+v", stats)
	}
}

func TestCapacityPolicyIsReportedToTheAdminSurface(t *testing.T) {
	f := newFixture(t,
		withProviderCeiling(2, `{}`),
		withCapacityQueue(7*time.Second, 5),
	)
	provider := f.providerID(t)
	// A registry reload is what creates the gate entries for providers that already carry a
	// ceiling (the composition root calls this).
	f.srv.deps.Dispatcher.SyncLimits()

	block := f.srv.capacityBlock()
	if block == nil {
		t.Fatal("the capacity block must exist once the port is wired")
	}
	if block["queue_wait_s"] != 7 || block["queue_max_waiters"] != 5 {
		t.Fatalf("block = %+v, want the live queue policy", block)
	}
	providers, ok := block["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers block = %T, want a map keyed by provider id", block["providers"])
	}
	entry, ok := providers[fmt.Sprint(provider)]
	if !ok {
		t.Fatalf("provider %d is missing from %+v", provider, providers)
	}
	stat, ok := entry.(runtime.CapacityStat)
	if !ok || stat.Limit != 2 {
		t.Fatalf("provider entry = %#v, want a stat with limit 2", entry)
	}
	if stat.Inflight != 0 || stat.Waiting != 0 {
		t.Fatalf("no request ran, yet the gate reports load: %+v", stat)
	}
}

// usageRow is the subset of usage_records these tests assert on.
type usageRow struct {
	latencyMS        int
	chargeMicros     int64
	errorCode        string
	terminatedReason string
}

// usageRows reads this account's usage rows straight from the store: they are where the
// per-attempt facts the public API does not expose live.
func (f *fixture) usageRows(t *testing.T) []usageRow {
	t.Helper()
	records, err := f.db.ListUsage(context.Background(), f.key.AccountID, time.Time{}, time.Now().Add(time.Minute), 50)
	if err != nil {
		t.Fatalf("listing usage rows: %v", err)
	}
	rows := make([]usageRow, 0, len(records))
	for _, rec := range records {
		rows = append(rows, usageRow{
			latencyMS: rec.LatencyMS, chargeMicros: rec.ChargeMicros,
			errorCode: rec.ErrorCode, terminatedReason: rec.TerminatedReason,
		})
	}
	return rows
}

// providerID returns the echo provider's id: the key every capacity accessor uses.
func (f *fixture) providerID(t *testing.T) int64 {
	t.Helper()
	providers, err := f.db.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("listing providers: %v", err)
	}
	for _, provider := range providers {
		if provider.Name == "echo" {
			return provider.ID
		}
	}
	t.Fatal("the echo provider is missing from the fixture")
	return 0
}

// waitForCondition polls until cond holds, so a test observes concurrent state instead of
// guessing at a sleep.
func waitForCondition(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
