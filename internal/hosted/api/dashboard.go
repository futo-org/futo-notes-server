package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/syncapi"
)

const (
	dashboardPath = "/dashboard"
	signInPath    = dashboardPath + "/sign-in"
	devicesPath   = dashboardPath + "/devices"
	regionPath    = dashboardPath + "/region"
	deletePath    = dashboardPath + "/delete"
	// reauthPath signs the dashboard in again, back to deletePath.
	reauthPath = "/api/auth/oidc/start?reauthenticate=true"

	// csrfHeader carries the CSRF token on htmx requests (hx-headers);
	// csrfField carries it on plain form posts, so forms work without
	// JavaScript.
	csrfHeader = "X-CSRF-Token"
	csrfField  = "csrf_token"

	// confirmPhrase is what the user types to confirm deleting the account.
	confirmPhrase = "DELETE"

	dashboardCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
		"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	htmxAsset     = "htmx-2.0.4.min.js"
	cssAsset      = "dashboard.css"
	checkoutAsset = "checkout.js"
)

//go:embed dashboard/templates dashboard/static
var dashboardFiles embed.FS

var (
	dashboardStatic = mustSub(dashboardFiles, "dashboard/static")
	dashboardPages  = parseDashboardPages()
)

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// parseDashboardPages returns each page's template set: the shared layout
// plus the page's "title" and "main".
func parseDashboardPages() map[string]*template.Template {
	funcs := template.FuncMap{
		"isoTime":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"utcTime":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"humanBytes": humanBytes,
	}
	layout := template.Must(template.New("").Funcs(funcs).ParseFS(dashboardFiles, "dashboard/templates/layout.html"))
	pages := map[string]*template.Template{}
	for _, name := range []string{"sign-in", "region", "overview", "devices", "delete", "error", "checkout"} {
		pages[name] = template.Must(template.Must(layout.Clone()).ParseFS(dashboardFiles, "dashboard/templates/"+name+".html"))
	}
	return pages
}

// humanBytes renders n bytes in binary units with one decimal.
func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value, unit := float64(n)/1024, 0
	for value >= 1024 && unit < 3 {
		value /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %s", value, [...]string{"KiB", "MiB", "GiB", "TiB"}[unit])
}

// dashboardView is what every dashboard template renders from; each page
// reads the fields it needs.
type dashboardView struct {
	SignedIn bool
	User     auth.User
	// CSRF is the session's CSRF token; HXHeaders sends it on every htmx
	// request.
	CSRF      string
	HXHeaders string
	// Nav names the current page in the navigation.
	Nav   string
	Error string

	Account       account.Account
	Region        *config.Region
	Regions       []config.Region
	Devices       []dashboardDevice
	GraceDays     int
	ConfirmPhrase string
	// Reauthenticate asks the user to sign in again before deleting.
	Reauthenticate bool
	ReauthPath     string
	// Billing is the account's billing state; nil while billing is off.
	Billing *billing.State
	// WebPayments reports that a web provider is configured.
	WebPayments bool
	// EmbeddedCheckout reports that the web provider's checkout mounts in
	// our pages, so Subscribe opens it in place.
	EmbeddedCheckout bool
	// CheckoutHost makes the page able to mount embedded checkout: it
	// loads the provider's script and is served checkoutCSP.
	CheckoutHost *checkoutHost
	// Checkout is the embedded checkout panel to show.
	Checkout *checkoutPanel
	// Notice confirms what just happened.
	Notice string
	// PendingURL is reloaded every few seconds while a payment clears.
	PendingURL string
}

type dashboardDevice struct {
	account.DeviceSession
	Current bool
}

// dashboardAction is a state-changing dashboard route and the JSON API
// route that does the same thing through the same service code.
type dashboardAction struct {
	path   string
	api    string
	handle http.HandlerFunc
}

func (s *Server) dashboardActions() []dashboardAction {
	return []dashboardAction{
		{regionPath, "PUT /api/account/region", s.handleDashboardChooseRegion},
		{devicesPath + "/{id}/revoke", "DELETE /api/sessions/{id}", s.handleDashboardRevoke},
		{deletePath, "DELETE /api/account", s.handleDashboardDelete},
		{dashboardPath + "/restore", "POST /api/account/restore", s.handleDashboardRestore},
		{dashboardPath + "/sign-out", "POST /api/auth/logout", s.handleDashboardSignOut},
		{dashboardPath + "/billing/checkout", "POST /api/billing/checkout", s.handleDashboardCheckout},
		{dashboardPath + "/billing/portal", "GET /api/billing/portal", s.handleDashboardPortal},
	}
}

