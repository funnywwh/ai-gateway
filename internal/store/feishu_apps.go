package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/funnywwh/ai-gateway/internal/domain"
)

// The console-managed company registry (M93). The store's job is small and mechanical: keep the
// row, refuse duplicate names and app ids at the database (two unique indexes), and never touch
// anything else. It never decrypts: the sealed secret travels as an opaque blob between the
// management layer and this table.
//
// Deleting a row deletes the registration only. Org nodes, memberships, person mappings and
// accounts keep their feishu_app_id values — that is the whole point of "delete the registration,
// keep the data", and it is why nothing here cascades.

const feishuAppCols = "id, name, app_id, secret_enc, root_node, note, enabled, created_by, updated_by, created_at, updated_at"

func scanFeishuApp(row rowScanner) (*domain.FeishuApp, error) {
	var (
		app                  domain.FeishuApp
		enabled              int
		createdAt, updatedAt int64
	)
	if err := row.Scan(&app.ID, &app.Name, &app.AppID, &app.SecretEnc, &app.RootNode, &app.Note,
		&enabled, &app.CreatedBy, &app.UpdatedBy, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	app.Enabled = enabled != 0
	app.CreatedAt = timeFromUnix(createdAt)
	app.UpdatedAt = timeFromUnix(updatedAt)
	return &app, nil
}

// ListFeishuApps returns every console-managed company, ordered by name so the console's table and
// every diff are stable.
func (db *DB) ListFeishuApps(ctx context.Context) ([]*domain.FeishuApp, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT "+feishuAppCols+" FROM feishu_apps ORDER BY name, id")
	if err != nil {
		return nil, fmt.Errorf("store: list feishu apps: %w", err)
	}
	defer rows.Close()

	out := []*domain.FeishuApp{}
	for rows.Next() {
		app, err := scanFeishuApp(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan feishu app: %w", err)
		}
		out = append(out, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate feishu apps: %w", err)
	}
	return out, nil
}

// GetFeishuApp loads one row by id.
func (db *DB) GetFeishuApp(ctx context.Context, id int64) (*domain.FeishuApp, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+feishuAppCols+" FROM feishu_apps WHERE id = ?", id)
	app, err := scanFeishuApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("feishu company " + strconv.FormatInt(id, 10))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get feishu app %d: %w", id, err)
	}
	return app, nil
}

// GetFeishuAppByAppID loads one row by its application id; a missing row is (nil, nil), because
// "this company is not registered here" is an ordinary answer while merging the company list.
func (db *DB) GetFeishuAppByAppID(ctx context.Context, appID string) (*domain.FeishuApp, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return nil, nil
	}
	row := db.read.QueryRowContext(ctx, "SELECT "+feishuAppCols+" FROM feishu_apps WHERE app_id = ?", appID)
	app, err := scanFeishuApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get feishu app %q: %w", appID, err)
	}
	return app, nil
}

