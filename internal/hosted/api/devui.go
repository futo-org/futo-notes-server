package api

import (
	_ "embed"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/httputil"
)

//go:embed devui.html
var devUIHTML []byte

// handleDevUI serves a page for exercising the hosted API by hand: dashboard
// and app sign-in, account, region, devices, deletion, storage tokens,
// receipt-backed object writes and the storage jobs. DEV_UI enables it;
// never in production.
func handleDevUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(devUIHTML)
}

// devJobRoutes mounts POST /dev/jobs/*, which run a storage job at once on
// this replica, outside the schedule, its advisory lock and its
// once-per-interval bookkeeping, as the self-hosted /dev/jobs/* do.
func (s *Server) devJobRoutes(r chi.Router) {
	r.Post("/dev/jobs/blob-gc", handleDevJob(func(r *http.Request) (hostedjobs.GCResult, error) {
		return hostedjobs.CollectBlobs(r.Context(), s.database, s.storage)
	}))
	r.Post("/dev/jobs/account-purge", handleDevJob(func(r *http.Request) (hostedjobs.PurgeResult, error) {
		return hostedjobs.PurgeAccounts(r.Context(), s.database, s.storage)
	}))
	r.Post("/dev/jobs/reconciliation", s.handleDevReconciliation)
}

// handleDevReconciliation runs orphan reconciliation in every region, or
// only in ?region=, and only under ?user='s prefix when given. ?now= (RFC
// 3339) is the time it runs as: S3 cannot backdate an upload, so a later now
// is how a fresh one counts as past hostedjobs.OrphanMinAge.
func (s *Server) handleDevReconciliation(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := ""
	if user := q.Get("user"); user != "" {
		prefix = user + "/"
	}
	regions := s.cfg.Regions
	if id := q.Get("region"); id != "" {
		i := slices.IndexFunc(regions, func(region config.Region) bool { return region.ID == id })
		if i < 0 {
			badRequest(w, r, fmt.Sprintf("unknown region %q", id))
			return
		}
		regions = regions[i : i+1]
	}
	now := time.Now()
	if value := q.Get("now"); value != "" {
		var err error
		if now, err = time.Parse(time.RFC3339, value); err != nil {
			badRequest(w, r, "now must be an RFC 3339 time")
			return
		}
	}
	handleDevJob(func(r *http.Request) (hostedjobs.ReconcileResult, error) {
		var total hostedjobs.ReconcileResult
		for _, region := range regions {
			result, err := hostedjobs.ReconcileOrphans(r.Context(), s.database, s.storage, region.ID, prefix, now)
			total.Listed += result.Listed
			total.Skipped += result.Skipped
			total.Deleted += result.Deleted
			total.Failed += result.Failed
			if err != nil {
				return total, err
			}
		}
		return total, nil
	})(w, r)
}

// handleDevJob answers with run's result, and with its error beside the
// partial result as a 500.
func handleDevJob[T any](run func(*http.Request) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := run(r)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, struct {
				Result T      `json:"result"`
				Error  string `json:"error"`
			}{Result: result, Error: err.Error()})
			return
		}
		httputil.WriteJSON(w, r, http.StatusOK, result)
	}
}
