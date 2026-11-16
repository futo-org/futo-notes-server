package config

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// pgx 5.11+ ends the userinfo at the first '@' (libpq's rule); pgx 5.10 and the
// TypeScript server ended it at the last one. Compose files splice the raw
// POSTGRES_PASSWORD into DATABASE_URL, so a password containing '@' must keep
// connecting after the upgrade.
func TestPostgresURLWithAtInPasswordKeepsItsHost(t *testing.T) {
	for _, raw := range []string{
		"postgres://futo_notes:part@two@postgres:5432/futo_notes",
		"postgresql://futo_notes:part@two@postgres:5432/futo_notes",
	} {
		parsed, err := pgx.ParseConfig(NormalizeDatabaseURL(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if parsed.Host != "postgres" || parsed.Password != "part@two" || parsed.User != "futo_notes" {
			t.Fatalf("%s: host=%q user=%q password=%q", raw, parsed.Host, parsed.User, parsed.Password)
		}
	}
}

// An '@' in a path-less URL's query is not part of the credentials.
func TestPostgresURLWithAtInQueryKeepsItsCredentials(t *testing.T) {
	for _, raw := range []string{
		"postgres://u:p@localhost?application_name=x@y",
		"postgres://u:p@localhost/db?application_name=x@y",
		"postgres://u:part@two@localhost?application_name=x@y",
	} {
		parsed, err := pgx.ParseConfig(NormalizeDatabaseURL(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		wantPassword := "p"
		if strings.Contains(raw, "part@two") {
			wantPassword = "part@two"
		}
		if parsed.Host != "localhost" || parsed.Password != wantPassword || parsed.RuntimeParams["application_name"] != "x@y" {
			t.Fatalf("%s: host=%q password=%q application_name=%q", raw, parsed.Host, parsed.Password, parsed.RuntimeParams["application_name"])
		}
	}
}

func TestNormalizeDatabaseURLLeavesOrdinaryURLsAlone(t *testing.T) {
	for _, raw := range []string{
		"sqlite:./data/notes.db",
		"postgres://futo_notes:secret@postgres:5432/futo_notes",
		"postgres://postgres:5432/futo_notes",
		"postgres://futo_notes:pa%40ss@postgres/futo_notes?sslmode=disable",
	} {
		if got := NormalizeDatabaseURL(raw); got != raw {
			t.Fatalf("NormalizeDatabaseURL(%q) = %q", raw, got)
		}
	}
}

func TestLoadNormalizesDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://futo_notes:part@two@postgres:5432/futo_notes")
	t.Setenv("FUTO_NOTES_PASSWORD", "x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := "postgres://futo_notes:part%40two@postgres:5432/futo_notes"; cfg.DatabaseURL != want {
		t.Fatalf("DatabaseURL = %q, want %q", cfg.DatabaseURL, want)
	}
	got, err := LoadRequiredDatabaseURL()
	if err != nil || got != cfg.DatabaseURL {
		t.Fatalf("LoadRequiredDatabaseURL = %q, %v", got, err)
	}
}
