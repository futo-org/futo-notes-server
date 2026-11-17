package config_test

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/config"
)

func readDevKey(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "dev", "hosted-keys", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDevRegionsFileParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "dev", "hosted-regions.json"))
	if err != nil {
		t.Fatal(err)
	}
	regions, err := config.ParseRegions(data)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range regions {
		ids = append(ids, r.ID+"="+r.Endpoint)
	}
	if got := strings.Join(ids, " "); got != "us-east=http://localhost:3015 eu-central=http://localhost:3016 ap-southeast=http://localhost:3017" {
		t.Fatalf("dev regions = %s", got)
	}
}

func TestParseRegionsRejects(t *testing.T) {
	key := strings.ReplaceAll(readDevKey(t, "us-east.pub.pem"), "\n", `\n`)
	for name, data := range map[string]string{
		"empty":         `[]`,
		"unknown field": `[{"id":"us-east","name":"US","endpoint":"http://s","receipt_public_keys":"` + key + `","extra":1}]`,
		"no name":       `[{"id":"us-east","endpoint":"http://s","receipt_public_keys":"` + key + `"}]`,
		"bad endpoint":  `[{"id":"us-east","name":"US","endpoint":"s3://s","receipt_public_keys":"` + key + `"}]`,
		"no keys":       `[{"id":"us-east","name":"US","endpoint":"http://s","receipt_public_keys":""}]`,
		"bad id":        `[{"id":"US East","name":"US","endpoint":"http://s","receipt_public_keys":"` + key + `"}]`,
		"duplicate": `[{"id":"us-east","name":"US","endpoint":"http://s","receipt_public_keys":"` + key + `"},
			{"id":"us-east","name":"US","endpoint":"http://s","receipt_public_keys":"` + key + `"}]`,
	} {
		if _, err := config.ParseRegions([]byte(data)); err == nil {
			t.Errorf("%s: ParseRegions succeeded, want an error", name)
		}
	}
}

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"POSTGRES_HOST":                "db.internal",
		"POSTGRES_DATABASE":            "notes",
		"POSTGRES_USERNAME":            "notes",
		"POSTGRES_PASSWORD":            "p@ss/word",
		"PUBLIC_URL":                   "https://notes.example.com/",
		"OIDC_ISSUER":                  "https://id.example.com",
		"OIDC_CLIENT_ID":               "client",
		"OIDC_CLIENT_SECRET":           "secret",
		"REGIONS_FILE":                 filepath.Join("..", "..", "..", "dev", "hosted-regions.json"),
		"API_SIGNING_KEY":              readDevKey(t, "api.key.pem"),
		"STORAGE_REFRESH_ROTATION_KEY": readDevKey(t, "storage-refresh-rotation.key"),
	}
}

