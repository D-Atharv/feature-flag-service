// Package middleware contains the hand-rolled Gin middleware chain.
// Middleware order (enforced in main.go):
//   RequestID → Recovery → Logger → Metrics → BodyLimit → Timeout → Auth → handler
package middleware

// Context keys stored in the Gin context by this middleware chain.
// Using typed string constants avoids collision with other packages.
const (
	// ContextKeyRequestID is set by RequestID middleware.
	// Same value as problem.ContextKeyRequestID — both packages use this string.
	// We duplicate it to avoid a circular import (middleware imports problem;
	// problem must not import middleware).
	ContextKeyRequestID = "request_id"

	// ContextKeyActorID is set by Auth middleware — the API key's UUID.
	// Matches the literal used in handlers/helpers.go actorID().
	ContextKeyActorID = "actor_key_id"

	// ContextKeyAPIKey is set by Auth middleware — the full domain.APIKey.
	// Consumed by the rate limiter (Phase 5) to get per-key RPS/burst limits.
	ContextKeyAPIKey = "api_key"
)
