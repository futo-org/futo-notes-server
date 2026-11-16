// Command server runs the FUTO Notes sync server. With the migrate-to-sqlite
// subcommand it instead copies a Postgres database into SQLite and exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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
		slog.Error("writing JSON response", "err", err)
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
	api := http.NewServeMux()
	switch cfg.AuthMode {
	case config.AuthModeDev:
		api.HandleFunc("POST "+devLoginPath, handleDevLogin(cfg, database))
	case config.AuthModePassword:
		api.HandleFunc("POST "+passwordLoginPath,
			rateLimited(auth.NewRateLimiter(), handlePasswordLogin(cfg, database)))
	}
	api.HandleFunc("GET /api/auth", handleCurrentUser)
	api.HandleFunc("POST /api/auth/logout", handleLogout(database))
	api.HandleFunc("POST /api/collections", handleClaimCollection(database))
	api.HandleFunc("GET /api/collections", handleListCollections(database))
	api.HandleFunc("GET /api/collections/{id}", handleGetCollection(database))
	api.HandleFunc("DELETE /api/collections/{id}", handleDeleteCollection(database))
	api.HandleFunc("GET /api/collections/{id}/key", handleGetCollectionKey(database))
	api.HandleFunc("PUT /api/collections/{id}/key", handlePutCollectionKey(database))
	api.HandleFunc("GET /api/collections/{id}/objects", handleListObjects(database))
	api.HandleFunc("POST /api/collections/{id}/objects", handleCreateObject(database, publisher))
	api.HandleFunc("GET /api/collections/{id}/objects/{objectId}", handleGetObject(database))
	api.HandleFunc("PUT /api/collections/{id}/objects/{objectId}", handleUpdateObject(database, publisher))
	api.HandleFunc("DELETE /api/collections/{id}/objects/{objectId}", handleDeleteObject(database, publisher))
	api.HandleFunc("POST /api/collections/{id}/blob-objects", handleCreateBlobObject(database, blobStore, publisher))
	api.HandleFunc("PUT /api/collections/{id}/blob-objects/{objectId}", handleUpdateBlobObject(database, blobStore, publisher))
	api.HandleFunc("POST /api/collections/{id}/blob-objects/batch", handleBatchBlobObjects(database, blobStore, publisher))
	api.HandleFunc("GET /api/collections/{id}/create-mutations/{mutationId}", handleRecoverCreate(database))
	api.HandleFunc("POST /api/blobs", handleUploadBlob(database, blobStore))
	api.HandleFunc("POST /api/blobs/batch", handleBatchFetchBlobs(blobStore))
	api.HandleFunc("GET /api/blobs/{userId}/{blobId}", handleDownloadBlob(blobStore))
	api.HandleFunc("DELETE /api/blobs/{userId}/{blobId}", handleDeleteBlob(database, blobStore))
	api.HandleFunc("GET /api/sync/events", handleSyncEvents(eventHub))
	api.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 Not Found"))
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleCapability(string(cfg.AuthMode)))
	mux.HandleFunc("GET /health", handleHealth(database))
	mux.Handle("/api/", requireAuth(cfg, database, api))
	if cfg.DevUI {
		mux.HandleFunc("GET /dev", handleDevUI(database))
		mux.HandleFunc("POST /dev/panic", handleDevPanic)
		registerDevJobHandlers(mux, database, blobStore)
		slog.Info("dev UI enabled", "path", "/dev")
	}

	return recoverPanic(mux)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if len(os.Args) > 1 && os.Args[1] == "migrate-to-sqlite" {
		os.Exit(runMigrateToSQLite(os.Args[2:], os.Stdout, os.Stderr))
	}
	if err := run(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
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
	for _, warning := range config.DroppedEnvWarnings() {
		slog.Warn("ignoring legacy setting", "warning", warning)
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
		slog.Info("applied database migrations", "count", len(applied), "migrations", strings.Join(applied, ", "))
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

	slog.Info("listening", "addr", listener.Addr().String())
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

	slog.Info("shutting down")
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
