package api

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/mail"
	"futo-notes-server/internal/hosted/pairing"
	"futo-notes-server/internal/httputil"
)

//go:embed devui.html
var devUIHTML []byte

// handleDevUI serves a page for exercising the hosted API by hand: dashboard
// and app sign-in, account, region, devices, deletion, storage tokens,
// receipt-backed object writes, batch upload and claim, vault key material
// with its recovery envelope, the pairing relay, billing (mock checkout,
// synthetic subscription events, the lapse clock), the jobs, and the lapse
// emails. DEV_UI enables it; never in production.
func handleDevUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(devUIHTML)
}

// devSignup is whether ALLOWED_EMAIL_DOMAINS restricts who can sign up. It
// leaves the domains out, as the refusal does.
type devSignup struct {
	Restricted bool `json:"restricted"`
}

// handleDevSignup serves GET /dev/signup, for /dev to show whether a new
// account can be refused.
func (s *Server) handleDevSignup(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, r, http.StatusOK, devSignup{Restricted: len(s.cfg.AllowedEmailDomains) > 0})
}

// devJobRoutes mounts POST /dev/jobs/*, which run a storage job or the
// pairing sweep (and, in devBillingRoutes, a billing job) at once on this
// replica, outside the schedule, its advisory
// lock and its once-per-interval bookkeeping, as the self-hosted /dev/jobs/*
// do.
func (s *Server) devJobRoutes(r chi.Router) {
	r.Post("/dev/jobs/blob-gc", handleDevJob(func(r *http.Request) (hostedjobs.GCResult, error) {
		return hostedjobs.CollectBlobs(r.Context(), s.database, s.storage)
	}))
	r.Post("/dev/jobs/account-purge", handleDevJob(func(r *http.Request) (hostedjobs.PurgeResult, error) {
		return hostedjobs.PurgeAccounts(r.Context(), s.JobBilling())
	}))
	r.Post("/dev/jobs/reconciliation", s.handleDevReconciliation)
	r.Post("/dev/jobs/pairing-sweep", handleDevJob(func(r *http.Request) (pairingSweep, error) {
		deleted, err := pairing.DeleteExpired(r.Context(), s.database)
		return pairingSweep{Deleted: deleted}, err
	}))
}

// pairingSweep is the result of a pairing sweep run from /dev.
type pairingSweep struct {
	Deleted int64 `json:"deleted"`
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

// devLapsePurgeDelay is how long after a lapse the account is purged
// (launch plan C5), for the purge date the dev emails show.
const devLapsePurgeDelay = 90 * 24 * time.Hour

// devEmailSent is the result of a dev email send.
type devEmailSent struct {
	Template string `json:"template"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	// Delivered is false when RESEND_API_KEY is unset and the send was
	// only logged.
	Delivered bool `json:"delivered"`
}

// handleDevEmail serves POST /dev/email/{template}: it renders lapse email
// {template} for the signed-in user, with the purge date it would carry if
// sent now, and sends it through the configured mailer (Mailpit in dev). It
// leaves the user's email stamps alone.
func (s *Server) handleDevEmail(w http.ResponseWriter, r *http.Request) {
	t := mail.Template(chi.URLParam(r, "template"))
	user := principalFrom(r).Session.User
	purgeAt := time.Now()
	switch t {
	case mail.Lapsed:
		purgeAt = purgeAt.Add(devLapsePurgeDelay)
	case mail.PurgeWarning:
		purgeAt = purgeAt.Add(7 * 24 * time.Hour)
	}
	message, err := mail.Render(t, mail.Data{Email: user.Email, PurgeAt: purgeAt, DashboardURL: s.cfg.PublicURL + "/dashboard"})
	if errors.Is(err, mail.ErrUnknownTemplate) {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, fmt.Sprintf("unknown template %q", t))
		return
	}
	if err != nil {
		serverError(w, r, "rendering email", err)
		return
	}
	if user.Email == "" {
		badRequest(w, r, "the signed-in user has no email address")
		return
	}
	if err := s.mailer.Send(r.Context(), message); err != nil {
		httputil.WriteError(w, r, http.StatusBadGateway, httputil.CodeInternalError, err.Error())
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, devEmailSent{
		Template: string(t), To: message.To, Subject: message.Subject, Delivered: s.cfg.Email.APIKey != "",
	})
}
