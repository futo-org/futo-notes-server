package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"html"
	"net/http"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/jobs"
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
