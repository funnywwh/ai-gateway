package domain

// MaxTagNameRunes is the maximum number of Unicode code points in a tag name.
const MaxTagNameRunes = 64

// NormalizeTagName trims surrounding Unicode whitespace and validates a tag name.
//
// A tag name is a human-facing label, exactly like an account name (NormalizeAccountName):
// any valid Unicode is accepted, and only blank, malformed UTF-8 and overlong values are
// refused. The ASCII-identifier rule (`[A-Za-z0-9._-]`) that used to guard this write path
// bought nothing — a tag name is compared as an exact string and never becomes a path
// segment, host name or file name — while rows that already carried such a name became
// permanently uneditable, because the only write path was an upsert keyed by that very name.
func NormalizeTagName(name string) (string, error) {
	return normalizeLabel("tag name", name, MaxTagNameRunes)
}
