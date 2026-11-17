package api

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/mockweb"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/httputil"
)

// devBillingRoutes mounts the DEV_UI billing tools: the mock web provider's
// checkout and portal pages, which act for the customer their signed link
// names, and, for the signed-in user, synthetic subscription events, a
// lapse clock, and the billing jobs.
func (s *Server) devBillingRoutes(r chi.Router) {
	r.Get(mockweb.CheckoutPath, s.handleMockCheckoutPage)
	r.Post(mockweb.CheckoutPath, s.handleMockCheckout)
	r.Get(mockweb.PortalPath, s.handleMockPortalPage)
	r.Post(mockweb.PortalPath, s.handleMockPortal)
	r.Group(func(signed chi.Router) {
		signed.Use(s.authenticate(hostedErrors))
		signed.Post("/dev/billing/events/{kind}", s.handleDevBillingEvent)
		signed.Post("/dev/billing/lapsed-at", s.handleDevLapsedAt)
	})
	r.Post("/dev/jobs/billing-reconciliation", handleDevJob(func(r *http.Request) (hostedjobs.ReconcileEntitlementsResult, error) {
		return hostedjobs.ReconcileEntitlements(r.Context(), s.billing, s.providers)
	}))
	r.Post("/dev/jobs/billing-lapses", handleDevJob(func(r *http.Request) (hostedjobs.LapseResult, error) {
		return hostedjobs.ProcessLapses(r.Context(), s.JobBilling())
	}))
}

// JobBilling is what the account purge and the billing jobs act through,
// sharing the server's providers (the mock keeps its state in memory) and
// mailer.
func (s *Server) JobBilling() hostedjobs.Billing {
	return hostedjobs.Billing{
		Database: s.database, Storage: s.storage, Store: s.billing, Providers: s.providers, Notifier: s.notifier,
	}
}

// mockWeb returns the mock web provider, or answers 404 when the web
// provider is something else.
func (s *Server) mockWeb(w http.ResponseWriter, r *http.Request) (*mockweb.Provider, bool) {
	mock, ok := s.providers.Web.(*mockweb.Provider)
	if !ok {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "The mock web provider is not configured")
	}
	return mock, ok
}

// mockCustomer reads the customer a mock page link names.
func (s *Server) mockCustomer(w http.ResponseWriter, r *http.Request) (*mockweb.Provider, string, bool) {
	mock, ok := s.mockWeb(w, r)
	if !ok {
		return nil, "", false
	}
	if err := r.ParseForm(); err != nil {
		badRequest(w, r, "Invalid form")
		return nil, "", false
	}
	userID, ok := mock.Customer(r.Form)
	if !ok {
		httputil.WriteError(w, r, http.StatusForbidden, httputil.CodeUnauthorized, "This mock billing link is not signed for that customer")
	}
	return mock, userID, ok
}

