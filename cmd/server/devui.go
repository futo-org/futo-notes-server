package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"time"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/polar"
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
		if _, err := database.ExecContext(r.Context(),
			`UPDATE plans SET storage_quota_bytes = $1 WHERE id = 'default'`, *body.StorageQuotaBytes); err != nil {
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

// handleDevSubscription is the /dev card's stand-in for Polar: it builds a
// subscription event for the signed-in account, signs it with the configured
// webhook secret, and puts it through the real webhook route. Signature
// verification runs for real, so the card demonstrates the whole path rather
// than a shortcut around it. Dev UI only, and only in hosted mode.
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

		var sub string
		if err := database.QueryRowContext(r.Context(),
			`SELECT sub FROM users WHERE id = $1`, sessionFrom(r).User.ID).Scan(&sub); err != nil {
			serverError(w, r, "reading the signed-in account's sub", err)
			return
		}
		now := time.Now()
		periodEnd := now.AddDate(0, 1, 0).UTC()
		delivery, err := polar.Delivery(polar.Event{
			Type: body.Type,
			Subscription: polar.Subscription{
				ID:                 "dev_subscription",
				Status:             body.Status,
				CustomerID:         "dev_customer",
				ExternalCustomerID: sub,
				CurrentPeriodEnd:   &periodEnd,
			},
		})
		if err != nil {
			serverError(w, r, "building a dev Polar delivery", err)
			return
		}
		header, err := polar.Sign(cfg.PolarWebhookSecret, "dev_"+db.Timestamp(now), now, delivery)
		if err != nil {
			serverError(w, r, "signing a dev Polar delivery", err)
			return
		}

		// The signed delivery goes through the webhook handler itself and
		// answers straight into this response, so what the card shows is
		// what Polar would have got.
		signed := r.Clone(r.Context())
		signed.Method = http.MethodPost
		signed.URL = &url.URL{Path: "/api/billing/polar/webhook"}
		signed.Header = header
		signed.Body = io.NopCloser(bytes.NewReader(delivery))
		signed.ContentLength = int64(len(delivery))
		webhook(w, signed)
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
