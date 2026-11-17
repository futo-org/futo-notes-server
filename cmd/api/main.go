// Command api runs the hosted FUTO Notes API. `api migrate` instead applies
// the shared and hosted database migrations and exits; the server refuses to
// start while either has migrations pending.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/api"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/hosteddb"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/storageclient"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/jobs"
)

const (
	serviceName = "notes-api"
	// readHeaderTimeout bounds how long a client may take to send request
	// headers. There is no read or write timeout: sync event streams are
	// long-lived.
	readHeaderTimeout = 10 * time.Second
	migrationTimeout  = 5 * time.Minute
	startupTimeout    = 30 * time.Second
)

// Overridden by release builds with -X main.version=<tag>.
var version = "0.1.0-dev"

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	configureLogging(zerolog.InfoLevel, false)

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrate(); err != nil {
			log.Fatal().Err(err).Msg("migration failed")
		}
		return
	}
	if len(os.Args) > 1 {
		log.Fatal().Strs("args", os.Args[1:]).Msg("unknown arguments; usage: api [migrate]")
	}
	if err := run(); err != nil {
		log.Fatal().Err(err).Msg("api failed")
	}
}

func configureLogging(level zerolog.Level, pretty bool) {
	var output io.Writer = os.Stderr
	if pretty {
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	}
	log.Logger = zerolog.New(output).With().Timestamp().Caller().Logger()
	zerolog.SetGlobalLevel(level)
}

func openDatabase(cfg config.Database) (*db.DB, error) {
	return db.Open(cfg.URL, db.Options{PoolMax: cfg.PoolMax, PoolIdleTimeout: cfg.PoolIdleTimeout})
}

// migrate applies the shared migrations, then the hosted track.
func migrate() error {
	cfg, err := config.LoadBase(os.Getenv)
	if err != nil {
		return err
	}
	configureLogging(cfg.LogLevel, cfg.LogPretty)
	database, err := openDatabase(cfg.Database)
	if err != nil {
		return err
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), migrationTimeout)
	defer cancel()
	shared, err := db.Migrate(ctx, database)
	if err != nil {
		return fmt.Errorf("applying shared migrations: %w", err)
	}
	hosted, err := hosteddb.Migrate(ctx, database)
	if err != nil {
		return fmt.Errorf("applying hosted migrations: %w", err)
	}
	log.Info().Strs("shared", shared).Strs("hosted", hosted).Msg("migrations applied")
	return nil
}

// checkMigrations refuses a database with migrations pending in either
// track: the migration Job applies them before new pods start.
func checkMigrations(ctx context.Context, database *db.DB) error {
	shared, err := db.Pending(ctx, database)
	if err != nil {
		return fmt.Errorf("checking shared migrations: %w", err)
	}
	hosted, err := hosteddb.Pending(ctx, database)
	if err != nil {
		return fmt.Errorf("checking hosted migrations: %w", err)
	}
	if len(shared) > 0 || len(hosted) > 0 {
		return fmt.Errorf("database has pending migrations (shared: %s; hosted: %s); run `api migrate` first",
			strings.Join(shared, ", "), strings.Join(hosted, ", "))
	}
	return nil
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	configureLogging(cfg.LogLevel, cfg.LogPretty)

	// Bound before the slow startup work, so the startup probe sees the
	// process alive.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}

	database, err := openDatabase(cfg.Database)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer database.Close()
	startupCtx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	err = checkMigrations(startupCtx, database)
	cancel()
	if err != nil {
		return err
	}

	meterProvider, meter, err := setupMetrics(cfg)
	if err != nil {
		return fmt.Errorf("setting up metrics: %w", err)
	}
	metrics, err := api.NewMetrics(meter, database)
	if err != nil {
		return fmt.Errorf("creating metrics: %w", err)
	}

	storage, err := storageclient.New(cfg.Regions, hostedauth.NewMinter(cfg.SigningKey), meter)
	if err != nil {
		return fmt.Errorf("creating storage client: %w", err)
	}
	jobMetrics, err := hostedjobs.NewMetrics(meter)
	if err != nil {
		return fmt.Errorf("creating job metrics: %w", err)
	}
	regionIDs := make([]string, 0, len(cfg.Regions))
	for _, region := range cfg.Regions {
		regionIDs = append(regionIDs, region.ID)
	}

	hub := events.NewHub()
	server, err := api.New(api.Options{Config: cfg, Database: database, Hub: hub, Storage: storage, Metrics: metrics, Version: version})
	if err != nil {
		return err
	}

	background, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	var workers sync.WaitGroup
	workers.Go(func() { events.Listen(background, cfg.Database.URL, hub) })
	workers.Go(func() {
		hostedjobs.Run(background, database, jobs.DefaultSchedule,
			hostedjobs.Sessions(database, server.Accounts()), hostedjobs.Maintenance(database, storage, regionIDs, jobMetrics), metrics.JobRun)
	})

	httpSrv := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	// Open sync event streams never finish on their own; end them when the
	// listener closes so the shutdown wait covers only real requests.
	httpSrv.RegisterOnShutdown(hub.CloseAll)

	signals, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	served := make(chan error, 1)
	go func() {
		log.Info().Int("port", cfg.Port).Str("version", version).Msg("starting api")
		served <- httpSrv.Serve(listener)
	}()
	select {
	case err := <-served:
		return fmt.Errorf("serving HTTP: %w", err)
	case <-signals.Done():
	}
	// A second signal now kills the process instead of being swallowed.
	stopSignals()

	if err := server.Drain(httpSrv, cfg.DrainDelay, cfg.ShutdownTimeout); err != nil {
		log.Error().Err(err).Msg("shutdown deadline hit with requests still in flight")
	}
	stopBackground()
	workers.Wait()

	telemetryCtx, cancelTelemetry := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTelemetry()
	if err := meterProvider.Shutdown(telemetryCtx); err != nil {
		log.Error().Err(err).Msg("meter provider shutdown")
	}
	log.Info().Msg("shutdown complete")
	return nil
}

// meterShutdown is the part of a meter provider shutdown needs.
type meterShutdown interface {
	Shutdown(context.Context) error
}

// noopShutdown stands in for the provider when metrics are off.
type noopShutdown struct{}

func (noopShutdown) Shutdown(context.Context) error { return nil }

// setupMetrics returns an OTLP-exporting meter when OTLP_METRICS_ENDPOINT is
// set, and a no-op meter otherwise.
func setupMetrics(cfg config.Config) (meterShutdown, otelmetric.Meter, error) {
	if cfg.OTLPMetricsEndpoint == "" {
		return noopShutdown{}, noop.NewMeterProvider().Meter(serviceName), nil
	}
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.OTLPMetricsEndpoint), otlpmetrichttp.WithInsecure()}
	if cfg.OTLPMetricsURLPath != "" {
		opts = append(opts, otlpmetrichttp.WithURLPath(cfg.OTLPMetricsURLPath))
	}
	exporter, err := otlpmetrichttp.New(context.Background(), opts...)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	// Without service.instance.id every replica exports identical series,
	// which the TSDB merges and reads as counter resets.
	resource, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", version),
		attribute.String("service.instance.id", host),
	))
	if err != nil {
		resource = sdkresource.Default()
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(resource),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.OTLPMetricsInterval))),
	)
	log.Info().Str("endpoint", cfg.OTLPMetricsEndpoint).Msg("OpenTelemetry metrics enabled")
	return provider, provider.Meter(serviceName), nil
}
