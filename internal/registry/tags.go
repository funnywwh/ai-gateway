package registry

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// ResolveTagRecords returns the effective, existing tag records for an account/key pair.
// Account names are considered before key names so equal-priority tags have deterministic
// ordering; the result is then sorted by priority for policy evaluation.
func ResolveTagRecords(snap *Snapshot, key *domain.APIKey) []*domain.Tag {
	if snap == nil || key == nil {
		return nil
	}
	names := make([]string, 0, 4)
	if account := snap.AccountByID[key.AccountID]; account != nil {
		names = appendTagNames(names, account.TagsJSON)
	}
	names = appendTagNames(names, key.TagsJSON)

	seen := make(map[string]struct{}, len(names))
	tags := make([]*domain.Tag, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if tag := snap.TagByName[name]; tag != nil {
			tags = append(tags, tag)
		}
	}
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
