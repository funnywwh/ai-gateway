package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// Job is one backup record; it is the domain type so the store can persist it without
// importing this package.
type Job = domain.BackupJob

// Retention is the pruning policy from the configuration.
type Retention struct {
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// Config configures the backup manager.
type Config struct {
	// DatabasePath is the live SQLite file the snapshots are taken from.
	DatabasePath string
	Dir          string
	Enabled      bool
	Verify       bool
	Retention    Retention
	// OnEvent publishes backup.finished / backup.failed; nil disables it.
	OnEvent func(name string, payload map[string]any)
}

// Store is the persistence the manager needs.
type Store interface {
	InsertBackupJob(ctx context.Context, job *Job) (int64, error)
	FinishBackupJob(ctx context.Context, id int64, status, quickCheck, note, failure string) error
	ListBackupJobs(ctx context.Context, limit int) ([]*Job, error)
	GetBackupJob(ctx context.Context, id int64) (*Job, error)
	DeleteBackupJob(ctx context.Context, id int64) error
	FailRunningBackupJobs(ctx context.Context, reason string) (int64, error)
}

// Manager runs backups and prunes old copies.
type Manager struct {
	cfg   Config
	store Store
	log   *slog.Logger
	// now is overridable in tests.
	now func() time.Time
	// schedule is set by the scheduler so the API can report the next run.
	schedule *Schedule
	// dsn builds the connection string used to open a snapshot for verification.
	openSnapshot func(path string) (*sql.DB, error)
}

// New builds a manager. The database is opened separately for verification so a
// failing snapshot can never take the live pool down with it.
func New(cfg Config, store Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Retention.Daily == 0 && cfg.Retention.Weekly == 0 && cfg.Retention.Monthly == 0 {
		cfg.Retention = Retention{Daily: 7, Weekly: 4, Monthly: 3}
	}
	return &Manager{
		cfg: cfg, store: store, log: log,
		now:          func() time.Time { return time.Now().UTC() },
		openSnapshot: defaultOpenSnapshot,
	}
}

// SetClock overrides the clock (tests).
func (m *Manager) SetClock(now func() time.Time) { m.now = now }

// Dir is where snapshots are written.
func (m *Manager) Dir() string { return m.cfg.Dir }

// Run takes one snapshot. It is safe to call concurrently with request traffic: the
// snapshot is produced by VACUUM INTO, which SQLite executes inside its own read
// transaction, so writers are never blocked.
func (m *Manager) Run(ctx context.Context, triggeredBy string) (*Job, error) {
	if strings.TrimSpace(m.cfg.DatabasePath) == "" {
		return nil, domain.ErrInvalidRequest("backup: the database path is not configured")
	}
	if strings.TrimSpace(m.cfg.Dir) == "" {
		return nil, domain.ErrInvalidRequest("backup: the backup directory is not configured")
	}
	started := m.now()
	job := &Job{
		StartedAt: started, Status: "running", TriggeredBy: triggeredBy,
		Path: filepath.Join(m.cfg.Dir, "aigw-"+started.Format("20060102-150405")+".db"),
	}
	if triggeredBy == "" {
		job.TriggeredBy = "manual"
	}
	id, err := m.store.InsertBackupJob(ctx, job)
	if err != nil {
		return nil, err
	}
	job.ID = id

	if err := os.MkdirAll(m.cfg.Dir, 0o700); err != nil {
		return m.fail(ctx, job, fmt.Errorf("create backup directory: %w", err))
	}
	// A leftover file from an interrupted run would make VACUUM INTO fail.
	if err := os.Remove(job.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return m.fail(ctx, job, fmt.Errorf("clear a stale backup file: %w", err))
	}

	if err := m.snapshot(ctx, job.Path); err != nil {
		return m.fail(ctx, job, err)
	}
	if err := os.Chmod(job.Path, 0o600); err != nil {
		m.log.Warn("restricting backup permissions failed", "err", err, "path", job.Path)
	}
	if info, err := os.Stat(job.Path); err == nil {
		job.SizeBytes = info.Size()
	}

	quickCheck := "skipped"
	if m.cfg.Verify {
		result, err := m.verify(ctx, job.Path)
		if err != nil {
			return m.fail(ctx, job, err)
		}
		quickCheck = result
		if !strings.EqualFold(result, "ok") {
			job.QuickCheck = result
			job.Note = "quick_check did not pass; the file is kept for inspection"
			return m.fail(ctx, job, fmt.Errorf("backup verification failed: %s", result))
		}
	}
	job.QuickCheck = quickCheck
	job.Status = "ok"
	finished := m.now()
	job.FinishedAt = &finished
	if err := m.store.FinishBackupJob(ctx, job.ID, job.Status, job.QuickCheck, job.Note, ""); err != nil {
		return nil, err
	}
	m.log.Info("backup finished",
		"job", job.ID, "path", job.Path, "bytes", job.SizeBytes,
		"quick_check", job.QuickCheck, "trigger", job.TriggeredBy)
	m.emit("backup.finished", job)

	if pruned, err := m.Prune(ctx); err != nil {
		m.log.Warn("pruning old backups failed", "err", err)
	} else if len(pruned) > 0 {
		m.log.Info("old backups pruned", "count", len(pruned))
	}
	return job, nil
}

// snapshot produces the consistent copy. VACUUM INTO is atomic from the reader's point
// of view and needs no cooperation from writers.
func (m *Manager) snapshot(ctx context.Context, target string) error {
	pool, err := m.openSnapshot(m.cfg.DatabasePath)
	if err != nil {
		return domain.ErrInternal("open database for backup: " + err.Error())
	}
	defer pool.Close()

	// TRUNCATE keeps the WAL small; a failure here is not fatal for the backup.
	if _, err := pool.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		m.log.Warn("wal checkpoint before backup failed", "err", err)
	}
	if _, err := pool.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		_ = os.Remove(target)
		return domain.ErrInternal("snapshot the database: " + err.Error())
	}
	return nil
}

