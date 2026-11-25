// Command mock-resend serves internal/mockresend for local development and
// compose: Resend's POST /emails, delivered over SMTP to Mailpit. Any bearer
// key is accepted. It is configured from the environment:
//
//	MOCK_RESEND_PORT       listen port (default 8093)
//	MOCK_RESEND_SMTP_ADDR  SMTP server host:port (default localhost:1025, Mailpit's)
//	LOG_LEVEL              trace, debug, info (default), warn, error,
//	                       fatal, panic or disabled
//	LOG_FORMAT             json or pretty (default json)
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hostedenv"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/mockresend"
)

func main() {
	hostedenv.ConfigureLogger(zerolog.InfoLevel, false)
	level, err := hostedenv.LogLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		log.Fatal().Err(err).Msg("invalid configuration")
	}
	pretty, err := hostedenv.LogPretty(os.Getenv("LOG_FORMAT"))
	if err != nil {
		log.Fatal().Err(err).Msg("invalid configuration")
	}
	hostedenv.ConfigureLogger(level, pretty)

	port, err := strconv.Atoi(envOr("MOCK_RESEND_PORT", "8093"))
	if err != nil || port < 1000 || port > 65535 {
		log.Fatal().Msg("MOCK_RESEND_PORT must be a number between 1000 and 65535")
	}
	smtpAddr := envOr("MOCK_RESEND_SMTP_ADDR", "localhost:1025")
	if _, _, err := net.SplitHostPort(smtpAddr); err != nil {
		log.Fatal().Err(err).Msg("MOCK_RESEND_SMTP_ADDR must be host:port")
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to listen")
	}
	handler := hlog.NewHandler(log.Logger)(hlog.AccessHandler(httputil.LogAccess)(mockresend.Handler(smtpAddr)))
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info().Int("port", port).Str("smtp", smtpAddr).Msg("starting mock-resend")
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
