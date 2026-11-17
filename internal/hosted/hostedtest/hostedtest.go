// Package hostedtest opens the hosted test database for tests.
package hostedtest

import (
	"testing"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/hosteddb"
)

// EnvVar names the database the hosted tests share. Tests skip when it is
// unset, and must use their own users because other tests use it too.
const EnvVar = "HOSTED_TEST_DATABASE_URL"

// Postgres opens the hosted test database with both migration tracks
// applied, skipping the test when EnvVar is unset.
func Postgres(t testing.TB) *db.DB {
	t.Helper()
	database := dbtest.Postgres(t, EnvVar)
	if _, err := hosteddb.Migrate(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

// URL returns the hosted test database URL, skipping the test when EnvVar is
// unset.
func URL(t testing.TB) string {
	t.Helper()
	return dbtest.PostgresURL(t, EnvVar)
}
