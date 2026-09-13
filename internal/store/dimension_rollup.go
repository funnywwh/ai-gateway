package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const dimensionBatchSize = 500

// DimensionRollupStatus exposes durable backfill state and this process's worker health.
type DimensionRollupStatus struct {
	Enabled          bool      `json:"enabled"`
	BackfillCursor   int64     `json:"backfill_cursor"`
	BackfillComplete bool      `json:"backfill_complete"`
	PendingHours     int64     `json:"pending_hours"`
	LastSuccess      time.Time `json:"last_success"`
	LastError        string    `json:"last_error"`
}

func (db *DB) DimensionRollupStats(ctx context.Context) (DimensionRollupStatus, error) {
	db.dimensionStatusMu.Lock()
	out := DimensionRollupStatus{Enabled: !db.dimensionRollupsDisabled.Load() && db.dimensionWAL, LastSuccess: db.dimensionLastSuccess, LastError: db.dimensionLastError}
	db.dimensionStatusMu.Unlock()
	if !db.dimensionWAL {
		out.LastError = "hourly rollups require database.wal; using exact raw statistics"
	}
	err := db.read.QueryRowContext(ctx, `SELECT cursor,complete,
 (SELECT COUNT(*) FROM request_dimension_hours WHERE hour<? AND (published_version IS NULL OR published_version!=version))
 FROM request_dimension_progress WHERE id=1`, hourFloor(time.Now().Unix())).Scan(&out.BackfillCursor, &out.BackfillComplete, &out.PendingHours)
	return out, err
}

