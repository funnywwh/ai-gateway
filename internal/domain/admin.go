package domain

import "time"

// AdminUser is an administrator of the management API.
//
// There used to be exactly one, seeded from the configuration. There are now as many as an
// operator creates: Role says what the row may do ("admin" or "viewer"), Status whether it
// may sign in at all ("pending" for an account with no usable credential yet, "active", or
// "disabled"), and the Feishu fields carry the identity that lets its owner sign in by
// scanning a code with Feishu instead of typing a password (M66).
type AdminUser struct {
	ID       int64
	Username string
	// PasswordHash is empty for an invitation-only administrator. The column is NOT NULL,
	// and an empty value means no password can ever match (see the migration 0023 comment).
	PasswordHash string
	Role         string
	Status       string
	CreatedAt    time.Time
	LastLoginAt  *time.Time

	FeishuOpenID  string
	FeishuUnionID string
	FeishuName    string
	FeishuBoundAt *time.Time
	FeishuBoundBy string
	// InviteNonce is the handle of the newest unredeemed invitation link. It never leaves
	// the server: the console only learns whether one is outstanding, and the callback
	// compares it to revoke a link that an operator regenerated or an account that was
	// disabled.
	InviteNonce string
}

// Administrator roles. Anything that is not RoleAdmin is read-only, which mirrors
// admin.RequireRole: an unknown value must never grant write access.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Administrator lifecycle states.
const (
	// AdminPending is an account with no usable credential yet: no password and no bound
	// Feishu identity. It exists so an invitation can be sent to somebody who has not
	// signed in even once.
	AdminPending = "pending"
	// AdminActive may sign in with whichever credentials it has.
	AdminActive = "active"
	// AdminDisabled was stopped by an operator: no sign-in method works, and existing
	// sessions stop being accepted.
	AdminDisabled = "disabled"
)

// ValidAdminRole reports whether a role may be stored. The console offers exactly these two.
func ValidAdminRole(role string) bool { return role == RoleAdmin || role == RoleViewer }

// ValidAdminStatus reports whether a lifecycle state may be stored.
func ValidAdminStatus(status string) bool {
	return status == AdminPending || status == AdminActive || status == AdminDisabled
}
