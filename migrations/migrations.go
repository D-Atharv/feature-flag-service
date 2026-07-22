// Package migrations embeds the .sql files so cmd/migrate can never drift
// from the schema it ships with.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
