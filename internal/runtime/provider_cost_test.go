package runtime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// costSourceStub is a CostSource that counts calls and answers from a map, so a test can prove
// both the numbers and "no provider with a cap ⇒ no query at all".
type costSourceStub struct {
	mu      sync.Mutex
	calls   [][]int64
	answers map[int64]int64
	err     error
}

func (s *costSourceStub) ProviderCostsSince(_ context.Context, windows map[int64]time.Time) (map[int64]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(windows))
	for id := range windows {
		ids = append(ids, id)
	}
	s.calls = append(s.calls, ids)
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[int64]int64, len(windows))
	for id := range windows {
		out[id] = s.answers[id]
	}
	return out, nil
}

func (s *costSourceStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func costTrackerFixture(t *testing.T, providers ...*domain.Provider) (*CostTracker, *costSourceStub) {
	t.Helper()
	src := &costSourceStub{answers: map[int64]int64{}}
	reg := registry.NewStatic(registry.NewSnapshot(nil, providers, nil, nil, nil, nil, nil))
	tracker := NewCostTracker(src, reg, slog.New(slog.DiscardHandler))
	tracker.interval = time.Millisecond
	return tracker, src
}

func cappedProvider(id int64, limit int64) *domain.Provider {
	return &domain.Provider{ID: id, Name: "p" + string(rune('0'+id)), Kind: "testecho",
		Enabled: true, CostLimitMicros: limit, CostPeriod: domain.CostPeriodNone}
}

// A deployment that never sets a cap must pay nothing: no query, no entries, no blocking.
func TestCostTrackerQueriesNothingWithoutACap(t *testing.T) {
	uncapped := &domain.Provider{ID: 1, Name: "plain", Kind: "testecho", Enabled: true}
	tracker, src := costTrackerFixture(t, uncapped)
	ctx := context.Background()

	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if src.callCount() != 0 {
		t.Fatalf("an uncapped deployment issued %d queries, want 0", src.callCount())
	}
	if stats := tracker.Stats(); len(stats) != 0 {
		t.Fatalf("uncapped providers must not appear in the stats: %v", stats)
	}
	now := time.Now().UTC()
	if tracker.Exceeded(uncapped, now) {
		t.Fatal("a provider without a cap is never exceeded")
	}
	if tracker.Exceeded(nil, now) {
		t.Fatal("a nil provider is never exceeded")
	}
}

func TestCostTrackerBlocksAtTheCapAndRecoversWhenItIsRaised(t *testing.T) {
	provider := cappedProvider(7, 1_000)
	tracker, src := costTrackerFixture(t, provider)
	ctx := context.Background()
	now := time.Now().UTC()

	src.answers[7] = 999
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if tracker.Exceeded(provider, now) {
		t.Fatal("999 of 1000 must not be exceeded")
	}

	src.answers[7] = 1_000
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !tracker.Exceeded(provider, now) {
		t.Fatal("reaching the cap exactly must block the provider")
	}
	stat := tracker.Stats()[7]
	if !stat.Exceeded || stat.UsedMicros != 1_000 || stat.LimitMicros != 1_000 {
		t.Fatalf("stat = %+v", stat)
	}

	// Raising the cap is effective immediately: the comparison uses the snapshot's limit, so
	// no refresh is needed for the operator's fix to land.
	provider.CostLimitMicros = 2_000
	if tracker.Exceeded(provider, now) {
		t.Fatal("raising the cap must release the provider without waiting for a refresh")
	}

	// Removing the cap releases it too, and it disappears from the read-back.
	provider.CostLimitMicros = 0
	if tracker.Exceeded(provider, now) {
		t.Fatal("a removed cap must release the provider")
	}
	if stats := tracker.Stats(); len(stats) != 0 {
		t.Fatalf("a removed cap must remove the stat entry: %v", stats)
	}
}

// The window the reading belongs to is derived from the provider's configuration, and the
// tracker must ask for exactly that instant.
func TestCostTrackerAsksForTheProvidersOwnWindowStart(t *testing.T) {
	reset := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)
	daily := cappedProvider(1, 1_000)
	daily.CostPeriod = domain.CostPeriodDaily
	since := cappedProvider(2, 1_000)
	since.CostWindowStart = &reset

	tracker, _ := costTrackerFixture(t, daily, since)
	captured := map[int64]time.Time{}
	src := &costSourceStub{answers: map[int64]int64{}}
	tracker.src = costCapture{captured: captured, inner: src}
	if err := tracker.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	wantDaily := time.Now().UTC().Truncate(24 * time.Hour)
	if got := captured[1]; !got.Equal(wantDaily) {
		t.Fatalf("daily window start = %s, want %s", got, wantDaily)
	}
	if got := captured[2]; !got.Equal(reset) {
		t.Fatalf("reset window start = %s, want %s", got, reset)
	}
}

