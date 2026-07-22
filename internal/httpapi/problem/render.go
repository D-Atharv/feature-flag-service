package problem

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Write maps err to the correct Problem and renders it.
// Unrecognised errors become a generic 500 — log the real cause before calling this.
func Write(c *gin.Context, err error) {
	p := fromDomainErr(err)
	if p == nil {
		p = NewInternal()
	}
	Render(c, p)
}

// WriteValidation renders a 400 for go-playground/validator errors from ShouldBindJSON/ShouldBindQuery.
func WriteValidation(c *gin.Context, err error) {
	Render(c, NewInvalidInput(validationDetail(err)))
}

// Render writes p as application/problem+json and aborts the Gin chain.
// Shallow-copies p so the caller's value is never mutated.
func Render(c *gin.Context, p *Problem) {
	out := *p // copy: never mutate the caller's Problem

	// Stamp request-ID from the requestid middleware.
	if rid, exists := c.Get(ContextKeyRequestID); exists {
		if s, ok := rid.(string); ok {
			out.RequestID = s
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		// Should never happen; fall back to a hard-coded body rather than a blank response.
		c.Header("Content-Type", ContentType)
		c.AbortWithStatus(http.StatusInternalServerError)
		_, _ = c.Writer.Write([]byte(
			`{"type":"/problems/internal-error","title":"Internal Server Error","status":500,"detail":"An unexpected error occurred."}`,
		))
		return
	}

	// Content-Type must be set before AbortWithStatus flushes the headers.
	c.Header("Content-Type", ContentType)
	c.AbortWithStatus(out.Status)
	_, _ = c.Writer.Write(body)
}
