// Package dbtest opens migrated databases for tests.
package dbtest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"futo-notes-server/internal/db"
)

// PostgresURL returns the Postgres URL held in the environment variable
// envVar, skipping the test when it is unset.
func PostgresURL(t testing.TB, envVar string) string {
	t.Helper()
	databaseURL := os.Getenv(envVar)
	if databaseURL == "" {
		t.Skip(envVar + " is not set")
	}
	return databaseURL
}

// Postgres opens and migrates the Postgres database named by the environment
// variable envVar, skipping the test when it is unset. The database is closed
// when the test ends.
func Postgres(t testing.TB, envVar string) *db.DB {
	t.Helper()
	return OpenPostgres(t, PostgresURL(t, envVar))
}

// OpenPostgres opens and migrates the Postgres database at databaseURL. The
// database is closed when the test ends.
func OpenPostgres(t testing.TB, databaseURL string) *db.DB {
	t.Helper()
	dialect, err := db.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := db.Wrap(pool, dialect)
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

// CleanupExec runs query against database when the test ends, after the test
// function has returned, and reports an error if it fails.
func CleanupExec(t testing.TB, database *db.DB, query string, args ...any) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := database.ExecContext(context.Background(), query, args...); err != nil {
			t.Errorf("cleanup %q: %v", query, err)
		}
	})
}

// SQLite opens and migrates a fresh SQLite database in t.TempDir(). The
// database is closed when the test ends.
func SQLite(t testing.TB) *db.DB {
	t.Helper()
	database, err := db.Open("sqlite:"+filepath.Join(t.TempDir(), "notes.db"), db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	return database
}