type costCapture struct {
	captured map[int64]time.Time
	inner    CostSource
}

func (c costCapture) ProviderCostsSince(ctx context.Context, windows map[int64]time.Time) (map[int64]int64, error) {
	for id, start := range windows {
		c.captured[id] = start
	}
	return c.inner.ProviderCostsSince(ctx, windows)
}

// A failed read keeps the last number and does not stop traffic, but it must be visible: the
// status carries the error, and a recovery clears it.
func TestCostTrackerFailsOpenAndReportsTheFailure(t *testing.T) {
	provider := cappedProvider(3, 100)
	tracker, src := costTrackerFixture(t, provider)
	ctx := context.Background()

	src.answers[3] = 50
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if status := tracker.Status(); status.LastError != "" || status.Tracked != 1 {
		t.Fatalf("healthy status = %+v", status)
	}
	if status := tracker.Status(); status.IntervalS != 0 && status.IntervalS != int(CostRefreshInterval/time.Second) {
		t.Fatalf("interval reported as %d", status.IntervalS)
	}

	src.err = errors.New("database is locked")
	if err := tracker.Refresh(ctx); err == nil {
		t.Fatal("a failed read must be reported to the caller")
	}
	if status := tracker.Status(); status.LastError == "" {
		t.Fatal("the failure must be visible in the status")
	}
	if tracker.Exceeded(provider, time.Now().UTC()) {
		t.Fatal("a stale reading below the cap must not start blocking")
	}

	src.err = nil
	src.answers[3] = 100
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !tracker.Exceeded(provider, time.Now().UTC()) {
		t.Fatal("the reading must be used again after a recovery")
	}
	if status := tracker.Status(); status.LastError != "" {
		t.Fatalf("a successful refresh must clear the error, got %q", status.LastError)
	}
}

// Before the first successful read there is no evidence, and a cost guard must not stop
// traffic on a number it does not have. A newly capped provider is anchored at "now" by the
// write path, so "no reading" really does mean "nothing spent yet".
func TestCostTrackerDoesNotBlockWithoutAReading(t *testing.T) {
	provider := cappedProvider(5, 1)
	tracker, src := costTrackerFixture(t, provider)
	src.err = errors.New("not readable")
	if err := tracker.Refresh(context.Background()); err == nil {
		t.Fatal("expected the read to fail")
	}
	if tracker.Exceeded(provider, time.Now().UTC()) {
		t.Fatal("a provider with no reading must not be blocked")
	}
}

// Resetting is exact: the write path moved the window start to now, so the reading is 0 and
// the provider becomes usable again without waiting for the next interval.
func TestCostTrackerMarkResetReleasesTheProvider(t *testing.T) {
	provider := cappedProvider(4, 100)
	tracker, src := costTrackerFixture(t, provider)
	ctx := context.Background()
	src.answers[4] = 500
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !tracker.Exceeded(provider, time.Now().UTC()) {
		t.Fatal("the provider should be over its cap")
	}

	at := time.Now().UTC()
	tracker.MarkReset(4, at)
	if tracker.Exceeded(provider, at) {
		t.Fatal("a reset must release the provider immediately")
	}
	if stat := tracker.Stats()[4]; stat.UsedMicros != 0 || !stat.WindowStart.Equal(at) {
		t.Fatalf("stat after reset = %+v", stat)
	}

	// A provider the tracker does not track has nothing to reset, and that must be a no-op
	// rather than an entry that later looks like a reading.
	tracker.MarkReset(99, at)
	if stat, ok := tracker.Stats()[99]; ok {
		t.Fatalf("resetting an untracked provider invented an entry: %+v", stat)
	}
}

