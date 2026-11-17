// Package config reads the storage service's configuration from the
// environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hostedauth"
)

// Config is the storage service's configuration.
type Config struct {
	Port     int
	RegionID string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3ForcePathStyle  bool

	// APIPublicKeys verify the access and admin tokens the API mints.
	APIPublicKeys hostedauth.KeySet
	// ReceiptSigningKey is this region's key for upload receipts.
	ReceiptSigningKey hostedauth.SigningKey

	DrainDelay      time.Duration
	ShutdownTimeout time.Duration

	OTLPMetricsEndpoint string
	OTLPMetricsURLPath  string
	OTLPMetricsInterval time.Duration

	LogLevel  zerolog.Level
	LogPretty bool
}

// Well under vmagent's 1m idle timeout: exporting once a minute races the
// server closing the kept-alive connection, and some exports fail with EOF.
const defaultOTLPMetricsInterval = 15 * time.Second

// LoadConfig reads the configuration from the environment and exits the
// process when it is invalid.
func LoadConfig() Config {
	cfg, err := Parse(os.Getenv)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid configuration")
	}
	return cfg
}

// Parse reads the configuration through getenv.
func Parse(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	cfg := Config{
		Port:                p.port("STORAGE_PORT"),
		RegionID:            p.required("REGION_ID"),
		S3Endpoint:          p.url("S3_ENDPOINT"),
		S3Region:            p.or("S3_REGION", "us-east-1"),
		S3Bucket:            p.required("S3_BUCKET"),
		S3AccessKeyID:       p.required("S3_ACCESS_KEY_ID"),
		S3SecretAccessKey:   p.required("S3_SECRET_ACCESS_KEY"),
		S3ForcePathStyle:    p.bool("S3_FORCE_PATH_STYLE", false),
		APIPublicKeys:       p.keySet("API_PUBLIC_KEYS"),
		ReceiptSigningKey:   p.signingKey("RECEIPT_SIGNING_KEY"),
		DrainDelay:          p.durationMS("DRAIN_DELAY_MS", 15*time.Second, 0),
		ShutdownTimeout:     p.durationMS("SHUTDOWN_TIMEOUT_MS", 60*time.Second, 0),
		OTLPMetricsEndpoint: getenv("OTLP_METRICS_ENDPOINT"),
		OTLPMetricsURLPath:  getenv("OTLP_METRICS_URL_PATH"),
		// time.NewTicker panics on zero, so unlike the drain settings zero
		// cannot switch the exporter off.
		OTLPMetricsInterval: p.durationMS("OTLP_METRICS_INTERVAL_MS", defaultOTLPMetricsInterval, 1),
		LogLevel:            p.logLevel("LOG_LEVEL"),
		LogPretty:           p.logPretty("LOG_FORMAT"),
	}
	if p.err != nil {
		return Config{}, p.err
	}
	// The verifier and signer check the region id themselves; checking here
	// too makes a bad REGION_ID a configuration error, not a startup one.
	if _, err := hostedauth.NewReceiptSigner(cfg.ReceiptSigningKey, cfg.RegionID); err != nil {
		return Config{}, fmt.Errorf("REGION_ID: %w", err)
	}
	return cfg, nil
}

// parser records the first error so Parse reads as a flat list of keys.
type parser struct {
	getenv func(string) string
	err    error
}

func (p *parser) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

func (p *parser) or(key, fallback string) string {
	if v := p.getenv(key); v != "" {
		return v
	}
	return fallback
}

func (p *parser) required(key string) string {
	v := p.getenv(key)
	if v == "" {
		p.fail(fmt.Errorf("%s is required", key))
	}
	return v
}

func (p *parser) port(key string) int {
	v := p.required(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1000 || n > 65535 {
		p.fail(fmt.Errorf("%s must be a number between 1000 and 65535", key))
	}
	return n
}

func (p *parser) url(key string) string {
	v := p.required(key)
	if v == "" {
		return ""
	}
	if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.fail(fmt.Errorf("%s must be an absolute http(s) URL", key))
	}
	return v
}

func (p *parser) bool(key string, fallback bool) bool {
	v := p.getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(fmt.Errorf("%s must be true or false", key))
	}
	return b
}

func (p *parser) durationMS(key string, fallback time.Duration, minimum int) time.Duration {
	v := p.getenv(key)
	if v == "" {
		return fallback
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < minimum {
		p.fail(fmt.Errorf("%s must be a number >= %d", key, minimum))
	}
	return time.Duration(ms) * time.Millisecond
}

func (p *parser) keySet(key string) hostedauth.KeySet {
	v := p.required(key)
	if v == "" {
		return hostedauth.KeySet{}
	}
	keys, err := hostedauth.ParseKeySet([]byte(v))
	if err != nil {
		p.fail(fmt.Errorf("%s: %w", key, err))
	}
	return keys
}

func (p *parser) signingKey(key string) hostedauth.SigningKey {
	v := p.required(key)
	if v == "" {
		return hostedauth.SigningKey{}
	}
	k, err := hostedauth.ParseSigningKey([]byte(v))
	if err != nil {
		p.fail(fmt.Errorf("%s: %w", key, err))
	}
	return k
}

func (p *parser) logLevel(key string) zerolog.Level {
	v := p.getenv(key)
	if v == "" {
		return zerolog.InfoLevel
	}
	level, err := zerolog.ParseLevel(v)
	if err != nil {
		p.fail(fmt.Errorf("%s must be a valid level (trace, debug, info, warn, error, fatal, panic)", key))
	}
	return level
}

func (p *parser) logPretty(key string) bool {
	switch v := p.getenv(key); v {
	case "", "json":
		return false
	case "pretty":
		return true
	default:
		p.fail(errors.New(key + " must be 'json' or 'pretty'"))
		return false
	}
}