// dashboardRoutes mounts the htmx dashboard.
func (s *Server) dashboardRoutes(r chi.Router) {
	r.Group(func(d chi.Router) {
		d.Use(dashboardHeaders)
		d.Get(dashboardPath+"/static/{name}", handleDashboardAsset)
		d.Get(signInPath, handleDashboardSignIn)
		d.Get(checkoutLinkPath+"/{token}", s.handleCheckoutLinkPage)
		d.Post(checkoutLinkPath+"/{token}/session", s.handleCheckoutLinkSession)
		d.Get(checkoutLinkReturnPath, s.handleCheckoutLinkReturn)
		d.Group(func(signed chi.Router) {
			signed.Use(s.dashboardSession)
			signed.Get(dashboardPath, s.handleDashboardOverview)
			signed.Get(subscribePath, s.handleDashboardSubscribe)
			signed.Get(checkoutReturnPath, s.handleDashboardCheckoutReturn)
			signed.Get(regionPath, s.handleDashboardRegion)
			signed.Get(devicesPath, s.handleDashboardDevices)
			signed.Get(deletePath, s.handleDashboardDeletePage)
			signed.Group(func(actions chi.Router) {
				actions.Use(requireCSRF)
				for _, a := range s.dashboardActions() {
					actions.Post(a.path, a.handle)
				}
			})
		})
	})
}

// dashboardHeaders sets the security headers of every dashboard response.
// Pages carry personal data and the CSRF token, so nothing is cached unless
// a handler says otherwise.
func dashboardHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", dashboardCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Vary", "HX-Request")
		next.ServeHTTP(w, r)
	})
}

// handleDashboardAsset serves the embedded htmx and stylesheet. htmx's file
// name carries its pinned version, so it can be cached for good.
func handleDashboardAsset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	switch name {
	case htmxAsset:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case cssAsset, checkoutAsset:
		w.Header().Set("Cache-Control", "no-cache")
	default:
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, dashboardStatic, name)
}

// isHTMX reports whether htmx sent the request, which then gets a fragment
// rather than a whole page.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

type csrfCtxKey struct{}

