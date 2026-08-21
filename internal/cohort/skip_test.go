//go:build !integration

package cohort_test

import "testing"

func TestDatabaseBackedCohortTestIsExplicitlySkipped(t *testing.T) {
	t.Log("build with -tags=integration and set SARNAUT_POSTGRES_DSN to run cohort persistence against PostgreSQL")
	t.Skip("skipping database-backed cohort test: built without -tags=integration")
}
