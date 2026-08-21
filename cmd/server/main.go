package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	serverName    = "futo-notes"
	serverVersion = "0.1.0"
)

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
		log.Printf("writeJSON: %v", err)
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

func handleHealth(database *sql.DB) http.HandlerFunc {
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

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	database, err := db.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	applied, err := db.Migrate(ctx, database)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	if len(applied) > 0 {
		log.Printf("applied %d migration(s): %s", len(applied), strings.Join(applied, ", "))
	}
	blobStore := &blobs.Store{Dir: cfg.BlobDir}
	eventHub := events.NewHub()
	go events.Listen(context.Background(), cfg.DatabaseURL, eventHub)
	go jobs.Run(context.Background(), database, blobStore, jobs.DefaultSchedule)

	api := http.NewServeMux()
	if cfg.AuthMode == "password" {
		api.HandleFunc("POST /api/auth/password/login",
			rateLimited(auth.NewRateLimiter(), handlePasswordLogin(cfg, database)))
	}
	api.HandleFunc("GET /api/auth", handleCurrentUser)
	api.HandleFunc("POST /api/auth/logout", handleLogout(cfg, database))
	api.HandleFunc("POST /api/collections", handleClaimCollection(database))
	api.HandleFunc("GET /api/collections", handleListCollections(database))
	api.HandleFunc("GET /api/collections/{id}", handleGetCollection(database))
	api.HandleFunc("DELETE /api/collections/{id}", handleDeleteCollection(database))
	api.HandleFunc("GET /api/collections/{id}/key", handleGetCollectionKey(database))
	api.HandleFunc("PUT /api/collections/{id}/key", handlePutCollectionKey(database))
	api.HandleFunc("GET /api/collections/{id}/objects", handleListObjects(database))
	api.HandleFunc("POST /api/collections/{id}/objects", handleCreateObject(database))
	api.HandleFunc("GET /api/collections/{id}/objects/{objectId}", handleGetObject(database))
	api.HandleFunc("PUT /api/collections/{id}/objects/{objectId}", handleUpdateObject(database))
	api.HandleFunc("DELETE /api/collections/{id}/objects/{objectId}", handleDeleteObject(database))
	api.HandleFunc("POST /api/collections/{id}/blob-objects", handleCreateBlobObject(database, blobStore))
	api.HandleFunc("PUT /api/collections/{id}/blob-objects/{objectId}", handleUpdateBlobObject(database, blobStore))
	api.HandleFunc("POST /api/collections/{id}/blob-objects/batch", handleBatchBlobObjects(database, blobStore))
	api.HandleFunc("GET /api/collections/{id}/create-mutations/{mutationId}", handleRecoverCreate(database))
	api.HandleFunc("POST /api/blobs", handleUploadBlob(database, blobStore))
	api.HandleFunc("POST /api/blobs/batch", handleBatchFetchBlobs(blobStore))
	api.HandleFunc("GET /api/blobs/{userId}/{blobId}", handleDownloadBlob(blobStore))
	api.HandleFunc("DELETE /api/blobs/{userId}/{blobId}", handleDeleteBlob(database, blobStore))
	api.HandleFunc("GET /api/sync/events", handleSyncEvents(eventHub))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleCapability(cfg.AuthMode))
	mux.HandleFunc("GET /health", handleHealth(database))
	mux.Handle("/api/", requireAuth(cfg, database, api))
	if cfg.DevUI {
		mux.HandleFunc("GET /dev", handleDevUI)
		registerDevJobHandlers(mux, database, blobStore)
		log.Print("dev UI enabled at /dev")
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
