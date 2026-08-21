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
