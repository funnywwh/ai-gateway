package billing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// FallbackRecord is one line of the fallback file: the settlement plus why it could
// not be written, so an operator can see the story without the logs.
type FallbackRecord struct {
	Settlement *Settlement `json:"settlement"`
	Error      string      `json:"error"`
	WrittenAt  time.Time   `json:"written_at"`
	ReplayedAt *time.Time  `json:"replayed_at,omitempty"`
}

// AppendFallback writes one settlement to the fallback file and fsyncs it, so a
// crash immediately after a database failure still leaves the money accounted for.
func AppendFallback(path string, settlement *Settlement, cause error) error {
	if path == "" || settlement == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return fmt.Errorf("billing: create fallback dir: %w", err)
	}
	record := FallbackRecord{Settlement: settlement, WrittenAt: time.Now().UTC()}
	if cause != nil {
		record.Error = cause.Error()
	}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("billing: marshal fallback record: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("billing: open fallback file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, byte(0x0A))); err != nil {
		return fmt.Errorf("billing: write fallback record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("billing: fsync fallback record: %w", err)
	}
	return nil
}

// ReplayResult summarises one replay pass.
type ReplayResult struct {
	Scanned  int `json:"scanned"`
	Replayed int `json:"replayed"`
	Failed   int `json:"failed"`
}

// ReplayFile re-applies every settlement in the fallback file. Records that fail again
// are kept (rewritten) so nothing is lost; a fully successful pass truncates the file.
func (w *Writer) ReplayFile(ctx context.Context) (ReplayResult, error) {
	result := ReplayResult{}
	path := w.cfg.FallbackFile
	if path == "" {
		return result, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, fmt.Errorf("billing: open fallback file: %w", err)
	}
	defer file.Close()

	kept := []FallbackRecord{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		result.Scanned++
		var record FallbackRecord
		if err := json.Unmarshal(line, &record); err != nil || record.Settlement == nil || record.Settlement.Usage == nil {
			result.Failed++
			kept = append(kept, FallbackRecord{Error: "unparsable fallback line", WrittenAt: time.Now().UTC()})
			continue
		}
		applied, err := w.applier.SettleAttempt(ctx, record.Settlement.Usage,
			record.Settlement.Entries, record.Settlement.Counters)
		if err != nil {
			result.Failed++
			record.Error = err.Error()
			kept = append(kept, record)
			continue
		}
		result.Replayed++
		if applied {
			w.settled.Add(1)
		} else {
			w.replayed.Add(1)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("billing: read fallback file: %w", err)
	}
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("billing: clear replayed fallback file: %w", err)
		}
		return result, nil
	}
	if err := rewriteFallback(path, kept); err != nil {
		return result, err
	}
	return result, nil
}

func rewriteFallback(path string, records []FallbackRecord) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("billing: write fallback temp file: %w", err)
	}
	writer := bufio.NewWriter(file)
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			continue
		}
		if _, err := writer.Write(append(line, byte(0x0A))); err != nil {
			_ = file.Close()
			return fmt.Errorf("billing: rewrite fallback record: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return fmt.Errorf("billing: flush fallback file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("billing: fsync fallback file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("billing: close fallback file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("billing: replace fallback file: %w", err)
	}
	return nil
}

// StartReplayer replays the fallback file at startup and then on an interval until
// the context is cancelled. Replays are idempotent, so running them often is safe.
func (w *Writer) StartReplayer(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			result, err := w.ReplayFile(ctx)
			if err != nil {
				log.Error("replaying the billing fallback file failed", "err", err)
			} else if result.Scanned > 0 {
				log.Warn("billing fallback replay finished",
					"scanned", result.Scanned, "replayed", result.Replayed, "failed", result.Failed)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
