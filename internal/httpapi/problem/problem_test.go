package problem_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newContext creates a test Gin context backed by an httptest.ResponseRecorder.
func newContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/", nil)
	return c, w
}

// newContextWithRequestID creates a test context with a pre-set request ID.
func newContextWithRequestID(rid string) (*gin.Context, *httptest.ResponseRecorder) {
	c, w := newContext()
	c.Set(problem.ContextKeyRequestID, rid)
	return c, w
}

// decodeProblem decodes the response body into a problem.Problem.
func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) problem.Problem {
	t.Helper()
	var p problem.Problem
	require.NoError(t, json.NewDecoder(w.Body).Decode(&p))
	return p
}

// ---------------------------------------------------------------------------
// Constructor tests
// ---------------------------------------------------------------------------

func TestConstructors(t *testing.T) {
	cases := []struct {
		name         string
		p            *problem.Problem
		wantStatus   int
		wantTypeSufx string
	}{
		{"NotFound", problem.NewNotFound("x"), http.StatusNotFound, "not-found"},
		{"Conflict", problem.NewConflict("x"), http.StatusConflict, "conflict"},
		{"VersionMismatch", problem.NewVersionMismatch("x"), http.StatusPreconditionFailed, "version-mismatch"},
		{"InvalidInput", problem.NewInvalidInput("x"), http.StatusBadRequest, "invalid-input"},
		{"Unauthorized", problem.NewUnauthorized("x"), http.StatusUnauthorized, "unauthorized"},
		{"Forbidden", problem.NewForbidden("x"), http.StatusForbidden, "forbidden"},
		{"TooManyRequests", problem.NewTooManyRequests("x"), http.StatusTooManyRequests, "too-many-requests"},
		{"ServiceUnavailable", problem.NewServiceUnavailable("x"), http.StatusServiceUnavailable, "service-unavailable"},
		{"Internal", problem.NewInternal(), http.StatusInternalServerError, "internal-error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantStatus, tc.p.Status)
			assert.Contains(t, tc.p.Type, tc.wantTypeSufx)
			assert.NotEmpty(t, tc.p.Title)
		})
	}
}

// ---------------------------------------------------------------------------
// Render tests
// ---------------------------------------------------------------------------

func TestRender_ContentType(t *testing.T) {
	c, w := newContext()
	problem.Render(c, problem.NewNotFound("test"))
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestRender_StatusCode(t *testing.T) {
	c, w := newContext()
	problem.Render(c, problem.NewNotFound("test"))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRender_BodyIsValidJSON(t *testing.T) {
	c, w := newContext()
	problem.Render(c, problem.NewConflict("already exists"))
	p := decodeProblem(t, w)
	assert.Equal(t, http.StatusConflict, p.Status)
	assert.Equal(t, "already exists", p.Detail)
}

func TestRender_StampsRequestID(t *testing.T) {
	c, w := newContextWithRequestID("req-abc-123")
	problem.Render(c, problem.NewNotFound("nope"))
	p := decodeProblem(t, w)
	assert.Equal(t, "req-abc-123", p.RequestID)
}

func TestRender_OmitsRequestIDWhenAbsent(t *testing.T) {
	c, w := newContext()
	problem.Render(c, problem.NewNotFound("nope"))
	// request_id should be omitted from JSON when not set
	raw := w.Body.String()
	assert.NotContains(t, raw, "request_id")
}

func TestRender_DoesNotMutateCallerProblem(t *testing.T) {
	p := problem.NewNotFound("original")
	c1, _ := newContextWithRequestID("rid-1")
	problem.Render(c1, p)

	// The original Problem should NOT have the request_id stamped onto it.
	assert.Empty(t, p.RequestID, "Render must not mutate the caller's *Problem")
}

func TestRender_AbortsChain(t *testing.T) {
	c, _ := newContext()
	problem.Render(c, problem.NewNotFound("test"))
	assert.True(t, c.IsAborted(), "Render must abort the Gin context")
}

// ---------------------------------------------------------------------------
// Write tests — domain sentinel mapping
// ---------------------------------------------------------------------------

func TestWrite_DomainSentinels(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
	}{
		{domain.ErrNotFound, http.StatusNotFound},
		{domain.ErrConflict, http.StatusConflict},
		{domain.ErrVersionMismatch, http.StatusPreconditionFailed},
		{domain.ErrInvalidInput, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			c, w := newContext()
			problem.Write(c, tc.err)
			assert.Equal(t, tc.wantStatus, w.Code)
			assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
		})
	}
}

func TestWrite_WrappedDomainError(t *testing.T) {
	wrapped := fmt.Errorf("flag 'dark-mode' in env 'prod': %w", domain.ErrNotFound)
	c, w := newContext()
	problem.Write(c, wrapped)
	assert.Equal(t, http.StatusNotFound, w.Code)

	p := decodeProblem(t, w)
	// Detail should contain the full wrapped message, not just "not found".
	assert.Contains(t, p.Detail, "dark-mode")
}

func TestWrite_UnknownError_Returns500(t *testing.T) {
	c, w := newContext()
	problem.Write(c, errors.New("some database driver error"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	p := decodeProblem(t, w)
	// Real cause must NOT leak into the response body.
	assert.NotContains(t, p.Detail, "database driver")
}

// ---------------------------------------------------------------------------
// WriteValidation tests
// ---------------------------------------------------------------------------

// validationErr builds a real validator.ValidationErrors by validating a
// purposely invalid struct.
func validationErr(t *testing.T) error {
	t.Helper()
	type req struct {
		Name  string `validate:"required"`
		Count int    `validate:"min=1,max=100"`
	}
	v := validator.New()
	err := v.Struct(req{Name: "", Count: 0})
	require.Error(t, err)
	return err
}

func TestWriteValidation_Status400(t *testing.T) {
	c, w := newContext()
	problem.WriteValidation(c, validationErr(t))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWriteValidation_ContentType(t *testing.T) {
	c, w := newContext()
	problem.WriteValidation(c, validationErr(t))
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestWriteValidation_DetailMentionsField(t *testing.T) {
	c, w := newContext()
	problem.WriteValidation(c, validationErr(t))
	p := decodeProblem(t, w)
	// Detail should name the failing field(s).
	assert.NotEmpty(t, p.Detail)
	assert.Contains(t, p.Detail, "Name")
}

func TestWriteValidation_NonValidationError(t *testing.T) {
	c, w := newContext()
	problem.WriteValidation(c, errors.New("plain error"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	p := decodeProblem(t, w)
	assert.Equal(t, "plain error", p.Detail)
}
