package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/evaluation"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

// EvalHandler serves both /evaluate/:key and /api/v1/evaluate/:key.
type EvalHandler struct {
	src evaluation.FlagSource
}

func NewEvalHandler(src evaluation.FlagSource) *EvalHandler {
	return &EvalHandler{src: src}
}

// Register mounts the evaluate endpoint on two route groups:
//   - root  → /evaluate/:key          (spec-literal path)
//   - v1    → /api/v1/evaluate/:key   (canonical API path)
//
// Both groups point to the same handler method. Documented in the README as
// intentional — the spec says /evaluate, the API convention says /api/v1.
func (h *EvalHandler) Register(root, v1 *gin.RouterGroup) {
	root.GET("/evaluate/:key", h.Evaluate)
	v1.GET("/evaluate/:key", h.Evaluate)
}

// evalResponse is the JSON body for every evaluate response.
// Always 200 — even FLAG_NOT_FOUND — so a typo never throws an exception in
// the caller's HTTP client. Use ?strict=true to opt into 404 semantics.
type evalResponse struct {
	Enabled     bool      `json:"enabled"`
	Reason      string    `json:"reason"`
	FlagKey     string    `json:"flag_key"`
	Environment string    `json:"environment"`
	Subject     string    `json:"subject"` // echoed back so degenerate case is self-evident
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// Evaluate handles GET /evaluate/:key and GET /api/v1/evaluate/:key.
//
// Query params:
//   - env      (required) — the environment to evaluate in
//   - subject  (optional) — the identity to hash on; resolved via chain below
//   - strict   (optional) — if "true", returns 404 for FLAG_NOT_FOUND instead of 200
func (h *EvalHandler) Evaluate(c *gin.Context) {
	flagKey := c.Param("key")
	env := c.Query("env")
	if env == "" {
		problem.Render(c, problem.NewInvalidInput("env query parameter is required"))
		return
	}

	subject := resolveSubject(c)
	strict := c.Query("strict") == "true"

	ctx, cancel := withTimeout(c)
	defer cancel()

	result, err := evaluation.Decide(ctx, h.src, flagKey, env, subject)
	if err != nil {
		problem.Write(c, err)
		return
	}

	// Strict mode: return 404 for unknown flags instead of the default 200+reason.
	if strict && result.Reason == evaluation.ReasonFlagNotFound {
		problem.Render(c, problem.NewNotFound(
			fmt.Sprintf("flag '%s' not found in environment '%s'", flagKey, env),
		))
		return
	}

	c.JSON(http.StatusOK, evalResponse{
		Enabled:     result.Enabled,
		Reason:      string(result.Reason),
		FlagKey:     flagKey,
		Environment: env,
		Subject:     subject,
		EvaluatedAt: time.Now().UTC(),
	})
}

// resolveSubject picks the subject identity to hash on, in priority order:
//  1. ?subject= query param
//  2. X-Subject-ID header
//  3. actor_key_id from Gin context (set by auth middleware in Phase 4)
//  4. "" — degenerate fallback: every caller without a subject hashes identically,
//     so a 50% rollout becomes 0% or 100% for all unauthenticated traffic.
//     The empty string is echoed in the response so the degenerate case is visible.
func resolveSubject(c *gin.Context) string {
	if s := c.Query("subject"); s != "" {
		return s
	}
	if s := c.GetHeader("X-Subject-ID"); s != "" {
		return s
	}
	if v, ok := c.Get("actor_key_id"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}
