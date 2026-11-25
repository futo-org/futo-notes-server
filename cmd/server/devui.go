package main

import (
	"bytes"
	"context"
	_ "embed"
	"html"
	"net/http"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/syncapi"
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

func registerDevJobHandlers(router chi.Router, database *db.DB, store *blobs.Store) {
	router.Post("/dev/jobs/sessions", handleDevJob(func(ctx context.Context) (jobs.SessionReapResult, error) {
		return jobs.ReapSessions(ctx, database)
	}))
	router.Post("/dev/jobs/reconciliation", handleDevJob(func(ctx context.Context) (jobs.ReconciliationResult, error) {
		return jobs.ReconcileStorage(ctx, database, store)
	}))
	router.Post("/dev/jobs/mutation-results", handleDevJob(func(ctx context.Context) (jobs.MutationExpiryResult, error) {
		return jobs.ExpireMutationResults(ctx, database)
	}))
	router.Post("/dev/jobs/blob-gc", handleDevJob(func(ctx context.Context) (jobs.BlobGCResult, error) {
		return jobs.GarbageCollectBlobs(ctx, database, store)
	}))
}

func handleDevJob[T any](run func(context.Context) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := run(r.Context())
		if err != nil {
			syncapi.WriteJSON(w, http.StatusInternalServerError, struct {
				Result T      `json:"result"`
				Error  string `json:"error"`
			}{Result: result, Error: err.Error()})
			return
		}
		syncapi.WriteJSON(w, http.StatusOK, result)
	}
}
