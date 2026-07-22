package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/evaluation"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/handlers"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
	"github.com/D-Atharv/feature-flag-service/internal/snapshot"
)

// ---- fake FlagSource ----

type fakeFlagSource struct {
	flags map[string]domain.Flag // "key|env"
	err   error                  // non-nil forces every call to return this error
}

func newFakeFlagSource() *fakeFlagSource {
	return &fakeFlagSource{flags: make(map[string]domain.Flag)}
}

func (f *fakeFlagSource) GetByKeyEnv(_ context.Context, key, env string) (domain.Flag, error) {
	if f.err != nil {
		return domain.Flag{}, f.err
	}
	flag, ok := f.flags[key+"|"+env]
	if !ok {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, env, domain.ErrNotFound)
	}
	return flag, nil
}

// ---- test router ----

func newEvalRouter(src evaluation.FlagSource) *gin.Engine {
	r := gin.New()
	h := handlers.NewEvalHandler(src)
	root := r.Group("/")
	v1 := r.Group("/api/v1")
	h.Register(root, v1)
	return r
}

// ---- response decoder ----

type evalBody struct {
	Enabled     bool   `json:"enabled"`
	Reason      string `json:"reason"`
	FlagKey     string `json:"flag_key"`
	Environment string `json:"environment"`
	Subject     string `json:"subject"`
}

func decodeEval(t *testing.T, w *httptest.ResponseRecorder) evalBody {
	t.Helper()
	var b evalBody
	require.NoError(t, json.NewDecoder(w.Body).Decode(&b))
	return b
}

