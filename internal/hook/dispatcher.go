// Package hook delivers gateway lifecycle events to external sinks (webhook, JSONL).
//
// Delivery is best-effort and asynchronous: Emit never blocks the request path, the
// queue is bounded, and overflow is counted rather than absorbed. Hooks are observability,
// not part of the billing durability path.
package hook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
)

// Config tunes the dispatcher.
type Config struct {
	QueueSize  int
	Workers    int
	Timeout    time.Duration
	Retries    int
	DeadLetter string
}

// DefaultConfig mirrors the YAML defaults.
func DefaultConfig() Config {
	return Config{QueueSize: 1024, Workers: 8, Timeout: 5 * time.Second, Retries: 5, DeadLetter: "./data/hooks-dead.jsonl"}
}

// Stats are the dispatcher counters (exposed on /metrics and the admin API).
type Stats struct {
	Queued    int64 `json:"queued"`
	Delivered int64 `json:"delivered"`
	Failed    int64 `json:"failed"`
	Dropped   int64 `json:"dropped"`
	Skipped   int64 `json:"skipped"`
}

// Dispatcher fans events out to the configured hooks.
type Dispatcher struct {
	cfg  Config
	log  *slog.Logger
	http *http.Client

	hooks atomic.Pointer[[]*domain.Hook]
	queue chan job
	wg    sync.WaitGroup

	queued    atomic.Int64
	delivered atomic.Int64
	failed    atomic.Int64
	dropped   atomic.Int64
	skipped   atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}

	rndMu sync.Mutex
	rnd   *rand.Rand
}

type job struct {
	hook    *domain.Hook
	event   string
	body    []byte
	deliver string
}

// New builds a dispatcher and starts its workers.
func New(cfg Config, hooks []*domain.Hook, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	defaults := DefaultConfig()
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaults.QueueSize
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaults.Workers
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaults.Timeout
	}
	if cfg.Retries < 0 {
		cfg.Retries = defaults.Retries
	}
	if cfg.DeadLetter == "" {
		cfg.DeadLetter = defaults.DeadLetter
	}

	d := &Dispatcher{
		cfg:    cfg,
		log:    log,
		http:   &http.Client{Timeout: cfg.Timeout},
		queue:  make(chan job, cfg.QueueSize),
		closed: make(chan struct{}),
		rnd:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	d.SetHooks(hooks)
	for i := 0; i < cfg.Workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// SetHooks atomically replaces the hook configuration (hot reload).
func (d *Dispatcher) SetHooks(hooks []*domain.Hook) {
	copied := make([]*domain.Hook, 0, len(hooks))
	for _, h := range hooks {
		if h == nil || !h.Enabled {
			continue
		}
		copied = append(copied, h)
	}
	d.hooks.Store(&copied)
}

// Hooks returns the current configuration (diagnostics).
func (d *Dispatcher) Hooks() []*domain.Hook {
	if ptr := d.hooks.Load(); ptr != nil {
		return *ptr
	}
	return nil
}

// Stats returns the delivery counters.
func (d *Dispatcher) Stats() Stats {
	return Stats{
		Queued:    d.queued.Load(),
		Delivered: d.delivered.Load(),
		Failed:    d.failed.Load(),
		Dropped:   d.dropped.Load(),
		Skipped:   d.skipped.Load(),
	}
}

// Emit queues an event for every matching hook. It never blocks: when the queue is
// full the event is dropped and counted.
func (d *Dispatcher) Emit(ctx context.Context, ev *domain.Event) {
	if ev == nil || ev.Name == "" {
		return
	}
	hooks := d.Hooks()
	if len(hooks) == 0 {
		return
	}
	for _, h := range hooks {
		if !h.MatchesEvent(ev.Name) {
			d.skipped.Add(1)
			continue
		}
		if h.SampleRate > 0 && h.SampleRate < 1 && !d.sample(h.SampleRate) {
			d.skipped.Add(1)
			continue
		}
		body, err := d.envelope(ev, h)
		if err != nil {
			d.log.Warn("encoding hook payload failed", "hook", h.Name, "err", err)
			continue
		}
		deliveryID := ids.New("dlv")
		select {
		case d.queue <- job{hook: h, event: ev.Name, body: body, deliver: deliveryID}:
			d.queued.Add(1)
		default:
			d.dropped.Add(1)
			d.log.Warn("hook queue full: event dropped", "hook", h.Name, "event", ev.Name)
		}
	}
}

func (d *Dispatcher) sample(rate float64) bool {
	d.rndMu.Lock()
	defer d.rndMu.Unlock()
	return d.rnd.Float64() < rate
}

// envelope renders the delivery payload; content is only included when the hook opts in.
func (d *Dispatcher) envelope(ev *domain.Event, h *domain.Hook) ([]byte, error) {
	payload := map[string]any{
		"event": ev.Name,
		"ts":    ev.Timestamp.UTC().Format(time.RFC3339),
	}
	for key, value := range ev.Payload {
		if key == "input" || key == "output" || key == "reasoning" {
			if !h.IncludeContent {
				continue
			}
			if h.MaxBytes > 0 {
				if text, ok := value.(string); ok && len(text) > h.MaxBytes {
					value = text[:h.MaxBytes]
					payload["truncated"] = true
				}
			}
		}
		payload[key] = value
	}
	return json.Marshal(payload)
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.closed:
			return
		case j, ok := <-d.queue:
			if !ok {
				return
			}
			d.deliver(j)
		}
	}
}