func getenv(env map[string]string) config.Getenv {
	return func(key string) string { return env[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load(getenv(validEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3005 || cfg.PublicURL != "https://notes.example.com" || !cfg.CookieSecure || cfg.DevUI ||
		cfg.DrainDelay != 15*time.Second || cfg.ShutdownTimeout != 60*time.Second || len(cfg.Regions) != 3 ||
		cfg.SigningKey.KeyID() != "dev-api-1" || len(cfg.RefreshRotationKey) != 32 || len(cfg.AppRedirectSchemes) != 0 || cfg.OTLPMetricsEndpoint != "" {
		t.Fatalf("config = %#v", cfg)
	}
	if cfg.Database.URL != "postgres://notes:p%40ss%2Fword@db.internal:5432/notes" || cfg.Database.PoolMax != 10 {
		t.Fatalf("database = %#v", cfg.Database)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, change := range map[string]map[string]string{
		"missing postgres":     {"POSTGRES_HOST": ""},
		"missing public url":   {"PUBLIC_URL": ""},
		"public url path":      {"PUBLIC_URL": "https://notes.example.com/app"},
		"missing oidc":         {"OIDC_CLIENT_SECRET": ""},
		"bad issuer":           {"OIDC_ISSUER": "id.example.com"},
		"missing regions":      {"REGIONS_FILE": ""},
		"unreadable regions":   {"REGIONS_FILE": "does-not-exist.json"},
		"missing signing key":  {"API_SIGNING_KEY": ""},
		"public signing key":   {"API_SIGNING_KEY": readDevKey(t, "api.pub.pem")},
		"missing rotation key": {"STORAGE_REFRESH_ROTATION_KEY": ""},
		"short rotation key":   {"STORAGE_REFRESH_ROTATION_KEY": "c2hvcnQ="},
		"rotation key not b64": {"STORAGE_REFRESH_ROTATION_KEY": "not base64!"},
		"http scheme":          {"APP_REDIRECT_SCHEMES": "org.futo.notes,https"},
		"bad scheme":           {"APP_REDIRECT_SCHEMES": "not a scheme"},
		"low port":             {"API_PORT": "80"},
		"bad bool":             {"COOKIE_SECURE": "maybe"},
		"bad log level":        {"LOG_LEVEL": "loud"},
		"bad log format":       {"LOG_FORMAT": "xml"},
		"bad drain delay":      {"DRAIN_DELAY_MS": "-1"},
		"bad resend url":       {"RESEND_API_URL": "api.resend.com"},
		"bad from address":     {"EMAIL_FROM_ADDRESS": "FUTO Notes"},
	} {
		env := validEnv(t)
		maps.Copy(env, change)
		if _, err := config.Load(getenv(env)); err == nil {
			t.Errorf("%s: Load succeeded, want an error", name)
		}
	}
}

func TestLoadEmail(t *testing.T) {
	cfg, err := config.Load(getenv(validEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Email.APIKey != "" || cfg.Email.APIURL != "https://api.resend.com" || cfg.Email.From != "FUTO Notes <noreply@notes.futo.cloud>" {
		t.Fatalf("email defaults = %#v", cfg.Email)
	}

	env := validEnv(t)
	maps.Copy(env, map[string]string{
		"RESEND_API_KEY":     " re_test \n",
		"RESEND_API_URL":     "http://localhost:8093/",
		"EMAIL_FROM_ADDRESS": "noreply@example.com",
	})
	if cfg, err = config.Load(getenv(env)); err != nil {
		t.Fatal(err)
	}
	if cfg.Email.APIKey != "re_test" || cfg.Email.APIURL != "http://localhost:8093" || cfg.Email.From != "noreply@example.com" {
		t.Fatalf("email = %#v", cfg.Email)
	}
}

func TestLoadRedirectSchemes(t *testing.T) {
	env := validEnv(t)
	env["APP_REDIRECT_SCHEMES"] = " org.futo.notes , Notes+Dev , tech.futo.notes.dev"
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.AppRedirectSchemes, ",") != "org.futo.notes,notes+dev,tech.futo.notes.dev" {
		t.Fatalf("schemes = %v", cfg.AppRedirectSchemes)
	}
}

func TestLoadBaseNeedsOnlyTheDatabase(t *testing.T) {
	base, err := config.LoadBase(getenv(map[string]string{
		"POSTGRES_HOST": "localhost", "POSTGRES_PORT": "15432", "POSTGRES_DATABASE": "notes",
		"POSTGRES_USERNAME": "postgres", "POSTGRES_PASSWORD": "postgres", "LOG_FORMAT": "pretty",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if base.Database.URL != "postgres://postgres:postgres@localhost:15432/notes" || !base.LogPretty {
		t.Fatalf("base = %#v", base)
	}
}

func TestLoadWebBilling(t *testing.T) {
	cfg, err := config.Load(getenv(validEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebBilling.Provider != "" || cfg.WebBilling.MockWebhookSecret != nil {
		t.Fatalf("web billing defaults = %#v", cfg.WebBilling)
	}

	env := validEnv(t)
	maps.Copy(env, map[string]string{
		"DEV_UI":                      "true",
		"BILLING_WEB_PROVIDER":        "mock",
		"BILLING_MOCK_WEBHOOK_SECRET": "dev-mock-webhook-secret",
	})
	if cfg, err = config.Load(getenv(env)); err != nil {
		t.Fatal(err)
	}
	if cfg.WebBilling.Provider != config.WebProviderMock || string(cfg.WebBilling.MockWebhookSecret) != "dev-mock-webhook-secret" {
		t.Fatalf("web billing = %#v", cfg.WebBilling)
	}

	for name, change := range map[string]map[string]string{
		"mock without DEV_UI": {"DEV_UI": "false"},
		"mock without secret": {"BILLING_MOCK_WEBHOOK_SECRET": ""},
		"short mock secret":   {"BILLING_MOCK_WEBHOOK_SECRET": "short"},
		"unknown provider":    {"BILLING_WEB_PROVIDER": "paypal"},
	} {
		bad := maps.Clone(env)
		maps.Copy(bad, change)
		if _, err := config.Load(getenv(bad)); err == nil {
			t.Errorf("%s: Load succeeded, want an error", name)
		}
	}
}