var mockPages = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.}} · mock billing</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem}
.note{color:#666}button{font:inherit;padding:.4rem .9rem}li{margin:.6rem 0}</style></head><body>
<p class="note">Mock web payment provider. No money moves; for development and tests only.</p>{{end}}
{{define "checkout"}}{{template "head" "Checkout"}}<h1>Subscribe to FUTO Notes</h1>
<p>One month, $5. Completing sends the API a signed webhook, as a real provider would.</p>
<form method="post"><input type="hidden" name="customer" value="{{.Customer}}"><input type="hidden" name="sig" value="{{.Sig}}">
<button type="submit">Complete checkout</button></form></body></html>{{end}}
{{define "done"}}{{template "head" "Subscribed"}}<h1>Subscribed</h1>
<p>Subscription <code>{{.ID}}</code> is active until {{.ExpiresAt.UTC.Format "2006-01-02 15:04 UTC"}}.</p>
<p><a href="/dashboard">Back to your account</a></p></body></html>{{end}}
{{define "portal"}}{{template "head" "Manage subscription"}}<h1>Manage subscription</h1>
{{if not .Subscriptions}}<p>No mock subscription. The mock forgets them when the API restarts.</p>{{end}}
<ul>{{range .Subscriptions}}<li><code>{{.ID}}</code>: {{.Status}} until {{.ExpiresAt.UTC.Format "2006-01-02 15:04 UTC"}},
{{if .WillRenew}}renews{{else}}does not renew{{end}}.
<form method="post"><input type="hidden" name="customer" value="{{$.Customer}}"><input type="hidden" name="sig" value="{{$.Sig}}">
<input type="hidden" name="subscription" value="{{.ID}}">
{{if .WillRenew}}<button name="action" value="cancel">Cancel at period end</button>{{else}}<button name="action" value="resume">Resume</button>{{end}}
</form></li>{{end}}</ul><p><a href="/dashboard">Back to your account</a></p></body></html>{{end}}
`))

func writeMockPage(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := mockPages.ExecuteTemplate(&buf, name, data); err != nil {
		serverError(w, r, "rendering mock billing page", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// handleMockCheckoutPage serves the mock checkout a CheckoutURL points at.
func (s *Server) handleMockCheckoutPage(w http.ResponseWriter, r *http.Request) {
	if _, userID, ok := s.mockCustomer(w, r); ok {
		writeMockPage(w, r, "checkout", map[string]string{"Customer": userID, "Sig": r.Form.Get("sig")})
	}
}

// handleMockCheckout completes a mock checkout: the mock sells a
// subscription and announces it through the webhook route.
func (s *Server) handleMockCheckout(w http.ResponseWriter, r *http.Request) {
	mock, userID, ok := s.mockCustomer(w, r)
	if !ok {
		return
	}
	sub := mock.Checkout(userID)
	if err := s.deliverMock(r, mock, sub); err != nil {
		serverError(w, r, "delivering mock checkout", err)
		return
	}
	writeMockPage(w, r, "done", sub)
}

// handleMockPortalPage serves the mock portal a PortalURL points at.
func (s *Server) handleMockPortalPage(w http.ResponseWriter, r *http.Request) {
	mock, userID, ok := s.mockCustomer(w, r)
	if !ok {
		return
	}
	writeMockPage(w, r, "portal", map[string]any{
		"Customer": userID, "Sig": r.Form.Get("sig"), "Subscriptions": mock.Subscriptions(userID),
	})
}

// handleMockPortal cancels or resumes a mock subscription, announces the
// change through the webhook route, and shows the portal again.
func (s *Server) handleMockPortal(w http.ResponseWriter, r *http.Request) {
	mock, userID, ok := s.mockCustomer(w, r)
	if !ok {
		return
	}
	id := r.Form.Get("subscription")
	if sub, ok := mock.Subscription(id); !ok || sub.UserID != userID {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "No such subscription")
		return
	}
	var err error
	switch r.Form.Get("action") {
	case "cancel":
		err = mock.Cancel(r.Context(), id)
	case "resume":
		_, err = mock.Resume(id)
	default:
		badRequest(w, r, "action must be cancel or resume")
		return
	}
	if err == nil {
		sub, _ := mock.Subscription(id)
		err = s.deliverMock(r, mock, sub)
	}
	if err != nil {
		serverError(w, r, "changing mock subscription", err)
		return
	}
	http.Redirect(w, r, mockweb.PortalPath+"?"+url.Values{"customer": {userID}, "sig": {r.Form.Get("sig")}}.Encode(), http.StatusSeeOther)
}

// deliverMock posts the mock's signed delivery for sub to this server's own
// webhook route, in process, so it takes the path a real provider's does.
func (s *Server) deliverMock(r *http.Request, mock *mockweb.Provider, sub mockweb.Subscription) error {
	delivery, err := mock.Delivery(sub)
	if err != nil {
		return err
	}
	req := httptest.NewRequestWithContext(r.Context(), http.MethodPost, "/api/billing/webhooks/"+string(billing.SourceWeb), bytes.NewReader(delivery.Body))
	req.Header = delivery.Header.Clone()
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("webhook answered %d: %s", rec.Code, rec.Body.String())
	}
	return nil
}

// devPeriod is how long a synthetic grant entitles the account.
const devPeriod = 30 * 24 * time.Hour

// handleDevBillingEvent serves POST /dev/billing/events/{kind}: a synthetic
// provider event for the signed-in user, applied through the entitlement
// store. grant adds a web subscription paid for 30 days (not renewing, so
// nothing is cancelled for it). The others act on every subscription that
// entitles the user: lapse reports it expired now, expire moves its paid
// period's end to now without any event, as if the provider had gone
// quiet, and refund reports it revoked. It answers with GET /api/billing.
func (s *Server) handleDevBillingEvent(w http.ResponseWriter, r *http.Request) {
	userID := principalFrom(r).Session.User.ID
	now := time.Now()
	var events []billing.Event
	switch kind := chi.URLParam(r, "kind"); kind {
	case "grant":
		events = append(events, billing.Event{
			Source: billing.SourceWeb, ExternalID: "dev_" + userID + "_" + strconv.FormatInt(now.UnixNano(), 36),
			UserID: userID, ProductID: mockweb.ProductID, Status: billing.StatusActive, ExpiresAt: new(now.Add(devPeriod)),
			Environment: billing.EnvironmentSandbox, EventAt: now, Raw: []byte(`{"dev":"grant"}`),
		})
	case "lapse", "expire", "refund":
		list, err := s.billing.Entitlements(r.Context(), userID)
		if err != nil {
			serverError(w, r, "listing entitlements", err)
			return
		}
		for _, e := range list {
			if !entitles(e, now) {
				continue
			}
			ev := billing.Event{
				Source: e.Source, ExternalID: e.ExternalID, ProductID: e.ProductID, Status: e.Status,
				WillRenew: false, ExpiresAt: new(now), Environment: e.Environment, EventAt: now,
				Raw: []byte(`{"dev":"` + kind + `"}`),
			}
			switch kind {
			case "lapse":
				ev.Status = billing.StatusExpired
			case "expire":
				ev.Status = billing.StatusActive
				ev.WillRenew = e.WillRenew
			case "refund":
				ev.Status = billing.StatusRevoked
				ev.ExpiresAt = e.ExpiresAt
			}
			events = append(events, ev)
		}
	default:
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, fmt.Sprintf("unknown event %q", kind))
		return
	}
	mock, _ := s.providers.Web.(*mockweb.Provider)
	for _, ev := range events {
		if ev.Source == billing.SourceWeb {
			ev.UserID = userID
		}
		if _, err := s.billing.Submit(r.Context(), userID, ev); err != nil {
			serverError(w, r, "applying synthetic event", err)
			return
		}
		if mock != nil {
			mock.Record(ev)
		}
	}
	s.writeBilling(w, r)
}

// entitles reports whether e entitles its account at now, as
// billing.EntitledSQL decides.
func entitles(e billing.Entitlement, now time.Time) bool {
	switch e.Status {
	case billing.StatusActive:
		return e.ExpiresAt != nil && e.ExpiresAt.After(now)
	case billing.StatusGrace:
		return e.GraceUntil != nil && e.GraceUntil.After(now)
	}
	return false
}

// devLapse is the signed-in user's lapse after /dev/billing/lapsed-at.
type devLapse struct {
	LapsedAt time.Time `json:"lapsed_at"`
	PurgeAt  time.Time `json:"purge_at"`
}

// handleDevLapsedAt serves POST /dev/billing/lapsed-at?days=N: it moves the
// signed-in user's lapse N days into the past, so the lapse job sends the
// purge warning (83 days) or purges (90 days) without waiting. The user must
// have lapsed: run the lapse job after the subscription ends.
func (s *Server) handleDevLapsedAt(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days < 1 || days > 365 {
		badRequest(w, r, "days must be 1 to 365")
		return
	}
	var lapse devLapse
	err = s.database.QueryRowContext(r.Context(), `UPDATE users SET lapsed_at = lapsed_at - make_interval(days => $2)
		WHERE id = $1 AND lapsed_at IS NOT NULL RETURNING lapsed_at`, principalFrom(r).Session.User.ID, days).Scan(&lapse.LapsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		badRequest(w, r, "The account has not lapsed; run the lapse job once its subscription has ended")
		return
	}
	if err != nil {
		serverError(w, r, "moving lapsed_at", err)
		return
	}
	lapse.LapsedAt = lapse.LapsedAt.UTC()
	lapse.PurgeAt = lapse.LapsedAt.Add(billing.PurgeAfter)
	httputil.WriteJSON(w, r, http.StatusOK, lapse)
}
