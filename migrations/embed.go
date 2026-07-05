// Package migrations embeds the goose SQL migrations so memd and memctl can
// apply them without shipping loose files.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
