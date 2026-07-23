package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/handlers"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

func init() { gin.SetMode(gin.TestMode) }

// ---- fake repo ----

type fakeRepo struct {
	flags  map[string]domain.Flag // "key|env"
	nextID int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{flags: make(map[string]domain.Flag), nextID: 1}
}

func (r *fakeRepo) mk(key, env string) string { return key + "|" + env }

func (r *fakeRepo) Create(_ context.Context, f domain.Flag, _ string) (domain.Flag, error) {
	k := r.mk(f.Key, f.Environment)
	if _, ok := r.flags[k]; ok {
		return domain.Flag{}, fmt.Errorf("create: %w", domain.ErrConflict)
	}
	f.ID = fmt.Sprintf("id-%d", r.nextID)
	r.nextID++
	f.Version = 1
	f.CreatedAt = time.Now()
	f.UpdatedAt = time.Now()
	r.flags[k] = f
	return f, nil
}

func (r *fakeRepo) GetByKeyEnv(_ context.Context, key, env string) (domain.Flag, error) {
	f, ok := r.flags[r.mk(key, env)]
	if !ok {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, env, domain.ErrNotFound)
	}
	return f, nil
}

func (r *fakeRepo) ListByKey(_ context.Context, key string) ([]domain.Flag, error) {
	var out []domain.Flag
	for _, f := range r.flags {
		if f.Key == key {
			out = append(out, f)
		}
	}
	return out, nil
}

func (r *fakeRepo) List(_ context.Context, env, _, _ string, limit int) ([]domain.Flag, error) {
	var out []domain.Flag
	for _, f := range r.flags {
		if env == "" || f.Environment == env {
			out = append(out, f)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRepo) Update(_ context.Context, key, env string, ver int, enabled *bool, rollout *int, _ string) (domain.Flag, error) {
	k := r.mk(key, env)
	f, ok := r.flags[k]
	if !ok {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, env, domain.ErrNotFound)
	}
	if f.Version != ver {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, env, domain.ErrVersionMismatch)
	}
	if enabled != nil {
		f.Enabled = *enabled
	}
	if rollout != nil {
		f.RolloutPercentage = *rollout
	}
	f.Version++
	f.UpdatedAt = time.Now()
	r.flags[k] = f
	return f, nil
}

func (r *fakeRepo) Delete(_ context.Context, key, env, _ string) error {
	k := r.mk(key, env)
	if _, ok := r.flags[k]; !ok {
		return fmt.Errorf("flag %s/%s: %w", key, env, domain.ErrNotFound)
	}
	delete(r.flags, k)
	return nil
}

// ---- test router ----

func newTestRouter(repo handlers.FlagStore) *gin.Engine {
	r := gin.New()
	v1 := r.Group("/api/v1")
	handlers.NewFlagHandler(repo).Register(v1)
	return r
}

// ---- http helpers ----

func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return bytes.NewBuffer(b)
}

func do(router *gin.Engine, method, path string, body *bytes.Buffer, headers map[string]string) *httptest.ResponseRecorder {
	if body == nil {
		body = &bytes.Buffer{}
	}
	req, _ := http.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&m))
	return m
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) problem.Problem {
	t.Helper()
	var p problem.Problem
	require.NoError(t, json.NewDecoder(w.Body).Decode(&p))
	return p
}

// ---- POST /api/v1/flags ----

func TestCreateFlag_HappyPath(t *testing.T) {
	r := newTestRouter(newFakeRepo())
	w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{
		"key": "dark-mode", "environment": "prod", "enabled": true, "rollout_percentage": 50,
	}), nil)

	assert.Equal(t, http.StatusCreated, w.Code)
	body := decodeMap(t, w)
	assert.Equal(t, "dark-mode", body["key"])
	assert.Equal(t, "prod", body["environment"])
	assert.Equal(t, float64(1), body["version"])
}

