// Package migrations embeds the Goose SQL files so the runtime can verify the
// database schema without reading the migrations directory from disk.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
