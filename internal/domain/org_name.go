package domain

// MaxOrgNodeNameRunes is the maximum number of Unicode code points in an organization node
// name. It matches the account and tag limits on purpose: the three are the human-facing
// labels of this system and the console shows them side by side.
const MaxOrgNodeNameRunes = 64

// NormalizeOrgNodeName trims surrounding Unicode whitespace and validates an organization
// node name.
//
// A node name is a human-facing label, not an identifier: it may be Chinese, contain
// punctuation, and — unlike a tag name — it may be changed at any time, because a node is
// addressed by its numeric id everywhere (accounts reference it through org_node_accounts,
// not by name). The shared normalizeLabel rule keeps the three label kinds from drifting
// apart, which is what once left tag rows permanently uneditable.
func NormalizeOrgNodeName(name string) (string, error) {
	return normalizeLabel("org node name", name, MaxOrgNodeNameRunes)
}
