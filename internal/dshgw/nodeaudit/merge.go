// Package nodeaudit pulls worker nodes' security events into the control plane's own audit stream
// (M77).
//
// Node-side events happen where the mount lives — a refused self-nesting ssh mount, a logout that
// had to break a FUSE mount, a browser mount that expired — and the operator reads one audit file
// in one place. The control plane therefore polls each node's tail with the cursor it last
// recorded, so a gateway restart resumes instead of re-importing history, and the events arrive
// tagged with the node they happened on.
package nodeaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
)

// DefaultInterval is how often each node is drained. Node events are operational signals, not a
// request path: a half-minute of latency is nothing next to opening a second audit file per node.
const DefaultInterval = 30 * time.Second

// Cursors remembers how far each node's audit has been read. The control plane persists it in the
// node store so a restart does not re-import (or skip) history.
type Cursors interface {
	AuditCursor(node string) string
	SetAuditCursor(node string, cursor string) error
}

// Merger drains node audit tails into one sink.
type Merger struct {
	Clients *nodeclient.Set
	Cursors Cursors
	Sink    audit.Sink
	Logger  *slog.Logger
	// Interval overrides DefaultInterval.
	Interval time.Duration
}

// New builds a merger over the control plane's node store.
func New(clients *nodeclient.Set, store *nodestore.Store, sink audit.Sink, logger *slog.Logger) *Merger {
	return &Merger{Clients: clients, Cursors: store, Sink: sink, Logger: logger}
}

func (m *Merger) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// Run drains every node until ctx is done. The first pass runs immediately: a gateway that has
// just started is exactly when an operator wants the recent node events.
func (m *Merger) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if err := m.DrainAll(ctx); err != nil {
		m.log().Warn("draining node audit tails failed", "err", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.DrainAll(ctx); err != nil {
				m.log().Warn("draining node audit tails failed", "err", err)
			}
		}
	}
}

// DrainAll drains every configured node, reporting the failures instead of stopping at the first:
// one unreachable node must not keep the others' events out of the stream.
func (m *Merger) DrainAll(ctx context.Context) error {
	if m.Clients == nil {
		return nil
	}
	var failures []error
	m.Clients.Each(func(client *nodeclient.Client) {
		if _, err := m.Drain(ctx, client); err != nil {
			failures = append(failures, err)
		}
	})
	return errors.Join(failures...)
}

// Drain pulls one node's new events and returns how many were written.
func (m *Merger) Drain(ctx context.Context, client *nodeclient.Client) (int, error) {
	if m.Cursors == nil || m.Sink == nil {
		return 0, nil
	}
	cursor := m.Cursors.AuditCursor(client.Name)
	result, err := client.AuditTail(ctx, cursor, 0)
	if err != nil {
		return 0, fmt.Errorf("node %s: audit tail: %w", client.Name, err)
	}
	written := 0
	for _, line := range result.Lines {
		event, err := decodeEvent(line)
		if err != nil {
			// A line this build cannot read is reported as its own event rather than dropped: the
			// operator's question ("did anything happen on that node?") still gets an answer.
			event = audit.Event{
				Time:   time.Now().UTC(),
				Kind:   "node_audit_unreadable",
				Node:   client.Name,
				Reason: truncate(line, 200),
				Status: 0,
			}
		}
		event.Node = client.Name
		if err := m.Sink.Write(event); err != nil {
			return written, fmt.Errorf("write a node event into this gateway's audit: %w", err)
		}
		written++
	}
	if err := m.Cursors.SetAuditCursor(client.Name, result.Cursor); err != nil {
		// The events are in the stream; failing to remember the cursor means the next drain repeats
		// them, which is noisy but never loses one. Report it and keep going.
		return written, fmt.Errorf("remember node %s audit cursor: %w", client.Name, err)
	}
	if result.Dropped > 0 {
		m.log().Warn("node audit history was rotated; events were skipped", "node", client.Name, "dropped", result.Dropped)
	}
	if written > 0 {
		m.log().Info("node audit events merged", "node", client.Name, "events", written)
	}
	return written, nil
}

// decodeEvent reads one JSONL record. The node writes the same shape this gateway does, so the
// decode is deliberately strict: a line that is not an event should be visible as such.
func decodeEvent(line string) (audit.Event, error) {
	var event audit.Event
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return event, err
	}
	if event.Kind == "" {
		return event, errors.New("event has no kind")
	}
	return event, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
