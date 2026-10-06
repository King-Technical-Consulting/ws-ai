package store

import "github.com/google/uuid"

// NullUUID wraps a uuid as valid-when-non-nil.
func NullUUID(id uuid.UUID) uuid.NullUUID {
	return uuid.NullUUID{UUID: id, Valid: id != uuid.Nil}
}
