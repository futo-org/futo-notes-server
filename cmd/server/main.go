package main

import (
	"context"
	"encoding/json"
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
	"futo-notes-server/internal/oidc"
	"futo-notes-server/internal/polar"
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

func handleCapability(cfg config.Config) http.HandlerFunc {
	signup := "closed"
	if cfg.Hosted() {
		signup = "open"
	}
	cap := capability{
		Name:     serverName,
		Version:  serverVersion,
		AuthMode: cfg.AuthMode,
		Signup:   signup,
		// Only the hosted service sells subscriptions. The app shows the
		// sign-up path when it sees both this and an open signup.
		Billing: cfg.Hosted(),
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

func routes(cfg config.Config, database *db.DB, blobStore blobs.Store, eventHub *events.Hub, publisher events.Publisher, provider *oidc.Provider) http.Handler {
	// The Storage Quota is a hosted limit, so only the hosted deployment
	// refuses a write for being over it. The per-blob cap comes from the Plan
	// everywhere, where the one plan carries the value the server used as a
	// constant.
	quotaEnforced := cfg.Hosted()

	// Entitlement gates the routes that mutate collection contents or stage
	// blobs, plus the vault key write and the vault delete. Reads, pulls,
	// downloads, batch fetch, the SSE stream, capability, auth, hand-off,
	// pairing, and billing itself are never gated: a lapsed card must not
	// strand a device or hide the data someone already stored. Claiming the
	// vault stays open, because it writes collection identity rather than
	// contents and a lapsed account still has to find its collection to
	// pull from it. Device Pairing stays open for the same reason one step
	// further out: someone who stopped paying has to be able to set up a
	// device before they can read anything on it at all.
	entitled := requireEntitlement(cfg, database)

	api := http.NewServeMux()
	if cfg.AuthMode == "dev" {
		api.HandleFunc("POST /api/auth/dev/login", handleDevLogin(cfg, database))
	}
	if cfg.AuthMode == "password" {
		api.HandleFunc("POST /api/auth/password/login",
			rateLimited(auth.NewRateLimiter(), handlePasswordLogin(cfg, database)))
	}
	if cfg.Hosted() {
		// One Polar client per server, used by the checkout and portal
		// routes only; nothing on the sync path ever calls Polar.
		polarClient := &polar.Client{BaseURL: cfg.PolarBaseURL, AccessToken: cfg.PolarAccessToken}
		api.HandleFunc("GET /api/auth/oidc/start", handleOIDCStart(cfg, provider))
		api.HandleFunc("GET /api/auth/oidc/callback",
			rateLimited(auth.NewRateLimiter(), handleOIDCCallback(cfg, provider, database)))
		api.HandleFunc("POST /api/auth/handoff",
			rateLimited(auth.NewRateLimiter(), handleMintHandoff(cfg, database)))
		api.HandleFunc("GET /api/auth/handoff/{ticket}", handleRedeemHandoff(database))
		api.HandleFunc("GET /api/billing", handleBillingStatus(database))
		api.HandleFunc("POST /api/billing/checkout", handleBillingCheckout(cfg, database, polarClient))
		api.HandleFunc("GET /api/billing/portal", handleBillingPortal(database, polarClient))
	}
	api.HandleFunc("GET /api/auth", handleCurrentUser)
	api.HandleFunc("POST /api/auth/logout", handleLogout(database))
	api.HandleFunc("POST /api/collections", handleClaimCollection(database))
	api.HandleFunc("GET /api/collections", handleListCollections(database))
	api.HandleFunc("GET /api/collections/{id}", handleGetCollection(database))
	api.HandleFunc("DELETE /api/collections/{id}", entitled(handleDeleteCollection(database)))
	api.HandleFunc("GET /api/collections/{id}/key", handleGetCollectionKey(database))
	api.HandleFunc("PUT /api/collections/{id}/key", entitled(handlePutCollectionKey(database)))
	// Device Pairing is about vault keys rather than about how anyone logged
	// in, so it is mounted in every auth mode, exactly as the vault key
	// routes above are.
	api.HandleFunc("POST /api/pairings", handleCreatePairing(database))
	api.HandleFunc("GET /api/pairings/{id}", handleCollectPairing(database))
	api.HandleFunc("POST /api/pairings/{id}/key", handleDeliverPairingKey(database))
	api.HandleFunc("GET /api/collections/{id}/objects", handleListObjects(database))
	api.HandleFunc("POST /api/collections/{id}/objects", entitled(handleCreateObject(database, publisher, quotaEnforced)))
	api.HandleFunc("GET /api/collections/{id}/objects/{objectId}", handleGetObject(database))
	api.HandleFunc("PUT /api/collections/{id}/objects/{objectId}", entitled(handleUpdateObject(database, publisher, quotaEnforced)))
	api.HandleFunc("DELETE /api/collections/{id}/objects/{objectId}", entitled(handleDeleteObject(database, publisher)))
	api.HandleFunc("POST /api/collections/{id}/blob-objects", entitled(handleCreateBlobObject(database, blobStore, publisher, quotaEnforced)))
	api.HandleFunc("PUT /api/collections/{id}/blob-objects/{objectId}", entitled(handleUpdateBlobObject(database, blobStore, publisher, quotaEnforced)))
	api.HandleFunc("POST /api/collections/{id}/blob-objects/batch", entitled(handleBatchBlobObjects(database, blobStore, publisher, quotaEnforced)))
	api.HandleFunc("GET /api/collections/{id}/create-mutations/{mutationId}", handleRecoverCreate(database))
	api.HandleFunc("POST /api/blobs", entitled(handleUploadBlob(database, blobStore, quotaEnforced)))
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
	mux.HandleFunc("GET /{$}", handleCapability(cfg))
	mux.HandleFunc("GET /health", handleHealth(database))
	mux.Handle("/api/", requireAuth(cfg, database, api))
	var polarWebhook http.HandlerFunc
	if cfg.Hosted() {
		// Polar authenticates with a signature over the body rather than a
		// session, so the webhook is mounted outside requireAuth. The
		// literal pattern takes precedence over "/api/".
		polarWebhook = handlePolarWebhook(cfg, database)
		mux.HandleFunc("POST "+polarWebhookPath, polarWebhook)
		// Polar returns the browser here after a checkout. It holds no
		// session, so it sits outside requireAuth too.
		mux.HandleFunc("GET "+billingDonePath, handleBillingDone)
		mux.HandleFunc("GET "+accountPath, handleAccountPage)
		if cfg.StandinMode {
			// Stand-in mode is hosted mode, so this is the only place these
			// can be reached from — and only when the flag asked for them.
			registerStandinRoutes(mux, cfg, database, polarWebhook)
		}
	}
	if cfg.DevUI {
		mux.HandleFunc("GET /dev", handleDevUI(database, blobStore))
		mux.HandleFunc("POST /dev/panic", handleDevPanic)
		registerDevJobHandlers(mux, database, blobStore)
		mux.HandleFunc("POST /dev/plan", handleDevPlan(database))
		// Reports the lifetime of whichever session made the request, so the
		// page can show a browser cookie and an app's hand-off session side
		// by side. Behind requireAuth like the cards below it.
		mux.Handle("GET /dev/session", requireAuth(cfg, database, http.HandlerFunc(handleDevSession)))
		if polarWebhook != nil {
			// These two act for the signed-in account, so they go behind the
			// same middleware /api/ uses rather than reading the session
			// cookie themselves. Two patterns because one is the exact path
			// and the other is everything under it.
			devAPI := http.NewServeMux()
			devAPI.HandleFunc("POST /dev/subscription", handleDevSubscription(cfg, database, polarWebhook))
			devAPI.HandleFunc("GET /dev/subscription/last", handleDevLastWebhook(database))
			authenticated := requireAuth(cfg, database, devAPI)
			mux.Handle("/dev/subscription", authenticated)
			mux.Handle("/dev/subscription/", authenticated)
		}
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

	addr := cfg.ListenAddr()
	if cfg.StandinMode {
		// Before anything else opens: the stand-ins supply the settings the
		// rest of the boot reads, and the OIDC discovery below has to find
		// the stand-in issuer already listening.
		var stopStandins func()
		cfg, stopStandins, err = startStandins(cfg, "http://"+addr)
		if err != nil {
			slog.Error("starting the stand-ins", "err", err)
			os.Exit(1)
		}
		defer stopStandins()
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
	var blobStore blobs.Store = &blobs.FileStore{Dir: cfg.BlobDir}
	if cfg.S3() {
		s3Store := blobs.NewS3Store(blobs.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Bucket:          cfg.S3Bucket,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			Region:          cfg.S3Region,
		})
		// The same treatment as an unreachable OIDC issuer: a server that
		// cannot reach its blobs refuses to start rather than serve 500s.
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s3Store.CheckBucket(checkCtx)
		checkCancel()
		if err != nil {
			slog.Error("reaching the S3 bucket", "endpoint", cfg.S3Endpoint, "bucket", cfg.S3Bucket, "err", err)
			os.Exit(1)
		}
		blobStore = s3Store
	}
	eventHub := events.NewHub()
	publisher := events.NewPublisher(database.Dialect(), eventHub)
	if database.Dialect().Engine() == db.Postgres {
		go events.Listen(context.Background(), cfg.DatabaseURL, eventHub)
	}
	go jobs.Run(context.Background(), database, blobStore, jobs.DefaultSchedule, cfg.BlobGCEnabled)

	var provider *oidc.Provider
	if cfg.Hosted() {
		discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 10*time.Second)
		provider, err = oidc.Discover(discoverCtx, cfg.OIDCIssuer)
		discoverCancel()
		if err != nil {
			slog.Error("discovering OIDC provider", "issuer", cfg.OIDCIssuer, "err", err)
			os.Exit(1)
		}
	}

	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, routes(cfg, database, blobStore, eventHub, publisher, provider)); err != nil {
		slog.Error("serving HTTP", "err", err)
		os.Exit(1)
	}
}
