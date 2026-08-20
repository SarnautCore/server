// Package migrations embeds the goose SQL migrations that define the durable
// schema described by ADR 0031.
//
// The files are embedded rather than read from disk so that a shipped binary
// carries the exact schema it was built against: `cmd/migrate` and any future
// start-up migration step apply the same bytes CI proved reversible.
package migrations

import "embed"

// FS holds every `NNNNN_description.sql` migration in this directory.
//
//go:embed *.sql
var FS embed.FS

// Dialect is the goose dialect these migrations are written for. PostgreSQL is
// the only durable store (ADR 0031 §1), so there is exactly one.
const Dialect = "postgres"