// verify opens the snapshot read-only and runs PRAGMA quick_check.
func (m *Manager) verify(ctx context.Context, path string) (string, error) {
	pool, err := m.openSnapshot(path)
	if err != nil {
		return "", domain.ErrInternal("open snapshot for verification: " + err.Error())
	}
	defer pool.Close()
	var result string
	if err := pool.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return "", domain.ErrInternal("verify snapshot: " + err.Error())
	}
	return result, nil
}

func (m *Manager) fail(ctx context.Context, job *Job, cause error) (*Job, error) {
	job.Status = "failed"
	finished := m.now()
	job.FinishedAt = &finished
	m.log.Error("backup failed", "job", job.ID, "err", cause)
	if job.Note == "" {
		job.Note = cause.Error()
	}
	if err := m.store.FinishBackupJob(ctx, job.ID, job.Status, job.QuickCheck, job.Note, cause.Error()); err != nil {
		m.log.Warn("recording the failed backup failed", "err", err)
	}
	m.emit("backup.failed", job)
	return job, cause
}

func (m *Manager) emit(name string, job *Job) {
	if m.cfg.OnEvent == nil {
		return
	}
	m.cfg.OnEvent(name, map[string]any{
		"job_id": job.ID, "path": job.Path, "size_bytes": job.SizeBytes,
		"status": job.Status, "quick_check": job.QuickCheck, "trigger": job.TriggeredBy,
	})
}

// NextRun is set by the scheduler so the API can report it.
func (m *Manager) Jobs(ctx context.Context, limit int) ([]*Job, error) {
	return m.store.ListBackupJobs(ctx, limit)
}

// Job loads one job.
func (m *Manager) Job(ctx context.Context, id int64) (*Job, error) {
	return m.store.GetBackupJob(ctx, id)
}

// Delete removes a snapshot file and its record.
func (m *Manager) Delete(ctx context.Context, id int64) error {
	job, err := m.store.GetBackupJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Path != "" {
		if err := os.Remove(job.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.ErrInternal("remove backup file: " + err.Error())
		}
	}
	return m.store.DeleteBackupJob(ctx, id)
}

