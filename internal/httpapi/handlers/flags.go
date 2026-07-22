// Package handlers contains thin Gin handlers: decode → call store → encode.
// Handlers never set status codes directly; all errors go through problem.Write.
package handlers

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

const defaultPageLimit = 20
const maxPageLimit = 100
const handlerTimeout = 10 * time.Second

// keyPattern mirrors the CHECK constraint in the flags table.
var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// FlagStore is the data-access interface required by FlagHandler.
// The real implementation is *store.FlagRepo; tests inject a fake.
type FlagStore interface {
	Create(ctx context.Context, f domain.Flag, actorKeyID string) (domain.Flag, error)
	GetByKeyEnv(ctx context.Context, key, env string) (domain.Flag, error)
	ListByKey(ctx context.Context, key string) ([]domain.Flag, error)
	List(ctx context.Context, env, afterKey, afterEnv string, limit int) ([]domain.Flag, error)
	Update(ctx context.Context, key, env string, ver int, enabled *bool, rollout *int, actorKeyID string) (domain.Flag, error)
	Delete(ctx context.Context, key, env, actorKeyID string) error
}

// FlagHandler holds the dependencies needed by all flag endpoints.
type FlagHandler struct {
	repo FlagStore
}

func NewFlagHandler(repo FlagStore) *FlagHandler {
	return &FlagHandler{repo: repo}
}

// Register mounts all five flag routes onto the given RouterGroup.
func (h *FlagHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/flags", h.Create)
	rg.GET("/flags", h.List)
	rg.GET("/flags/:key", h.Get)
	rg.PATCH("/flags/:key", h.Update)
	rg.DELETE("/flags/:key", h.Delete)
}

// ---- request / response types ----

type createRequest struct {
	Key               string `json:"key"`
	Environment       string `json:"environment"`
	Enabled           bool   `json:"enabled"`
	RolloutPercentage int    `json:"rollout_percentage"`
}

type updateRequest struct {
	Enabled           *bool `json:"enabled"`
	RolloutPercentage *int  `json:"rollout_percentage"`
}

type flagResponse struct {
	ID                string    `json:"id"`
	Key               string    `json:"key"`
	Environment       string    `json:"environment"`
	Enabled           bool      `json:"enabled"`
	RolloutPercentage int       `json:"rollout_percentage"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type listResponse struct {
	Data       []flagResponse `json:"data"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

func toResponse(f domain.Flag) flagResponse {
	return flagResponse{
		ID:                f.ID,
		Key:               f.Key,
		Environment:       f.Environment,
		Enabled:           f.Enabled,
		RolloutPercentage: f.RolloutPercentage,
		Version:           f.Version,
		CreatedAt:         f.CreatedAt,
		UpdatedAt:         f.UpdatedAt,
	}
}

// ---- helpers ----

// actorID extracts the API key ID stored in the Gin context by auth middleware.
// Returns "" when auth middleware hasn't run (e.g. tests without the full chain).
func actorID(c *gin.Context) string {
	v, _ := c.Get("actor_key_id")
	s, _ := v.(string)
	return s
}

func withTimeout(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), handlerTimeout)
}

// validateKey checks the key against the DB constraint pattern.
func validateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("key must match ^[a-z0-9][a-z0-9_-]{0,63}$")
	}
	return nil
}

// validateRollout checks rollout_percentage is in [0, 100].
func validateRollout(p int) error {
	if p < 0 || p > 100 {
		return fmt.Errorf("rollout_percentage must be between 0 and 100")
	}
	return nil
}

// ---- handlers ----

// Create handles POST /api/v1/flags.
// Returns 201 on success, 409 on duplicate (key, environment), 400 on bad input.
func (h *FlagHandler) Create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteValidation(c, err)
		return
	}

	if req.Key == "" || req.Environment == "" {
		problem.Render(c, problem.NewInvalidInput("key and environment are required"))
		return
	}
	if err := validateKey(req.Key); err != nil {
		problem.Render(c, problem.NewInvalidInput(err.Error()))
		return
	}
	if err := validateRollout(req.RolloutPercentage); err != nil {
		problem.Render(c, problem.NewInvalidInput(err.Error()))
		return
	}

	ctx, cancel := withTimeout(c)
	defer cancel()

	f, err := h.repo.Create(ctx, domain.Flag{
		Key:               req.Key,
		Environment:       req.Environment,
		Enabled:           req.Enabled,
		RolloutPercentage: req.RolloutPercentage,
	}, actorID(c))
	if err != nil {
		problem.Write(c, err)
		return
	}

	c.JSON(http.StatusCreated, toResponse(f))
}

