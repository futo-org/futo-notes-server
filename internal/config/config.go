// Package config reads server configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL       string
	Port              int
	AuthMode          string
	Password          string // FUTO_NOTES_PASSWORD, plaintext
	PasswordHash      string // FUTO_NOTES_PASSWORD_HASH, scrypt self-describing form
	CookieSecure      bool   // COOKIE_SECURE, Secure cookie flag; on unless "false"
	DevUI             bool   // DEV_UI, serves the dev test page at /dev when "true"
	BlobDir           string // BLOB_DIR, root of on-disk blob storage
	BlobGCEnabled     bool   // BLOB_GC_ENABLED, disables irreversible blob garbage collection when false
	DBPoolMax         int
	DBPoolIdleTimeout time.Duration
}

func Load() (Config, error) {
	if err := loadDotenv(".env"); err != nil {
		return Config{}, fmt.Errorf("loading .env: %w", err)
	}

	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		AuthMode:      getDefault("AUTH_MODE", "password"),
		Password:      os.Getenv("FUTO_NOTES_PASSWORD"),
		PasswordHash:  os.Getenv("FUTO_NOTES_PASSWORD_HASH"),
		CookieSecure:  os.Getenv("COOKIE_SECURE") != "false",
		DevUI:         os.Getenv("DEV_UI") == "true",
		BlobDir:       getDefault("BLOB_DIR", "./blobs"),
		BlobGCEnabled: os.Getenv("BLOB_GC_ENABLED") != "false",
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.AuthMode != "dev" && cfg.AuthMode != "password" {
		return Config{}, fmt.Errorf("AUTH_MODE must be dev or password, got %q", cfg.AuthMode)
	}
	if cfg.AuthMode == "password" {
		switch {
		case cfg.Password != "" && cfg.PasswordHash != "":
			return Config{}, fmt.Errorf("set only one of FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH when AUTH_MODE=password")
		case cfg.Password == "" && cfg.PasswordHash == "":
			return Config{}, fmt.Errorf("FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH is required when AUTH_MODE=password")
		}
	}

	var err error
	if cfg.Port, err = getInt("PORT", 3005); err != nil {
		return Config{}, err
	}
	if cfg.DBPoolMax, err = getInt("DB_POOL_MAX", 10); err != nil {
		return Config{}, err
	}
	idleMs, err := getInt("DB_POOL_IDLE_TIMEOUT_MS", 10000)
	if err != nil {
		return Config{}, err
	}
	cfg.DBPoolIdleTimeout = time.Duration(idleMs) * time.Millisecond

	return cfg, nil
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
		{"LOG_LEVEL", "the Go server currently uses its built-in log level"},
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
