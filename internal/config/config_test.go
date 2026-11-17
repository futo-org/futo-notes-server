package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestLoadDotenvSkipsLinesWithoutKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("=orphan\nFUTO_DOTENV_TEST=value # comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUTO_DOTENV_TEST", "")
	if err := os.Unsetenv("FUTO_DOTENV_TEST"); err != nil {
		t.Fatal(err)
	}

	if err := loadDotenv(path); err != nil {
		t.Fatalf("loadDotenv() = %v", err)
	}
	if got := os.Getenv("FUTO_DOTENV_TEST"); got != "value" {
		t.Fatalf("FUTO_DOTENV_TEST = %q, want value", got)
	}
}

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

func TestLogSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AUTH_MODE", "dev")

	for _, tt := range []struct {
		level, format string
		wantLevel     zerolog.Level
		wantPretty    bool
		wantError     string
	}{
		{wantLevel: zerolog.InfoLevel},
		{level: "debug", format: "json", wantLevel: zerolog.DebugLevel},
		{level: "WARN", format: "pretty", wantLevel: zerolog.WarnLevel, wantPretty: true},
		{level: "verbose", wantLevel: zerolog.InfoLevel},
		{level: "silent", wantLevel: zerolog.Disabled},
		{format: "text", wantError: `LOG_FORMAT must be json or pretty, got "text"`},
	} {
		t.Setenv("LOG_LEVEL", tt.level)
		t.Setenv("LOG_FORMAT", tt.format)
		cfg, err := Load()
		var gotError string
		if err != nil {
			gotError = err.Error()
		}
		if gotError != tt.wantError || cfg.LogLevel != tt.wantLevel || cfg.LogPretty != tt.wantPretty {
			t.Errorf("LOG_LEVEL=%q LOG_FORMAT=%q: Load() = level %v pretty %t error %q; want level %v pretty %t error %q",
				tt.level, tt.format, cfg.LogLevel, cfg.LogPretty, gotError, tt.wantLevel, tt.wantPretty, tt.wantError)
		}
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
	t.Setenv("LOG_LEVEL", "info")
	warnings = DroppedEnvWarnings()
	if slices.ContainsFunc(warnings, func(warning string) bool {
		return strings.Contains(warning, "BLOB_GC_ENABLED") || strings.Contains(warning, "LOG_LEVEL")
	}) {
		t.Fatalf("warnings %q include an honored setting", warnings)
	}
}

func TestUnrecognizedLogLevelWarnsInsteadOfFailing(t *testing.T) {
	t.Setenv("LOG_LEVEL", "verbose")
	if !slices.ContainsFunc(DroppedEnvWarnings(), func(warning string) bool {
		return strings.Contains(warning, `LOG_LEVEL is ignored: "verbose"`)
	}) {
		t.Fatalf("warnings %q do not report the unrecognized LOG_LEVEL", DroppedEnvWarnings())
	}
	t.Setenv("LOG_LEVEL", "silent")
	if slices.ContainsFunc(DroppedEnvWarnings(), func(warning string) bool {
		return strings.Contains(warning, "LOG_LEVEL")
	}) {
		t.Fatalf("warnings %q report the recognized LOG_LEVEL silent", DroppedEnvWarnings())
	}
}
