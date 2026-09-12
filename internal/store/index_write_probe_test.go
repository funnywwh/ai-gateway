package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

// Index probes: what a new index on request_logs costs to write and buys to read.
//
// These are measurements, not guards — they assert nothing and are skipped unless asked
// for, so the suite stays fast while the numbers quoted in the design docs stay
// reproducible. Every index added since M27 was decided with the same two probes:
//
//	M30_PROBE=1 go test ./internal/store -run TestRequestLogIndexWriteAmplificationProbe -v -count=1
//	M30_FILTER_PROBE=1 go test ./internal/store -run TestRequestLogFilteredPageProbe -v -count=1
//	M30_MIGRATE_PROBE=1 M30_SRC_DB=/abs/path/to/aigw.db go test ./internal/store \
//	    -run TestMigrationTimingProbe -v -count=1
//
// The write metric is WAL page bytes per row: it is deterministic, while wall clock is
// drowned by checkpoint jitter on a multi-hundred-MB database (the same reasoning as
// docs/design/m27-request-dimensions.md §2.2). Autocheckpoint is off, so the WAL file size
// at the end of the run is the total written during it.
//
// Measured for migration 0009 (two credential indexes) on 2026-09-12, 2.5 KB rows, 256
// rows per transaction: 4709.8 → 4937.7 B/row (1.048×) with real locality, 6945.1 B/row
// (1.475×) when every row carries a different account and key. A page filtered by one of
// 5000 accounts out of 60k rows: 59.9 ms → 92 µs. See
// docs/design/m30-request-log-owner-dimensions.md §7.
func TestRequestLogIndexWriteAmplificationProbe(t *testing.T) {
	if os.Getenv("M30_PROBE") == "" {
		t.Skip("set M30_PROBE=1 to run the request-log index write-amplification probe")
	}
	const (
		rows      = 30000
		batch     = 256
		payloadKB = 2560
	)
	baseline := runProbe(t, "baseline", 8, rows, batch, payloadKB, false)
	indexed := runProbe(t, "with-0009", 9, rows, batch, payloadKB, false)
	// The pessimistic case: every row a different account/key, so the two new indexes get
	// no locality at all (the same shape M27 measured as its upper bound).
	random := runProbe(t, "with-0009-random", 9, rows, batch, payloadKB, true)

	t.Logf("WAL bytes/row: baseline(0008)=%.1f  +0009=%.1f (%.3f×)  +0009 random owners=%.1f (%.3f×)",
		baseline, indexed, indexed/baseline, random, random/baseline)
}