// StartDimensionRollups returns a stop-and-wait function. Only one refresher can run on
// a DB; cancellation also cancels snapshot reads and staging writes before publication.
func (db *DB) StartDimensionRollups(parent context.Context, log *slog.Logger) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if !db.dimensionRollupsDisabled.Load() {
				if err := db.RefreshDimensionRollups(ctx); err != nil && ctx.Err() == nil && log != nil {
					log.Warn("dimension rollup refresh failed", "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(cancel); <-done }
}

// RefreshDimensionRollups makes bounded progress. A failed/incomplete generation is
// invisible. Source tables remain authoritative throughout discovery and rebuilding.
func (db *DB) RefreshDimensionRollups(ctx context.Context) (err error) {
	if !db.dimensionRefreshMu.TryLock() {
		return nil
	}
	defer db.dimensionRefreshMu.Unlock()
	if db.dimensionRollupsDisabled.Load() || !db.dimensionWAL {
		return nil
	}
	defer func() {
		db.dimensionStatusMu.Lock()
		defer db.dimensionStatusMu.Unlock()
		if err != nil {
			db.dimensionLastError = err.Error()
		} else {
			db.dimensionLastError = ""
			db.dimensionLastSuccess = time.Now().UTC()
		}
	}()
	// Clear leftovers before creating a new generation, so cleanup cannot eat our staging.
	if err = db.cleanDimensionGenerations(ctx); err != nil {
		return err
	}
	for i := 0; i < 200; i++ {
		var done bool
		done, err = db.discoverDimensionHours(ctx)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	rows, e := db.read.QueryContext(ctx, `SELECT hour FROM request_dimension_hours
 WHERE hour<? AND (published_version IS NULL OR published_version!=version) ORDER BY hour DESC LIMIT 200`, hourFloor(time.Now().Unix()))
	if e != nil {
		return e
	}
	hours := []int64{}
	for rows.Next() {
		var h int64
		if e = rows.Scan(&h); e != nil {
			break
		}
		hours = append(hours, h)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	for _, h := range hours {
		if err = db.rebuildDimensionHour(ctx, h); err != nil {
			return err
		}
	}
	return db.cleanDimensionGenerations(ctx)
}

// Discovery's cursor and hour rows commit together. Concurrent new inserts are already
// tracked by triggers, and ON CONFLICT DO NOTHING cannot reset a dirty version.
func (db *DB) discoverDimensionHours(ctx context.Context) (bool, error) {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var cursor int64
	var complete bool
	if err = tx.QueryRowContext(ctx, `SELECT cursor,complete FROM request_dimension_progress WHERE id=1`).Scan(&cursor, &complete); err != nil {
		return false, err
	}
	if complete {
		return true, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,created_at FROM request_logs WHERE id>? ORDER BY id LIMIT ?`, cursor, dimensionBatchSize)
	if err != nil {
		return false, err
	}
	hours := map[int64]bool{}
	n := 0
	for rows.Next() {
		var id, at int64
		if err = rows.Scan(&id, &at); err != nil {
			break
		}
		cursor = id
		hours[hourFloor(at)] = true
		n++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return false, err
	}
	for h := range hours {
		if _, err = tx.ExecContext(ctx, `INSERT INTO request_dimension_hours(hour) VALUES(?) ON CONFLICT(hour) DO NOTHING`, h); err != nil {
			return false, err
		}
	}
	complete = n < dimensionBatchSize
	if _, err = tx.ExecContext(ctx, `UPDATE request_dimension_progress SET cursor=?,complete=? WHERE id=1`, cursor, complete); err != nil {
		return false, err
	}
	return complete, tx.Commit()
}

// The source version and all contributions are read from one WAL snapshot. Staging is
// streamed in bounded batches through the writer, without holding its transaction while
// reading or aggregating the source hour.
func (db *DB) rebuildDimensionHour(ctx context.Context, h int64) error {
	tx, err := db.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT version FROM request_dimension_hours WHERE hour=?`, h).Scan(&version); err != nil {
		return err
	}
	res, err := db.write.ExecContext(ctx, `INSERT INTO request_dimension_generations(hour,source_version) VALUES(?,?)`, h, version)
	if err != nil {
		return err
	}
	generation, err := res.LastInsertId()
	if err != nil {
		return err
	}
	source := rawDimensionSource(" WHERE 1=1", false)
	query := `WITH contributions AS (` + source + `) SELECT ` + dimensionKeys + `,MAX(title),SUM(requests),SUM(metered),MIN(first_seen),MAX(last_seen),
 SUM(input_tokens),SUM(cached_tokens),SUM(output_tokens),SUM(reasoning_tokens),SUM(cost_micros),SUM(charge_micros)
 FROM contributions GROUP BY ` + dimensionKeys
	ranges := fmt.Sprintf("[[%d,%d]]", h, h+3599)
	rows, err := tx.QueryContext(ctx, query, ranges)
	if err != nil {
		return err
	}
	defer rows.Close()
	batch := make([][]any, 0, dimensionBatchSize)
	for rows.Next() {
		// Strings and numbers only: do not retain driver-owned byte slices between scans.
		var account, key, requests, metered, first, last, input, cached, output, reasoning, cost, charge int64
		var client, model, resolved, workspace, session, kind, title string
		err = rows.Scan(&account, &key, &client, &model, &resolved, &workspace, &session, &kind, &title, &requests, &metered, &first, &last, &input, &cached, &output, &reasoning, &cost, &charge)
		if err != nil {
			return err
		}
		batch = append(batch, []any{generation, h, account, key, client, model, resolved, workspace, session, kind, title, requests, metered, first, last, input, cached, output, reasoning, cost, charge})
		if len(batch) == dimensionBatchSize {
			if err = db.stageDimensionRows(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(batch) > 0 {
		if err = db.stageDimensionRows(ctx, batch); err != nil {
			return err
		}
	}
	_, err = db.publishDimensionGeneration(ctx, h, version, generation)
	return err
}

func (db *DB) stageDimensionRows(ctx context.Context, batch [][]any) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO request_dimension_rollups(generation,hour,`+dimensionColumns+`) VALUES(`+idPlaceholders(21)+`)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, args := range batch {
		if _, err = stmt.ExecContext(ctx, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) publishDimensionGeneration(ctx context.Context, h, version, generation int64) (bool, error) {
	// This compare-and-swap is a single write statement: a concurrent invalidation can
	// happen before it (publication rejected) or after it (the new generation is dirty).
	res, err := db.write.ExecContext(ctx, `UPDATE request_dimension_hours SET published_generation=?,published_version=? WHERE hour=? AND version=?`, generation, version, h, version)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (db *DB) cleanDimensionGenerations(ctx context.Context) error {
	for i := 0; i < 200; i++ {
		res, err := db.write.ExecContext(ctx, cleanDimensionRowsSQL)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n < dimensionBatchSize {
			break
		}
	}
	_, err := db.write.ExecContext(ctx, `DELETE FROM request_dimension_generations WHERE id IN (
 SELECT g.id FROM request_dimension_generations g
 LEFT JOIN request_dimension_hours h ON h.hour=g.hour AND h.published_generation=g.id
 WHERE h.hour IS NULL AND NOT EXISTS(SELECT 1 FROM request_dimension_rollups r WHERE r.generation=g.id) LIMIT 500)`)
	return err
}

// Drive cleanup from the small generation table, seeking only obsolete generations.
// An ordinary JOIN lets SQLite scan every live contribution on an idle refresh.
const cleanDimensionRowsSQL = `DELETE FROM request_dimension_rollups WHERE id IN (
 SELECT r.id FROM request_dimension_generations g
 LEFT JOIN request_dimension_hours h ON h.hour=g.hour AND h.published_generation=g.id
 CROSS JOIN request_dimension_rollups r INDEXED BY idx_dimension_rollups_generation
 WHERE h.hour IS NULL AND r.generation=g.id LIMIT 500)`
