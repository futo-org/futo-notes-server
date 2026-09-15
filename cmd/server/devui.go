package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"html"
	"net/http"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/subscriptions"
	"futo-notes-server/internal/usage"
)

//go:embed devui.html
var devUIHTML []byte

func handleDevUI(database *db.DB) http.HandlerFunc {
	engine := string(database.Dialect().Engine())
	if database.Dialect().Path() != "" {
		engine += ": " + database.Dialect().Path()
	}
	body := bytes.ReplaceAll(devUIHTML, []byte("{{DATABASE_INFO}}"), []byte(html.EscapeString(engine)))
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}
}

func handleDevPanic(http.ResponseWriter, *http.Request) {
	panic("intentional dev panic")
}

func registerDevJobHandlers(mux *http.ServeMux, database *db.DB, store *blobs.Store) {
	mux.HandleFunc("POST /dev/jobs/sessions", handleDevJob(func(ctx context.Context) (jobs.SessionReapResult, error) {
		return jobs.ReapSessions(ctx, database)
	}))
	mux.HandleFunc("POST /dev/jobs/reconciliation", handleDevJob(func(ctx context.Context) (jobs.ReconciliationResult, error) {
		return jobs.ReconcileStorage(ctx, database, store)
	}))
	mux.HandleFunc("POST /dev/jobs/mutation-results", handleDevJob(func(ctx context.Context) (jobs.MutationExpiryResult, error) {
		return jobs.ExpireMutationResults(ctx, database)
	}))
	mux.HandleFunc("POST /dev/jobs/blob-gc", handleDevJob(func(ctx context.Context) (jobs.BlobGCResult, error) {
		return jobs.GarbageCollectBlobs(ctx, database, store)
	}))
	mux.HandleFunc("POST /dev/jobs/login-handoffs", handleDevJob(func(ctx context.Context) (jobs.HandoffReapResult, error) {
		return jobs.ReapLoginHandoffs(ctx, database)
	}))
	mux.HandleFunc("POST /dev/jobs/device-pairings", handleDevJob(func(ctx context.Context) (jobs.PairingReapResult, error) {
		return jobs.ReapDevicePairings(ctx, database)
	}))
	mux.HandleFunc("POST /dev/jobs/usage", handleDevJob(func(ctx context.Context) (usage.ReconcileResult, error) {
		return usage.Reconcile(ctx, database)
	}))
}

// handleDevPlan rewrites the one plan's Storage Quota so the /dev quota card
// can reach a ceiling without uploading ten gigabytes. Dev UI only.
func handleDevPlan(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StorageQuotaBytes *int64 `json:"storage_quota_bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.StorageQuotaBytes == nil || *body.StorageQuotaBytes < 0 {
			writeError(w, http.StatusBadRequest, "invalid storage_quota_bytes")
			return
		}
		if err := setStorageQuota(r.Context(), database, *body.StorageQuotaBytes); err != nil {
			serverError(w, r, "setting the dev plan quota", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"storage_quota_bytes": *body.StorageQuotaBytes})
	}
}

func handleDevJob[T any](run func(context.Context) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := run(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, struct {
				Result T      `json:"result"`
				Error  string `json:"error"`
			}{Result: result, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// handleDevSubscription is the /dev card's stand-in for Polar: it signs a
// subscription event for the signed-in account and puts it through the real
// webhook route, so the card demonstrates the whole path rather than a
// shortcut around it. Dev UI only, and only in hosted mode.
//
// Mounted behind requireAuth, so sessionFrom holds the account it acts for.
func handleDevSubscription(cfg config.Config, database *db.DB, webhook http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.Type == "" || body.Status == "" {
			writeError(w, http.StatusBadRequest, "type and status are required")
			return
		}
		deliverSignedSubscription(w, r, cfg, database, webhook, body.Type, body.Status)
	}
}

// handleDevLastWebhook shows the delivery the subscription record was last
// written from, so the /dev card can display what Polar actually said rather
// than only what the server concluded. Dev UI only, behind requireAuth.
func handleDevLastWebhook(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record, err := subscriptions.Of(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			serverError(w, r, "reading the last Polar delivery", err)
			return
		}
		if record.LastEvent == "" {
			writeError(w, http.StatusNotFound, "no webhook received yet")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"received_at": db.Timestamp(record.UpdatedAt),
			"event":       json.RawMessage(record.LastEvent),
		})
	}
}

// handleDevSession reports how the requesting session's expiry is decided:
// a Sliding Session renewed by use, or a fixed one that dies on schedule.
// The /dev page shows it for the browser's cookie and for a hand-off token
// side by side, which is the only place the difference is visible. Dev UI
// only.
//
// Mounted behind requireAuth, so sessionFrom holds the session it describes —
// including the expiry this request just wrote, if it renewed one.
func handleDevSession(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r)
	sliding := session.Kind == auth.SlidingSession
	body := map[string]any{
		"sliding":    sliding,
		"lifetime":   session.Kind.TTL().String(),
		"expires_at": db.Timestamp(session.ExpiresAt),
	}
	if sliding {
		body["renews_after"] = auth.SlidingRenewInterval.String()
	}
	writeJSON(w, http.StatusOK, body)
}