func evalGET(router *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// ---- tests ----

func TestEvaluate_HappyPath_Enabled(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["dark-mode|prod"] = domain.Flag{
		Key: "dark-mode", Environment: "prod", Enabled: true, RolloutPercentage: 100,
	}

	w := evalGET(newEvalRouter(src), "/evaluate/dark-mode?env=prod&subject=user-1", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	body := decodeEval(t, w)
	assert.True(t, body.Enabled)
	assert.Equal(t, string(evaluation.ReasonRolloutFull), body.Reason)
	assert.Equal(t, "dark-mode", body.FlagKey)
	assert.Equal(t, "prod", body.Environment)
	assert.Equal(t, "user-1", body.Subject)
}

func TestEvaluate_HappyPath_Disabled(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["dark-mode|prod"] = domain.Flag{
		Key: "dark-mode", Environment: "prod", Enabled: false, RolloutPercentage: 100,
	}

	w := evalGET(newEvalRouter(src), "/evaluate/dark-mode?env=prod&subject=user-1", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	body := decodeEval(t, w)
	assert.False(t, body.Enabled)
	assert.Equal(t, string(evaluation.ReasonFlagDisabled), body.Reason)
}

// Stickiness: same subject evaluated twice must return the same result.
func TestEvaluate_Stickiness(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["checkout|prod"] = domain.Flag{
		Key: "checkout", Environment: "prod", Enabled: true, RolloutPercentage: 50,
	}
	router := newEvalRouter(src)

	w1 := evalGET(router, "/evaluate/checkout?env=prod&subject=user-42", nil)
	w2 := evalGET(router, "/evaluate/checkout?env=prod&subject=user-42", nil)

	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, http.StatusOK, w2.Code)
	b1 := decodeEval(t, w1)
	b2 := decodeEval(t, w2)
	assert.Equal(t, b1.Enabled, b2.Enabled, "same subject must get the same result on every call")
	assert.Equal(t, b1.Reason, b2.Reason)
}

func TestEvaluate_FlagNotFound_Returns200(t *testing.T) {
	w := evalGET(newEvalRouter(newFakeFlagSource()), "/evaluate/missing?env=prod&subject=user-1", nil)

	assert.Equal(t, http.StatusOK, w.Code, "FLAG_NOT_FOUND must return 200, not 404")
	body := decodeEval(t, w)
	assert.False(t, body.Enabled)
	assert.Equal(t, string(evaluation.ReasonFlagNotFound), body.Reason)
}

func TestEvaluate_StrictMode_FlagNotFound_Returns404(t *testing.T) {
	w := evalGET(newEvalRouter(newFakeFlagSource()), "/evaluate/missing?env=prod&subject=user-1&strict=true", nil)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	var p problem.Problem
	require.NoError(t, json.NewDecoder(w.Body).Decode(&p))
	assert.Equal(t, http.StatusNotFound, p.Status)
}

func TestEvaluate_StrictMode_ExistingFlag_Still200(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["f|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 100}

	// strict=true on a found flag must not affect the response.
	w := evalGET(newEvalRouter(src), "/evaluate/f?env=prod&subject=u&strict=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestEvaluate_MissingEnv_Returns400(t *testing.T) {
	w := evalGET(newEvalRouter(newFakeFlagSource()), "/evaluate/dark-mode?subject=user-1", nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestEvaluate_StoreError_Returns500(t *testing.T) {
	src := newFakeFlagSource()
	src.err = errors.New("postgres: connection refused")

	w := evalGET(newEvalRouter(src), "/evaluate/f?env=prod&subject=u", nil)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

// Subject resolution: ?subject= takes priority over header.
func TestEvaluate_SubjectFromQueryParam(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["f|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 100}

	w := evalGET(newEvalRouter(src), "/evaluate/f?env=prod&subject=from-query",
		map[string]string{"X-Subject-ID": "from-header"})

	body := decodeEval(t, w)
	assert.Equal(t, "from-query", body.Subject, "?subject= must take priority over X-Subject-ID")
}

// Subject resolution: header used when no query param.
func TestEvaluate_SubjectFromHeader(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["f|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 100}

	w := evalGET(newEvalRouter(src), "/evaluate/f?env=prod",
		map[string]string{"X-Subject-ID": "from-header"})

	body := decodeEval(t, w)
	assert.Equal(t, "from-header", body.Subject)
}

// Subject resolution: empty string when nothing is provided.
func TestEvaluate_SubjectFallsBackToEmpty(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["f|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 100}

	w := evalGET(newEvalRouter(src), "/evaluate/f?env=prod", nil)

	body := decodeEval(t, w)
	assert.Equal(t, "", body.Subject, "subject must be echoed as empty when not supplied")
}

// Both URL paths must return identical results.
func TestEvaluate_BothPathsReturnSameResult(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["dark-mode|prod"] = domain.Flag{
		Key: "dark-mode", Environment: "prod", Enabled: true, RolloutPercentage: 100,
	}
	router := newEvalRouter(src)

	w1 := evalGET(router, "/evaluate/dark-mode?env=prod&subject=user-1", nil)
	w2 := evalGET(router, "/api/v1/evaluate/dark-mode?env=prod&subject=user-1", nil)

	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, http.StatusOK, w2.Code)

	b1 := decodeEval(t, w1)
	b2 := decodeEval(t, w2)
	assert.Equal(t, b1.Enabled, b2.Enabled)
	assert.Equal(t, b1.Reason, b2.Reason)
	assert.Equal(t, b1.Subject, b2.Subject)
}

func TestEvaluate_RolloutZero_AlwaysDisabled(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["f|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 0}

	w := evalGET(newEvalRouter(src), "/evaluate/f?env=prod&subject=user-1", nil)

	body := decodeEval(t, w)
	assert.False(t, body.Enabled)
	assert.Equal(t, string(evaluation.ReasonRolloutZero), body.Reason)
}

func TestEvaluate_ResponseContainsAllFields(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["my-flag|staging"] = domain.Flag{Enabled: true, RolloutPercentage: 100}

	w := evalGET(newEvalRouter(src), "/evaluate/my-flag?env=staging&subject=user-7", nil)

	require.Equal(t, http.StatusOK, w.Code)
	var raw map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&raw))

	for _, field := range []string{"enabled", "reason", "flag_key", "environment", "subject", "evaluated_at"} {
		assert.Contains(t, raw, field, "response must contain field %q", field)
	}
}

// Subject resolution: actor_key_id from Gin context (Phase 4 auth middleware path).
func TestEvaluate_SubjectFromActorKeyID(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["f|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 100}

	// Build a router that pre-sets actor_key_id in the Gin context,
	// simulating what the auth middleware will do in Phase 4.
	r := gin.New()
	h := handlers.NewEvalHandler(src)
	root := r.Group("/")
	v1 := r.Group("/api/v1")
	h.Register(root, v1)
	r.Use(func(c *gin.Context) {
		c.Set("actor_key_id", "key-abc-123")
		c.Next()
	})

	// Inject actor_key_id before the handler runs by using a wrapper route.
	rWithActor := gin.New()
	rWithActor.GET("/evaluate/:key", func(c *gin.Context) {
		c.Set("actor_key_id", "key-abc-123")
		c.Next()
	}, h.Evaluate)

	req, _ := http.NewRequest(http.MethodGet, "/evaluate/f?env=prod", nil)
	// No ?subject= and no X-Subject-ID header — should fall through to actor_key_id.
	w := httptest.NewRecorder()
	rWithActor.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := decodeEval(t, w)
	assert.Equal(t, "key-abc-123", body.Subject, "actor_key_id must be used as subject when no query param or header")
}

// Partial rollout: handler must return ROLLOUT_INCLUDED or ROLLOUT_EXCLUDED reason.
func TestEvaluate_PartialRollout_ReasonIsIncludedOrExcluded(t *testing.T) {
	src := newFakeFlagSource()
	src.flags["checkout|prod"] = domain.Flag{Enabled: true, RolloutPercentage: 50}
	router := newEvalRouter(src)

	validReasons := map[string]bool{
		string(evaluation.ReasonRolloutIncluded): true,
		string(evaluation.ReasonRolloutExcluded): true,
	}

	// Try several subjects — each must get one of the two partial-rollout reasons.
	for i := range 20 {
		subj := fmt.Sprintf("user-%d", i)
		w := evalGET(router, fmt.Sprintf("/evaluate/checkout?env=prod&subject=%s", subj), nil)
		require.Equal(t, http.StatusOK, w.Code)
		body := decodeEval(t, w)
		assert.True(t, validReasons[body.Reason],
			"subject %s: reason %q must be ROLLOUT_INCLUDED or ROLLOUT_EXCLUDED at rollout=50",
			subj, body.Reason,
		)
	}
}

// TestEvaluate_UnloadedSnapshot_Returns503 pins the whole chain: an unloaded
// snapshot must reach the client as a retryable 503, never as
// 200 {"enabled":false,"reason":"FLAG_NOT_FOUND"}.
//
// The failure this guards against is silent — every flag in the system would
// read as off, with a success status code, and nothing would alert.
func TestEvaluate_UnloadedSnapshot_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A brand new snapshot: constructed, never loaded.
	r := newEvalRouter(snapshot.New())

	req := httptest.NewRequest(http.MethodGet, "/evaluate/any-flag?env=prod&subject=u1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"an unloaded snapshot must not answer 200 with FLAG_NOT_FOUND")
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))

	var p problem.Problem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
	assert.Equal(t, http.StatusServiceUnavailable, p.Status)
}

// TestEvaluate_LoadedButEmptySnapshot_Returns200NotFound is the other side of
// the same line: a database with no flags is a loaded, healthy state.
func TestEvaluate_LoadedButEmptySnapshot_Returns200NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	s := snapshot.New()
	s.Replace(nil)
	r := newEvalRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/evaluate/any-flag?env=prod&subject=u1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body evalBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.Enabled)
	assert.Equal(t, string(evaluation.ReasonFlagNotFound), body.Reason)
}
