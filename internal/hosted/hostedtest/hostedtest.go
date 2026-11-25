// Package hostedtest opens the hosted test database for tests.
package hostedtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/hosteddb"
)

// EnvVar names the Postgres database the hosted tests start from. Tests skip
// when it is unset. Each test binary works in a database of its own, created
// beside it (see URL), so packages run in parallel without seeing each
// other's users, jobs or purges; tests within one package still share it and
// must use their own users.
const EnvVar = "HOSTED_TEST_DATABASE_URL"

var (
	packageOnce sync.Once
	packageURL  string
	packageErr  error
)

// Postgres opens this test binary's hosted database with both migration
// tracks applied, skipping the test when EnvVar is unset.
func Postgres(t testing.TB) *db.DB {
	t.Helper()
	database := dbtest.OpenPostgres(t, URL(t))
	if _, err := hosteddb.Migrate(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

// URL returns this test binary's hosted database, skipping the test when
// EnvVar is unset. The first call in a process recreates it empty, named
// after EnvVar's database and the test binary.
func URL(t testing.TB) string {
	t.Helper()
	base := dbtest.PostgresURL(t, EnvVar)
	packageOnce.Do(func() { packageURL, packageErr = recreate(base, binaryName()) })
	if packageErr != nil {
		t.Fatal(packageErr)
	}
	return packageURL
}

// binaryName is the test binary's package name: "hostedjobs" for
// hostedjobs.test.
func binaryName() string {
	return strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
}

// recreate drops and creates the database named after base's and suffix on
// base's server, and returns its URL.
func recreate(base, suffix string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", EnvVar, err)
	}
	name := databaseName(strings.TrimPrefix(u.Path, "/"), suffix)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return "", err
	}
	defer admin.Close()
	ctx := context.Background()
	quoted := `"` + name + `"`
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoted+` WITH (FORCE)`); err != nil {
		return "", fmt.Errorf("dropping %s: %w", name, err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoted); err != nil {
		return "", fmt.Errorf("creating %s: %w", name, err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// databaseName joins base and suffix into a Postgres identifier of lowercase
// letters, digits and underscores, within Postgres's 63-byte limit.
func databaseName(base, suffix string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, base+"_"+suffix)
	return name[:min(len(name), 63)]
}
