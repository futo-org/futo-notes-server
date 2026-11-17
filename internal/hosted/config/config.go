// Package config reads the hosted API's configuration from the environment
// and its regions file. Load and LoadBase return an error naming the first
// invalid setting; the API refuses to start on any of them.
package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"futo-notes-server/internal/hosted/account"
	hostedmail "futo-notes-server/internal/hosted/mail"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/hostedenv"
)

// Base is the configuration every API command needs, migrate included.
type Base struct {
	Database  Database
	LogLevel  zerolog.Level
	LogPretty bool
}

// Database is how the API reaches Postgres. The connection settings arrive
// as CloudNativePG's <cluster>-app secret keys mapped to POSTGRES_*.
type Database struct {
	URL             string
	PoolMax         int           // DB_POOL_MAX
	PoolIdleTimeout time.Duration // DB_POOL_IDLE_TIMEOUT_MS
}

// Config is the configuration of the API server.
type Config struct {
	Base
	Port int // API_PORT
	// PublicURL is the external base URL, without a trailing slash. The OIDC
	// callback is PublicURL + "/api/auth/oidc/callback".
	PublicURL string
	OIDC      OIDC
	// AppRedirectSchemes are the custom URL schemes an app may name as its
	// redirect_uri, besides loopback http.
	AppRedirectSchemes []string
	Regions            []Region
	// SigningKey signs the access tokens storage services accept.
	SigningKey hostedauth.SigningKey
	// RefreshRotationKey derives each storage refresh token's successor
	// (STORAGE_REFRESH_ROTATION_KEY).
	RefreshRotationKey []byte
	// Email is how the API sends email; without RESEND_API_KEY it logs and
	// skips every send.
	Email hostedmail.Config
	// WebBilling is the web payment provider; none when its Provider is "".
	WebBilling   WebBilling
	CookieSecure bool
	DevUI        bool
	// DrainDelay is how long readiness fails before the listener closes, and
	// ShutdownTimeout how long in-flight requests may then take.
	DrainDelay      time.Duration
	ShutdownTimeout time.Duration
	// OTLPMetricsEndpoint is host:port; empty disables metrics.
	OTLPMetricsEndpoint string
	OTLPMetricsURLPath  string
	OTLPMetricsInterval time.Duration
}

// OIDC is the identity provider the API signs users in with, as a
// confidential client.
type OIDC struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// Region is a storage region from the regions file.
type Region struct {
	ID          string
	Name        string
	Description string
	// Endpoint is the base URL clients reach the region's storage service at.
	Endpoint string
	// ReceiptKeys are the public keys the region signs upload receipts with.
	ReceiptKeys hostedauth.KeySet
}

// Getenv is os.Getenv, or a stand-in for tests.
type Getenv func(string) string

var schemePattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// LoadBase reads the configuration the migrate command needs.
func LoadBase(getenv Getenv) (Base, error) {
	var base Base
	var err error
	if base.LogLevel, err = hostedenv.LogLevel(getenv("LOG_LEVEL")); err != nil {
		return Base{}, err
	}
	if base.LogPretty, err = hostedenv.LogPretty(getenv("LOG_FORMAT")); err != nil {
		return Base{}, err
	}
	if base.Database, err = loadDatabase(getenv); err != nil {
		return Base{}, err
	}
	return base, nil
}

