package migrations

import "embed"

// FS embeds SQLite migration files in execution order.
//
//go:embed *.sql
var FS embed.FS