func TestCreateFlag_DuplicateReturns409(t *testing.T) {
	r := newTestRouter(newFakeRepo())
	do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{"key": "x", "environment": "dev"}), nil)
	w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{"key": "x", "environment": "dev"}), nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	assert.Equal(t, http.StatusConflict, decodeProblem(t, w).Status)
}

func TestCreateFlag_MissingKeyReturns400(t *testing.T) {
	r := newTestRouter(newFakeRepo())
	w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{"environment": "dev"}), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestCreateFlag_MissingEnvironmentReturns400(t *testing.T) {
	r := newTestRouter(newFakeRepo())
	w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{"key": "my-flag"}), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateFlag_InvalidKeyPatternReturns400(t *testing.T) {
	// Valid key is ^[a-z0-9][a-z0-9_-]{0,63}$ — single digit "0" IS valid.
	cases := []string{"UPPER", "-starts-with-dash", "has space", ""}
	r := newTestRouter(newFakeRepo())
	for _, key := range cases {
		w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{
			"key": key, "environment": "dev",
		}), nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, "key=%q", key)
	}
}

func TestCreateFlag_InvalidRolloutReturns400(t *testing.T) {
	for _, pct := range []int{-1, 101, 200} {
		r := newTestRouter(newFakeRepo())
		w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{
			"key": "valid-key", "environment": "dev", "rollout_percentage": pct,
		}), nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, "rollout=%d", pct)
	}
}

func TestCreateFlag_BoundaryRolloutValues(t *testing.T) {
	for _, pct := range []int{0, 100} {
		r := newTestRouter(newFakeRepo())
		w := do(r, http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{
			"key": "my-flag", "environment": "dev", "rollout_percentage": pct,
		}), nil)
		assert.Equal(t, http.StatusCreated, w.Code, "rollout=%d should be valid", pct)
	}
}

// ---- GET /api/v1/flags ----

func TestListFlags_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["a|prod"] = domain.Flag{ID: "1", Key: "a", Environment: "prod", Version: 1}
	repo.flags["b|prod"] = domain.Flag{ID: "2", Key: "b", Environment: "prod", Version: 1}

	w := do(newTestRouter(repo), http.MethodGet, "/api/v1/flags?environment=prod", nil, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	resp := decodeMap(t, w)
	assert.Len(t, resp["data"].([]any), 2)
}

func TestListFlags_EmptyReturnsEmptyArray(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodGet, "/api/v1/flags", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	// data must be [] (empty array), not null
	resp := decodeMap(t, w)
	data, ok := resp["data"]
	require.True(t, ok, "response must have a 'data' field")
	assert.IsType(t, []any{}, data, "'data' must be an array, not null")
}

func TestListFlags_InvalidLimitReturns400(t *testing.T) {
	for _, bad := range []string{"0", "101", "abc", "-1"} {
		w := do(newTestRouter(newFakeRepo()), http.MethodGet, "/api/v1/flags?limit="+bad, nil, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, "limit=%s", bad)
	}
}

