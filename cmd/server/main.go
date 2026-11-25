// Command server runs the FUTO Notes sync server. With the migrate-to-sqlite
// subcommand it instead copies a Postgres database into SQLite and exits.
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/syncapi"
)

const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers. There is deliberately no read or write timeout: sync event
	// streams and large blob transfers are long-lived.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout bounds how long in-flight requests may finish after a
	// shutdown signal before their connections are closed.
	shutdownTimeout = 10 * time.Second
	// migrationTimeout bounds startup schema migrations.
	migrationTimeout = 60 * time.Second
)

// Overridden by release builds with -X main.serverVersion=<tag>.
var serverVersion = "0.1.0"

// healthStatus is the body of GET /health.
type healthStatus struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

func handleHealth(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.PingContext(ctx); err != nil {
			syncapi.WriteJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "degraded", DB: "unreachable"})
			return
		}
		syncapi.WriteJSON(w, http.StatusOK, healthStatus{Status: "ok", DB: "connected"})
	}
}

func routes(cfg config.Config, database *db.DB, blobStore *blobs.Store, eventHub *events.Hub, publisher events.Publisher) http.Handler {
	api := chi.NewRouter()
	api.NotFound(apiCatchAll)
	api.MethodNotAllowed(apiCatchAll)
	api.Use(func(next http.Handler) http.Handler { return requireAuth(cfg, database, next) })
	endpoints := api.With(decodePathValues)
	switch cfg.AuthMode {
	case config.AuthModeDev:
		endpoints.Post(strings.TrimPrefix(devLoginPath, "/api"), handleDevLogin(cfg, database))
	case config.AuthModePassword:
		endpoints.Post(strings.TrimPrefix(passwordLoginPath, "/api"),
			rateLimited(auth.NewRateLimiter(), handlePasswordLogin(cfg, database)))
	}
	handleGet(endpoints, "/auth", syncapi.HandleCurrentUser)
	endpoints.Post("/auth/logout", syncapi.HandleLogout(database))
	endpoints.Post("/collections", syncapi.HandleClaimCollection(database))
	handleGet(endpoints, "/collections", syncapi.HandleListCollections(database))
	handleGet(endpoints, "/collections/{id}", syncapi.HandleGetCollection(database))
	endpoints.Delete("/collections/{id}", syncapi.HandleDeleteCollection(database))
	handleGet(endpoints, "/collections/{id}/key", syncapi.HandleGetCollectionKey(collections.Keys{DB: database}))
	endpoints.Put("/collections/{id}/key", syncapi.HandlePutCollectionKey(collections.Keys{DB: database}))
	handleGet(endpoints, "/collections/{id}/objects", syncapi.HandleListObjects(database))
	endpoints.Post("/collections/{id}/objects", syncapi.HandleCreateObject(database, publisher, stagedBlobs{}))
	handleGet(endpoints, "/collections/{id}/objects/{objectId}", syncapi.HandleGetObject(database))
	endpoints.Put("/collections/{id}/objects/{objectId}", syncapi.HandleUpdateObject(database, publisher, stagedBlobs{}))
	endpoints.Delete("/collections/{id}/objects/{objectId}", syncapi.HandleDeleteObject(database, publisher))
	endpoints.Post("/collections/{id}/blob-objects", handleCreateBlobObject(database, blobStore, publisher))
	endpoints.Put("/collections/{id}/blob-objects/{objectId}", handleUpdateBlobObject(database, blobStore, publisher))
	endpoints.Post("/collections/{id}/blob-objects/batch", handleBatchBlobObjects(database, blobStore, publisher))
	handleGet(endpoints, "/collections/{id}/create-mutations/{mutationId}", syncapi.HandleRecoverCreate(database))
	endpoints.Post("/blobs", handleUploadBlob(database, blobStore))
	endpoints.Post("/blobs/batch", handleBatchFetchBlobs(blobStore))
	handleGet(endpoints, "/blobs/{userId}/{blobId}", handleDownloadBlob(blobStore))
	endpoints.Delete("/blobs/{userId}/{blobId}", handleDeleteBlob(database, blobStore))
	handleGet(endpoints, "/sync/events", syncapi.HandleSyncEvents(eventHub))

	router := chi.NewRouter()
	router.Use(
		hlog.NewHandler(log.Logger),
		hlog.RequestIDHandler("request_id", "X-Request-Id"),
		hlog.RemoteAddrHandler("remote_ip"),
		hlog.UserAgentHandler("user_agent"),
		hlog.AccessHandler(logAccess),
		syncapi.RecoverPanic,
		matchServeMux,
	)
	router.NotFound(http.NotFound)
	router.MethodNotAllowed(serveMuxMethodNotAllowed(router))
	handleGet(router, "/", syncapi.HandleCapability(syncapi.NewCapability(serverVersion, string(cfg.AuthMode), "closed")))
	handleGet(router, "/health", handleHealth(database))
	router.Mount("/api", api)
	if cfg.DevUI {
		handleGet(router, "/dev", handleDevUI(database))
		router.Post("/dev/panic", handleDevPanic)
		registerDevJobHandlers(router, database, blobStore)
		log.Info().Str("path", "/dev").Msg("dev UI enabled")
	}

	return router
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	configureLogging(zerolog.InfoLevel, true)
	if len(os.Args) > 1 && os.Args[1] == "migrate-to-sqlite" {
		os.Exit(runMigrateToSQLite(os.Args[2:], os.Stdout, os.Stderr))
	}
	if err := run(); err != nil {
		log.Fatal().Err(err).Msg("server failed")
	}
}

