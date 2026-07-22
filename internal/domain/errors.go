package domain

import "errors"

// Sentinel errors mapped to HTTP status by the handler layer:
// NotFound -> 404, Conflict -> 409, VersionMismatch -> 412, InvalidInput -> 400,
// Unavailable -> 503.
var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrVersionMismatch = errors.New("version mismatch")
	ErrInvalidInput    = errors.New("invalid input")

	// ErrUnavailable: the data needed to answer isn't loaded yet. Retryable,
	// and deliberately distinct from ErrNotFound — conflating them would
	// report every flag as off, with a 200, until the first load lands.
	ErrUnavailable = errors.New("temporarily unavailable")
)
