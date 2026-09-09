// Package config reads server configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// oidcCallbackPath is the route OIDC_REDIRECT_URI has to point at. The Login
// Hand-off login URL is built by swapping this route for its start sibling,
// so a redirect URI aimed anywhere else would produce a URL that 404s.
const oidcCallbackPath = "/api/auth/oidc/callback"

type Config struct {
	DatabaseURL        string
	Port               int
	AuthMode           string
	Password           string // FUTO_NOTES_PASSWORD, plaintext
	PasswordHash       string // FUTO_NOTES_PASSWORD_HASH, scrypt self-describing form
	CookieSecure       bool   // COOKIE_SECURE, Secure cookie flag; on unless "false"
	DevUI              bool   // DEV_UI, serves the dev test page at /dev when "true"
	BlobDir            string // BLOB_DIR, root of on-disk blob storage
	BlobGCEnabled      bool   // BLOB_GC_ENABLED, disables irreversible blob garbage collection when false
	AllowFreshDatabase bool   // ALLOW_FRESH_DATABASE, overrides the SQLite/blob safety guard
	DBPoolMax          int
	DBPoolIdleTimeout  time.Duration
	OIDCIssuer         string // OIDC_ISSUER, trailing slash trimmed
	OIDCClientID       string // OIDC_CLIENT_ID
	OIDCClientSecret   string // OIDC_CLIENT_SECRET
	OIDCRedirectURI    string // OIDC_REDIRECT_URI

	// Polar, the billing provider. Hosted mode is AUTH_MODE=oidc plus these
	// four; a server without them cannot tell an entitled account from a
	// lapsed one, so it refuses to boot rather than let everyone write.
	PolarBaseURL       string // POLAR_BASE_URL, the sandbox or production API
	PolarAccessToken   string // POLAR_ACCESS_TOKEN
	PolarProductID     string // POLAR_PRODUCT_ID, the subscribed product
	PolarWebhookSecret string // POLAR_WEBHOOK_SECRET, the Standard Webhooks signing secret

	// HOSTED_ALLOWED_EMAIL_DOMAINS, lowercased. Empty admits every identity
	// the provider authenticates; otherwise sign-in is limited to these
	// email domains. Internal-MVP gate, removed before public sign-up.
	HostedAllowedEmailDomains []string
}

func Load() (Config, error) {
	if err := loadDotenv(".env"); err != nil {
		return Config{}, fmt.Errorf("loading .env: %w", err)
	}

	cfg := Config{
		DatabaseURL:        getDefault("DATABASE_URL", "sqlite:./data/notes.db"),
		AuthMode:           getDefault("AUTH_MODE", "password"),
		Password:           os.Getenv("FUTO_NOTES_PASSWORD"),
		PasswordHash:       os.Getenv("FUTO_NOTES_PASSWORD_HASH"),
		CookieSecure:       os.Getenv("COOKIE_SECURE") != "false",
		DevUI:              os.Getenv("DEV_UI") == "true",
		BlobDir:            getDefault("BLOB_DIR", "./blobs"),
		BlobGCEnabled:      os.Getenv("BLOB_GC_ENABLED") != "false",
		AllowFreshDatabase: os.Getenv("ALLOW_FRESH_DATABASE") == "true",
		OIDCIssuer:         strings.TrimSuffix(os.Getenv("OIDC_ISSUER"), "/"),
		OIDCClientID:       os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:   os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURI:    os.Getenv("OIDC_REDIRECT_URI"),
		PolarBaseURL:       strings.TrimSuffix(os.Getenv("POLAR_BASE_URL"), "/"),
		PolarAccessToken:   os.Getenv("POLAR_ACCESS_TOKEN"),
		PolarProductID:     os.Getenv("POLAR_PRODUCT_ID"),
		PolarWebhookSecret: os.Getenv("POLAR_WEBHOOK_SECRET"),

		HostedAllowedEmailDomains: emailDomains(os.Getenv("HOSTED_ALLOWED_EMAIL_DOMAINS")),
	}
	if cfg.AuthMode != "dev" && cfg.AuthMode != "password" && cfg.AuthMode != "oidc" {
		return Config{}, fmt.Errorf("AUTH_MODE must be dev, password, or oidc, got %q", cfg.AuthMode)
	}
	if cfg.AuthMode == "password" {
		switch {
		case cfg.Password != "" && cfg.PasswordHash != "":
			return Config{}, fmt.Errorf("set only one of FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH when AUTH_MODE=password")
		case cfg.Password == "" && cfg.PasswordHash == "":
			return Config{}, fmt.Errorf("FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH is required when AUTH_MODE=password")
		}
	}
	if cfg.AuthMode == "oidc" {
		var missing []string
		if cfg.OIDCIssuer == "" {
			missing = append(missing, "OIDC_ISSUER")
		}
		if cfg.OIDCClientID == "" {
			missing = append(missing, "OIDC_CLIENT_ID")
		}
		if cfg.OIDCClientSecret == "" {
			missing = append(missing, "OIDC_CLIENT_SECRET")
		}
		if cfg.OIDCRedirectURI == "" {
			missing = append(missing, "OIDC_REDIRECT_URI")
		}
		if cfg.PolarBaseURL == "" {
			missing = append(missing, "POLAR_BASE_URL")
		}
		if cfg.PolarAccessToken == "" {
			missing = append(missing, "POLAR_ACCESS_TOKEN")
		}
		if cfg.PolarProductID == "" {
			missing = append(missing, "POLAR_PRODUCT_ID")
		}
		if cfg.PolarWebhookSecret == "" {
			missing = append(missing, "POLAR_WEBHOOK_SECRET")
		}
		if len(missing) > 0 {
			return Config{}, fmt.Errorf("%s required when AUTH_MODE=oidc", strings.Join(missing, ", "))
		}
		u, err := url.Parse(cfg.OIDCRedirectURI)
		if err != nil || !u.IsAbs() {
			return Config{}, fmt.Errorf("OIDC_REDIRECT_URI must be an absolute URL, got %q", cfg.OIDCRedirectURI)
		}
		if !strings.HasSuffix(u.Path, oidcCallbackPath) {
			return Config{}, fmt.Errorf("OIDC_REDIRECT_URI path must end in %s, got %q", oidcCallbackPath, cfg.OIDCRedirectURI)
		}
	}

	var err error
	if cfg.Port, err = getInt("PORT", 3005); err != nil {
		return Config{}, err
	}
	if strings.HasPrefix(cfg.DatabaseURL, "postgres://") || strings.HasPrefix(cfg.DatabaseURL, "postgresql://") {
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
		return "", fmt.Errorf("DATABASE_URL is required")
	}
	return databaseURL, nil
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

// AllowsEmailDomain reports whether an identity with this email address may
// sign in. An unset HOSTED_ALLOWED_EMAIL_DOMAINS admits everyone the
// provider authenticates; otherwise the address's domain has to be on the
// list. Both sides are lowercased, so the comparison ignores case.
func (c Config) AllowsEmailDomain(email string) bool {
	if len(c.HostedAllowedEmailDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	return slices.Contains(c.HostedAllowedEmailDomains, strings.ToLower(email[at+1:]))
}

// emailDomains parses HOSTED_ALLOWED_EMAIL_DOMAINS: comma-separated domains,
// lowercased, without surrounding whitespace or empty entries.
func emailDomains(value string) []string {
	var entries []string
	for entry := range strings.SplitSeq(value, ",") {
		if entry = strings.ToLower(strings.TrimSpace(entry)); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
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