func runProbe(t *testing.T, name string, maxVersion, rows, batch, payloadKB int, randomOwners bool) float64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > maxVersion {
			continue
		}
		if _, err := db.ExecContext(context.Background(), m.body); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"deepseek-flash","input":"` + strings.Repeat("x", payloadKB-60) + `"}`
	started := time.Now()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < rows; i += batch {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < batch && i+j < rows; j++ {
			n := i + j
			// Real-machine locality: one session spans 200 rows, a client and a key span a
			// session, so an index on the credential columns gets the same clustering it
			// sees in production.
			session := fmt.Sprintf("session-%04d", n/200)
			account, key := int64(1+(n/2000)%3), int64(1+(n/200)%4)
			if randomOwners {
				account, key = int64(1+rng.Intn(5000)), int64(1+rng.Intn(20000))
			}
			client := []string{"dsh", "codex"}[(n/2000)%2]
			if _, err := tx.ExecContext(ctx, putRequestLogSQL,
				fmt.Sprintf("req-%08d", n), key, account, "/v1/responses", body,
				"", "", 0, 0, len(body), 0, 0, "user", 0, 0, "completed", unix(time.Now().UTC()),
				client, "deepseek-flash", "deepseek-flash",
				"/home/winger/work/ai_gateway", session, "agent", ""); err != nil {
				t.Fatalf("insert %d: %v", n, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(started)

	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	perRow := float64(info.Size()) / float64(rows)
	t.Logf("%-22s rows=%d payload=%dB batch=%d → %.1f WAL B/row, wall %s (%.1f µs/row)",
		name, rows, payloadKB, batch, perRow, elapsed.Round(time.Millisecond),
		float64(elapsed.Microseconds())/float64(rows))
	return perRow
}

// What an equality-prefix index buys: one filtered page of a multi-tenant table. The page
// query projects every column (recorded bodies included), so without the index SQLite scans
// the whole window looking for matches before LIMIT can apply.
func TestRequestLogFilteredPageProbe(t *testing.T) {
	if os.Getenv("M30_FILTER_PROBE") == "" {
		t.Skip("set M30_FILTER_PROBE=1 to measure a filtered page with and without migration 0009")
	}
	const (
		rows    = 60000
		batch   = 256
		tenants = 5000
	)
	for _, tc := range []struct {
		name       string
		maxVersion int
	}{{"baseline(0008)", 8}, {"with-0009", 9}} {
		timing := timeFilteredPage(t, tc.name, tc.maxVersion, rows, batch, tenants)
		t.Logf("%-14s filtered page over %d rows / %d accounts (50-row page, all columns): %s",
			tc.name, rows, tenants, timing)
	}
}

func timeFilteredPage(t *testing.T, name string, maxVersion, rows, batch, tenants int) time.Duration {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > maxVersion {
			continue
		}
		if _, err := db.ExecContext(context.Background(), m.body); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}

	body := `{"model":"deepseek-flash","input":"` + strings.Repeat("x", 2440) + `"}`
	ctx := context.Background()
	for i := 0; i < rows; i += batch {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < batch && i+j < rows; j++ {
			n := i + j
			if _, err := tx.ExecContext(ctx, putRequestLogSQL,
				fmt.Sprintf("req-%08d", n), int64(1+(n%20)), int64(1+(n%tenants)), "/v1/responses", body,
				"", "", 0, 0, len(body), 0, 0, "user", 0, 0, "completed", unix(time.Now().UTC()),
				"dsh", "deepseek-flash", "deepseek-flash", "/w/a", fmt.Sprintf("session-%04d", n/200), "agent", ""); err != nil {
				t.Fatalf("insert %d: %v", n, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}

	// One tenant in 5000: about 12 matching rows out of 60k. That is the common shape of
	// the console's account filter once a deployment has more than a handful of tenants.
	now := time.Now().UTC()
	where, args := requestLogFilter("", domain.RequestLogFilter{
		From: now.AddDate(0, 0, -30), To: now.AddDate(0, 0, 1), AccountID: 4999,
	})
	query := requestLogListSQL(where)
	var best time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		started := time.Now()
		page, err := db.QueryContext(ctx, query, append(args, 50, 0)...)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for page.Next() {
			count++
		}
		page.Close()
		if count == 0 {
			t.Fatal("the probe filter matched nothing; the measurement would be meaningless")
		}
		if elapsed := time.Since(started); best == 0 || elapsed < best {
			best = elapsed
		}
	}
	return best.Round(time.Microsecond)
}

// What a migration costs on a real deployment database: the CREATE INDEX statements are
// the only write-lock window it opens. The source database is copied, never touched —
// production applies the migration on its next start, and this probe only reports what
// that costs.
func TestMigrationTimingProbe(t *testing.T) {
	src := os.Getenv("M30_SRC_DB")
	if os.Getenv("M30_MIGRATE_PROBE") == "" || src == "" {
		t.Skip("set M30_MIGRATE_PROBE=1 and M30_SRC_DB=<absolute path> to measure the migration")
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	dst := filepath.Join("..", "..", ".cache", "m30probe", "migration-copy.db")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	copyStarted := time.Now()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	size, err := io.Copy(out, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	copiedAt := time.Now()
	started := time.Now()
	db, err := Open(context.Background(), config.Database{Path: dst, WAL: true, BusyTimeoutMS: 10000})
	if err != nil {
		t.Fatalf("open+migrate the copy: %v", err)
	}
	elapsed := time.Since(started)
	defer db.Close()

	var rows int
	if err := db.read.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM request_logs").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	var walBytes int64
	if info, err := os.Stat(dst + "-wal"); err == nil {
		walBytes = info.Size()
	}
	t.Logf("copied %.1f MB in %s; open+migrate 0009 on %d request_logs rows took %s (WAL now %.1f MB)",
		float64(size)/(1<<20), copiedAt.Sub(copyStarted).Round(time.Millisecond),
		rows, elapsed.Round(time.Millisecond), float64(walBytes)/(1<<20))
	for _, name := range []string{"idx_request_logs_account", "idx_request_logs_key"} {
		var found string
		if err := db.read.QueryRowContext(context.Background(),
			"SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&found); err != nil {
			t.Fatalf("index %s missing after the migration: %v", name, err)
		}
	}
}
