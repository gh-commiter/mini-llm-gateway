package core

import (
	"errors"
	"fmt"
	"time"
)

// Errors of the policy layer; the HTTP layer maps them one to one (section 6 of the contracts).
var (
	ErrHardLinked      = errors.New("a core file has more than one name") // the core refuses to open (#106)
	ErrNoProjectAccess = errors.New("no project access")                  // 403 no_project_access; wrapped detail is for the log only
	ErrNotFound        = errors.New("not found")                          // 404 (missing or not visible)

	// Reasons of ErrNoProjectAccess that the member route names to the caller (with the owner's command): the
	// device is not on the gateway's approved list, or it is but no user is bound to it.
	ErrDeviceNotListed = errors.New("device not on the gateway's device list")
	ErrDeviceNotBound  = errors.New("device not bound to a user")
	ErrForbidden       = errors.New("forbidden") // 403 forbidden
	// ErrNoSubmit is an ErrForbidden that says the caller's role allows no hand-in at all in the project (a reader), as against a note or a class
	// it may not write: 403 no_submit_access, so that a pusher stops at the first instead of trying every note.
	ErrNoSubmit    = fmt.Errorf("no right to hand in: %w", ErrForbidden)
	ErrArchived    = errors.New("project is archived")
	ErrIdempotency = errors.New("idempotency key used with another payload") // 409 idempotency_conflict
	ErrTooLarge    = errors.New("too large")                                 // 413
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

// ErrRefusal is a refusal that already is a sentence ("this bundle was exported by this gateway: ..."): it reads as it is,
// without the "invalid " that ErrInvalid puts before a noun phrase. It is a bad request (400) like ErrInvalid.
type ErrRefusal struct{ Msg string }

func (e ErrRefusal) Error() string { return e.Msg }
func (e ErrRefusal) Unwrap() error { return ErrInvalid{e.Msg} }

func noAccess(reason string) error { return fmt.Errorf("%w: %s", ErrNoProjectAccess, reason) }

// noAccessErr: a no-project-access error whose reason the member route can name (errors.Is), not only log.
func noAccessErr(reason error) error { return fmt.Errorf("%w: %w", ErrNoProjectAccess, reason) }
