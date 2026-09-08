package config

import (
	"slices"
	"strings"
	"testing"
)

func TestBlobGCEnabled(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "dev")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BlobGCEnabled {
		t.Fatal("BlobGCEnabled = false by default, want true")
	}

	t.Setenv("BLOB_GC_ENABLED", "false")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlobGCEnabled {
		t.Fatal("BlobGCEnabled = true for BLOB_GC_ENABLED=false")
	}
}

func TestDatabaseDefaultsToSQLite(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AUTH_MODE", "dev")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "sqlite:./data/notes.db" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

func TestSQLiteIgnoresPostgresPoolSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "sqlite:notes.db")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("DB_POOL_MAX", "not-an-integer")
	t.Setenv("DB_POOL_IDLE_TIMEOUT_MS", "also-not-an-integer")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() validated ignored Postgres pool settings: %v", err)
	}
}

func TestPasswordConfigurationRequiresExactlyOneCredential(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "password")
	t.Setenv("FUTO_NOTES_PASSWORD", "")
	t.Setenv("FUTO_NOTES_PASSWORD_HASH", "")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("Load() error = %v, want missing password error", err)
	}

	t.Setenv("FUTO_NOTES_PASSWORD", "plaintext")
	t.Setenv("FUTO_NOTES_PASSWORD_HASH", "scrypt:hash")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("Load() error = %v, want ambiguous password error", err)
	}

	t.Setenv("FUTO_NOTES_PASSWORD", "")
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with hash only: %v", err)
	}
}

func TestOIDCConfigurationRequiresAllFourVars(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "")
	t.Setenv("OIDC_CLIENT_ID", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("OIDC_REDIRECT_URI", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want missing OIDC vars error")
	}
	for _, name := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URI"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("Load() error = %v, want it to name %s", err, name)
		}
	}

	t.Setenv("OIDC_ISSUER", "https://idp.example.test/")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URI", "http://localhost:3005/api/auth/oidc/callback")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with all OIDC vars set: %v", err)
	}
	if cfg.OIDCIssuer != "https://idp.example.test" {
		t.Fatalf("OIDCIssuer = %q, want trailing slash trimmed", cfg.OIDCIssuer)
	}
}

func TestOIDCRedirectURIMustBeAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://idp.example.test")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URI", "/api/auth/oidc/callback")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Load() error = %v, want absolute redirect URI error", err)
	}
}

func TestDroppedEnvWarnings(t *testing.T) {
	t.Setenv("DB_SSL", "true")
	t.Setenv("MAX_BLOB_BYTES", "123")

	warnings := DroppedEnvWarnings()
	if !slices.ContainsFunc(warnings, func(warning string) bool {
		return strings.Contains(warning, "DB_SSL is ignored") && strings.Contains(warning, "DATABASE_URL")
	}) {
		t.Fatalf("warnings %q do not explain DB_SSL replacement", warnings)
	}
	if !slices.ContainsFunc(warnings, func(warning string) bool {
		return strings.Contains(warning, "MAX_BLOB_BYTES is ignored") && strings.Contains(warning, "100 MiB")
	}) {
		t.Fatalf("warnings %q do not explain MAX_BLOB_BYTES replacement", warnings)
	}
	if slices.ContainsFunc(warnings, func(warning string) bool {
		return strings.Contains(warning, "BLOB_GC_ENABLED")
	}) {
		t.Fatalf("warnings %q include honored BLOB_GC_ENABLED", warnings)
	}
}
