package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func devEnv(t *testing.T) map[string]string {
	t.Helper()
	read := func(name string) string {
		data, err := os.ReadFile("../../../dev/hosted-keys/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	return map[string]string{
		"STORAGE_PORT":         "3015",
		"REGION_ID":            "us-east",
		"S3_ENDPOINT":          "http://localhost:9000",
		"S3_BUCKET":            "notes-us-east",
		"S3_ACCESS_KEY_ID":     "minio",
		"S3_SECRET_ACCESS_KEY": "miniominio",
		"API_PUBLIC_KEYS":      read("api.pub.pem"),
		"RECEIPT_SIGNING_KEY":  read("us-east.key.pem"),
	}
}

func parse(env map[string]string) (Config, error) {
	return Parse(func(k string) string { return env[k] })
}

func TestParseDefaults(t *testing.T) {
	cfg, err := parse(devEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3015 || cfg.RegionID != "us-east" || cfg.S3Region != "us-east-1" || cfg.S3ForcePathStyle {
		t.Fatalf("cfg %+v", cfg)
	}
	if cfg.DrainDelay != 15*time.Second || cfg.ShutdownTimeout != 120*time.Second || cfg.OTLPMetricsInterval != 15*time.Second {
		t.Fatalf("durations %v %v %v", cfg.DrainDelay, cfg.ShutdownTimeout, cfg.OTLPMetricsInterval)
	}
	if cfg.LogLevel != zerolog.InfoLevel || cfg.LogPretty || cfg.OTLPMetricsEndpoint != "" {
		t.Fatalf("logging/metrics %+v", cfg)
	}
	if kids := cfg.APIPublicKeys.KeyIDs(); len(kids) != 1 || kids[0] != "dev-api-1" {
		t.Fatalf("api kids %v", kids)
	}
	if cfg.ReceiptSigningKey.KeyID() != "dev-us-east-1" {
		t.Fatalf("receipt kid %q", cfg.ReceiptSigningKey.KeyID())
	}
}

func TestParseOverrides(t *testing.T) {
	env := devEnv(t)
	env["S3_REGION"] = "minio"
	env["S3_FORCE_PATH_STYLE"] = "true"
	env["DRAIN_DELAY_MS"] = "0"
	env["SHUTDOWN_TIMEOUT_MS"] = "5000"
	env["OTLP_METRICS_ENDPOINT"] = "localhost:8428"
	env["OTLP_METRICS_URL_PATH"] = "/opentelemetry/v1/metrics"
	env["OTLP_METRICS_INTERVAL_MS"] = "1000"
	env["LOG_LEVEL"] = "debug"
	env["LOG_FORMAT"] = "pretty"
	cfg, err := parse(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3Region != "minio" || !cfg.S3ForcePathStyle || cfg.DrainDelay != 0 || cfg.ShutdownTimeout != 5*time.Second ||
		cfg.OTLPMetricsEndpoint != "localhost:8428" || cfg.OTLPMetricsURLPath != "/opentelemetry/v1/metrics" ||
		cfg.OTLPMetricsInterval != time.Second || cfg.LogLevel != zerolog.DebugLevel || !cfg.LogPretty {
		t.Fatalf("cfg %+v", cfg)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	cases := map[string]struct {
		key, value, wantErr string
	}{
		"port missing":         {"STORAGE_PORT", "", "STORAGE_PORT is required"},
		"port too low":         {"STORAGE_PORT", "80", "STORAGE_PORT"},
		"port not a number":    {"STORAGE_PORT", "http", "STORAGE_PORT"},
		"region missing":       {"REGION_ID", "", "REGION_ID is required"},
		"region invalid":       {"REGION_ID", "US East", "REGION_ID"},
		"region is the api":    {"REGION_ID", "notes-api", "REGION_ID"},
		"endpoint missing":     {"S3_ENDPOINT", "", "S3_ENDPOINT is required"},
		"endpoint relative":    {"S3_ENDPOINT", "localhost:9000", "S3_ENDPOINT"},
		"bucket missing":       {"S3_BUCKET", "", "S3_BUCKET is required"},
		"access key missing":   {"S3_ACCESS_KEY_ID", "", "S3_ACCESS_KEY_ID is required"},
		"secret missing":       {"S3_SECRET_ACCESS_KEY", "", "S3_SECRET_ACCESS_KEY is required"},
		"path style":           {"S3_FORCE_PATH_STYLE", "yes please", "S3_FORCE_PATH_STYLE"},
		"public keys missing":  {"API_PUBLIC_KEYS", "", "API_PUBLIC_KEYS is required"},
		"public keys garbage":  {"API_PUBLIC_KEYS", "not pem", "API_PUBLIC_KEYS"},
		"signing key missing":  {"RECEIPT_SIGNING_KEY", "", "RECEIPT_SIGNING_KEY is required"},
		"signing key garbage":  {"RECEIPT_SIGNING_KEY", "not pem", "RECEIPT_SIGNING_KEY"},
		"negative drain":       {"DRAIN_DELAY_MS", "-1", "DRAIN_DELAY_MS"},
		"zero metric interval": {"OTLP_METRICS_INTERVAL_MS", "0", "OTLP_METRICS_INTERVAL_MS"},
		"log level":            {"LOG_LEVEL", "loud", "LOG_LEVEL"},
		"log format":           {"LOG_FORMAT", "xml", "LOG_FORMAT"},
	}
	for name, tc := range cases {
		env := devEnv(t)
		env[tc.key] = tc.value
		if _, err := parse(env); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err %v, want %q", name, err, tc.wantErr)
		}
	}

	env := devEnv(t)
	env["API_PUBLIC_KEYS"], env["RECEIPT_SIGNING_KEY"] = env["RECEIPT_SIGNING_KEY"], env["API_PUBLIC_KEYS"]
	if _, err := parse(env); err == nil {
		t.Error("swapped key kinds must fail")
	}
}
