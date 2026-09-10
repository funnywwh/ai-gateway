package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

func TestScheduleNext(t *testing.T) {
	base := time.Date(2026, 3, 10, 3, 29, 30, 0, time.UTC)
	cases := []struct {
		expression string
		want       string
	}{
		{"30 3 * * *", "2026-03-10T03:30:00Z"},
		{"*/15 * * * *", "2026-03-10T03:30:00Z"},
		{"0 0 1 * *", "2026-04-01T00:00:00Z"},
		{"0 12 * * mon", "2026-03-16T12:00:00Z"},
		{"5,35 4-5 * * *", "2026-03-10T04:05:00Z"},
	}
	for _, tc := range cases {
		schedule, err := ParseSchedule(tc.expression)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.expression, err)
		}
		got := schedule.Next(base)
		if got.UTC().Format(time.RFC3339) != tc.want {
			t.Errorf("%s: next = %s, want %s", tc.expression, got.UTC().Format(time.RFC3339), tc.want)
		}
	}

	if _, err := ParseSchedule("not a cron"); err == nil {
		t.Fatal("a malformed expression must be rejected")
	}
	if _, err := ParseSchedule("99 * * * *"); err == nil {
		t.Fatal("an out-of-range minute must be rejected")
	}
}

func TestRetentionPlan(t *testing.T) {
	start := time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)
	jobs := []*Job{}
	for day := 0; day < 40; day++ {
		at := start.AddDate(0, 0, day)
		jobs = append(jobs, &Job{ID: int64(day + 1), StartedAt: at, Status: "ok"})
	}
	plan := RetentionPlan(jobs, Retention{Daily: 7, Weekly: 4, Monthly: 2})
	// Newest seven plus the newest of each of the last four ISO weeks and two months.
	if !plan[40] || !plan[34] {
		t.Fatalf("the newest daily snapshots must be kept: %v", plan)
	}
	if plan[1] {
		t.Fatal("the oldest snapshot should have been pruned")
	}
	kept := 0
	for _, job := range jobs {
		if plan[job.ID] {
			kept++
		}
	}
	if kept < 7 || kept > 14 {
		t.Fatalf("kept %d snapshots, expected the daily/weekly/monthly union", kept)
	}
}

func newBackupFixture(t *testing.T) (*Manager, *store.DB, string) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	dir := t.TempDir()
	cfg.Database.Path = filepath.Join(dir, "gateway.db")
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", BillingMode: domain.BillingPrepaid, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	manager := New(Config{
		DatabasePath: cfg.Database.Path,
		Dir:          filepath.Join(dir, "backups"),
		Verify:       true,
		Retention:    Retention{Daily: 3},
	}, db, nil)
	return manager, db, cfg.Database.Path
}

func TestBackupProducesAVerifiedSnapshot(t *testing.T) {
	ctx := context.Background()
	manager, db, databasePath := newBackupFixture(t)

	job, err := manager.Run(ctx, "manual:test")
	if err != nil {
		t.Fatalf("run backup: %v", err)
	}
	if job.Status != "ok" || job.QuickCheck != "ok" {
		t.Fatalf("job = %+v", job)
	}
	info, err := os.Stat(job.Path)
	if err != nil {
		t.Fatalf("the snapshot file must exist: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the snapshot is empty")
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot permissions = %v, want 0600", info.Mode().Perm())
	}

	// The snapshot must be a usable database containing the data we wrote.
	snapshot, err := openSnapshotReadOnly(job.Path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snapshot.Close()
	var accounts int
	if err := snapshot.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&accounts); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("snapshot has %d accounts, want 1", accounts)
	}

	jobs, err := manager.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != "ok" {
		t.Fatalf("backup_jobs = %+v", jobs)
	}
	_ = db
	_ = databasePath
}

func TestBackupVerifyFailureKeepsTheFile(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newBackupFixture(t)
	// A verifier that always fails stands in for a corrupt snapshot.
	manager.openSnapshot = func(path string) (*sql.DB, error) {
		if filepath.Ext(path) != ".db" || filepath.Base(path) == "gateway.db" {
			return openSnapshotReadOnly(path)
		}
		return nil, os.ErrInvalid
	}
	job, err := manager.Run(ctx, "manual:test")
	if err == nil {
		t.Fatal("a snapshot that cannot be verified must fail the job")
	}
	if job.Status != "failed" {
		t.Fatalf("job status = %s, want failed", job.Status)
	}
	if _, statErr := os.Stat(job.Path); statErr != nil {
		t.Fatalf("a failed snapshot must be kept for inspection: %v", statErr)
	}
	// Pruning must never delete a failed snapshot.
	if _, err := manager.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(job.Path); statErr != nil {
		t.Fatalf("pruning removed a failed snapshot: %v", statErr)
	}
	_ = db
}

func TestApplyPendingRestoreSwapsFiles(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "gateway.db")
	if err := os.WriteFile(database, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database+".restore-pending", []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	preserved, err := ApplyPendingRestore(database, time.Date(2026, 3, 10, 4, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("apply restore: %v", err)
	}
	if preserved == "" {
		t.Fatal("the previous database must be preserved")
	}
	content, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "snapshot" {
		t.Fatalf("database content = %q, want the snapshot", content)
	}
	if _, err := os.Stat(database + "-wal"); !os.IsNotExist(err) {
		t.Fatal("the stale WAL file must be removed")
	}
	old, err := os.ReadFile(preserved)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "current" {
		t.Fatalf("the preserved database = %q, want the previous content", old)
	}
	if _, err := os.Stat(database + ".restore-pending"); !os.IsNotExist(err) {
		t.Fatal("the pending file must be consumed")
	}

	// A second call with nothing staged is a no-op.
	preserved, err = ApplyPendingRestore(database, time.Now().UTC(), nil)
	if err != nil || preserved != "" {
		t.Fatalf("second apply = %q, %v", preserved, err)
	}
}

// openSnapshotReadOnly opens a file read-only, which is what a restore would hand to a
// fresh process.
func openSnapshotReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
}
