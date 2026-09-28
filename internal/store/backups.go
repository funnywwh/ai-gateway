package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const backupCols = `id, started_at, finished_at, path, size_bytes, status, quick_check, triggered_by, note`

func scanBackupJob(row rowScanner) (*domain.BackupJob, error) {
	var (
		job        domain.BackupJob
		startedAt  int64
		finishedAt sql.NullInt64
	)
	if err := row.Scan(&job.ID, &startedAt, &finishedAt, &job.Path, &job.SizeBytes,
		&job.Status, &job.QuickCheck, &job.TriggeredBy, &job.Note); err != nil {
		return nil, err
	}
	job.StartedAt = timeFromUnix(startedAt)
	job.FinishedAt = timePtrFromNull(finishedAt)
	return &job, nil
}

// InsertBackupJob records the start of a backup.
func (db *DB) InsertBackupJob(ctx context.Context, job *domain.BackupJob) (int64, error) {
	if job == nil {
		return 0, domain.ErrInvalidRequest("backup job is required")
	}
	if job.StartedAt.IsZero() {
		job.StartedAt = time.Now().UTC()
	}
	res, err := db.write.ExecContext(ctx, `
INSERT INTO backup_jobs(started_at, path, size_bytes, status, quick_check, triggered_by, note, created_at)
VALUES(?,?,?,?,?,?,?,?)`,
		unix(job.StartedAt), job.Path, job.SizeBytes, job.Status, job.QuickCheck,
		job.TriggeredBy, job.Note, unix(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: insert backup job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: backup job id: %w", err)
	}
	return id, nil
}

// FinishBackupJob closes out a backup. It takes the whole job because the row's closing
// facts — status, quick_check, note, and the snapshot size the manager measured — all
// belong to the same record, and a size that is not written here is never written at all:
// the console's 大小/占用 columns read exactly this column (v4.7.1).
func (db *DB) FinishBackupJob(ctx context.Context, job *domain.BackupJob, failure string) error {
	if job == nil {
		return domain.ErrInvalidRequest("backup job is required")
	}
	finalNote := job.Note
	if failure != "" {
		if finalNote != "" {
			finalNote += "; "
		}
		finalNote += failure
	}
	if _, err := db.write.ExecContext(ctx, `
UPDATE backup_jobs SET finished_at = ?, status = ?, quick_check = ?, note = ?, size_bytes = ? WHERE id = ?`,
		unix(time.Now()), job.Status, job.QuickCheck, finalNote, job.SizeBytes, job.ID); err != nil {
		return fmt.Errorf("store: finish backup job: %w", err)
	}
	return nil
}

// SetBackupJobSize rewrites one job's recorded snapshot size. It exists for the startup
// repair of rows written before the size was persisted (see backup.Manager.RepairMissingSizes);
// an id that is no longer there is not an error, because pruning can race a repair.
func (db *DB) SetBackupJobSize(ctx context.Context, id, size int64) error {
	if _, err := db.write.ExecContext(ctx,
		`UPDATE backup_jobs SET size_bytes = ? WHERE id = ?`, size, id); err != nil {
		return fmt.Errorf("store: set backup job size: %w", err)
	}
	return nil
}

// ListBackupJobs returns recent backups, newest first.
func (db *DB) ListBackupJobs(ctx context.Context, limit int) ([]*domain.BackupJob, error) {
	if limit <= 0 || limit > 2000 {
		limit = 100
	}
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+backupCols+" FROM backup_jobs ORDER BY started_at DESC, id DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("store: list backup jobs: %w", err)
	}
	defer rows.Close()
	out := []*domain.BackupJob{}
	for rows.Next() {
		job, err := scanBackupJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan backup job: %w", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate backup jobs: %w", err)
	}
	return out, nil
}

// GetBackupJob loads one backup record.
func (db *DB) GetBackupJob(ctx context.Context, id int64) (*domain.BackupJob, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+backupCols+" FROM backup_jobs WHERE id = ?", id)
	job, err := scanBackupJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("backup %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get backup job: %w", err)
	}
	return job, nil
}

// DeleteBackupJob removes one backup record.
func (db *DB) DeleteBackupJob(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM backup_jobs WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete backup job: %w", err)
	}
	return nil
}

// FailRunningBackupJobs closes out backups that were interrupted by a crash.
func (db *DB) FailRunningBackupJobs(ctx context.Context, reason string) (int64, error) {
	res, err := db.write.ExecContext(ctx, `
UPDATE backup_jobs SET status = 'failed', finished_at = ?, note = note || ? WHERE status = 'running'`,
		unix(time.Now()), "; "+reason)
	if err != nil {
		return 0, fmt.Errorf("store: fail running backup jobs: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: fail running backup jobs rows: %w", err)
	}
	return affected, nil
}
