package domain

import "errors"

// Sentinel errors mapped to HTTP status by the handler layer:
// NotFound -> 404, Conflict -> 409, VersionMismatch -> 412, InvalidInput -> 400.
var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrVersionMismatch = errors.New("version mismatch")
	ErrInvalidInput    = errors.New("invalid input")
)
