// Command server runs the FUTO Notes sync server. With the migrate-to-sqlite
// subcommand it instead copies a Postgres database into SQLite and exits.
package main

import (
	"context"
	"encoding/json"
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
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/jobs"
)

const (
	serverName = "futo-notes"

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

// capability is the discovery document served at GET /. Clients call it once
// when adding a server, to learn which login flow to drive and whether
// retry-safe mutations exist.
type capability struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	AuthMode    string      `json:"auth_mode"`
	Signup      string      `json:"signup"`
	Billing     bool        `json:"billing"`
	MutationIDs mutationIDs `json:"mutation_ids"`
}

// mutationIDs describes the server's Mutation-Id support in the capability
// document.
type mutationIDs struct {
	Supported                bool   `json:"supported"`
	Required                 bool   `json:"required"`
	RetentionDays            int    `json:"retention_days"`
	SuccessfulCreateOutcomes string `json:"successful_create_outcomes"`
}

// errorResponse is the JSON body of every API error.
type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error().Err(err).Msg("writing JSON response")
	}
}

func handleCapability(authMode string) http.HandlerFunc {
	document := capability{
		Name:     serverName,
		Version:  serverVersion,
		AuthMode: authMode,
		Signup:   "closed",
		Billing:  false,
		MutationIDs: mutationIDs{
			Supported:                true,
			Required:                 false,
			RetentionDays:            30,
			SuccessfulCreateOutcomes: "durable",
		},
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, document)
	}
}

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
			writeJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "degraded", DB: "unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, healthStatus{Status: "ok", DB: "connected"})
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
	handleGet(endpoints, "/auth", handleCurrentUser)
	endpoints.Post("/auth/logout", handleLogout(database))
	endpoints.Post("/collections", handleClaimCollection(database))
	handleGet(endpoints, "/collections", handleListCollections(database))
	handleGet(endpoints, "/collections/{id}", handleGetCollection(database))
	endpoints.Delete("/collections/{id}", handleDeleteCollection(database))
	handleGet(endpoints, "/collections/{id}/key", handleGetCollectionKey(database))
	endpoints.Put("/collections/{id}/key", handlePutCollectionKey(database))
	handleGet(endpoints, "/collections/{id}/objects", handleListObjects(database))
	endpoints.Post("/collections/{id}/objects", handleCreateObject(database, publisher))
	handleGet(endpoints, "/collections/{id}/objects/{objectId}", handleGetObject(database))
	endpoints.Put("/collections/{id}/objects/{objectId}", handleUpdateObject(database, publisher))
	endpoints.Delete("/collections/{id}/objects/{objectId}", handleDeleteObject(database, publisher))
	endpoints.Post("/collections/{id}/blob-objects", handleCreateBlobObject(database, blobStore, publisher))
	endpoints.Put("/collections/{id}/blob-objects/{objectId}", handleUpdateBlobObject(database, blobStore, publisher))
	endpoints.Post("/collections/{id}/blob-objects/batch", handleBatchBlobObjects(database, blobStore, publisher))
	handleGet(endpoints, "/collections/{id}/create-mutations/{mutationId}", handleRecoverCreate(database))
	endpoints.Post("/blobs", handleUploadBlob(database, blobStore))
	endpoints.Post("/blobs/batch", handleBatchFetchBlobs(blobStore))
	handleGet(endpoints, "/blobs/{userId}/{blobId}", handleDownloadBlob(blobStore))
	endpoints.Delete("/blobs/{userId}/{blobId}", handleDeleteBlob(database, blobStore))
	handleGet(endpoints, "/sync/events", handleSyncEvents(eventHub))

	router := chi.NewRouter()
	router.Use(
		hlog.NewHandler(log.Logger),
		hlog.RequestIDHandler("request_id", "X-Request-Id"),
		hlog.RemoteAddrHandler("remote_ip"),
		hlog.UserAgentHandler("user_agent"),
		hlog.AccessHandler(logAccess),
		recoverPanic,
		matchServeMux,
	)
	router.NotFound(http.NotFound)
	router.MethodNotAllowed(serveMuxMethodNotAllowed(router))
	handleGet(router, "/", handleCapability(string(cfg.AuthMode)))
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
	configureLogging(zerolog.InfoLevel, false)
	if len(os.Args) > 1 && os.Args[1] == "migrate-to-sqlite" {
		os.Exit(runMigrateToSQLite(os.Args[2:], os.Stdout, os.Stderr))
	}
	if err := run(); err != nil {
		log.Fatal().Err(err).Msg("server failed")
	}
}

// configureLogging points the global logger at stderr, as JSON or, when
// pretty, as human-readable console output.
func configureLogging(level zerolog.Level, pretty bool) {
	var output io.Writer = os.Stderr
	if pretty {
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
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
