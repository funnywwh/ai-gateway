package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/funnywwh/ai-gateway/internal/domain"
)

// The company-scoped half of the Feishu directory sync (M92).
//
// Two things live here. First the adoption of pre-M92 rows: before this milestone a deployment
// served exactly one Feishu enterprise, so a department link without a company unambiguously
// belongs to the identity app — a fact only the configuration knows, which is why it is a
// startup call and not a SQL migration. Second the person ↔ account mapping of companies that
// are *not* the identity app; the identity app's mapping stays on accounts.feishu_* because that
// column is also the DSH portal login identity (M72).

// ListFeishuPersonLinks returns every company-scoped mapping, ordered so the result is stable
// (the sync and the console both diff it against a directory walk).
func (db *DB) ListFeishuPersonLinks(ctx context.Context) ([]domain.FeishuPersonLink, error) {
	rows, err := db.read.QueryContext(ctx, `
SELECT feishu_app_id, open_id, union_id, name, account_id, bound_by, bound_at
FROM feishu_person_links ORDER BY feishu_app_id, open_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list feishu person links: %w", err)
	}
	defer rows.Close()

	out := []domain.FeishuPersonLink{}
	for rows.Next() {
		var (
			link    domain.FeishuPersonLink
			boundAt sql.NullInt64
		)
		if err := rows.Scan(&link.AppID, &link.OpenID, &link.UnionID, &link.Name, &link.AccountID,
			&link.BoundBy, &boundAt); err != nil {
			return nil, fmt.Errorf("store: scan feishu person link: %w", err)
		}
		if boundAt.Valid {
			at := timeFromUnix(boundAt.Int64)
			link.BoundAt = &at
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate feishu person links: %w", err)
	}
	return out, nil
}

// UpsertFeishuPersonLink writes one person → account mapping for one company.
//
// It is an upsert on (app_id, open_id) because that pair is the person's identity inside that
// company: a re-sync of the same person updates the display name and the union id instead of
// failing. The other unique key — (app_id, account_id) — is what refuses "two of this company's
// people on one account"; the database, not a read-then-write, is the referee.
func (db *DB) UpsertFeishuPersonLink(ctx context.Context, link domain.FeishuPersonLink) error {
	appID := strings.TrimSpace(link.AppID)
	openID := strings.TrimSpace(link.OpenID)
	if appID == "" || openID == "" {
		return domain.ErrInvalidRequest("a Feishu person link requires an app id and an open id")
	}
	if link.AccountID <= 0 {
		return domain.ErrInvalidRequest("a Feishu person link requires an account id")
	}
	if _, err := db.GetAccount(ctx, link.AccountID); err != nil {
		return err
	}
	boundAt := link.BoundAt
	now := time.Now().UTC()
	if boundAt == nil {
		boundAt = &now
	}
	_, err := db.write.ExecContext(ctx, `
INSERT INTO feishu_person_links(feishu_app_id, open_id, union_id, name, account_id, bound_by, bound_at, created_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(feishu_app_id, open_id) DO UPDATE SET
  union_id   = excluded.union_id,
  name       = excluded.name,
  account_id = excluded.account_id,
  bound_by   = excluded.bound_by,
  bound_at   = excluded.bound_at`,
		appID, openID, link.UnionID, link.Name, link.AccountID, link.BoundBy, unix(*boundAt), unix(now))
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict(
				"this account already maps to another person of that company; unbind that person first")
		}
		return fmt.Errorf("store: link feishu person %s: %w", openID, err)
	}
	return nil
}

// DeleteFeishuPersonLink removes one mapping and reports whether anything changed, so the
// console can answer idempotently.
func (db *DB) DeleteFeishuPersonLink(ctx context.Context, appID, openID string) (bool, error) {
	result, err := db.write.ExecContext(ctx,
		"DELETE FROM feishu_person_links WHERE feishu_app_id = ? AND open_id = ?",
		strings.TrimSpace(appID), strings.TrimSpace(openID))
	if err != nil {
		return false, fmt.Errorf("store: delete feishu person link %s: %w", openID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete feishu person link %s: %w", openID, err)
	}
	return affected > 0, nil
}

// DeleteFeishuPersonLinksByApp drops every mapping of one company (a client offboarding) and
// reports how many rows went. It touches nothing else: org nodes, memberships, accounts and API
// keys are the operator's to delete, one decision at a time.
func (db *DB) DeleteFeishuPersonLinksByApp(ctx context.Context, appID string) (int, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return 0, domain.ErrInvalidRequest("an app id is required")
	}
	result, err := db.write.ExecContext(ctx,
		"DELETE FROM feishu_person_links WHERE feishu_app_id = ?", appID)
	if err != nil {
		return 0, fmt.Errorf("store: delete feishu person links of %s: %w", appID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete feishu person links of %s: %w", appID, err)
	}
	return int(affected), nil
}

// AdoptLegacyFeishuScope stamps the identity application on department links written before
// M92 (feishu_app_id = ''), so later syncs can tell one company's departments from another's.
//
// It is idempotent and cheap once the upgrade is done (the WHERE clause matches nothing), which
// is what lets it run on every start the way the M72 key-identity backfill does. Failures are
// the caller's to log, not fatal: a deployment that cannot adopt keeps behaving exactly as it
// did before the upgrade, which is strictly better than refusing to start a healthy gateway.
func (db *DB) AdoptLegacyFeishuScope(ctx context.Context, identityAppID string) (int, error) {
	appID := strings.TrimSpace(identityAppID)
	if appID == "" {
		return 0, nil
	}
	result, err := db.write.ExecContext(ctx, `
UPDATE org_nodes SET feishu_app_id = ?
WHERE feishu_app_id = '' AND feishu_department_id <> ''`, appID)
	if err != nil {
		return 0, fmt.Errorf("store: adopt legacy feishu org scope: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: adopt legacy feishu org scope: %w", err)
	}
	return int(affected), nil
}

// CountFeishuPersonLinksByApp reports how many people of one company are mapped, plus how many
// distinct accounts they cover. The console uses both numbers on the companies list; a read
// error is reported rather than swallowed because a wrong count in a management page is worse
// than an error message.
func (db *DB) CountFeishuPersonLinksByApp(ctx context.Context, appID string) (people, accounts int, err error) {
	row := db.read.QueryRowContext(ctx, `
SELECT COUNT(*), COUNT(DISTINCT account_id) FROM feishu_person_links WHERE feishu_app_id = ?`,
		strings.TrimSpace(appID))
	if err := row.Scan(&people, &accounts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("store: count feishu person links of %s: %w", appID, err)
	}
	return people, accounts, nil
}

// CountOrgNodesByFeishuApp reports how many org nodes one company brought in (the company node
// itself included). It answers the console's "this company has N nodes" line without loading
// the tree.
func (db *DB) CountOrgNodesByFeishuApp(ctx context.Context, appID string) (int, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return 0, nil
	}
	var count int
	if err := db.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM org_nodes WHERE feishu_app_id = ?", appID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count org nodes of %s: %w", appID, err)
	}
	return count, nil
}
