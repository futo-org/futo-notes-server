// Command mock-oidc serves internal/mockoidc for local development and
// compose. It is configured from the environment:
//
//	MOCK_OIDC_PORT                       listen port (default 8092)
//	MOCK_OIDC_ISSUER                     external issuer URL (default http://localhost:8092)
//	MOCK_OIDC_CLIENT_ID                  confidential client id (default notes-dev)
//	MOCK_OIDC_CLIENT_SECRET              its secret (default notes-dev-secret)
//	MOCK_OIDC_REDIRECT_URIS              comma-separated, matched exactly
//	                                     (default http://localhost:3005/api/auth/oidc/callback)
//	MOCK_OIDC_POST_LOGOUT_REDIRECT_URIS  comma-separated (default http://localhost:3005/)
//	LOG_LEVEL                            zerolog level (default info)
//	LOG_FORMAT                           json or pretty (default json)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/mockoidc"
)

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }

	var output io.Writer = os.Stderr
	switch format := envOr("LOG_FORMAT", "json"); format {
	case "json":
	case "pretty":
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	default:
		fmt.Fprintf(os.Stderr, "LOG_FORMAT must be json or pretty, got %q\n", format)
		os.Exit(1)
	}
	log.Logger = zerolog.New(output).With().Timestamp().Caller().Logger()
	level, err := zerolog.ParseLevel(envOr("LOG_LEVEL", "info"))
	if err != nil {
		log.Fatal().Err(err).Msg("invalid LOG_LEVEL")
	}
	zerolog.SetGlobalLevel(level)

	port, err := strconv.Atoi(envOr("MOCK_OIDC_PORT", "8092"))
	if err != nil || port < 1000 || port > 65535 {
		log.Fatal().Msg("MOCK_OIDC_PORT must be a number between 1000 and 65535")
	}
	provider, err := mockoidc.New(mockoidc.Config{
		Issuer:                 envOr("MOCK_OIDC_ISSUER", "http://localhost:8092"),
		ClientID:               envOr("MOCK_OIDC_CLIENT_ID", "notes-dev"),
		ClientSecret:           envOr("MOCK_OIDC_CLIENT_SECRET", "notes-dev-secret"),
		RedirectURIs:           envList("MOCK_OIDC_REDIRECT_URIS", "http://localhost:3005/api/auth/oidc/callback"),
		PostLogoutRedirectURIs: envList("MOCK_OIDC_POST_LOGOUT_REDIRECT_URIS", "http://localhost:3005/"),
	})
	if err != nil {
		log.Fatal().Err(err).Msg("invalid configuration")
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to listen")
	}
	httpSrv := &http.Server{Handler: router(provider), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info().Int("port", port).Str("issuer", provider.Issuer()).Msg("starting mock-oidc")
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server error")
		}
	}()
	<-ctx.Done()
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("shutdown")
	}
	log.Info().Msg("shutdown complete")
}

func router(provider *mockoidc.Provider) http.Handler {
	r := chi.NewRouter()
	r.Use(hlog.NewHandler(log.Logger))
	r.Use(hlog.RequestIDHandler("request_id", "X-Request-Id"))
	r.Use(hlog.RemoteAddrHandler("remote_ip"))
	r.Use(hlog.UserAgentHandler("user_agent"))
	r.Use(hlog.AccessHandler(httputil.LogAccess))
	r.Use(chimw.Recoverer)
	r.Mount("/", provider.Handler())
	return r
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envList(key, fallback string) []string {
	var out []string
	for v := range strings.SplitSeq(envOr(key, fallback), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
