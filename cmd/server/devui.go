package main

import (
	"context"
	"database/sql"
	_ "embed"
	"net/http"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/jobs"
)

//go:embed devui.html
var devUIHTML []byte

func handleDevUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(devUIHTML)
}

func registerDevJobHandlers(mux *http.ServeMux, database *sql.DB, store *blobs.Store) {
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