// configureLogging points the global logger at stderr, as JSON or, when
// pretty, as human-readable console output, colored only on a terminal so
// `docker logs` redirected to a file stays free of escape codes.
func configureLogging(level zerolog.Level, pretty bool) {
	var output io.Writer = os.Stderr
	if pretty {
		info, err := os.Stderr.Stat()
		terminal := err == nil && info.Mode()&os.ModeCharDevice != 0
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339, NoColor: !terminal}
	}
	log.Logger = zerolog.New(output).With().Timestamp().Caller().Logger()
	zerolog.SetGlobalLevel(level)
}

// run starts the server and blocks until SIGINT or SIGTERM has shut it down
// or it fails.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second one
	// terminates a shutdown that is taking too long.
	context.AfterFunc(ctx, stop)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	configureLogging(cfg.LogLevel, cfg.LogPretty)
	for _, warning := range config.DroppedEnvWarnings() {
		log.Warn().Str("warning", warning).Msg("ignoring legacy setting")
	}

	database, err := db.Open(cfg.DatabaseURL, db.Options{
		BlobDir:         cfg.BlobDir,
		AllowFresh:      cfg.AllowFreshDatabase,
		PoolMax:         cfg.DBPoolMax,
		PoolIdleTimeout: cfg.DBPoolIdleTimeout,
	})
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer database.Close()

	migrateCtx, cancel := context.WithTimeout(ctx, migrationTimeout)
	applied, err := db.Migrate(migrateCtx, database)
	cancel()
	if err != nil {
		return fmt.Errorf("applying database migrations: %w", err)
	}
	if len(applied) > 0 {
		log.Info().Int("count", len(applied)).Str("migrations", strings.Join(applied, ", ")).Msg("applied database migrations")
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}

	blobStore := &blobs.Store{Dir: cfg.BlobDir}
	eventHub := events.NewHub()
	publisher := events.NewPublisher(database.Dialect(), eventHub)

	// Background work stops with the server, before the database closes.
	var background sync.WaitGroup
	defer background.Wait()
	ctx, cancelBackground := context.WithCancel(ctx)
	defer cancelBackground()
	if database.Dialect().Engine() == db.Postgres {
		background.Go(func() { events.Listen(ctx, cfg.DatabaseURL, eventHub) })
	}
	background.Go(func() { jobs.Run(ctx, database, blobStore, jobs.DefaultSchedule, cfg.BlobGCEnabled) })

	log.Info().Str("addr", listener.Addr().String()).Msg("listening")
	return serve(ctx, listener, routes(cfg, database, blobStore, eventHub, publisher), eventHub.CloseAll)
}

// serve answers HTTP requests on listener until ctx is done, then shuts down
// gracefully: it stops accepting connections, calls endStreams so open sync
// event streams return, and waits up to shutdownTimeout for in-flight requests
// before closing the connections that remain.
func serve(ctx context.Context, listener net.Listener, handler http.Handler, endStreams func()) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	server.RegisterOnShutdown(endStreams)

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("serving HTTP: %w", err)
	case <-ctx.Done():
	}

	log.Info().Msg("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.Join(fmt.Errorf("shutting down HTTP server: %w", err), server.Close())
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}
	return nil
}
