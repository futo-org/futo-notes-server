package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
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

type mutationIDs struct {
	Supported                bool   `json:"supported"`
	Required                 bool   `json:"required"`
	RetentionDays            int    `json:"retention_days"`
	SuccessfulCreateOutcomes string `json:"successful_create_outcomes"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing JSON response", "err", err)
	}
}

func handleCapability(authMode string) http.HandlerFunc {
	cap := capability{
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
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, cap)
	}
}

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
	if cfg.AuthMode == "dev" {
		api.HandleFunc("POST /api/auth/dev/login", handleDevLogin(cfg, database))
	}
	if cfg.AuthMode == "password" {
		api.HandleFunc("POST /api/auth/password/login",
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
	mux.HandleFunc("GET /{$}", handleCapability(cfg.AuthMode))
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

	cfg, err := config.Load()
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}
	for _, warning := range config.DroppedEnvWarnings() {
		slog.Warn(warning)
	}

	database, err := db.Open(cfg)
	if err != nil {
		slog.Error("opening database", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	applied, err := db.Migrate(ctx, database)
	cancel()
	if err != nil {
		slog.Error("applying database migrations", "err", err)
		os.Exit(1)
	}
	if len(applied) > 0 {
		slog.Info("applied database migrations", "count", len(applied), "migrations", strings.Join(applied, ", "))
	}
	blobStore := &blobs.Store{Dir: cfg.BlobDir}
	eventHub := events.NewHub()
	publisher := events.NewPublisher(database.Dialect(), eventHub)
	if database.Dialect().Engine() == db.Postgres {
		go events.Listen(context.Background(), cfg.DatabaseURL, eventHub)
	}
	go jobs.Run(context.Background(), database, blobStore, jobs.DefaultSchedule, cfg.BlobGCEnabled)

	addr := fmt.Sprintf(":%d", cfg.Port)
	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, routes(cfg, database, blobStore, eventHub, publisher)); err != nil {
		slog.Error("serving HTTP", "err", err)
		os.Exit(1)
	}
}
