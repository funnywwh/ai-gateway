package store

import (
	"database/sql"
	"time"
)

// Time is stored as UTC unix seconds; 0 means "not set".
func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}

func timeFromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func unixPtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Unix()
}

func timePtrFromNull(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// int64PtrNull is its inverse: a nil pointer binds as SQL NULL. A zero value is treated as
// NULL too, because no row in this schema uses 0 to mean "row id zero" — an unset reference
// and a null one are the same absence.
func int64PtrNull(v *int64) any {
	if v == nil || *v == 0 {
		return nil
	}
	return *v
}
