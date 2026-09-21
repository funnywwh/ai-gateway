package store

import (
	"context"
	"fmt"

	"github.com/winger/ai-gateway/internal/domain"
)

// FeishuBindingMove describes one identity the backfill wrote onto an account. The caller turns
// each of these into an audit entry, so the backfill is attributable: "this upgrade moved the
// identity bound to key N onto account M" is answerable long after the fact.
type FeishuBindingMove struct {
	AccountID   int64
	AccountName string
	FromKeyID   int64
	OpenID      string
	Name        string
}

// FeishuMigrationOutcome reports what the M72 backfill of legacy key-level bindings did.
//
// The counts are what the caller logs: "migrated" is the number of accounts that gained an
// identity they did not have, and "conflicts" is the number of bound keys left untouched
// because the account they belong to is already bound to someone else (or because two keys of
// the same account disagree). Nothing is silently dropped: a conflicting row stays on the key,
// where the console can still see and unbind it.
type FeishuMigrationOutcome struct {
	// Moves lists the identities that landed on an account.
	Moves []FeishuBindingMove
	// Cleared lists key ids whose legacy binding was removed (either because it moved to the
	// account or because the account already had the same identity).
	Cleared []int64
	// Conflicts lists key ids whose binding was left in place.
	Conflicts []int64
}

// MigrateKeyFeishuToAccounts moves M60's key-level Feishu bindings onto their accounts (M72).
//
// Why a startup backfill rather than a schema migration: the move has to read rows, decide per
// account, and leave a conflict readable instead of aborting the upgrade — none of which SQL
// migration files in this repository do (they are pure DDL). Why migrate at all: from M72 the
// DSH portal resolves an identity through accounts.feishu_open_id only, so a deployment that
// upgraded without this step would refuse logins that worked the day before.
//
// The order per account is: bind the account (if it has no identity yet and no other key of
// the same account disagrees), then clear the key rows. Every migrated binding is written
// through BindAccountFeishu, so the unique index still guarantees one Feishu person per
// account; a violation is reported as a conflict rather than failing the upgrade.
//
// The migration is idempotent: it runs on every start, and after the first run there are no
// bound keys left to move.
func (db *DB) MigrateKeyFeishuToAccounts(ctx context.Context) (FeishuMigrationOutcome, error) {
	out := FeishuMigrationOutcome{}
	identities, err := db.ListAPIKeyFeishuIdentities(ctx)
	if err != nil {
		return out, err
	}
	if len(identities) == 0 {
		return out, nil
	}
	// Group by account, keeping key order stable so the surviving identity of an account whose
	// keys disagree is decided by the oldest key rather than by map iteration.
	order := []int64{}
	byAccount := map[int64][]domain.KeyFeishuIdentity{}
	for _, identity := range identities {
		if _, seen := byAccount[identity.AccountID]; !seen {
			order = append(order, identity.AccountID)
		}
		byAccount[identity.AccountID] = append(byAccount[identity.AccountID], identity)
	}

	for _, accountID := range order {
		group := byAccount[accountID]
		account, err := db.GetAccount(ctx, accountID)
		if err != nil {
			// A key whose account is gone: nothing to migrate to, and the key row keeps its
			// binding (it is unreachable through the console anyway).
			for _, identity := range group {
				out.Conflicts = append(out.Conflicts, identity.KeyID)
			}
			continue
		}
		winner, conflict := pickFeishuMigrationSource(group, account)
		if conflict {
			for _, identity := range group {
				out.Conflicts = append(out.Conflicts, identity.KeyID)
			}
			continue
		}
		if winner != nil {
			if err := db.BindAccountFeishu(ctx, account.ID, domain.FeishuBinding{
				OpenID:  winner.Binding.OpenID,
				UnionID: winner.Binding.UnionID,
				Name:    winner.Binding.Name,
				BoundAt: winner.Binding.BoundAt,
				BoundBy: "key-migration",
			}); err != nil {
				// A unique violation here means another account already holds this identity.
				// Leave the key row alone so nothing is lost.
				for _, identity := range group {
					out.Conflicts = append(out.Conflicts, identity.KeyID)
				}
				continue
			}
			out.Moves = append(out.Moves, FeishuBindingMove{
				AccountID: account.ID, AccountName: account.Name, FromKeyID: winner.KeyID,
				OpenID: winner.Binding.OpenID, Name: winner.Binding.Name,
			})
		}
		// The account now carries the identity (either just migrated or already the same one),
		// so the key rows have nothing left to contribute.
		for _, identity := range group {
			changed, err := db.UnbindAPIKeyFeishu(ctx, identity.KeyID)
			if err != nil {
				return out, fmt.Errorf("store: clear migrated key %d: %w", identity.KeyID, err)
			}
			if changed {
				out.Cleared = append(out.Cleared, identity.KeyID)
			}
		}
	}
	return out, nil
}

// pickFeishuMigrationSource decides which key binding (if any) becomes the account's identity.
//
// It returns (nil, false) when the account is already bound to exactly the same person: the key
// rows can be cleared without writing anything. (nil, true) is a conflict — the account is bound
// to someone else, or two keys of the account name different people — and the caller must leave
// every key row untouched, because guessing which person is the right one would be exactly the
// kind of silent identity change this milestone exists to avoid.
func pickFeishuMigrationSource(group []domain.KeyFeishuIdentity, account *domain.Account) (*domain.KeyFeishuIdentity, bool) {
	distinct := map[string]bool{}
	for i := range group {
		distinct[group[i].Binding.OpenID] = true
	}
	if account.FeishuOpenID != "" {
		if len(distinct) == 1 && distinct[account.FeishuOpenID] {
			return nil, false
		}
		return nil, true
	}
	if len(distinct) != 1 {
		return nil, true
	}
	return &group[0], false
}
