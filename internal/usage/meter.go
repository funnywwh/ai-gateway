// Package usage records metered provider attempts and accumulates streaming usage.
package usage

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Recorder is the persistence subset the meter needs.
type Recorder interface {
	InsertUsage(ctx context.Context, rec *domain.UsageRecord) (int64, error)
}

// Meter writes one row per upstream attempt.
type Meter struct {
	store Recorder
	now   func() time.Time
}

// New builds a meter.
func New(store Recorder) *Meter {
	return &Meter{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides the clock (tests).
func (m *Meter) SetClock(now func() time.Time) { m.now = now }

// Attempt is one metered upstream call (successful or not).
type Attempt struct {
	RequestID     string
	AttemptNo     int
	AccountID     int64
	APIKeyID      int64
	Model         string
	ResolvedModel string
	ProviderID    int64
	// RouteID is the route this attempt went through; UpstreamModel is the model name the
	// provider adapter was handed. Both are recorded per attempt, because a request can fail
	// over and a later attempt may run on another route with another upstream model.
	RouteID          int64
	UpstreamModel    string
	Dimensions       map[string]int64
	Estimated        bool
	OvershootCost    int64
	LatencyMS        int
	TTFTMS           int
	Status           string
	ErrorCode        string
	DegradedFeatures []string
	TerminatedReason string
}

// Record persists the attempt. Local rejections (which never reach an upstream)
// must not call this: they are recorded in request_logs instead.
func (m *Meter) Record(ctx context.Context, a *Attempt) (*domain.UsageRecord, error) {
	rec, err := m.Build(a)
	if err != nil {
		return nil, err
	}
	if _, err := m.store.InsertUsage(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Build turns an attempt into a usage row without persisting it. The billing path
// uses it so the usage row can be written by the same transaction that moves the
// money (see store.SettleBatch), instead of a second, unrelated insert.
func (m *Meter) Build(a *Attempt) (*domain.UsageRecord, error) {
	if a == nil || a.RequestID == "" {
		return nil, domain.ErrInvalidRequest("usage attempt requires request_id")
	}
	dims := a.Dimensions
	if dims == nil {
		dims = map[string]int64{}
	}
	dimsJSON, err := json.Marshal(dims)
	if err != nil {
		return nil, err
	}
	degradedJSON := ""
	if len(a.DegradedFeatures) > 0 {
		sorted := append([]string(nil), a.DegradedFeatures...)
		sort.Strings(sorted)
		raw, err := json.Marshal(sorted)
		if err != nil {
			return nil, err
		}
		degradedJSON = string(raw)
	}

	rec := &domain.UsageRecord{
		RequestID:        a.RequestID,
		AttemptNo:        maxInt(a.AttemptNo, 1),
		AccountID:        a.AccountID,
		APIKeyID:         a.APIKeyID,
		Model:            a.Model,
		ResolvedModel:    a.ResolvedModel,
		ProviderID:       a.ProviderID,
		RouteID:          a.RouteID,
		UpstreamModel:    a.UpstreamModel,
		DimensionsJSON:   string(dimsJSON),
		OvershootCost:    a.OvershootCost,
		LatencyMS:        a.LatencyMS,
		TTFTMS:           a.TTFTMS,
		Status:           a.Status,
		ErrorCode:        a.ErrorCode,
		DegradedFeatures: degradedJSON,
		UsageSource:      SourceOf(a.Estimated, dims),
		TerminatedReason: a.TerminatedReason,
		CreatedAt:        m.now(),
	}
	return rec, nil
}

// SourceOf classifies where the usage numbers came from.
func SourceOf(estimated bool, dims map[string]int64) string {
	if len(dims) == 0 {
		return "unavailable"
	}
	if estimated {
		return "estimated"
	}
	return "provider"
}

// Accumulator merges streaming usage events: usage.delta values accumulate, and a
// final usage event (when present) replaces them authoritatively.
type Accumulator struct {
	dims      map[string]int64
	estimated bool
	sawFinal  bool
	deltas    int
}

// NewAccumulator builds an empty accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{dims: map[string]int64{}}
}

// Add consumes one provider event (ignores non-usage events).
func (a *Accumulator) Add(ev pluginapi.Event) {
	switch ev.Type {
	case pluginapi.EventUsage:
		if ev.Usage != nil {
			a.dims = copyDims(ev.Usage.Dimensions)
			a.estimated = ev.Usage.Estimated
			a.sawFinal = true
		}
	case pluginapi.EventUsageDelta:
		if ev.Usage != nil {
			a.sawFinal = false
			for k, v := range ev.Usage.Dimensions {
				a.dims[k] += v
			}
			a.estimated = true
			a.deltas++
		}
	}
}

// Dimensions returns the merged usage dimensions.
func (a *Accumulator) Dimensions() map[string]int64 {
	if a == nil {
		return map[string]int64{}
	}
	return copyDims(a.dims)
}

// Estimated reports whether only incremental (estimated) usage was seen.
func (a *Accumulator) Estimated() bool {
	if a == nil {
		return false
	}
	if a.sawFinal {
		return a.estimated
	}
	return len(a.dims) > 0
}

// Final reports whether an authoritative usage event was received.
func (a *Accumulator) Final() bool { return a != nil && a.sawFinal }

// Deltas reports how many incremental usage events were seen.
func (a *Accumulator) Deltas() int {
	if a == nil {
		return 0
	}
	return a.deltas
}

// TotalTokens sums the token-like dimensions.
func TotalTokens(dims map[string]int64) int64 {
	var total int64
	for _, v := range dims {
		total += v
	}
	return total
}

// Describe renders dimensions as a stable string (logs, hooks).
func Describe(dims map[string]int64) string {
	if len(dims) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+itoa(dims[k]))
	}
	return strings.Join(parts, ",")
}

func copyDims(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
