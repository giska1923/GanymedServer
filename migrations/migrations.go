// Package migrations embeds the numbered SQL files into the binary, so a deployed backend
// carries its own schema and needs nothing from disk. Applied by internal/db.Migrate.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
