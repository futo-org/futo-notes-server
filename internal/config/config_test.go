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

// setPolarEnv supplies the Polar configuration hosted mode requires, so a
// test about something else is not tripped by it.
func setPolarEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POLAR_BASE_URL", "https://sandbox-api.polar.sh")
	t.Setenv("POLAR_ACCESS_TOKEN", "polar_oat_test")
	t.Setenv("POLAR_PRODUCT_ID", "prod_test")
	t.Setenv("POLAR_WEBHOOK_SECRET", "whsec_cG9sYXItd2ViaG9vay1rZXk=")
}

func TestHostedModeRequiresThePolarConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://idp.example.test")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URI", "http://localhost:3005/api/auth/oidc/callback")

	err := errorFromLoad(t)
	// One error, naming every missing value, so an operator fixes them in one pass.
	for _, name := range []string{"POLAR_BASE_URL", "POLAR_ACCESS_TOKEN", "POLAR_PRODUCT_ID", "POLAR_WEBHOOK_SECRET"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("Load() error = %v, want it to name %s", err, name)
		}
	}

	setPolarEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with the Polar configuration set: %v", err)
	}
	if cfg.PolarBaseURL != "https://sandbox-api.polar.sh" || cfg.PolarProductID != "prod_test" {
		t.Fatalf("Polar configuration = %+v", cfg)
	}
}

func TestSelfHostedModesDoNotAskForPolar(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "sqlite:notes.db")
	t.Setenv("AUTH_MODE", "password")
	t.Setenv("FUTO_NOTES_PASSWORD", "correct password")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() in password mode asked for Polar configuration: %v", err)
	}
}

func errorFromLoad(t *testing.T) error {
	t.Helper()
	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want a configuration error")
	}
	return err
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
	setPolarEnv(t)

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
	setPolarEnv(t)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Load() error = %v, want absolute redirect URI error", err)
	}
}

func TestOIDCRedirectURIMustBeTheCallbackRoute(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://idp.example.test")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	setPolarEnv(t)

	// The Login Hand-off login URL is the callback's start sibling, so a
	// redirect URI aimed elsewhere would hand apps a URL that 404s.
	t.Setenv("OIDC_REDIRECT_URI", "https://notes.example.test/callback")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), OIDCCallbackPath) {
		t.Fatalf("Load() error = %v, want it to name %s", err, OIDCCallbackPath)
	}

	// A server mounted under a path prefix still passes.
	t.Setenv("OIDC_REDIRECT_URI", "https://notes.example.test/notes"+OIDCCallbackPath)
	if _, err := Load(); err != nil {
		t.Fatalf("Load() rejected a prefixed callback route: %v", err)
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

func TestHostedAllowedEmailDomains(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "dev")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HostedAllowedEmailDomains) != 0 {
		t.Fatalf("HostedAllowedEmailDomains = %q with the variable unset, want empty", cfg.HostedAllowedEmailDomains)
	}

	t.Setenv("HOSTED_ALLOWED_EMAIL_DOMAINS", " FUTO.org ,example.test,, ")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.HostedAllowedEmailDomains, []string{"futo.org", "example.test"}) {
		t.Fatalf("HostedAllowedEmailDomains = %q, want lowercased, trimmed, no empties", cfg.HostedAllowedEmailDomains)
	}
}

func TestAllowsEmailDomain(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed []string
		email   string
		want    bool
	}{
		{name: "unset admits everyone", email: "anyone@somewhere.test", want: true},
		{name: "unset admits a malformed address", email: "no-at-sign", want: true},
		{name: "on the list", allowed: []string{"futo.org"}, email: "person@futo.org", want: true},
		{name: "case is ignored", allowed: []string{"futo.org"}, email: "Person@FUTO.ORG", want: true},
		{name: "off the list", allowed: []string{"futo.org"}, email: "person@example.test", want: false},
		{name: "second entry counts", allowed: []string{"futo.org", "example.test"}, email: "person@example.test", want: true},
		{name: "no domain at all", allowed: []string{"futo.org"}, email: "person", want: false},
		// The domain is what follows the last @, so an address cannot smuggle
		// an allowed domain in ahead of its real one.
		{name: "domain is after the last at-sign", allowed: []string{"futo.org"}, email: "person@futo.org@example.test", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{HostedAllowedEmailDomains: test.allowed}
			if got := cfg.AllowsEmailDomain(test.email); got != test.want {
				t.Fatalf("AllowsEmailDomain(%q) with %q = %t, want %t", test.email, test.allowed, got, test.want)
			}
		})
	}
}

func TestStandinModeIsOffByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example.test/notes")
	t.Setenv("AUTH_MODE", "dev")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StandinMode {
		t.Fatal("StandinMode = true with STANDIN_MODE unset")
	}
	if cfg.ListenAddr() != ":3005" {
		t.Fatalf("ListenAddr() = %q, want every interface", cfg.ListenAddr())
	}

	// Anything other than the exact word fails closed.
	for _, value := range []string{"", "1", "yes", "TRUE", "on"} {
		t.Setenv("STANDIN_MODE", value)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StandinMode {
			t.Fatalf("STANDIN_MODE=%q turned the stand-ins on", value)
		}
	}
}

func TestStandinModeSuppliesItsOwnHostedSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "sqlite:notes.db")
	t.Setenv("STANDIN_MODE", "true")

	// None of the eight hosted settings are given, and hosted mode boots
	// anyway: the stand-ins are what fills them in.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() in stand-in mode: %v", err)
	}
	if !cfg.StandinMode {
		t.Fatal("StandinMode = false for STANDIN_MODE=true")
	}
	if cfg.AuthMode != "oidc" {
		t.Fatalf("AuthMode = %q, want oidc", cfg.AuthMode)
	}
	if cfg.CookieSecure {
		t.Fatal("CookieSecure = true: stand-in mode serves plain http on loopback")
	}
	if cfg.ListenAddr() != "127.0.0.1:3005" {
		t.Fatalf("ListenAddr() = %q, want loopback only", cfg.ListenAddr())
	}
}

func TestStandinModeRefusesSettingsThatNameSomethingReal(t *testing.T) {
	real := map[string]string{
		"OIDC_ISSUER":                  "https://futo.zitadel.cloud",
		"OIDC_CLIENT_ID":               "client-id",
		"OIDC_CLIENT_SECRET":           "client-secret",
		"OIDC_REDIRECT_URI":            "https://notes.example.test/api/auth/oidc/callback",
		"POLAR_BASE_URL":               "https://api.polar.sh",
		"POLAR_ACCESS_TOKEN":           "polar_oat_real",
		"POLAR_PRODUCT_ID":             "prod_real",
		"POLAR_WEBHOOK_SECRET":         "whsec_cG9sYXItd2ViaG9vay1rZXk=",
		"POLAR_DISCOUNT_ID":            "disc_real",
		"HOSTED_ALLOWED_EMAIL_DOMAINS": "futo.org",
	}
	for name, value := range real {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("DATABASE_URL", "sqlite:notes.db")
			t.Setenv("STANDIN_MODE", "true")
			t.Setenv(name, value)

			err := errorFromLoad(t)
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("Load() error = %v, want it to name %s", err, name)
			}
			if !strings.Contains(err.Error(), "STANDIN_MODE") {
				t.Fatalf("Load() error = %v, want it to name STANDIN_MODE", err)
			}
		})
	}
}

func TestStandinModeRefusesAnotherAuthMode(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "sqlite:notes.db")
	t.Setenv("STANDIN_MODE", "true")

	// Quietly replacing a self-hoster's password login with a stand-in that
	// signs anyone in is exactly the accident this refuses.
	t.Setenv("AUTH_MODE", "password")
	t.Setenv("FUTO_NOTES_PASSWORD", "correct password")
	err := errorFromLoad(t)
	if !strings.Contains(err.Error(), "AUTH_MODE") || !strings.Contains(err.Error(), "STANDIN_MODE") {
		t.Fatalf("Load() error = %v, want it to name both AUTH_MODE and STANDIN_MODE", err)
	}

	// Naming oidc explicitly is the same request, so it is allowed.
	t.Setenv("AUTH_MODE", "oidc")
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with AUTH_MODE=oidc and STANDIN_MODE=true: %v", err)
	}
}

func TestStandinModeRefusesSecureCookies(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "sqlite:notes.db")
	t.Setenv("STANDIN_MODE", "true")

	// A server told to mark its cookies Secure is a server someone put behind
	// TLS. Stand-in mode serves plain http on loopback, so silently turning
	// the flag off would be the same quiet override the refusals above exist
	// to prevent.
	t.Setenv("COOKIE_SECURE", "true")
	err := errorFromLoad(t)
	if !strings.Contains(err.Error(), "COOKIE_SECURE") || !strings.Contains(err.Error(), "STANDIN_MODE") {
		t.Fatalf("Load() error = %v, want it to name both COOKIE_SECURE and STANDIN_MODE", err)
	}

	t.Setenv("COOKIE_SECURE", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with COOKIE_SECURE=false and STANDIN_MODE=true: %v", err)
	}
	if cfg.CookieSecure {
		t.Fatal("CookieSecure = true for COOKIE_SECURE=false")
	}
}
