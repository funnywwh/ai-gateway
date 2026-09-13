package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

// Opt-in measurements, using exactly the production queries and refresher. No production
// branch sees these environment variables. Seed data deliberately includes both strong
// locality and the worst case (a distinct session for every request).
// DIMENSION_PROBE=1 go test ./internal/store -run '^TestDimensionRollupProbe$' -v -count=1 -timeout 30m
func TestDimensionRollupProbe(t *testing.T) {
	if os.Getenv("DIMENSION_PROBE") == "" {
		t.Skip("set DIMENSION_PROBE=1 to measure")
	}
	sizes := []int{100000, 1000000}
	if value := os.Getenv("DIMENSION_PROBE_ROWS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		sizes = []int{n}
	}
	shapes := []string{"repeated", "unique", "skewed"}
	if value := os.Getenv("DIMENSION_PROBE_SHAPE"); value != "" {
		shapes = []string{value}
	}
	for _, n := range sizes {
		for _, shape := range shapes {
			t.Run(fmt.Sprintf("%d/%s", n, shape), func(t *testing.T) {
				ctx := context.Background()
				h := time.Now().UTC().Truncate(time.Hour).Add(-30 * 24 * time.Hour)
				f := domain.RequestLogFilter{From: h, To: h.Add(30*24*time.Hour - time.Second)}
				var db *DB
				var baseWrite, newWrite time.Duration
				var baseWAL, newWAL int64
				// Separate databases at schema 12 and current schema isolate trigger write overhead.
				for _, baseline := range []bool{true, false} {
					candidate := dimensionProbeDB(t, baseline)
					elapsed, wal := seedDimensionProbe(t, candidate, n, shape, h)
					if baseline {
						baseWrite, baseWAL = elapsed, wal
						candidate.Close()
					} else {
						db = candidate
						newWrite, newWAL = elapsed, wal
					}
				}
				defer db.Close()
				raw := dimensionOracle(t, db, f, "client", "charge", 20, 0)
				rawP50, rawP95 := dimensionProbeTimes(t, func() {
					got := dimensionOracle(t, db, f, "client", "charge", 20, 0)
					if !reflect.DeepEqual(got, raw) {
						t.Fatal("raw changed")
					}
				})
				started := time.Now()
				for {
					if err := db.RefreshDimensionRollups(ctx); err != nil {
						t.Fatal(err)
					}
					st, err := db.DimensionRollupStats(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if st.BackfillComplete && st.PendingHours == 0 {
						break
					}
				}
				backfill := time.Since(started)
				rolledP50, rolledP95 := dimensionProbeTimes(t, func() {
					got, err := db.RequestLogDimensionsPage(ctx, f, "client", "charge", 20, 0)
					if err != nil || !reflect.DeepEqual(got, raw) {
						t.Fatalf("rolled mismatch: %v", err)
					}
				})
				var count int64
				if err := db.read.QueryRow(`SELECT COUNT(*) FROM request_dimension_rollups`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				t.Logf("rows=%d shape=%s raw_p50=%s raw_p95=%s rollup_p50=%s rollup_p95=%s speedup=%.2fx summary_rows=%d compression=%.2fx backfill=%s write_before=%.0f_rps write_after=%.0f_rps WAL_before=%.1f_B/req WAL_after=%.1f_B/req", n, shape, rawP50, rawP95, rolledP50, rolledP95, float64(rawP95)/float64(rolledP95), count, float64(n)/float64(count), backfill, float64(n)/baseWrite.Seconds(), float64(n)/newWrite.Seconds(), float64(baseWAL)/float64(n), float64(newWAL)/float64(n))
				if n == 1000000 && shape == "repeated" && rawP95 < 3*rolledP95 {
					t.Fatalf("repeated million-row workload did not meet 3x target")
				}
			})
		}
	}
}

func dimensionProbeDB(t *testing.T, baseline bool) *DB {
	t.Helper()
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "probe.db")
	if !baseline {
		db, err := Open(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	pool, err := sql.Open("sqlite", buildDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	db := &DB{write: pool, read: pool, path: cfg.Path, stmts: newWriterStmtCache()}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version < 13 {
			if _, err = pool.Exec(m.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func seedDimensionProbe(t *testing.T, db *DB, n int, shape string, start time.Time) (time.Duration, int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.write.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for offset := 0; offset < n; offset += 500 {
		tx, err := db.write.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		logStmt, err := tx.PrepareContext(ctx, putRequestLogSQL)
		if err != nil {
			t.Fatal(err)
		}
		usageStmt, err := tx.PrepareContext(ctx, `INSERT INTO usage_records(request_id,attempt_no,dimensions_json,cost_micros,charge_micros,created_at) VALUES(?,1,?,3,5,?)`)
		if err != nil {
			t.Fatal(err)
		}
		for i := offset; i < offset+500 && i < n; i++ {
			session := i / 100
			if shape == "unique" {
				session = i
			}
			account := session%100 + 1
			key := session%500 + 1
			if shape == "skewed" && session%10 != 0 {
				account = 1
				key = 1
			}
			rec := &domain.RequestLogRecord{RequestID: fmt.Sprintf("probe-%d", i), AccountID: int64(account), APIKeyID: int64(key), Client: fmt.Sprintf("c%d", session%3), Model: fmt.Sprintf("m%d", session%5), ResolvedModel: fmt.Sprintf("r%d", session%5), Workspace: fmt.Sprintf("/workspace/%d", session%50), SessionID: fmt.Sprintf("session-%d", session), CallKind: "agent", CreatedAt: start.Add(time.Duration(int64(i)*30*24*3600/int64(n)) * time.Second), RequestJSON: `{"input":"metadata-sized payload"}`}
			args, err := requestLogArgs(rec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = logStmt.ExecContext(ctx, args...); err != nil {
				t.Fatal(err)
			}
			if _, err = usageStmt.ExecContext(ctx, rec.RequestID, `{"input_cache_hit":10,"input_cache_miss":20,"output":5,"reasoning":2}`, rec.CreatedAt.Unix()); err != nil {
				t.Fatal(err)
			}
		}
		logStmt.Close()
		usageStmt.Close()
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(began)
	st, err := os.Stat(db.Path() + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	wal := st.Size()
	if _, err = db.write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.write.Exec(`PRAGMA wal_autocheckpoint=1000`); err != nil {
		t.Fatal(err)
	}
	return elapsed, wal
}

func dimensionProbeTimes(t *testing.T, fn func()) (time.Duration, time.Duration) {
	t.Helper()
	fn() // warm-up; report ten subsequent samples on both paths.
	samples := make([]time.Duration, 10)
	for i := range samples {
		started := time.Now()
		fn()
		samples[i] = time.Since(started)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[4], samples[9]
}
