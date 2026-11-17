// Package config reads server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"futo-notes-server/internal/db"
)

// AuthMode selects the login flow the server offers. Its value is the
// AUTH_MODE environment value and is reported to clients in the capability
// document.
type AuthMode string

// The supported AUTH_MODE values.
const (
	// AuthModeDev logs in any email address without a password, for
	// development servers.
	AuthModeDev AuthMode = "dev"
	// AuthModePassword logs in the single local user with the configured
	// password.
	AuthModePassword AuthMode = "password"
)

// Config is the server configuration read from the environment and .env.
type Config struct {
	DatabaseURL        string
	Port               int
	AuthMode           AuthMode
	Password           string        // FUTO_NOTES_PASSWORD, plaintext
	PasswordHash       string        // FUTO_NOTES_PASSWORD_HASH, scrypt self-describing form
	CookieSecure       bool          // COOKIE_SECURE, Secure cookie flag; on unless "false"
	DevUI              bool          // DEV_UI, serves the dev test page at /dev when "true"
	BlobDir            string        // BLOB_DIR, root of on-disk blob storage
	BlobGCEnabled      bool          // BLOB_GC_ENABLED, disables irreversible blob garbage collection when false
	AllowFreshDatabase bool          // ALLOW_FRESH_DATABASE, overrides the SQLite/blob safety guard
	DBPoolMax          int           // DB_POOL_MAX, Postgres only
	DBPoolIdleTimeout  time.Duration // DB_POOL_IDLE_TIMEOUT_MS, Postgres only
	LogLevel           zerolog.Level // LOG_LEVEL, a zerolog level name; info by default
	LogPretty          bool          // LOG_FORMAT=pretty, human-readable console logs instead of JSON
}

// Load reads the server configuration, applying .env values that the
// environment does not already set. In password mode exactly one of the
// plaintext password and the scrypt hash is guaranteed to be set.
func Load() (Config, error) {
	if err := loadDotenv(".env"); err != nil {
		return Config{}, fmt.Errorf("loading .env: %w", err)
	}

	cfg := Config{
		DatabaseURL:        NormalizeDatabaseURL(getDefault("DATABASE_URL", db.DefaultSQLiteURL)),
		AuthMode:           AuthMode(getDefault("AUTH_MODE", string(AuthModePassword))),
		Password:           os.Getenv("FUTO_NOTES_PASSWORD"),
		PasswordHash:       os.Getenv("FUTO_NOTES_PASSWORD_HASH"),
		CookieSecure:       os.Getenv("COOKIE_SECURE") != "false",
		DevUI:              os.Getenv("DEV_UI") == "true",
		BlobDir:            getDefault("BLOB_DIR", "./blobs"),
		BlobGCEnabled:      os.Getenv("BLOB_GC_ENABLED") != "false",
		AllowFreshDatabase: os.Getenv("ALLOW_FRESH_DATABASE") == "true",
	}
	if cfg.AuthMode != AuthModeDev && cfg.AuthMode != AuthModePassword {
		return Config{}, fmt.Errorf("AUTH_MODE must be dev or password, got %q", cfg.AuthMode)
	}
	if cfg.AuthMode == AuthModePassword {
		switch {
		case cfg.Password != "" && cfg.PasswordHash != "":
			return Config{}, errors.New("set only one of FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH when AUTH_MODE=password")
		case cfg.Password == "" && cfg.PasswordHash == "":
			return Config{}, errors.New("FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH is required when AUTH_MODE=password")
		}
	}

	var err error
	if cfg.LogLevel, err = zerolog.ParseLevel(getDefault("LOG_LEVEL", "info")); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL must be trace, debug, info, warn, error, fatal, panic or disabled, got %q", os.Getenv("LOG_LEVEL"))
	}
	switch format := getDefault("LOG_FORMAT", "json"); format {
	case "json":
	case "pretty":
		cfg.LogPretty = true
	default:
		return Config{}, fmt.Errorf("LOG_FORMAT must be json or pretty, got %q", format)
	}
	if cfg.Port, err = getInt("PORT", 3005); err != nil {
		return Config{}, err
	}
	if dialect, parseErr := db.ParseDialect(cfg.DatabaseURL); parseErr == nil && dialect.Engine() == db.Postgres {
		if cfg.DBPoolMax, err = getInt("DB_POOL_MAX", 10); err != nil {
			return Config{}, err
		}
		idleMs, err := getInt("DB_POOL_IDLE_TIMEOUT_MS", 10000)
		if err != nil {
			return Config{}, err
		}
		cfg.DBPoolIdleTimeout = time.Duration(idleMs) * time.Millisecond
	}

	return cfg, nil
}

// LoadRequiredDatabaseURL loads .env and returns the explicitly configured
// source URL used by offline database commands. Unlike Load, it never supplies
// the SQLite server default and does not require authentication settings.
func LoadRequiredDatabaseURL() (string, error) {
	if err := loadDotenv(".env"); err != nil {
		return "", fmt.Errorf("loading .env: %w", err)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return "", errors.New("DATABASE_URL is required")
	}
	return NormalizeDatabaseURL(databaseURL), nil
}

// DroppedEnvWarnings reports legacy TypeScript-server settings that the Go
// server deliberately does not honor. It is evaluated after loading .env so a
// self-hoster sees the warning regardless of where the old setting was defined.
func DroppedEnvWarnings() []string {
	replacements := []struct {
		name    string
		message string
	}{
		{"AUTH_RATE_LIMIT", "login rate limiting is fixed at 10 attempts per 60 seconds"},
		{"AUTH_RATE_LIMIT_WINDOW_MS", "login rate limiting is fixed at 10 attempts per 60 seconds"},
		{"BLOB_GC_INTERVAL_MS", "maintenance runs on the fixed built-in schedule"},
		{"BLOB_RETENTION_DAYS", "retained blob lifetime is fixed at 365 days"},
		{"DB_SSL", "configure pgx TLS with sslmode in DATABASE_URL"},
		{"DB_SSL_CA", "configure pgx TLS with sslrootcert in DATABASE_URL"},
		{"DB_SSL_INSECURE", "configure pgx TLS with sslmode in DATABASE_URL"},
		{"MAX_BATCH_BYTES", "the batch limit is fixed at 32 MiB"},
		{"MAX_BLOB_BYTES", "the upload limit is fixed at 100 MiB"},
		{"TRUST_PROXY", "forwarded client addresses are not trusted; rate limiting uses the direct peer"},
	}

	warnings := make([]string, 0, len(replacements))
	for _, replacement := range replacements {
		if _, set := os.LookupEnv(replacement.name); set {
			warnings = append(warnings, fmt.Sprintf("%s is ignored: %s", replacement.name, replacement.message))
		}
	}
	return warnings
}

func getDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: expected an integer, got %q", key, v)
	}
	return n, nil
}