// Load reads the API server's configuration, including the regions file.
func Load(getenv Getenv) (Config, error) {
	base, err := LoadBase(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Base: base}
	if cfg.Port, err = hostedenv.Int(getenv, "API_PORT", 3005, 1000); err != nil {
		return Config{}, err
	}
	if cfg.PublicURL, err = publicURL(getenv("PUBLIC_URL")); err != nil {
		return Config{}, err
	}
	if cfg.OIDC, err = loadOIDC(getenv); err != nil {
		return Config{}, err
	}
	if cfg.AppRedirectSchemes, err = redirectSchemes(getenv("APP_REDIRECT_SCHEMES")); err != nil {
		return Config{}, err
	}
	path := getenv("REGIONS_FILE")
	if path == "" {
		return Config{}, errors.New("REGIONS_FILE is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("REGIONS_FILE: %w", err)
	}
	if cfg.Regions, err = ParseRegions(data); err != nil {
		return Config{}, fmt.Errorf("REGIONS_FILE %s: %w", path, err)
	}
	signingKey := getenv("API_SIGNING_KEY")
	if signingKey == "" {
		return Config{}, errors.New("API_SIGNING_KEY is required")
	}
	if cfg.SigningKey, err = hostedauth.ParseSigningKey([]byte(signingKey)); err != nil {
		return Config{}, fmt.Errorf("API_SIGNING_KEY: %w", err)
	}
	if cfg.RefreshRotationKey, err = rotationKey(getenv("STORAGE_REFRESH_ROTATION_KEY")); err != nil {
		return Config{}, err
	}
	if cfg.Email, err = loadEmail(getenv); err != nil {
		return Config{}, err
	}
	if cfg.CookieSecure, err = hostedenv.Bool(getenv, "COOKIE_SECURE", true); err != nil {
		return Config{}, err
	}
	if cfg.DevUI, err = hostedenv.Bool(getenv, "DEV_UI", false); err != nil {
		return Config{}, err
	}
	if cfg.WebBilling, err = loadWebBilling(getenv, cfg.DevUI); err != nil {
		return Config{}, err
	}
	if cfg.DrainDelay, err = hostedenv.Millis(getenv, "DRAIN_DELAY_MS", 15*time.Second, 0); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = hostedenv.Millis(getenv, "SHUTDOWN_TIMEOUT_MS", 60*time.Second, 0); err != nil {
		return Config{}, err
	}
	cfg.OTLPMetricsEndpoint = getenv("OTLP_METRICS_ENDPOINT")
	cfg.OTLPMetricsURLPath = getenv("OTLP_METRICS_URL_PATH")
	// Well under vmagent's one-minute idle timeout, which a once-a-minute
	// export races.
	if cfg.OTLPMetricsInterval, err = hostedenv.Millis(getenv, "OTLP_METRICS_INTERVAL_MS", 15*time.Second, 1); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// regionFile is one entry of the regions file.
type regionFile struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Endpoint          string `json:"endpoint"`
	ReceiptPublicKeys string `json:"receipt_public_keys"`
}

// ParseRegions parses a regions file: a JSON array of regions, each with an
// id, name, description, storage endpoint, and receipt_public_keys, the PEM
// blocks (with kid headers) the region signs receipts with.
func ParseRegions(data []byte) ([]Region, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var entries []regionFile
	if err := dec.Decode(&entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("no regions")
	}
	regions := make([]Region, 0, len(entries))
	receiptKeys := map[string]hostedauth.KeySet{}
	for i, entry := range entries {
		if entry.ID == "" || entry.Name == "" {
			return nil, fmt.Errorf("region %d: id and name are required", i)
		}
		if slices.ContainsFunc(regions, func(r Region) bool { return r.ID == entry.ID }) {
			return nil, fmt.Errorf("duplicate region %q", entry.ID)
		}
		if !hostedenv.IsHTTPURL(entry.Endpoint) {
			return nil, fmt.Errorf("region %q: endpoint must be an http(s) URL", entry.ID)
		}
		keys, err := hostedauth.ParseKeySet([]byte(entry.ReceiptPublicKeys))
		if err != nil {
			return nil, fmt.Errorf("region %q: receipt_public_keys: %w", entry.ID, err)
		}
		receiptKeys[entry.ID] = keys
		regions = append(regions, Region{
			ID:          entry.ID,
			Name:        entry.Name,
			Description: entry.Description,
			Endpoint:    strings.TrimSuffix(entry.Endpoint, "/"),
			ReceiptKeys: keys,
		})
	}
	// The receipt verifier checks region ids the way tokens carry them.
	if _, err := hostedauth.NewReceiptVerifier(receiptKeys); err != nil {
		return nil, err
	}
	return regions, nil
}

func loadDatabase(getenv Getenv) (Database, error) {
	var missing []string
	for _, key := range []string{"POSTGRES_HOST", "POSTGRES_DATABASE", "POSTGRES_USERNAME", "POSTGRES_PASSWORD"} {
		if getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Database{}, fmt.Errorf("%s required", strings.Join(missing, ", "))
	}
	port, err := hostedenv.Int(getenv, "POSTGRES_PORT", 5432, 1)
	if err != nil {
		return Database{}, err
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(getenv("POSTGRES_USERNAME"), getenv("POSTGRES_PASSWORD")),
		Host:   net.JoinHostPort(getenv("POSTGRES_HOST"), strconv.Itoa(port)),
		Path:   "/" + getenv("POSTGRES_DATABASE"),
	}
	database := Database{URL: u.String()}
	if database.PoolMax, err = hostedenv.Int(getenv, "DB_POOL_MAX", 10, 1); err != nil {
		return Database{}, err
	}
	if database.PoolIdleTimeout, err = hostedenv.Millis(getenv, "DB_POOL_IDLE_TIMEOUT_MS", 10*time.Second, 0); err != nil {
		return Database{}, err
	}
	return database, nil
}

func loadOIDC(getenv Getenv) (OIDC, error) {
	o := OIDC{
		Issuer:       getenv("OIDC_ISSUER"),
		ClientID:     getenv("OIDC_CLIENT_ID"),
		ClientSecret: getenv("OIDC_CLIENT_SECRET"),
	}
	if o.Issuer == "" || o.ClientID == "" || o.ClientSecret == "" {
		return OIDC{}, errors.New("OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET are required")
	}
	if !hostedenv.IsHTTPURL(o.Issuer) {
		return OIDC{}, fmt.Errorf("OIDC_ISSUER must be an http(s) URL, got %q", o.Issuer)
	}
	return o, nil
}

// The email defaults. The From domain is a placeholder until the sending
// domain is confirmed (docs/Hosted deployment.md).
const (
	defaultResendAPIURL = "https://api.resend.com"
	defaultEmailFrom    = "FUTO Notes <noreply@notes.futo.cloud>"
)

func loadEmail(getenv Getenv) (hostedmail.Config, error) {
	cfg := hostedmail.Config{
		APIKey: strings.TrimSpace(getenv("RESEND_API_KEY")),
		APIURL: getenv("RESEND_API_URL"),
		From:   getenv("EMAIL_FROM_ADDRESS"),
	}
	if cfg.APIURL == "" {
		cfg.APIURL = defaultResendAPIURL
	}
	if !hostedenv.IsHTTPURL(cfg.APIURL) {
		return hostedmail.Config{}, fmt.Errorf("RESEND_API_URL must be an http(s) URL, got %q", cfg.APIURL)
	}
	cfg.APIURL = strings.TrimSuffix(cfg.APIURL, "/")
	if cfg.From == "" {
		cfg.From = defaultEmailFrom
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return hostedmail.Config{}, fmt.Errorf("EMAIL_FROM_ADDRESS must be an address such as %q: %w", defaultEmailFrom, err)
	}
	return cfg, nil
}

func publicURL(value string) (string, error) {
	if value == "" {
		return "", errors.New("PUBLIC_URL is required")
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("PUBLIC_URL must be an http(s) origin such as https://notes.example.com, got %q", value)
	}
	return strings.TrimSuffix(value, "/"), nil
}

// rotationKey parses STORAGE_REFRESH_ROTATION_KEY: standard base64 of
// account.RotationKeySize random bytes.
func rotationKey(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("STORAGE_REFRESH_ROTATION_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != account.RotationKeySize {
		return nil, fmt.Errorf("STORAGE_REFRESH_ROTATION_KEY must be the base64 of %d random bytes (openssl rand -base64 %d)",
			account.RotationKeySize, account.RotationKeySize)
	}
	return key, nil
}

// redirectSchemes parses APP_REDIRECT_SCHEMES. http and https are refused:
// loopback http is always allowed, and anything wider would let a login hand
// its code to any web page.
func redirectSchemes(value string) ([]string, error) {
	var schemes []string
	for scheme := range strings.SplitSeq(value, ",") {
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if scheme == "" {
			continue
		}
		if !schemePattern.MatchString(scheme) || scheme == "http" || scheme == "https" {
			return nil, fmt.Errorf("APP_REDIRECT_SCHEMES: %q is not a custom URL scheme", scheme)
		}
		schemes = append(schemes, scheme)
	}
	return schemes, nil
}
