package usage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

type fakeRecorder struct {
	records []*domain.UsageRecord
}

func (f *fakeRecorder) InsertUsage(ctx context.Context, rec *domain.UsageRecord) (int64, error) {
	rec.ID = int64(len(f.records) + 1)
	f.records = append(f.records, rec)
	return rec.ID, nil
}

func TestRecordWritesEveryField(t *testing.T) {
	store := &fakeRecorder{}
	m := New(store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return now })

	rec, err := m.Record(context.Background(), &Attempt{
		RequestID: "req_1", AttemptNo: 2, AccountID: 7, APIKeyID: 3,
		Model: "gpt-x", ResolvedModel: "gpt-x", ProviderID: 10,
		Dimensions:       map[string]int64{"input_cache_hit": 80, "input_cache_miss": 20, "output": 5},
		Estimated:        false,
		OvershootCost:    1234,
		LatencyMS:        900,
		TTFTMS:           120,
		Status:           "completed",
		DegradedFeatures: []string{"tools", "vision"},
		TerminatedReason: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.UsageSource != "provider" {
		t.Fatalf("source = %q, want provider", rec.UsageSource)
	}
	if rec.CreatedAt != now {
		t.Fatalf("timestamp not stamped: %v", rec.CreatedAt)
	}
	var dims map[string]int64
	if err := json.Unmarshal([]byte(rec.DimensionsJSON), &dims); err != nil {
		t.Fatal(err)
	}
	if dims["input_cache_hit"] != 80 || dims["output"] != 5 {
		t.Fatalf("dimensions mismatch: %+v", dims)
	}
	var degraded []string
	if err := json.Unmarshal([]byte(rec.DegradedFeatures), &degraded); err != nil {
		t.Fatal(err)
	}
	if len(degraded) != 2 || degraded[0] != "tools" {
		t.Fatalf("degraded features mismatch: %+v", degraded)
	}
	if rec.OvershootCost != 1234 || rec.AttemptNo != 2 {
		t.Fatalf("record mismatch: %+v", rec)
	}
}

func TestRecordDefaultsAndValidation(t *testing.T) {
	store := &fakeRecorder{}
	m := New(store)

	if _, err := m.Record(context.Background(), nil); err == nil {
		t.Fatal("nil attempt must be rejected")
	}
	if _, err := m.Record(context.Background(), &Attempt{}); err == nil {
		t.Fatal("attempt without request_id must be rejected")
	}

	rec, err := m.Record(context.Background(), &Attempt{RequestID: "req_2", Status: "failed", ErrorCode: "upstream_5xx"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.AttemptNo != 1 {
		t.Fatalf("attempt number must default to 1, got %d", rec.AttemptNo)
	}
	if rec.UsageSource != "unavailable" {
		t.Fatalf("source = %q, want unavailable", rec.UsageSource)
	}
	if rec.DimensionsJSON != "{}" {
		t.Fatalf("empty dimensions must serialise to {}, got %q", rec.DimensionsJSON)
	}
	if rec.Status != "failed" || rec.ErrorCode != "upstream_5xx" {
		t.Fatalf("failed attempt must be recorded: %+v", rec)
	}
}

func TestSourceOf(t *testing.T) {
	cases := []struct {
		estimated bool
		dims      map[string]int64
		want      string
	}{
		{false, map[string]int64{"output": 1}, "provider"},
		{true, map[string]int64{"output": 1}, "estimated"},
		{false, nil, "unavailable"},
		{true, map[string]int64{}, "unavailable"},
	}
	for _, tc := range cases {
		if got := SourceOf(tc.estimated, tc.dims); got != tc.want {
			t.Errorf("SourceOf(%v,%v) = %q, want %q", tc.estimated, tc.dims, got, tc.want)
		}
	}
}

func TestAccumulatorDeltasThenFinal(t *testing.T) {
	acc := NewAccumulator()
	acc.Add(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: "ignored"})
	acc.Add(pluginapi.Event{Type: pluginapi.EventUsageDelta, Usage: &pluginapi.Usage{Dimensions: map[string]int64{"output": 3}}, Estimated: true})
	acc.Add(pluginapi.Event{Type: pluginapi.EventUsageDelta, Usage: &pluginapi.Usage{Dimensions: map[string]int64{"output": 4}}, Estimated: true})

	if got := acc.Dimensions()["output"]; got != 7 {
		t.Fatalf("deltas must accumulate, got %d", got)
	}
	if !acc.Estimated() {
		t.Fatal("accumulated deltas must be flagged as estimated")
	}
	if acc.Final() {
		t.Fatal("no final usage event yet")
	}
	if SourceOf(acc.Estimated(), acc.Dimensions()) != "estimated" {
		t.Fatalf("deltas must be classified as estimated")
	}
	if acc.Deltas() != 2 {
		t.Fatalf("delta count = %d", acc.Deltas())
	}

	// A final usage event replaces the accumulated deltas.
	acc.Add(pluginapi.Event{Type: pluginapi.EventUsage, Usage: &pluginapi.Usage{Dimensions: map[string]int64{"output": 6, "input": 10}}})
	if !acc.Final() || acc.Estimated() {
		t.Fatalf("final usage must be authoritative: final=%v estimated=%v", acc.Final(), acc.Estimated())
	}
	if acc.Dimensions()["output"] != 6 || acc.Dimensions()["input"] != 10 {
		t.Fatalf("final dimensions mismatch: %+v", acc.Dimensions())
	}
	if got := SourceOf(acc.Estimated(), acc.Dimensions()); got != "provider" {
		t.Fatalf("source = %q, want provider", got)
	}
}

func TestAccumulatorEmpty(t *testing.T) {
	acc := NewAccumulator()
	if acc.Estimated() || acc.Final() {
		t.Fatal("empty accumulator must be neither estimated nor final")
	}
	if len(acc.Dimensions()) != 0 {
		t.Fatalf("empty dimensions expected: %+v", acc.Dimensions())
	}
	if got := SourceOf(acc.Estimated(), acc.Dimensions()); got != "unavailable" {
		t.Fatalf("source = %q, want unavailable", got)
	}
}

func TestTotalTokensAndDescribe(t *testing.T) {
	dims := map[string]int64{"input": 10, "output": 5, "input_cache_hit": 20}
	if got := TotalTokens(dims); got != 35 {
		t.Fatalf("total = %d, want 35", got)
	}
	want := "input=10,input_cache_hit=20,output=5"
	if got := Describe(dims); got != want {
		t.Fatalf("describe = %q, want %q", got, want)
	}
	if Describe(nil) != "none" {
		t.Fatal("empty dimensions must describe as none")
	}
}
