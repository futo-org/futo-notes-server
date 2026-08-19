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
	DBPoolMax         int
	DBPoolIdleTimeout time.Duration
}

func Load() (Config, error) {
	if err := loadDotenv(".env"); err != nil {
		return Config{}, fmt.Errorf("loading .env: %w", err)
	}

	cfg := Config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		AuthMode:     getDefault("AUTH_MODE", "password"),
		Password:     os.Getenv("FUTO_NOTES_PASSWORD"),
		PasswordHash: os.Getenv("FUTO_NOTES_PASSWORD_HASH"),
		CookieSecure: os.Getenv("COOKIE_SECURE") != "false",
		DevUI:        os.Getenv("DEV_UI") == "true",
		BlobDir:      getDefault("BLOB_DIR", "./blobs"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
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
