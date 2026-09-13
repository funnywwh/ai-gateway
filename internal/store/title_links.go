package store

import (
	"context"
	"database/sql"

	"github.com/winger/ai-gateway/internal/domain"
)

// Linking is part of the log transaction for both synchronous and batched writers.
// Only title rows move. The distinct main sessions are counted again on every new
// observation, so a later competing session restores the original title identity.
func (db *DB) linkTitleSessionTx(ctx context.Context, tx *sql.Tx, rec *domain.RequestLogRecord) error {
	if rec.TitleFingerprint == "" || rec.Client != "codex" || rec.SessionID == "" || rec.Workspace == "" || (rec.CallKind != "agent" && rec.CallKind != "title") {
		return nil
	}
	at := rec.StartedAt
	if at.IsZero() {
		at = rec.CreatedAt
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO request_title_links
 (request_id,account_id,api_key_id,workspace,fingerprint,source_session,call_kind,started_at)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(request_id) DO NOTHING`, rec.RequestID, rec.AccountID, rec.APIKeyID, rec.Workspace, rec.TitleFingerprint, rec.SessionID, rec.CallKind, unix(at))
	if err != nil {
		return err
	}
	// A session's first observed prompt time is stable across continuations. A newly
	// seen title can arrive before the main request finishes writing its log.
	_, err = tx.ExecContext(ctx, `WITH candidates AS (
 SELECT source_session, MIN(started_at) AS first_seen FROM request_title_links
 WHERE account_id=? AND api_key_id=? AND workspace=? AND fingerprint=? AND call_kind='agent'
 GROUP BY source_session
 ), targets AS (
 SELECT t.request_id, CASE WHEN COUNT(c.source_session)=1 THEN MIN(c.source_session)
 ELSE t.source_session END AS session_id
 FROM request_title_links t LEFT JOIN candidates c
 ON c.first_seen BETWEEN t.started_at-120 AND t.started_at+120
 WHERE t.account_id=? AND t.api_key_id=? AND t.workspace=? AND t.fingerprint=? AND t.call_kind='title'
 GROUP BY t.request_id
 ) UPDATE request_logs SET session_id=(SELECT session_id FROM targets WHERE targets.request_id=request_logs.request_id)
 WHERE request_id IN (SELECT request_id FROM targets)
 AND session_id IS NOT (SELECT session_id FROM targets WHERE targets.request_id=request_logs.request_id)`,
		rec.AccountID, rec.APIKeyID, rec.Workspace, rec.TitleFingerprint,
		rec.AccountID, rec.APIKeyID, rec.Workspace, rec.TitleFingerprint)
	return err
}