// csrfToken derives the session's CSRF token from its raw session token: a
// synchronizer token bound to the session that needs no storage. Only a
// holder of the HttpOnly session cookie, or a page the server rendered for
// it, knows it.
func csrfToken(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	mac.Write([]byte("futo-notes dashboard csrf"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// dashboardSession requires the session cookie, sending signed-out browsers
// to the sign-in page. It extends the cookie whenever the session slides.
func (s *Server) dashboardSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(syncapi.SessionCookie)
		if err != nil || cookie.Value == "" {
			redirectDashboard(w, r, signInPath)
			return
		}
		principal, err := s.accounts.Authenticate(r.Context(), cookie.Value)
		if errors.Is(err, auth.ErrNoSession) {
			s.clearSessionCookie(w)
			redirectDashboard(w, r, signInPath)
			return
		}
		if err != nil {
			dashboardServerError(w, r, "validating session", err)
			return
		}
		if principal.Slid {
			s.setSessionCookie(w, cookie.Value)
		}
		hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("user", principal.Session.User.ID)
		})
		ctx := context.WithValue(r.Context(), principalCtxKey{}, principal)
		ctx = context.WithValue(ctx, csrfCtxKey{}, csrfToken(cookie.Value))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF refuses a state-changing request unless it carries the
// session's CSRF token in the header htmx sends or in the form.
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		got := r.Header.Get(csrfHeader)
		if got == "" {
			got = r.PostFormValue(csrfField)
		}
		want, _ := r.Context().Value(csrfCtxKey{}).(string)
		if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			renderDashboardError(w, r, http.StatusForbidden, "This form has expired. Reload the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// redirectDashboard sends the browser to path. htmx follows HX-Redirect
// with a full page load; a 3xx would be followed inside its XHR instead.
func redirectDashboard(w http.ResponseWriter, r *http.Request, path string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// view returns the signed-in caller's base view.
func view(r *http.Request, nav string) dashboardView {
	p := principalFrom(r)
	token, _ := r.Context().Value(csrfCtxKey{}).(string)
	headers, _ := json.Marshal(map[string]string{csrfHeader: token})
	return dashboardView{SignedIn: true, User: p.Session.User, CSRF: token, HXHeaders: string(headers), Nav: nav}
}

// renderDashboard answers 200 with page: the whole document, or for htmx
// just the part inside #page. A page that hosts embedded checkout is served
// checkoutCSP instead of the strict policy.
func renderDashboard(w http.ResponseWriter, r *http.Request, page string, v dashboardView) {
	if v.CheckoutHost != nil {
		w.Header().Set("Content-Security-Policy", checkoutCSP)
	}
	name := "layout"
	if isHTMX(r) {
		name = "fragment"
	}
	writeTemplate(w, r, http.StatusOK, page, name, v)
}

func writeTemplate(w http.ResponseWriter, r *http.Request, status int, page, name string, v any) {
	var buf bytes.Buffer
	if err := dashboardPages[page].ExecuteTemplate(&buf, name, v); err != nil {
		hlog.FromRequest(r).Error().Err(err).Str("page", page).Msg("rendering dashboard")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderDashboardError shows message in the page: htmx swaps it into the
// alert region, other requests get an error page.
func renderDashboardError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "#alert")
		w.Header().Set("HX-Reswap", "innerHTML")
		writeTemplate(w, r, status, "error", "alert", message)
		return
	}
	v := dashboardView{Error: message}
	if _, ok := r.Context().Value(principalCtxKey{}).(account.Principal); ok {
		v = view(r, "")
		v.Error = message
	}
	writeTemplate(w, r, status, "error", "layout", v)
}

func dashboardServerError(w http.ResponseWriter, r *http.Request, action string, err error) {
	hlog.FromRequest(r).Error().Err(err).Str("action", action).Msg("internal server error")
	renderDashboardError(w, r, http.StatusInternalServerError, "Something went wrong on our side. Try again in a moment.")
}

// afterAction finishes an action that changes which page the user is on:
// htmx gets that page's fragment and the new URL, a plain form post a
// redirect to it.
func afterAction(w http.ResponseWriter, r *http.Request, path string, render func()) {
	if isHTMX(r) {
		w.Header().Set("HX-Push-Url", path)
		render()
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// handleDashboardSignIn serves the signed-out page.
func handleDashboardSignIn(w http.ResponseWriter, r *http.Request) {
	renderDashboard(w, r, "sign-in", dashboardView{})
}

// handleDashboardOverview serves the account overview.
func (s *Server) handleDashboardOverview(w http.ResponseWriter, r *http.Request) {
	s.overviewWith(w, r)
}

// overviewWith serves the account overview, changed by each of extra,
// sending a user who has not chosen a region to choose one first.
func (s *Server) overviewWith(w http.ResponseWriter, r *http.Request, extra ...func(*dashboardView)) {
	a, err := s.accounts.Account(r.Context(), principalFrom(r).Session.User.ID)
	if err != nil {
		dashboardServerError(w, r, "reading account", err)
		return
	}
	if a.RegionID == "" && a.DeletionRequestedAt == nil {
		redirectDashboard(w, r, regionPath)
		return
	}
	s.renderOverview(w, r, a, extra...)
}

func (s *Server) renderOverview(w http.ResponseWriter, r *http.Request, a account.Account, extra ...func(*dashboardView)) {
	v := view(r, "overview")
	v.Account = a
	if region, ok := s.regions[a.RegionID]; ok {
		v.Region = &region
	}
	if s.providers.Any() {
		state, err := s.billing.State(r.Context(), a.User.ID)
		if err != nil {
			dashboardServerError(w, r, "reading billing state", err)
			return
		}
		v.Billing = &state
		v.WebPayments = s.providers.Web != nil
		if s.embedded() != nil && !state.Entitled {
			v.EmbeddedCheckout = true
			hostCheckout(&v)
		}
	}
	for _, change := range extra {
		change(&v)
	}
	renderDashboard(w, r, "overview", v)
}

// handleDashboardCheckout is the dashboard's POST /api/billing/checkout. An
// embedded checkout's panel calls it for the session it mounts; otherwise it
// sends the browser to the web checkout.
func (s *Server) handleDashboardCheckout(w http.ResponseWriter, r *http.Request) {
	userID := principalFrom(r).Session.User.ID
	if s.embedded() != nil {
		s.writeCheckoutSession(w, r, userID, s.cfg.PublicURL+checkoutReturnPath+"?session_id="+checkoutSessionID)
		return
	}
	target, err := s.checkoutURL(r.Context(), userID)
	if err != nil {
		dashboardBillingError(w, r, "creating checkout", err)
		return
	}
	redirectAway(w, r, target)
}

// handleDashboardPortal is the dashboard's GET /api/billing/portal: it sends
// the browser to the web provider's subscription page.
func (s *Server) handleDashboardPortal(w http.ResponseWriter, r *http.Request) {
	target, err := s.portalURL(r.Context(), principalFrom(r).Session.User.ID)
	if err != nil {
		dashboardBillingError(w, r, "creating portal session", err)
		return
	}
	redirectAway(w, r, target)
}

func dashboardBillingError(w http.ResponseWriter, r *http.Request, action string, err error) {
	switch {
	case errors.Is(err, errBillingUnavailable):
		renderDashboardError(w, r, http.StatusServiceUnavailable, "Paying on the web is not available yet. Subscribe in the app instead.")
	case errors.Is(err, errAlreadySubscribed):
		renderDashboardError(w, r, http.StatusConflict, "You already have an active subscription.")
	case errors.Is(err, errManagedByStore):
		renderDashboardError(w, r, http.StatusConflict, "Your subscription was bought in an app store. Manage it in that store's subscription settings.")
	default:
		dashboardServerError(w, r, action, err)
	}
}

// redirectAway sends the browser to a payment provider's page. htmx gets
// HX-Redirect, a full page load that the CSP's form-action does not govern;
// a form posted without JavaScript gets a 303.
func redirectAway(w http.ResponseWriter, r *http.Request, target string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// showOverview renders the caller's current overview after an action.
func (s *Server) showOverview(w http.ResponseWriter, r *http.Request) {
	afterAction(w, r, dashboardPath, func() {
		a, err := s.accounts.Account(r.Context(), principalFrom(r).Session.User.ID)
		if err != nil {
			dashboardServerError(w, r, "reading account", err)
			return
		}
		s.renderOverview(w, r, a)
	})
}

// handleDashboardRegion serves the region choice, which is offered only
// while the user has none.
func (s *Server) handleDashboardRegion(w http.ResponseWriter, r *http.Request) {
	if principalFrom(r).RegionID != "" {
		redirectDashboard(w, r, dashboardPath)
		return
	}
	v := view(r, "")
	v.Regions = s.cfg.Regions
	renderDashboard(w, r, "region", v)
}

// handleDashboardChooseRegion is the dashboard's PUT /api/account/region.
func (s *Server) handleDashboardChooseRegion(w http.ResponseWriter, r *http.Request) {
	regionID := r.PostFormValue("region_id")
	if _, ok := s.regions[regionID]; !ok {
		renderDashboardError(w, r, http.StatusBadRequest, "Choose one of the listed regions.")
		return
	}
	err := s.accounts.ChooseRegion(r.Context(), principalFrom(r).Session.User.ID, regionID)
	if errors.Is(err, account.ErrRegionChosen) {
		renderDashboardError(w, r, http.StatusConflict, "Your region is already chosen and cannot be changed.")
		return
	}
	if err != nil {
		dashboardServerError(w, r, "choosing region", err)
		return
	}
	s.showOverview(w, r)
}

func (s *Server) devicesView(r *http.Request) (dashboardView, error) {
	p := principalFrom(r)
	sessions, err := s.accounts.Sessions(r.Context(), p.Session.User.ID)
	if err != nil {
		return dashboardView{}, err
	}
	v := view(r, "devices")
	for _, d := range sessions {
		v.Devices = append(v.Devices, dashboardDevice{DeviceSession: d, Current: d.ID == p.Session.ID})
	}
	return v, nil
}

// handleDashboardDevices lists the caller's signed-in devices.
func (s *Server) handleDashboardDevices(w http.ResponseWriter, r *http.Request) {
	v, err := s.devicesView(r)
	if err != nil {
		dashboardServerError(w, r, "listing sessions", err)
		return
	}
	renderDashboard(w, r, "devices", v)
}

// handleDashboardRevoke is the dashboard's DELETE /api/sessions/{id}. It
// refuses the current session: signing this browser out is Sign out, which
// also clears the cookie.
func (s *Server) handleDashboardRevoke(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	id := chi.URLParam(r, "id")
	if id == p.Session.ID {
		renderDashboardError(w, r, http.StatusBadRequest, "This is the device you are using. Use Sign out instead.")
		return
	}
	err := s.accounts.RevokeSession(r.Context(), p.Session.User.ID, id)
	if errors.Is(err, account.ErrNotFound) {
		renderDashboardError(w, r, http.StatusNotFound, "That device is already signed out.")
		return
	}
	if err != nil {
		dashboardServerError(w, r, "revoking session", err)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, devicesPath, http.StatusSeeOther)
		return
	}
	v, err := s.devicesView(r)
	if err != nil {
		dashboardServerError(w, r, "listing sessions", err)
		return
	}
	writeTemplate(w, r, http.StatusOK, "devices", "devices-list", v)
}

// handleDashboardDeletePage explains account deletion and asks for
// confirmation, or shows the pending deletion.
func (s *Server) handleDashboardDeletePage(w http.ResponseWriter, r *http.Request) {
	a, err := s.accounts.Account(r.Context(), principalFrom(r).Session.User.ID)
	if err != nil {
		dashboardServerError(w, r, "reading account", err)
		return
	}
	v := view(r, "delete")
	v.Account = a
	v.GraceDays = int(account.DeletionGrace / (24 * time.Hour))
	v.ConfirmPhrase = confirmPhrase
	v.Reauthenticate = !s.accounts.RecentlyAuthenticated(principalFrom(r))
	v.ReauthPath = reauthPath
	renderDashboard(w, r, "delete", v)
}

// handleDashboardDelete is the dashboard's DELETE /api/account, once the
// user has typed the confirmation phrase. A sign-in older than
// account.ReauthWindow sends the browser to sign in again first.
func (s *Server) handleDashboardDelete(w http.ResponseWriter, r *http.Request) {
	if !s.accounts.RecentlyAuthenticated(principalFrom(r)) {
		redirectDashboard(w, r, reauthPath)
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirm")) != confirmPhrase {
		renderDashboardError(w, r, http.StatusUnprocessableEntity, "Type "+confirmPhrase+" to confirm deleting your account.")
		return
	}
	err := s.accounts.ScheduleDeletion(r.Context(), principalFrom(r))
	if errors.Is(err, account.ErrReauthenticationRequired) {
		redirectDashboard(w, r, reauthPath)
		return
	}
	if err != nil {
		dashboardServerError(w, r, "requesting account deletion", err)
		return
	}
	s.cancelSubscriptions(r.Context(), principalFrom(r).Session.User.ID)
	s.showOverview(w, r)
}

// handleDashboardRestore is the dashboard's POST /api/account/restore.
func (s *Server) handleDashboardRestore(w http.ResponseWriter, r *http.Request) {
	err := s.accounts.Restore(r.Context(), principalFrom(r).Session.User.ID)
	if errors.Is(err, account.ErrPurgeStarted) {
		renderDashboardError(w, r, http.StatusConflict, "The grace period has ended and your account is being deleted. It can no longer be restored.")
		return
	}
	if err != nil {
		dashboardServerError(w, r, "restoring account", err)
		return
	}
	s.showOverview(w, r)
}

// handleDashboardSignOut is the dashboard's POST /api/auth/logout: it ends
// the session and clears the cookie.
func (s *Server) handleDashboardSignOut(w http.ResponseWriter, r *http.Request) {
	if err := auth.DeleteSession(r.Context(), s.database, principalFrom(r).Session.ID); err != nil {
		dashboardServerError(w, r, "signing out", err)
		return
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, signInPath, http.StatusSeeOther)
}

// setSessionCookie gives the browser the session cookie for token, expiring
// with the session.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	syncapi.SetSessionCookie(w, token, int(account.SessionTTL.Seconds()), s.cfg.CookieSecure)
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	syncapi.SetSessionCookie(w, "", -1, s.cfg.CookieSecure)
}
