//go:build !integration

package charstore_test

import (
	"os"
	"testing"
)

// TestDatabaseBackedTestsAreSkipped is the visible record that a whole suite did
// not run.
//
// Without it, `go test ./internal/charstore/...` on a machine with no database
// reports success and gives no hint that none of the PostgreSQL behaviour —
// migrations, the unique index, transaction rollback — was exercised. The skip
// reason names both conditions and the command that satisfies them.
func TestDatabaseBackedTestsAreSkipped(t *testing.T) {
	t.Parallel()

	if os.Getenv(postgresDSNEnvironment) == "" {
		t.Logf("no %s is configured; %s", postgresDSNEnvironment, dsnHint)
	} else {
		t.Logf("%s is configured but this binary was built without -tags=integration; %s",
			postgresDSNEnvironment, dsnHint)
	}
	t.Skip("skipping database-backed store tests: built without -tags=integration")
}