func (d *Dispatcher) deliver(j job) {
	switch j.hook.Type {
	case "jsonl":
		if err := writeJSONL(j.hook.URL, j.body); err != nil {
			d.failed.Add(1)
			d.deadLetter(j, err)
			return
		}
		d.delivered.Add(1)
	default:
		var lastErr error
		for attempt := 0; attempt <= d.cfg.Retries; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(backoff(attempt)):
				case <-d.closed:
					return
				}
			}
			status, err := d.post(j)
			if err == nil && status >= 200 && status < 300 {
				d.delivered.Add(1)
				return
			}
			// 4xx means the target rejected the payload: retrying cannot help.
			if err == nil && status >= 400 && status < 500 {
				d.failed.Add(1)
				d.deadLetter(j, fmt.Errorf("target returned %d", status))
				return
			}
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("target returned %d", status)
			}
		}
		d.failed.Add(1)
		d.deadLetter(j, lastErr)
	}
}

func (d *Dispatcher) post(j job) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.hook.URL, bytes.NewReader(j.body))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AIGW-Event", j.event)
	req.Header.Set("X-AIGW-Delivery", j.deliver)
	req.Header.Set("X-AIGW-Signature", Signature(j.hook.Secret, ts, j.body))

	resp, err := d.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Signature renders the HMAC header value: t=<unix>,v1=<hex hmac of "t.body">.
func Signature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a signature header against a body (used by receivers and tests).
func VerifySignature(secret, header string, body []byte) bool {
	timestamp, signature, ok := parseSignature(header)
	if !ok {
		return false
	}
	expected := Signature(secret, timestamp, body)
	_, want, ok := parseSignature(expected)
	if !ok {
		return false
	}
	return hmac.Equal([]byte(signature), []byte(want))
}

func parseSignature(header string) (timestamp, signature string, ok bool) {
	for _, part := range splitComma(header) {
		if len(part) > 2 && part[:2] == "t=" {
			timestamp = part[2:]
		}
		if len(part) > 3 && part[:3] == "v1=" {
			signature = part[3:]
		}
	}
	return timestamp, signature, timestamp != "" && signature != ""
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func backoff(attempt int) time.Duration {
	delay := time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	return delay
}

// deadLetter appends a failed delivery to the dead-letter file for later replay.
func (d *Dispatcher) deadLetter(j job, cause error) {
	record := map[string]any{
		"event":       j.event,
		"delivery_id": j.deliver,
		"hook":        j.hook.Name,
		"target":      j.hook.URL,
		"error":       fmt.Sprint(cause),
		"ts":          time.Now().UTC().Format(time.RFC3339),
		"payload":     json.RawMessage(j.body),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	if err := writeJSONL(d.cfg.DeadLetter, raw); err != nil {
		d.log.Error("writing hook dead letter failed", "err", err)
		return
	}
	d.log.Warn("hook delivery failed", "hook", j.hook.Name, "event", j.event, "err", cause)
}

func writeJSONL(path string, line []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, byte(0x0A))); err != nil {
		return err
	}
	return nil
}

// Close drains in-flight deliveries (bounded) and stops the workers.
func (d *Dispatcher) Close(timeout time.Duration) {
	d.closeOnce.Do(func() {
		close(d.closed)
		done := make(chan struct{})
		go func() {
			d.wg.Wait()
			close(done)
		}()
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		select {
		case <-done:
		case <-time.After(timeout):
		}
	})
}
