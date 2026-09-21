package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/winger/ai-gateway/internal/store"
)

// migrateKeyFeishuBindings runs the M72 backfill once at startup and reports what it did.
//
// The DSH portal resolves a Feishu identity through accounts.feishu_open_id from M72 on, so a
// deployment that upgraded with bindings still sitting on keys would refuse logins that worked
// the day before. The move itself lives in the store (it needs row reads and per-account
// decisions, which a SQL migration file cannot express); this function adds the two things the
// store cannot: the log line an operator reads after an upgrade, and one audit entry per moved
// identity, so "which key's identity landed on which account" is answerable later.
//
// Failures are logged rather than fatal: a backfill that could not run leaves the deployment in
// the old shape (key-level bindings intact, account-level logins missing) rather than refusing
// to start a gateway that is otherwise healthy.
func migrateKeyFeishuBindings(ctx context.Context, db *store.DB, log *slog.Logger) {
	outcome, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		log.Error("migrating legacy key-level Feishu bindings failed; they stay on their keys", "err", err)
		return
	}
	if len(outcome.Moves) == 0 && len(outcome.Cleared) == 0 && len(outcome.Conflicts) == 0 {
		return
	}
	for _, move := range outcome.Moves {
		changes, err := json.Marshal(map[string]any{
			"open_id":      move.OpenID,
			"name":         move.Name,
			"from_key_id":  move.FromKeyID,
			"migrated_at":  "startup",
			"account_name": move.AccountName,
		})
		if err != nil {
			log.Warn("encoding the audit entry of a migrated Feishu binding failed", "err", err, "account", move.AccountID)
			continue
		}
		if err := db.InsertAudit(ctx, &store.AuditEntry{
			Actor:  "startup",
			Action: "feishu_bind",
			// The account is the target because that is where the identity now lives; the key it
			// came from is in the changes payload, which is what makes the move reversible by
			// hand if an operator disagrees with it.
			TargetType:  "account",
			TargetID:    strconv.FormatInt(move.AccountID, 10),
			ChangesJSON: string(changes),
			Result:      "ok",
		}); err != nil {
			log.Warn("recording the migration of a Feishu binding failed", "err", err, "account", move.AccountID)
		}
	}
	log.Info("legacy key-level Feishu bindings migrated to accounts",
		"migrated", len(outcome.Moves),
		"keys_cleared", len(outcome.Cleared),
		"conflicts", len(outcome.Conflicts))
	if len(outcome.Conflicts) > 0 {
		// A conflict is a key whose binding could not move (usually because the account is bound
		// to someone else). It stays visible on the key row, so this is a "look at it when you
		// have time" warning, not a failure.
		log.Warn("some legacy key-level Feishu bindings were left in place",
			"keys", outcome.Conflicts,
			"hint", "the account is already bound to another Feishu identity; see the API Keys page")
	}
}
