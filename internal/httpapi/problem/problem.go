// Package problem implements RFC 7807 problem details (application/problem+json).
//
// Three entry points:
//   - Write(c, err)           — maps a domain error to the right status, aborts the chain
//   - WriteValidation(c, err) — 400 for ShouldBindJSON / ShouldBindQuery failures
//   - Render(c, p)            — send a manually-constructed *Problem
//
// For 5xx errors, log the real cause before calling Write — it is never sent to the client.
package problem

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/go-playground/validator/v10"
)

// ContextKeyRequestID is the Gin context key set by the requestid middleware.
// Exported here so the middleware can reference it without a circular import.
const ContextKeyRequestID = "request_id"

// typeBase is the URI prefix for all problem type values.
// Relative by design — no canonical domain at build time.
const typeBase = "/problems/"

// ContentType is the media type required by RFC 7807 §3.
const ContentType = "application/problem+json"

// Problem is the RFC 7807 problem-details object.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id,omitempty"`
}

// Constructors — one per HTTP error class.

func NewNotFound(detail string) *Problem {
	return &Problem{Type: typeBase + "not-found", Title: "Resource Not Found", Status: http.StatusNotFound, Detail: detail}
}

func NewConflict(detail string) *Problem {
	return &Problem{Type: typeBase + "conflict", Title: "Resource Conflict", Status: http.StatusConflict, Detail: detail}
}

// NewVersionMismatch is for optimistic-concurrency failures (If-Match mismatch).
func NewVersionMismatch(detail string) *Problem {
	return &Problem{Type: typeBase + "version-mismatch", Title: "Precondition Failed", Status: http.StatusPreconditionFailed, Detail: detail}
}

func NewInvalidInput(detail string) *Problem {
	return &Problem{Type: typeBase + "invalid-input", Title: "Invalid Input", Status: http.StatusBadRequest, Detail: detail}
}

func NewUnauthorized(detail string) *Problem {
	return &Problem{Type: typeBase + "unauthorized", Title: "Unauthorized", Status: http.StatusUnauthorized, Detail: detail}
}

func NewForbidden(detail string) *Problem {
	return &Problem{Type: typeBase + "forbidden", Title: "Forbidden", Status: http.StatusForbidden, Detail: detail}
}

// NewTooManyRequests is for rate-limit rejections. Set Retry-After header separately.
func NewTooManyRequests(detail string) *Problem {
	return &Problem{Type: typeBase + "too-many-requests", Title: "Too Many Requests", Status: http.StatusTooManyRequests, Detail: detail}
}

// NewServiceUnavailable is used when a required component (e.g. flag snapshot) is not ready.
func NewServiceUnavailable(detail string) *Problem {
	return &Problem{Type: typeBase + "service-unavailable", Title: "Service Unavailable", Status: http.StatusServiceUnavailable, Detail: detail}
}

// NewInternal returns a generic 500. Never include the real cause in the detail.
func NewInternal() *Problem {
	return &Problem{Type: typeBase + "internal-error", Title: "Internal Server Error", Status: http.StatusInternalServerError, Detail: "An unexpected error occurred."}
}

// fromDomainErr maps a domain sentinel (or a wrapped one) to a Problem.
// Returns nil for unrecognised errors; caller falls back to NewInternal().
func fromDomainErr(err error) *Problem {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return NewNotFound(err.Error())
	case errors.Is(err, domain.ErrConflict):
		return NewConflict(err.Error())
	case errors.Is(err, domain.ErrVersionMismatch):
		return NewVersionMismatch(err.Error())
	case errors.Is(err, domain.ErrInvalidInput):
		return NewInvalidInput(err.Error())
	default:
		return nil
	}
}

// validationDetail formats go-playground/validator errors into one readable string per field.
// Falls back to err.Error() for non-ValidationErrors.
// fe.Field() returns the struct field name, not the JSON tag name.
func validationDetail(err error) string {
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return err.Error()
	}
	b := make([]byte, 0, 80*len(ve))
	for i, fe := range ve {
		if i > 0 {
			b = append(b, '\n')
		}
		if p := fe.Param(); p != "" {
			b = append(b, fmt.Sprintf("%s: failed '%s' validation (param: %s)", fe.Field(), fe.Tag(), p)...)
		} else {
			b = append(b, fmt.Sprintf("%s: failed '%s' validation", fe.Field(), fe.Tag())...)
		}
	}
	return string(b)
}
