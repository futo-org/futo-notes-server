// Command storage is one region's hosted blob storage service: it stores
// clients' ciphertext in the region's S3 bucket under API-signed access
// tokens and signs a receipt for every upload. See internal/storagesvc/config
// for its environment.
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
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"

	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/hostedmetrics"
	"futo-notes-server/internal/storagesvc/config"
	"futo-notes-server/internal/storagesvc/handlers"
	"futo-notes-server/internal/storagesvc/metrics"
	"futo-notes-server/internal/storagesvc/storage"
)

// Overridden by release builds with -X main.version=<tag>.
var version = "dev"

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }

	cfg := config.LoadConfig()

	var output io.Writer = os.Stderr
	if cfg.LogPretty {
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	}
	log.Logger = zerolog.New(output).With().Timestamp().Caller().Str("region", cfg.RegionID).Logger()
	zerolog.SetGlobalLevel(cfg.LogLevel)

	// Bind before the rest of startup so the tcpSocket startup probe sees the
	// process alive.
	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", addr).Msg("failed to bind listener")
	}

	meterProvider, err := hostedmetrics.NewProvider(hostedmetrics.Export{
		Endpoint: cfg.OTLPMetricsEndpoint, URLPath: cfg.OTLPMetricsURLPath, Interval: cfg.OTLPMetricsInterval,
	}, metrics.ServiceName, version, attribute.String("region", cfg.RegionID))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to set up meter provider")
	}
	m, err := metrics.NewMetrics(meterProvider.Meter(metrics.ServiceName))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create metrics")
	}

	verifier, err := hostedauth.NewVerifier(cfg.APIPublicKeys, cfg.RegionID)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create token verifier")
	}
	signer, err := hostedauth.NewReceiptSigner(cfg.ReceiptSigningKey, cfg.RegionID)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create receipt signer")
	}
	srv := &handlers.Server{
		Storage: storage.NewS3Storage(storage.S3Options{
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			Bucket:          cfg.S3Bucket,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			ForcePathStyle:  cfg.S3ForcePathStyle,
			Observer:        m,
		}),
		Verifier: verifier,
		Signer:   signer,
		Metrics:  m,
	}

	// No ReadTimeout or WriteTimeout: a 100 MiB upload or download on a slow
	// mobile link can legitimately take many minutes.
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info().Int("port", cfg.Port).Str("version", version).Str("bucket", cfg.S3Bucket).
			Strs("api_key_ids", cfg.APIPublicKeys.KeyIDs()).Str("receipt_key_id", cfg.ReceiptSigningKey.KeyID()).
			Msg("starting storage")
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	<-ctx.Done()
	// A second SIGTERM or SIGINT now kills the process instead of being
	// swallowed by the drain.
	stop()

	if err := srv.Drain(httpSrv, cfg.DrainDelay, cfg.ShutdownTimeout); err != nil {
		log.Error().Err(err).Msg("shutdown deadline hit with requests still in flight")
	}

	// Telemetry gets its own budget: a drain that used up its deadline would
	// otherwise leave the final export an expired context.
	telemetryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := meterProvider.Shutdown(telemetryCtx); err != nil {
		log.Error().Err(err).Msg("meter provider shutdown error")
	}
	log.Info().Msg("shutdown complete")
}
