package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/backup"
	"github.com/winger/ai-gateway/internal/domain"
)

// BackupAdmin is the backup surface the management API needs.
type BackupAdmin interface {
	Run(ctx context.Context, triggeredBy string) (*backup.Job, error)
	Jobs(ctx context.Context, limit int) ([]*backup.Job, error)
	Job(ctx context.Context, id int64) (*backup.Job, error)
	Delete(ctx context.Context, id int64) error
	ScheduleRestore(ctx context.Context, id int64) (string, error)
	Prune(ctx context.Context) ([]int64, error)
	NextRun() time.Time
	Dir() string
}

func (s *Server) handleAdminListBackups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	manager, ok := portReady(w, s.deps.Backups, "backups")
	if !ok {
		return
	}
	jobs, err := manager.Jobs(r.Context(), adminLimit(r, 100, 500))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	var totalBytes int64
	for _, job := range jobs {
		out = append(out, backupJobJSON(job))
		totalBytes += job.SizeBytes
	}
	payload := map[string]any{
		"data": out, "count": len(out),
		"dir": manager.Dir(), "total_bytes": totalBytes,
	}
	if next := manager.NextRun(); !next.IsZero() {
		payload["next_run"] = next.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, payload)
}

func backupJobJSON(job *backup.Job) map[string]any {
	return map[string]any{
		"id": job.ID, "path": filepath.Base(job.Path), "size_bytes": job.SizeBytes,
		"status": job.Status, "quick_check": job.QuickCheck, "trigger": job.TriggeredBy,
		"note":        job.Note,
		"started_at":  job.StartedAt.UTC().Format(time.RFC3339),
		"finished_at": timeOrNil(job.FinishedAt),
	}
}

func (s *Server) handleAdminRunBackup(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	manager, ok := portReady(w, s.deps.Backups, "backups")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	job, err := manager.Run(ctx, "manual"+actorSuffix(actor.Username))
	if err != nil {
		if job != nil {
			s.audit(r.Context(), actor.Username, "backup", "backup_job", strconv.FormatInt(job.ID, 10),
				map[string]any{"status": job.Status, "error": err.Error()}, "failed")
			writeJSON(w, http.StatusOK, map[string]any{
				"job": backupJobJSON(job), "ok": false, "error": err.Error(),
			})
			return
		}
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "backup", "backup_job", strconv.FormatInt(job.ID, 10),
		map[string]any{"size_bytes": job.SizeBytes, "quick_check": job.QuickCheck}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"job": backupJobJSON(job), "ok": true})
}

func actorSuffix(username string) string {
	if username == "" {
		return ""
	}
	return ":" + username
}

func (s *Server) handleAdminDeleteBackup(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	manager, ok := portReady(w, s.deps.Backups, "backups")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid backup id"))
		return
	}
	if err := manager.Delete(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "backup_job", strconv.FormatInt(id, 10), nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleAdminDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, true); !ok {
		return
	}
	manager, ok := portReady(w, s.deps.Backups, "backups")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid backup id"))
		return
	}
	job, err := manager.Job(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	file, err := os.Open(job.Path)
	if err != nil {
		writeAPIError(w, domain.ErrNotFound("backup file"))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(job.Path)))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// handleAdminRestoreBackup stages a snapshot for the next start. Swapping the file of a
// running process would corrupt the open pools, so the swap happens at startup.
func (s *Server) handleAdminRestoreBackup(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	manager, ok := portReady(w, s.deps.Backups, "backups")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid backup id"))
		return
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if !body.Confirm {
		writeAPIError(w, domain.ErrInvalidRequest("restoring replaces the database at the next start: send {\"confirm\": true} to proceed"))
		return
	}
	pending, err := manager.ScheduleRestore(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "restore", "backup_job", strconv.FormatInt(id, 10),
		map[string]any{"staged": pending}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"staged": pending, "restart_required": true,
		"note": "the snapshot was staged; restart the gateway to apply it. The current database is preserved as <db>.pre-restore-<timestamp>",
	})
}

func (s *Server) handleAdminPruneBackups(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	manager, ok := portReady(w, s.deps.Backups, "backups")
	if !ok {
		return
	}
	deleted, err := manager.Prune(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "prune", "backup_job", "", map[string]any{"deleted": len(deleted)}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "count": len(deleted)})
}

var _ = strings.TrimSpace
