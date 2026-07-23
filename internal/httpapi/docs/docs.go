// Package docs serves the OpenAPI spec and an embedded API console.
//
// Both routes are deliberately unauthenticated: an interviewer opening the
// bare URL must reach something useful, and a 401 is as unhelpful as a 404.
package docs

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed openapi.json
var spec []byte

// console is Scalar's standalone bundle, loaded from a CDN. That fetch happens
// in the viewer's browser, not in this container, so the service itself keeps
// no external dependency.
//
// The version is pinned. An unpinned URL means a third party can change what
// executes in a reviewer's browser between now and when they open the link,
// and a breaking release would take the demo with it — the same mistake as
// pinning a tool's version while fetching its installer from a moving branch.
const scalarVersion = "1.63.0"

const console = `<!doctype html>
<html>
  <head>
    <title>Feature Flag Service — API</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/docs/openapi.json"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@` + scalarVersion + `"></script>
  </body>
</html>`

// Register mounts /docs and /docs/openapi.json.
func Register(r gin.IRoutes) {
	r.GET("/docs", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(console))
	})
	r.GET("/docs/openapi.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", spec)
	})
}

// Spec returns the raw OpenAPI document. Used in tests.
func Spec() []byte { return spec }