// List handles GET /api/v1/flags.
// Supports ?environment=, ?enabled=, ?limit=, ?cursor= (keyset pagination).
func (h *FlagHandler) List(c *gin.Context) {
	env := c.Query("environment")

	limit := defaultPageLimit
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxPageLimit {
			problem.Render(c, problem.NewInvalidInput(fmt.Sprintf("limit must be an integer between 1 and %d", maxPageLimit)))
			return
		}
		limit = n
	}

	// cursor encodes "afterKey|afterEnv" from the last item of the previous page.
	// environment names cannot contain "|" (DB CHECK constraint), so the format is unambiguous.
	afterKey, afterEnv := parseCursor(c.Query("cursor"))

	ctx, cancel := withTimeout(c)
	defer cancel()

	// Fetch one extra to determine whether a next page exists.
	flags, err := h.repo.List(ctx, env, afterKey, afterEnv, limit+1)
	if err != nil {
		problem.Write(c, err)
		return
	}

	var nextCursor string
	if len(flags) > limit {
		nextCursor = buildCursor(flags[limit-1].Key, flags[limit-1].Environment)
		flags = flags[:limit]
	}

	items := make([]flagResponse, len(flags))
	for i, f := range flags {
		items[i] = toResponse(f)
	}
	c.JSON(http.StatusOK, listResponse{Data: items, NextCursor: nextCursor})
}

// Get handles GET /api/v1/flags/:key.
// With ?environment=: returns the single flag for that (key, env).
// Without ?environment=: returns all environments for that key.
func (h *FlagHandler) Get(c *gin.Context) {
	key := c.Param("key")
	env := c.Query("environment")

	ctx, cancel := withTimeout(c)
	defer cancel()

	if env != "" {
		f, err := h.repo.GetByKeyEnv(ctx, key, env)
		if err != nil {
			problem.Write(c, err)
			return
		}
		c.JSON(http.StatusOK, toResponse(f))
		return
	}

	// No environment — return all envs for this key using a dedicated store query
	// so we never silently drop results beyond the List page size.
	flags, err := h.repo.ListByKey(ctx, key)
	if err != nil {
		problem.Write(c, err)
		return
	}
	if len(flags) == 0 {
		problem.Render(c, problem.NewNotFound(fmt.Sprintf("flag '%s' not found in any environment", key)))
		return
	}

	items := make([]flagResponse, len(flags))
	for i, f := range flags {
		items[i] = toResponse(f)
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

// Update handles PATCH /api/v1/flags/:key?environment=.
// Requires If-Match header with the current version; returns 412 on mismatch.
// Accepts a partial body — only the fields present are updated.
func (h *FlagHandler) Update(c *gin.Context) {
	key := c.Param("key")
	env := c.Query("environment")
	if env == "" {
		problem.Render(c, problem.NewInvalidInput("environment query parameter is required"))
		return
	}

	// If-Match: "3"  →  strip quotes  →  parse int
	ifMatch := c.GetHeader("If-Match")
	if ifMatch == "" {
		problem.Render(c, problem.NewInvalidInput(`If-Match header is required (e.g. If-Match: "3")`))
		return
	}
	expectedVersion, err := strconv.Atoi(stripQuotes(ifMatch))
	if err != nil {
		problem.Render(c, problem.NewInvalidInput(`If-Match must be an integer version (e.g. If-Match: "3")`))
		return
	}

	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteValidation(c, err)
		return
	}
	if req.Enabled == nil && req.RolloutPercentage == nil {
		problem.Render(c, problem.NewInvalidInput("request body must include at least one of: enabled, rollout_percentage"))
		return
	}
	if req.RolloutPercentage != nil {
		if err := validateRollout(*req.RolloutPercentage); err != nil {
			problem.Render(c, problem.NewInvalidInput(err.Error()))
			return
		}
	}

	ctx, cancel := withTimeout(c)
	defer cancel()

	// Pass nil pointers for unchanged fields; the store handles the merge.
	updated, err := h.repo.Update(ctx, key, env, expectedVersion, req.Enabled, req.RolloutPercentage, actorID(c))
	if err != nil {
		problem.Write(c, err)
		return
	}

	c.JSON(http.StatusOK, toResponse(updated))
}

// Delete handles DELETE /api/v1/flags/:key?environment=.
// Returns 204 on success; writes an audit row atomically.
func (h *FlagHandler) Delete(c *gin.Context) {
	key := c.Param("key")
	env := c.Query("environment")
	if env == "" {
		problem.Render(c, problem.NewInvalidInput("environment query parameter is required"))
		return
	}

	ctx, cancel := withTimeout(c)
	defer cancel()

	if err := h.repo.Delete(ctx, key, env, actorID(c)); err != nil {
		problem.Write(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// ---- cursor helpers ----

// buildCursor encodes the last item of a page as an opaque cursor string.
// Format: "<key>|<environment>" — safe because neither field can contain "|".
func buildCursor(key, env string) string {
	return key + "|" + env
}

// parseCursor decodes a cursor into (afterKey, afterEnv).
// Returns ("", "") for an empty or malformed cursor — selects the first page.
func parseCursor(cursor string) (afterKey, afterEnv string) {
	for i, ch := range cursor {
		if ch == '|' {
			return cursor[:i], cursor[i+1:]
		}
	}
	return "", ""
}

// stripQuotes removes one level of surrounding double-quotes from s.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
