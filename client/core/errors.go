package core

import (
	"errors"
	"fmt"
	"time"
)

// Errors of the policy layer; the HTTP layer maps them one to one (section 6 of the contracts).
var (
	ErrNoProjectAccess = errors.New("no project access") // 403 no_project_access; wrapped detail is for the log only
	ErrNotFound        = errors.New("not found")         // 404 (missing or not visible)
	ErrForbidden       = errors.New("forbidden")         // 403 forbidden
	ErrArchived        = errors.New("project is archived")
	ErrIdempotency     = errors.New("idempotency key used with another payload") // 409 idempotency_conflict
	ErrTooLarge        = errors.New("too large")                                 // 413
)

// ErrRevision (409 revision_mismatch) is returned only to a caller who may change the note.
type ErrRevision struct{ Current int64 }

func (e ErrRevision) Error() string { return fmt.Sprintf("revision mismatch (current %d)", e.Current) }

// ErrLimit (429): RetryAfter is the time until the rate window ends.
type ErrLimit struct{ RetryAfter time.Duration }

func (e ErrLimit) Error() string {
	return fmt.Sprintf("rate limit (retry after %s)", e.RetryAfter.Round(time.Second))
}

// ErrInvalid (400): What names the field.
type ErrInvalid struct{ What string }

func (e ErrInvalid) Error() string { return "invalid " + e.What }

func noAccess(reason string) error { return fmt.Errorf("%w: %s", ErrNoProjectAccess, reason) }