// Prune applies the retention policy and returns the job ids it deleted.
func (m *Manager) Prune(ctx context.Context) ([]int64, error) {
	jobs, err := m.store.ListBackupJobs(ctx, 1000)
	if err != nil {
		return nil, err
	}
	keep := RetentionPlan(jobs, m.cfg.Retention)
	deleted := []int64{}
	for _, job := range jobs {
		if keep[job.ID] {
			continue
		}
		if job.Status != "ok" {
			// Never silently delete a snapshot that failed verification: it is the one
			// an operator may need to look at.
			continue
		}
		if err := m.Delete(ctx, job.ID); err != nil {
			m.log.Warn("deleting a pruned backup failed", "err", err, "job", job.ID)
			continue
		}
		deleted = append(deleted, job.ID)
	}
	return deleted, nil
}

// RetentionPlan returns the set of job ids to keep: the newest N daily snapshots, plus
// the newest snapshot of each of the last M weeks and K months.
func RetentionPlan(jobs []*Job, retention Retention) map[int64]bool {
	ordered := append([]*Job(nil), jobs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartedAt.After(ordered[j].StartedAt) })
	keep := map[int64]bool{}
	for index, job := range ordered {
		if retention.Daily > 0 && index < retention.Daily {
			keep[job.ID] = true
		}
	}
	markFirst(ordered, keep, retention.Weekly, func(job *Job) string {
		year, week := job.StartedAt.ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	})
	markFirst(ordered, keep, retention.Monthly, func(job *Job) string {
		return job.StartedAt.Format("2006-01")
	})
	return keep
}

func markFirst(jobs []*Job, keep map[int64]bool, count int, key func(*Job) string) {
	if count <= 0 {
		return
	}
	seen := map[string]bool{}
	groups := 0
	for _, job := range jobs {
		group := key(job)
		if seen[group] {
			continue
		}
		seen[group] = true
		keep[job.ID] = true
		groups++
		if groups >= count {
			return
		}
	}
}

// ScheduleRestore stages a snapshot for the next start. Replacing the database file of a
// running process would corrupt the open pools, so restore is deliberately a two-step
// operation: stage now, swap at startup.
func (m *Manager) ScheduleRestore(ctx context.Context, id int64) (string, error) {
	job, err := m.store.GetBackupJob(ctx, id)
	if err != nil {
		return "", err
	}
	if job.Status != "ok" {
		return "", domain.ErrConflict("only a verified backup can be restored")
	}
	if _, err := os.Stat(job.Path); err != nil {
		return "", domain.ErrNotFound("backup file " + job.Path)
	}
	pending := m.cfg.DatabasePath + ".restore-pending"
	if err := copyFile(job.Path, pending); err != nil {
		return "", domain.ErrInternal("stage the restore: " + err.Error())
	}
	return pending, nil
}

// ApplyPendingRestore performs the swap at startup. It returns the path of the
// preserved previous database, or \"\" when there was nothing to do.
func ApplyPendingRestore(databasePath string, now time.Time, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	pending := databasePath + ".restore-pending"
	if _, err := os.Stat(pending); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("backup: check for a pending restore: %w", err)
	}
	preserved := ""
	if _, err := os.Stat(databasePath); err == nil {
		preserved = databasePath + ".pre-restore-" + now.UTC().Format("20060102-150405")
		if err := os.Rename(databasePath, preserved); err != nil {
			return "", fmt.Errorf("backup: preserve the current database: %w", err)
		}
	}
	// The WAL and shared-memory files belong to the replaced database.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(databasePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("backup: clear %s: %w", suffix, err)
		}
	}
	if err := os.Rename(pending, databasePath); err != nil {
		return "", fmt.Errorf("backup: install the snapshot: %w", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		log.Warn("restricting restored database permissions failed", "err", err)
	}
	log.Warn("restored the database from a staged snapshot",
		"database", databasePath, "preserved", preserved)
	return preserved, nil
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// SetSchedule records the cron expression so NextRun can report the next instant.
func (m *Manager) SetSchedule(schedule *Schedule) { m.schedule = schedule }

// NextRun reports when the next scheduled backup fires (zero when unscheduled).
func (m *Manager) NextRun() time.Time {
	if m.schedule == nil {
		return time.Time{}
	}
	return m.schedule.Next(m.now())
}
