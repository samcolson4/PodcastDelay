package store

import (
	"database/sql"
	"time"
)

// Times are stored as RFC3339Nano text rather than relying on driver
// magic to round-trip time.Time, so behaviour doesn't depend on which
// SQLite driver is in use.
const timeLayout = time.RFC3339Nano

func toDBTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func toDBTimePtr(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: toDBTime(*t), Valid: true}
}

func fromDBTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

func fromDBTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := fromDBTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func stringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func nullInt64(i *int64) sql.NullInt64 {
	if i == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *i, Valid: true}
}

func int64Ptr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64
	return &v
}

func nullIntFromIntPtr(i *int) sql.NullInt64 {
	if i == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*i), Valid: true}
}

func intPtrFromNullInt(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullBoolFromPtr(b *bool) sql.NullInt64 {
	if b == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(boolToInt(*b)), Valid: true}
}

func boolPtrFromNullInt(ni sql.NullInt64) *bool {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64 != 0
	return &v
}