func TestListFlags_PaginationCursor(t *testing.T) {
	repo := newFakeRepo()
	// Insert 3 flags with known sort order (key, environment).
	repo.flags["a|dev"] = domain.Flag{ID: "1", Key: "a", Environment: "dev", Version: 1}
	repo.flags["b|dev"] = domain.Flag{ID: "2", Key: "b", Environment: "dev", Version: 1}
	repo.flags["c|dev"] = domain.Flag{ID: "3", Key: "c", Environment: "dev", Version: 1}

	r := newTestRouter(repo)

	// Page 1: limit=2 — fake repo doesn't honour cursor, but handler must set next_cursor.
	w := do(r, http.MethodGet, "/api/v1/flags?limit=2", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	resp := decodeMap(t, w)
	// With 3 items and limit=2, handler fetches 3 (limit+1), finds >limit, sets cursor.
	assert.NotEmpty(t, resp["next_cursor"], "next_cursor must be set when more items exist")
	assert.Len(t, resp["data"].([]any), 2)
}

// ---- GET /api/v1/flags/:key ----

func TestGetFlag_WithEnvironment(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["dark-mode|prod"] = domain.Flag{ID: "1", Key: "dark-mode", Environment: "prod", Version: 1}

	w := do(newTestRouter(repo), http.MethodGet, "/api/v1/flags/dark-mode?environment=prod", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "dark-mode", decodeMap(t, w)["key"])
}

func TestGetFlag_NotFoundReturns404(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodGet, "/api/v1/flags/missing?environment=prod", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	assert.Equal(t, http.StatusNotFound, decodeProblem(t, w).Status)
}

func TestGetFlag_AllEnvsWhenNoEnvironment(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["dark-mode|prod"] = domain.Flag{ID: "1", Key: "dark-mode", Environment: "prod", Version: 1}
	repo.flags["dark-mode|dev"] = domain.Flag{ID: "2", Key: "dark-mode", Environment: "dev", Version: 1}

	w := do(newTestRouter(repo), http.MethodGet, "/api/v1/flags/dark-mode", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, decodeMap(t, w)["data"].([]any), 2)
}

func TestGetFlag_AllEnvsNotFoundReturns404(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodGet, "/api/v1/flags/ghost", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ---- PATCH /api/v1/flags/:key ----

func TestUpdateFlag_EnabledOnly(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["f|prod"] = domain.Flag{ID: "1", Key: "f", Environment: "prod", Enabled: false, RolloutPercentage: 10, Version: 1}

	w := do(newTestRouter(repo), http.MethodPatch, "/api/v1/flags/f?environment=prod",
		jsonBody(t, map[string]any{"enabled": true}),
		map[string]string{"If-Match": `"1"`},
	)
	assert.Equal(t, http.StatusOK, w.Code)
	body := decodeMap(t, w)
	assert.Equal(t, true, body["enabled"])
	assert.Equal(t, float64(10), body["rollout_percentage"], "rollout must be unchanged")
	assert.Equal(t, float64(2), body["version"])
}

func TestUpdateFlag_RolloutOnly(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["f|prod"] = domain.Flag{ID: "1", Key: "f", Environment: "prod", Enabled: true, RolloutPercentage: 10, Version: 1}

	w := do(newTestRouter(repo), http.MethodPatch, "/api/v1/flags/f?environment=prod",
		jsonBody(t, map[string]any{"rollout_percentage": 75}),
		map[string]string{"If-Match": `"1"`},
	)
	assert.Equal(t, http.StatusOK, w.Code)
	body := decodeMap(t, w)
	assert.Equal(t, true, body["enabled"], "enabled must be unchanged")
	assert.Equal(t, float64(75), body["rollout_percentage"])
}

func TestUpdateFlag_StaleVersionReturns412(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["f|prod"] = domain.Flag{ID: "1", Key: "f", Environment: "prod", Version: 3}

	w := do(newTestRouter(repo), http.MethodPatch, "/api/v1/flags/f?environment=prod",
		jsonBody(t, map[string]any{"enabled": true}),
		map[string]string{"If-Match": `"1"`}, // stale
	)
	assert.Equal(t, http.StatusPreconditionFailed, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	assert.Equal(t, http.StatusPreconditionFailed, decodeProblem(t, w).Status)
}

func TestUpdateFlag_MissingIfMatchReturns400(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodPatch, "/api/v1/flags/x?environment=prod",
		jsonBody(t, map[string]any{"enabled": true}), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateFlag_NonIntegerIfMatchReturns400(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodPatch, "/api/v1/flags/x?environment=prod",
		jsonBody(t, map[string]any{"enabled": true}),
		map[string]string{"If-Match": `"abc"`},
	)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateFlag_MissingEnvironmentReturns400(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodPatch, "/api/v1/flags/x",
		jsonBody(t, map[string]any{"enabled": true}),
		map[string]string{"If-Match": `"1"`},
	)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateFlag_EmptyBodyReturns400(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodPatch, "/api/v1/flags/x?environment=prod",
		jsonBody(t, map[string]any{}),
		map[string]string{"If-Match": `"1"`},
	)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateFlag_InvalidRolloutReturns400(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["f|prod"] = domain.Flag{ID: "1", Key: "f", Environment: "prod", Version: 1}
	w := do(newTestRouter(repo), http.MethodPatch, "/api/v1/flags/f?environment=prod",
		jsonBody(t, map[string]any{"rollout_percentage": 999}),
		map[string]string{"If-Match": `"1"`},
	)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateFlag_NotFoundReturns404(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodPatch, "/api/v1/flags/ghost?environment=prod",
		jsonBody(t, map[string]any{"enabled": true}),
		map[string]string{"If-Match": `"1"`},
	)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ---- DELETE /api/v1/flags/:key ----

func TestDeleteFlag_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	repo.flags["bye|dev"] = domain.Flag{ID: "1", Key: "bye", Environment: "dev", Version: 1}

	r := newTestRouter(repo)
	w := do(r, http.MethodDelete, "/api/v1/flags/bye?environment=dev", nil, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Body.String())

	w2 := do(r, http.MethodGet, "/api/v1/flags/bye?environment=dev", nil, nil)
	assert.Equal(t, http.StatusNotFound, w2.Code)
}

func TestDeleteFlag_NotFoundReturns404(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodDelete, "/api/v1/flags/ghost?environment=prod", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestDeleteFlag_MissingEnvironmentReturns400(t *testing.T) {
	w := do(newTestRouter(newFakeRepo()), http.MethodDelete, "/api/v1/flags/x", nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ---- RFC 7807 guarantees ----

func TestAllErrors_HaveCorrectContentType(t *testing.T) {
	repo := newFakeRepo()
	r := newTestRouter(repo)

	cases := []struct {
		method string
		path   string
		body   *bytes.Buffer
		hdrs   map[string]string
		want   int
	}{
		{http.MethodGet, "/api/v1/flags/x?environment=prod", nil, nil, http.StatusNotFound},
		{http.MethodDelete, "/api/v1/flags/x?environment=prod", nil, nil, http.StatusNotFound},
		{http.MethodPost, "/api/v1/flags", jsonBody(t, map[string]any{"environment": "dev"}), nil, http.StatusBadRequest},
		{http.MethodPatch, "/api/v1/flags/x?environment=prod", jsonBody(t, map[string]any{"enabled": true}), nil, http.StatusBadRequest},
	}
	for _, tc := range cases {
		w := do(r, tc.method, tc.path, tc.body, tc.hdrs)
		assert.Equal(t, tc.want, w.Code, "%s %s", tc.method, tc.path)
		assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"), "%s %s", tc.method, tc.path)
		p := decodeProblem(t, w)
		assert.Equal(t, tc.want, p.Status, "status in body must mirror HTTP status")
		assert.NotEmpty(t, p.Type, "type must be set")
		assert.NotEmpty(t, p.Detail, "detail must be set")
	}
}

// TestCreate_OversizeBody_Returns413 proves the BodyLimit middleware's stated
// contract end-to-end: a request body over the 1 MiB cap must surface as 413,
// not 400. The bug this guards against: the handler called WriteValidation
// directly (→ 400) instead of HandleBindError (→ 413), so the BodyLimit
// middleware's documented behaviour never actually happened.
func TestCreate_OversizeBody_Returns413(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(middleware.BodyLimit()) // the real middleware, same as main.go
	v1 := r.Group("/api/v1")
	handlers.NewFlagHandler(newFakeRepo()).Register(v1)

	// A syntactically valid JSON body that exceeds MaxBodyBytes (1 MiB).
	big := make([]byte, middleware.MaxBodyBytes+1024)
	for i := range big {
		big[i] = 'a'
	}
	body := bytes.NewBufferString(`{"key":"x","environment":"prod","description":"` + string(big) + `"}`)

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/flags", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code,
		"an over-size body must be 413, not 400")
	assert.Equal(t, problem.ContentType, rec.Header().Get("Content-Type"))
}
