package registry

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// ResolveTagRecords returns the effective, existing tag records for an account/key pair.
//
// The order is the contract: organization-inherited names first (each node's ancestors before
// the node itself, member nodes in id order), then the account's own names, then the key's.
// Account-side names are considered before key names so equal-priority tags have deterministic
// ordering; the result is then sorted by priority for policy evaluation.
//
// PERFORMANCE. This runs on every authenticated request — the model list, the quota limits and
// the route plan each ask for the same pair — so the account side is resolved when the
// snapshot is built (Snapshot.accountTagRecords) rather than here. When the key carries no
// tags of its own, the precomputed slice is returned as-is: no parsing, no allocation, no
// sorting, and in particular no walk up the organization tree.
//
// The returned slice is READ-ONLY: on that fast path it is the snapshot's own slice. Callers
// range over it; none may mutate or append to it.
func ResolveTagRecords(snap *Snapshot, key *domain.APIKey) []*domain.Tag {
	if snap == nil || key == nil {
		return nil
	}
	accountRecords := snap.accountTagRecords[key.AccountID]
	if strings.TrimSpace(key.TagsJSON) == "" {
		// The common case in a deployment that uses tags or organizations: the account side
		// is the whole answer (and empty for an account that carries neither).
		return accountRecords
	}
	var keyNames []string
	if err := json.Unmarshal([]byte(key.TagsJSON), &keyNames); err != nil {
		// A malformed tag list on a key is tolerated exactly as it always was: it contributes
		// nothing rather than failing the request.
		return accountRecords
	}
	accountSeen := snap.accountTagNames[key.AccountID]

	// Filter the names in place (the standard keep-idiom: kept never advances past the read
	// index) so that a list which contributes nothing — every name already inherited, or
	// unknown — returns the precomputed slice without building a result or sorting it. The
	// local set is only allocated to collapse duplicates within the key's own list.
	var localSeen map[string]struct{}
	if len(keyNames) > 1 {
		localSeen = make(map[string]struct{}, len(keyNames))
	}
	kept := keyNames[:0]
	for _, raw := range keyNames {
		name := strings.TrimSpace(raw)
		if name == "" || snap.TagByName[name] == nil {
			continue
		}
		if _, repeat := accountSeen[name]; repeat {
			// The account side already contributed this name; adding it twice would merge its
			// policy twice and move it in the evaluation order.
			continue
		}
		if localSeen != nil {
			if _, repeat := localSeen[name]; repeat {
				continue
			}
			localSeen[name] = struct{}{}
		}
		kept = append(kept, name)
	}
	if len(kept) == 0 {
		return accountRecords
	}

	// A fresh slice, never an append onto accountRecords: that slice belongs to the snapshot
	// and another request may be reading it right now.
	tags := make([]*domain.Tag, 0, len(accountRecords)+len(kept))
	tags = append(tags, accountRecords...)
	for _, name := range kept {
		tags = append(tags, snap.TagByName[name])
	}
	// The account-side records are already in priority order, so a stable sort keeps them
	// ahead of the key's tags at equal priority.
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].Priority < tags[j].Priority })
	return tags
}

// ResolveTagNames returns effective existing tag names in policy evaluation order.
func ResolveTagNames(snap *Snapshot, key *domain.APIKey) []string {
	tags := ResolveTagRecords(snap, key)
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, tag.Name)
	}
	return out
}

func appendTagNames(dst []string, raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return dst
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return dst
	}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			dst = append(dst, name)
		}
	}
	return dst
}