// UpsertFeishuApp inserts or updates one company row (addressed by id; the caller resolves an
// existing row first). Name and app id are unique — a collision is a 409 the operator can act on,
// not a 500 from a raw constraint.
func (db *DB) UpsertFeishuApp(ctx context.Context, app *domain.FeishuApp) (int64, error) {
	if app == nil || strings.TrimSpace(app.Name) == "" || strings.TrimSpace(app.AppID) == "" {
		return 0, domain.ErrInvalidRequest("a company needs a name and an app id")
	}
	now := time.Now().UTC()
	if app.CreatedAt.IsZero() {
		app.CreatedAt = now
	}
	app.UpdatedAt = now

	if app.ID == 0 {
		res, err := db.write.ExecContext(ctx, `
INSERT INTO feishu_apps(name, app_id, secret_enc, root_node, note, enabled, created_by, updated_by, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
			app.Name, app.AppID, app.SecretEnc, app.RootNode, app.Note, boolInt(app.Enabled),
			app.CreatedBy, app.UpdatedBy, unix(app.CreatedAt), unix(app.UpdatedAt))
		if err != nil {
			if isUniqueViolation(err) {
				return 0, domain.ErrConflict(feishuAppConflict(app))
			}
			return 0, fmt.Errorf("store: create feishu app %q: %w", app.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("store: create feishu app %q: %w", app.Name, err)
		}
		app.ID = id
		return id, nil
	}

	res, err := db.write.ExecContext(ctx, `
UPDATE feishu_apps SET name = ?, app_id = ?, secret_enc = ?, root_node = ?, note = ?, enabled = ?,
  updated_by = ?, updated_at = ?
WHERE id = ?`,
		app.Name, app.AppID, app.SecretEnc, app.RootNode, app.Note, boolInt(app.Enabled),
		app.UpdatedBy, unix(app.UpdatedAt), app.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, domain.ErrConflict(feishuAppConflict(app))
		}
		return 0, fmt.Errorf("store: update feishu app %d: %w", app.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: update feishu app %d: %w", app.ID, err)
	}
	if affected == 0 {
		if _, err := db.GetFeishuApp(ctx, app.ID); err != nil {
			return 0, err
		}
	}
	return app.ID, nil
}

// feishuAppConflict explains which uniqueness rule was broken, so the operator knows whether to
// rename the company or fix the app id.
func feishuAppConflict(app *domain.FeishuApp) string {
	return "another company is already registered as 「" + app.Name + "」/" + app.AppID +
		"：公司名与 App ID 都必须唯一（company 参数接受其中之一）"
}

// SetFeishuAppEnabled flips the enabled flag without touching anything else.
func (db *DB) SetFeishuAppEnabled(ctx context.Context, id int64, enabled bool, by string) error {
	res, err := db.write.ExecContext(ctx,
		"UPDATE feishu_apps SET enabled = ?, updated_by = ?, updated_at = ? WHERE id = ?",
		boolInt(enabled), by, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: set feishu app %d enabled=%v: %w", id, enabled, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set feishu app %d enabled=%v: %w", id, enabled, err)
	}
	if affected == 0 {
		if _, err := db.GetFeishuApp(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteFeishuApp removes the registration (idempotent) and reports whether a row went.
func (db *DB) DeleteFeishuApp(ctx context.Context, id int64) (bool, error) {
	res, err := db.write.ExecContext(ctx, "DELETE FROM feishu_apps WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("store: delete feishu app %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete feishu app %d: %w", id, err)
	}
	return affected > 0, nil
}

// CountFeishuAppData counts what a company still owns: its org nodes (the company node included)
// and its person mappings. The delete dialog reports both so "delete the registration" is never
// mistaken for "delete the data".
func (db *DB) CountFeishuAppData(ctx context.Context, appID string) (nodes, links int, err error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return 0, 0, nil
	}
	if err := db.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM org_nodes WHERE feishu_app_id = ?", appID).Scan(&nodes); err != nil {
		return 0, 0, fmt.Errorf("store: count org nodes of %s: %w", appID, err)
	}
	if err := db.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM feishu_person_links WHERE feishu_app_id = ?", appID).Scan(&links); err != nil {
		return 0, 0, fmt.Errorf("store: count person links of %s: %w", appID, err)
	}
	return nodes, links, nil
}

// --- company name overrides (M94) ---------------------------------------------------------
//
// A company whose name comes from the configuration (the identity application, or a company in
// feishu.companies) has no row of its own, so a console rename is stored here instead. The override
// wins over the configuration and loses to a console row's own name; deleting it returns the
// company to the configured name.

// ListFeishuCompanyNames returns every override, keyed by app id.
func (db *DB) ListFeishuCompanyNames(ctx context.Context) (map[string]string, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT app_id, name FROM feishu_company_names")
	if err != nil {
		return nil, fmt.Errorf("store: list feishu company names: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var appID, name string
		if err := rows.Scan(&appID, &name); err != nil {
			return nil, fmt.Errorf("store: scan feishu company name: %w", err)
		}
		out[appID] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate feishu company names: %w", err)
	}
	return out, nil
}

// SetFeishuCompanyName writes (or replaces) one override.
func (db *DB) SetFeishuCompanyName(ctx context.Context, appID, name, by string) error {
	appID = strings.TrimSpace(appID)
	name = strings.TrimSpace(name)
	if appID == "" || name == "" {
		return domain.ErrInvalidRequest("a company name override needs an app id and a name")
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO feishu_company_names(app_id, name, updated_by, updated_at) VALUES(?,?,?,?)
ON CONFLICT(app_id) DO UPDATE SET name = excluded.name, updated_by = excluded.updated_by,
  updated_at = excluded.updated_at`,
		appID, name, by, unix(time.Now())); err != nil {
		return fmt.Errorf("store: set feishu company name for %s: %w", appID, err)
	}
	return nil
}

// DeleteFeishuCompanyName drops one override and reports whether anything changed, so "back to the
// configured name" is idempotent.
func (db *DB) DeleteFeishuCompanyName(ctx context.Context, appID string) (bool, error) {
	res, err := db.write.ExecContext(ctx,
		"DELETE FROM feishu_company_names WHERE app_id = ?", strings.TrimSpace(appID))
	if err != nil {
		return false, fmt.Errorf("store: delete feishu company name for %s: %w", appID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete feishu company name for %s: %w", appID, err)
	}
	return affected > 0, nil
}
