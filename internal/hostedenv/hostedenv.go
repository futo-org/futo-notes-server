// Package hostedenv reads the environment settings the hosted binaries (api,
// storage, mock-oidc) share, and sets up their logger. Unlike the
// self-hosted server's internal/config, every invalid value is an error: a
// hosted pod refuses to start rather than run misconfigured.
package hostedenv

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Int reads key as a number of at least minimum, or fallback when unset.
func Int(getenv func(string) string, key string, fallback, minimum int) (int, error) {
	v := getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum {
		return 0, fmt.Errorf("%s must be a number >= %d, got %q", key, minimum, v)
	}
	return n, nil
}

// Millis reads key as a number of milliseconds of at least minimum, or
// fallback when unset.
func Millis(getenv func(string) string, key string, fallback time.Duration, minimum int) (time.Duration, error) {
	if getenv(key) == "" {
		return fallback, nil
	}
	n, err := Int(getenv, key, 0, minimum)
	return time.Duration(n) * time.Millisecond, err
}

// Bool reads key as a boolean, or fallback when unset.
func Bool(getenv func(string) string, key string, fallback bool) (bool, error) {
	v := getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, v)
	}
	return b, nil
}

// IsHTTPURL reports whether v is an absolute http or https URL.
func IsHTTPURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// LogLevel parses LOG_LEVEL: a zerolog level name (disabled turns logging
// off), info when empty.
func LogLevel(value string) (zerolog.Level, error) {
	if value == "" {
		return zerolog.InfoLevel, nil
	}
	level, err := zerolog.ParseLevel(value)
	if err != nil || level == zerolog.NoLevel {
		return 0, fmt.Errorf("LOG_LEVEL must be one of trace, debug, info, warn, error, fatal, panic, disabled; got %q", value)
	}
	return level, nil
}

// LogPretty parses LOG_FORMAT: json (the default) or pretty.
func LogPretty(value string) (bool, error) {
	switch value {
	case "", "json":
		return false, nil
	case "pretty":
		return true, nil
	default:
		return false, fmt.Errorf("LOG_FORMAT must be json or pretty, got %q", value)
	}
}

// ConfigureLogger points the global logger at stderr, as JSON with UTC
// RFC 3339 timestamps or, when pretty, as human-readable console output.
func ConfigureLogger(level zerolog.Level, pretty bool) {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	var output io.Writer = os.Stderr
	if pretty {
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	}
	log.Logger = zerolog.New(output).With().Timestamp().Caller().Logger()
	zerolog.SetGlobalLevel(level)
}
