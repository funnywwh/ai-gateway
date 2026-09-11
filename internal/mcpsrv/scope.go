package mcpsrv

// Token scopes decide what an MCP credential may do. A token is issued with one
// scope and never gains capability implicitly: the default is the historical
// read-only query scope, so existing tokens keep working exactly as before.
const (
	// ScopeQuery can only read the token's own account through the query tools.
	ScopeQuery = "query"
	// ScopeAdminRead adds the administrative tool surface with a read-only role:
	// every write endpoint answers 403.
	ScopeAdminRead = "admin_read"
	// ScopeAdmin may execute every administrative endpoint, including destructive
	// ones. It is equivalent to an administrator credential.
	ScopeAdmin = "admin"
)

// ValidScope reports whether scope is a scope this server understands.
func ValidScope(scope string) bool {
	switch scope {
	case ScopeQuery, ScopeAdminRead, ScopeAdmin:
		return true
	default:
		return false
	}
}

// NormalizeScope maps unknown or empty values onto the read-only default so a
// row written by an older version never escalates by accident.
func NormalizeScope(scope string) string {
	if ValidScope(scope) {
		return scope
	}
	return ScopeQuery
}

// AllowsAdminTools reports whether the scope may see and call the administrative
// tool surface at all.
func AllowsAdminTools(scope string) bool {
	switch NormalizeScope(scope) {
	case ScopeAdminRead, ScopeAdmin:
		return true
	default:
		return false
	}
}

// AdminRole maps a scope onto the management role the synthetic principal gets.
// An empty role means "no administrative access at all".
func AdminRole(scope string) string {
	switch NormalizeScope(scope) {
	case ScopeAdmin:
		return "admin"
	case ScopeAdminRead:
		return "viewer"
	default:
		return ""
	}
}
