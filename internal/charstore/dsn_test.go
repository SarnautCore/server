package charstore_test

// postgresDSNEnvironment is the same variable every service reads, so there is
// one place a developer points the tests at a database.
const postgresDSNEnvironment = "SARNAUT_POSTGRES_DSN"

const dsnHint = "start infra/compose and export " + postgresDSNEnvironment +
	`="postgres://sarnaut:sarnaut_dev@127.0.0.1:5433/sarnaut?sslmode=disable", ` +
	"then run: go test -tags=integration ./internal/charstore/..."
