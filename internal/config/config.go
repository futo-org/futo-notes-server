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

// OIDCCallbackPath is the route OIDC_REDIRECT_URI has to point at. The Login
// Hand-off login URL is built by swapping this route for its start sibling,
// so a redirect URI aimed anywhere else would produce a URL that 404s.
//
// It is exported because the same suffix is what a caller trims to recover
// the server's own public origin — the only origin the server is ever told.
// Two copies of this string that drifted would leave the boot check passing
// and the trim silently doing nothing.
const OIDCCallbackPath = "/api/auth/oidc/callback"

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

	// POLAR_DISCOUNT_ID, applied to every checkout while set. The internal
	// MVP points it at a 100%-forever discount so FUTO people pay nothing
	// while going through the real product's checkout; dropping the
	// variable is the launch flip, with no product-ID swap on the day.
	// Optional, so a server without it sells at list price.
	PolarDiscountID string

	// HOSTED_ALLOWED_EMAIL_DOMAINS, lowercased. Empty admits every identity
	// the provider authenticates; otherwise sign-in is limited to these
	// email domains. Internal-MVP gate, removed before public sign-up.
	HostedAllowedEmailDomains []string

	// STANDIN_MODE, a test-only mode that runs hosted mode against
	// in-process stand-ins for Zitadel and Polar. It exists so a client's
	// automated suite can drive the whole sign-up path on localhost with no
	// network and no credentials, and it must never be set anywhere real:
	// its login signs anyone in and its checkout entitles for free.
	//
	// This is the only place the variable is read. Everything the mode does
	// hangs off this one boolean, so a default server has no stand-in
	// anything: no listeners, no routes, no settings replaced.
	StandinMode bool
}

// standinRefusedEnv names the settings stand-in mode will not run alongside.
// Every one of them names a real identity provider, a real payment provider,
// or a real sign-up gate, so finding one set alongside STANDIN_MODE means the
// variable has reached a server that was configured to be real. That is the
// accident worth failing the boot over, so it is refused rather than
// overridden.
//
// Stand-in mode supplies its own value for eight of these. It supplies none
// for POLAR_DISCOUNT_ID or HOSTED_ALLOWED_EMAIL_DOMAINS — a stand-in checkout
// carries no discount and a stand-in server admits its one identity — and
// refuses them all the same, because what they name is just as real.
var standinRefusedEnv = []string{
	"OIDC_ISSUER",
	"OIDC_CLIENT_ID",
	"OIDC_CLIENT_SECRET",
	"OIDC_REDIRECT_URI",
	"POLAR_BASE_URL",
	"POLAR_ACCESS_TOKEN",
	"POLAR_PRODUCT_ID",
	"POLAR_WEBHOOK_SECRET",
	"POLAR_DISCOUNT_ID",
	"HOSTED_ALLOWED_EMAIL_DOMAINS",
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
		PolarDiscountID:    os.Getenv("POLAR_DISCOUNT_ID"),

		HostedAllowedEmailDomains: emailDomains(os.Getenv("HOSTED_ALLOWED_EMAIL_DOMAINS")),

		StandinMode: os.Getenv("STANDIN_MODE") == "true",
	}
	if cfg.StandinMode {
		if set := namesAlreadySet(standinRefusedEnv); len(set) > 0 {
			return Config{}, fmt.Errorf("STANDIN_MODE=true refuses to run alongside %s, because it replaces what they name with fakes; unset %s, or drop STANDIN_MODE if this server is meant to be real",
				strings.Join(standinRefusedEnv, ", "), strings.Join(set, ", "))
		}
		// Nothing below is overridden either. A self-hosted server whose
		// password login was quietly replaced by a stand-in that signs
		// anyone in is exactly the accident worth refusing to boot over,
		// and a server told to mark its cookies Secure is a server someone
		// put behind TLS.
		if mode, named := os.LookupEnv("AUTH_MODE"); named && mode != "" && mode != "oidc" {
			return Config{}, fmt.Errorf("STANDIN_MODE=true runs hosted mode, so AUTH_MODE must be oidc or unset, got %q", mode)
		}
		if secure, named := os.LookupEnv("COOKIE_SECURE"); named && secure != "false" {
			return Config{}, fmt.Errorf("STANDIN_MODE=true serves plain http on loopback, where a Secure cookie never comes back, so COOKIE_SECURE must be false or unset, got %q", secure)
		}
		// The stand-ins are hosted mode's two upstreams, so this is hosted
		// mode with fakes in their place.
		cfg.AuthMode = "oidc"
		cfg.CookieSecure = false
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
	// Stand-in mode is exempt: it has none of these yet, because the
	// stand-ins it points them at are not listening until the server starts
	// them.
	if cfg.Hosted() && !cfg.StandinMode {
		// Named in one table so the boot failure lists every missing value
		// at once rather than one per restart.
		required := []struct {
			name  string
			value string
		}{
			{"OIDC_ISSUER", cfg.OIDCIssuer},
			{"OIDC_CLIENT_ID", cfg.OIDCClientID},
			{"OIDC_CLIENT_SECRET", cfg.OIDCClientSecret},
			{"OIDC_REDIRECT_URI", cfg.OIDCRedirectURI},
			{"POLAR_BASE_URL", cfg.PolarBaseURL},
			{"POLAR_ACCESS_TOKEN", cfg.PolarAccessToken},
			{"POLAR_PRODUCT_ID", cfg.PolarProductID},
			{"POLAR_WEBHOOK_SECRET", cfg.PolarWebhookSecret},
		}
		var missing []string
		for _, setting := range required {
			if setting.value == "" {
				missing = append(missing, setting.name)
			}
		}
		if len(missing) > 0 {
			return Config{}, fmt.Errorf("%s required when AUTH_MODE=oidc", strings.Join(missing, ", "))
		}
		u, err := url.Parse(cfg.OIDCRedirectURI)
		if err != nil || !u.IsAbs() {
			return Config{}, fmt.Errorf("OIDC_REDIRECT_URI must be an absolute URL, got %q", cfg.OIDCRedirectURI)
		}
		if !strings.HasSuffix(u.Path, OIDCCallbackPath) {
			return Config{}, fmt.Errorf("OIDC_REDIRECT_URI path must end in %s, got %q", OIDCCallbackPath, cfg.OIDCRedirectURI)
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

// ListenAddr is the address the HTTP server binds. Stand-in mode binds
// loopback and nothing else: its login signs anyone in and its checkout
// entitles for free, so a server running with it must be unreachable across
// a network even when the variable has been set somewhere it should not be.
func (c Config) ListenAddr() string {
	if c.StandinMode {
		return fmt.Sprintf("127.0.0.1:%d", c.Port)
	}
	return fmt.Sprintf(":%d", c.Port)
}

// namesAlreadySet returns the names, in the order given, that are set to a
// non-empty value.
func namesAlreadySet(names []string) []string {
	var set []string
	for _, name := range names {
		if os.Getenv(name) != "" {
			set = append(set, name)
		}
	}
	return set
}

func getDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Hosted reports whether this is the hosted service rather than a self-hosted
// server. Hosted mode is AUTH_MODE=oidc plus Polar, and oidc mode will not
// boot without Polar, so the auth mode is the whole test — named here so the
// concept has one spelling rather than a string comparison at every site that
// asks.
func (c Config) Hosted() bool { return c.AuthMode == "oidc" }

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
