package domain

import "time"

// AdminUser is an administrator of the management API.
type AdminUser struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}