// Removing a cap (or deleting the provider) must drop the reading: the map is replaced
// wholesale, so nothing lingers to be applied to a later provider that reuses the id.
func TestCostTrackerDropsProvidersThatLostTheirCap(t *testing.T) {
	provider := cappedProvider(6, 100)
	tracker, src := costTrackerFixture(t, provider)
	ctx := context.Background()
	src.answers[6] = 100
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if status := tracker.Status(); status.Tracked != 1 {
		t.Fatalf("tracked = %d, want 1", status.Tracked)
	}

	provider.CostLimitMicros = 0
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if status := tracker.Status(); status.Tracked != 0 {
		t.Fatalf("tracked after removing the cap = %d, want 0", status.Tracked)
	}
	if src.callCount() != 1 {
		t.Fatalf("queries = %d, want 1 (the second refresh has nothing to read)", src.callCount())
	}
}

// Start seeds synchronously, so a provider that was already over its cap at startup is enforced
// from the first request rather than from the first tick.
func TestCostTrackerStartSeedsBeforeTheFirstTick(t *testing.T) {
	provider := cappedProvider(8, 100)
	tracker, src := costTrackerFixture(t, provider)
	tracker.interval = time.Hour // the ticker must not be what makes this test pass
	src.answers[8] = 250

	stop := tracker.Start(context.Background())
	defer stop()
	if !tracker.Exceeded(provider, time.Now().UTC()) {
		t.Fatal("Start must seed the reading before returning")
	}
}

// Concurrent refreshes collapse into one scan: the ticker and an explicit refresh must not
// read the metering table in parallel.
func TestCostTrackerRefreshCollapsesConcurrentCalls(t *testing.T) {
	provider := cappedProvider(2, 100)
	tracker, _ := costTrackerFixture(t, provider)
	blocking := &blockingSource{release: make(chan struct{})}
	tracker.src = blocking

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tracker.Refresh(context.Background())
		}()
	}
	// Let the first goroutine take the lock, then release it once the rest have tried.
	time.Sleep(20 * time.Millisecond)
	close(blocking.release)
	wg.Wait()
	if blocking.entered() != 1 {
		t.Fatalf("the source was entered %d times, want 1", blocking.entered())
	}
}

type blockingSource struct {
	release chan struct{}
	mu      sync.Mutex
	count   int
}

func (b *blockingSource) ProviderCostsSince(_ context.Context, _ map[int64]time.Time) (map[int64]int64, error) {
	b.mu.Lock()
	b.count++
	b.mu.Unlock()
	<-b.release
	return map[int64]int64{}, nil
}

func (b *blockingSource) entered() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// The crossing is announced once, not once per interval: an operator who reads the log must see
// when the budget ran out, and a line every five seconds forever would bury everything else.
func TestCostTrackerAnnouncesTheCrossingOnce(t *testing.T) {
	provider := cappedProvider(9, 100)
	var buf strings.Builder
	reg := registry.NewStatic(registry.NewSnapshot(nil, []*domain.Provider{provider}, nil, nil, nil, nil, nil))
	src := &costSourceStub{answers: map[int64]int64{9: 150}}
	tracker := NewCostTracker(src, reg, slog.New(slog.NewTextHandler(&buf, nil)))

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := tracker.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "reached its cost cap"); got != 1 {
		t.Fatalf("the crossing was logged %d times, want 1:\n%s", got, buf.String())
	}

	// Coming back under the cap is announced too — and then crossing again is a new event,
	// otherwise a provider that flaps would go silent after its first recovery.
	src.answers[9] = 0
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "back under its cost cap") {
		t.Fatalf("the recovery was not logged:\n%s", buf.String())
	}
	src.answers[9] = 200
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "reached its cost cap"); got != 2 {
		t.Fatalf("the second crossing was logged %d times, want 2:\n%s", got, buf.String())
	}
}

// A reset is a new accounting window, so a later crossing is a new event too.
func TestCostTrackerAnnouncesAfterAReset(t *testing.T) {
	provider := cappedProvider(10, 100)
	var buf strings.Builder
	reg := registry.NewStatic(registry.NewSnapshot(nil, []*domain.Provider{provider}, nil, nil, nil, nil, nil))
	src := &costSourceStub{answers: map[int64]int64{10: 500}}
	tracker := NewCostTracker(src, reg, slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()

	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	tracker.MarkReset(10, time.Now().UTC())
	if err := tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "reached its cost cap"); got != 2 {
		t.Fatalf("crossings logged %d times, want 2 (before and after the reset):\n%s", got, buf.String())
	}
}
