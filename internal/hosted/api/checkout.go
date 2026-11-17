package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/stripe"
	"futo-notes-server/internal/httputil"
)

// Embedded checkout: a provider whose checkout mounts inside our own pages
// (billing.EmbeddedCheckout, i.e. Stripe). The dashboard opens it in place;
// the apps get a one-time link to a page of ours that needs no dashboard
// session. When the checkout completes, the server fetches the session from
// the provider and applies its subscription at once, so entitlement never
// waits for the webhook, which stays the source of every later change.
const (
	subscribePath = dashboardPath + "/billing/subscribe"
	// checkoutReturnPath is where a dashboard checkout returns.
	checkoutReturnPath = dashboardPath + "/billing/return"
	// checkoutLinkPath prefixes the apps' one-time checkout pages,
	// checkoutLinkPath/<token>.
	checkoutLinkPath = "/billing/checkout"
	// checkoutLinkReturnPath is where a checkout opened from a link
	// returns.
	checkoutLinkReturnPath = checkoutLinkPath + "/return"
	// checkoutSessionID is the provider's placeholder for the session id in
	// a return URL.
	checkoutSessionID = "{CHECKOUT_SESSION_ID}"

	// checkoutLinkTTL is how long an app's checkout link may be opened.
	checkoutLinkTTL = 15 * time.Minute
	// checkoutCompletionTTL is how long a link checkout's return URL can
	// still confirm it: past a payment that takes a while.
	checkoutCompletionTTL = 24 * time.Hour

	// checkoutCSP is dashboardCSP widened by what Stripe documents for
	// Stripe.js and Checkout (docs.stripe.com/security/guide, "Content
	// Security Policy"), for the pages that host embedded checkout and only
	// them. Stripe.js must load from js.stripe.com; checkout runs in a
	// checkout.stripe.com frame; 3D Secure uses hooks.stripe.com.
	checkoutCSP = "default-src 'none'; " +
		"script-src 'self' https://js.stripe.com https://*.js.stripe.com https://checkout.stripe.com; " +
		"style-src 'self'; img-src 'self' https://*.stripe.com; font-src 'self'; " +
		"connect-src 'self' https://api.stripe.com https://checkout.stripe.com; " +
		"frame-src https://js.stripe.com https://*.js.stripe.com https://hooks.stripe.com https://checkout.stripe.com; " +
		"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// The purposes of a checkout token.
const (
	// purposeOpen opens one checkout for its user: an app's link.
	purposeOpen = "open"
	// purposeComplete confirms a checkout opened from a link.
	purposeComplete = "complete"
)

// embedded returns the web provider when its checkout is embedded.
func (s *Server) embedded() billing.EmbeddedCheckout {
	e, _ := s.providers.Web.(billing.EmbeddedCheckout)
	return e
}

// checkoutClaims are what a checkout token says, under the server's HMAC.
type checkoutClaims struct {
	Purpose string `json:"p"`
	UserID  string `json:"u"`
	Expires int64  `json:"e"`
	// Nonce identifies an open token, so it is used once.
	Nonce string `json:"n,omitempty"`
}

// signCheckoutToken returns c as base64url(JSON) "." base64url(HMAC).
func (s *Server) signCheckoutToken(c checkoutClaims) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, s.cfg.WebBilling.CheckoutTokenKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// readCheckoutToken returns the claims of a token signed for purpose that
// has not expired.
func (s *Server) readCheckoutToken(token, purpose string) (checkoutClaims, bool) {
	key := s.cfg.WebBilling.CheckoutTokenKey
	encoded, sig, ok := strings.Cut(token, ".")
	if !ok || len(key) == 0 {
		return checkoutClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return checkoutClaims{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return checkoutClaims{}, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return checkoutClaims{}, false
	}
	var c checkoutClaims
	if json.Unmarshal(payload, &c) != nil || c.Purpose != purpose || c.UserID == "" || time.Now().Unix() >= c.Expires ||
		(purpose == purposeOpen && c.Nonce == "") {
		return checkoutClaims{}, false
	}
	return c, true
}

// checkoutLink is a one-time link to the checkout page for userID.
func (s *Server) checkoutLink(userID string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	token := s.signCheckoutToken(checkoutClaims{
		Purpose: purposeOpen, UserID: userID, Expires: time.Now().Add(checkoutLinkTTL).Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	})
	return s.cfg.PublicURL + checkoutLinkPath + "/" + token, nil
}

// checkoutLinkUsed reports whether an open token has opened its checkout.
func (s *Server) checkoutLinkUsed(ctx context.Context, c checkoutClaims) (bool, error) {
	var used bool
	err := s.database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM checkout_token_uses WHERE nonce = $1)`, c.Nonce).Scan(&used)
	return used, err
}

// useCheckoutLink records that an open token opened its checkout, and
// reports false if it already had. Uses are kept until the token would have
// expired anyway.
func (s *Server) useCheckoutLink(ctx context.Context, c checkoutClaims) (bool, error) {
	if _, err := s.database.ExecContext(ctx, `DELETE FROM checkout_token_uses WHERE expires_at < now()`); err != nil {
		return false, err
	}
	result, err := s.database.ExecContext(ctx, `INSERT INTO checkout_token_uses (nonce, expires_at) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, c.Nonce, time.Unix(c.Expires, 0))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// checkoutSession is what a checkout panel mounts: the provider's client
// secret, and where to go once it completes. The client secret is for the
// browser alone and is never logged.
type checkoutSession struct {
	ClientSecret string `json:"client_secret"`
	ReturnURL    string `json:"return_url"`
}

// writeCheckoutSession opens an embedded checkout for userID returning to
// returnURL (with checkoutSessionID in it) and answers with its session.
func (s *Server) writeCheckoutSession(w http.ResponseWriter, r *http.Request, userID, returnURL string) {
	err := s.checkoutAllowed(r.Context(), userID)
	var checkout billing.Checkout
	if err == nil {
		var customer billing.Customer
		if customer, err = s.billing.Customer(r.Context(), userID); err == nil {
			checkout, err = s.embedded().StartCheckout(r.Context(), customer, returnURL)
		}
	}
	if err != nil {
		writeBillingURLError(w, r, "creating checkout", err)
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, checkoutSession{
		ClientSecret: checkout.ClientSecret,
		ReturnURL:    strings.ReplaceAll(returnURL, checkoutSessionID, checkout.ID),
	})
}

// checkoutOutcome is what confirming a checkout found.
type checkoutOutcome int

const (
	// checkoutOpen: not paid; the panel is shown again.
	checkoutOpen checkoutOutcome = iota
	// checkoutEntitled: paid and the account is entitled.
	checkoutEntitled
	// checkoutProcessing: complete, but the payment has not cleared.
	checkoutProcessing
)

// completeCheckout fetches userID's checkout from the provider and applies
// the subscription it created, as a refresh does. It returns
// billing.ErrCheckoutForeign for another account's checkout.
func (s *Server) completeCheckout(ctx context.Context, userID, checkoutID string) (checkoutOutcome, error) {
	customer, err := s.billing.Customer(ctx, userID)
	if err != nil {
		return 0, err
	}
	ev, err := s.embedded().CompleteCheckout(ctx, customer, checkoutID)
	if errors.Is(err, billing.ErrCheckoutIncomplete) {
		return checkoutOpen, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := s.billing.Apply(ctx, ev); err != nil {
		return 0, err
	}
	state, err := s.billing.State(ctx, userID)
	if err != nil {
		return 0, err
	}
	if !state.Entitled {
		return checkoutProcessing, nil
	}
	return checkoutEntitled, nil
}

// checkoutPanel is the embedded checkout as a page shows it.
type checkoutPanel struct {
	PublishableKey string
	// SessionURL is POSTed for the checkoutSession to mount.
	SessionURL string
	// PageURL is a page that hosts checkout, for a panel that landed in
	// a document without checkoutCSP.
	PageURL string
}

// checkoutHost is what a page that hosts embedded checkout loads.
type checkoutHost struct {
	ScriptURL string
}

// hostCheckout makes v a page that can mount embedded checkout.
func hostCheckout(v *dashboardView) {
	v.CheckoutHost = &checkoutHost{ScriptURL: stripe.JSURL}
}

// handleDashboardSubscribe shows the overview with the checkout panel open.
func (s *Server) handleDashboardSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.embedded() == nil {
		redirectDashboard(w, r, dashboardPath)
		return
	}
	s.overviewWith(w, r, func(v *dashboardView) {
		if !v.Billing.Entitled {
			v.Checkout = s.dashboardPanel()
		}
	})
}

func (s *Server) dashboardPanel() *checkoutPanel {
	return &checkoutPanel{PublishableKey: s.embedded().PublishableKey(), SessionURL: dashboardPath + "/billing/checkout", PageURL: subscribePath}
}

// handleDashboardCheckoutReturn is where a dashboard checkout ends: Stripe
// sends the browser here after a payment that left the page, and the
// panel's onComplete swaps it in otherwise. It confirms the checkout and
// shows the overview.
func (s *Server) handleDashboardCheckoutReturn(w http.ResponseWriter, r *http.Request) {
	if s.embedded() == nil {
		redirectDashboard(w, r, dashboardPath)
		return
	}
	outcome, err := s.completeCheckout(r.Context(), principalFrom(r).Session.User.ID, r.URL.Query().Get("session_id"))
	if errors.Is(err, billing.ErrCheckoutForeign) {
		renderDashboardError(w, r, http.StatusForbidden, "That checkout belongs to another account.")
		return
	}
	if err != nil {
		dashboardServerError(w, r, "confirming checkout", err)
		return
	}
	s.overviewWith(w, r, func(v *dashboardView) {
		switch outcome {
		case checkoutEntitled:
			v.Notice = "Thank you. Your subscription is active."
		case checkoutProcessing:
			v.Notice = "Your payment is processing. This page updates when it clears."
			v.PendingURL = r.URL.RequestURI()
		case checkoutOpen:
			v.Notice = "The payment did not go through. You can try again."
			if !v.Billing.Entitled {
				v.Checkout = s.dashboardPanel()
			}
		}
	})
}

// checkoutPageView is the signed-out checkout page.
func checkoutPageView() dashboardView {
	v := dashboardView{}
	hostCheckout(&v)
	return v
}

// renderCheckoutPage answers with the signed-out checkout page.
func renderCheckoutPage(w http.ResponseWriter, r *http.Request, status int, v dashboardView) {
	if v.CheckoutHost != nil {
		w.Header().Set("Content-Security-Policy", checkoutCSP)
	}
	writeTemplate(w, r, status, "checkout", "layout", v)
}

const checkoutLinkExpiredMessage = "This checkout link has expired or was already used. Start again from the app."

// handleCheckoutLinkPage serves an app's checkout link: our page that
// mounts the embedded checkout, without a dashboard session. Opening it does
// not use the link; mounting the checkout does.
func (s *Server) handleCheckoutLinkPage(w http.ResponseWriter, r *http.Request) {
	if s.embedded() == nil {
		renderCheckoutPage(w, r, http.StatusNotFound, dashboardView{Error: "Paying on the web is not available."})
		return
	}
	token := chi.URLParam(r, "token")
	claims, ok := s.readCheckoutToken(token, purposeOpen)
	used := false
	if ok {
		var err error
		if used, err = s.checkoutLinkUsed(r.Context(), claims); err != nil {
			dashboardServerError(w, r, "reading checkout link", err)
			return
		}
	}
	if !ok || used {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return
	}
	err := s.checkoutAllowed(r.Context(), claims.UserID)
	if errors.Is(err, errAlreadySubscribed) {
		renderCheckoutPage(w, r, http.StatusOK, dashboardView{Notice: "You already have an active subscription. You can return to the app."})
		return
	}
	if err != nil {
		dashboardServerError(w, r, "checking subscription", err)
		return
	}
	v := checkoutPageView()
	v.Checkout = &checkoutPanel{
		PublishableKey: s.embedded().PublishableKey(),
		SessionURL:     checkoutLinkPath + "/" + token + "/session",
		PageURL:        checkoutLinkPath + "/" + token,
	}
	renderCheckoutPage(w, r, http.StatusOK, v)
}

// handleCheckoutLinkSession uses an app's checkout link to open its one
// checkout, answering with the session the page mounts.
func (s *Server) handleCheckoutLinkSession(w http.ResponseWriter, r *http.Request) {
	if s.embedded() == nil {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not found")
		return
	}
	claims, ok := s.readCheckoutToken(chi.URLParam(r, "token"), purposeOpen)
	if ok {
		var err error
		if ok, err = s.useCheckoutLink(r.Context(), claims); err != nil {
			serverError(w, r, "using checkout link", err)
			return
		}
	}
	if ok {
		var exists bool
		if err := s.database.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, claims.UserID).Scan(&exists); err != nil {
			serverError(w, r, "reading checkout account", err)
			return
		}
		ok = exists
	}
	if !ok {
		httputil.WriteError(w, r, http.StatusGone, codeCheckoutLinkExpired, checkoutLinkExpiredMessage)
		return
	}
	completion := s.signCheckoutToken(checkoutClaims{
		Purpose: purposeComplete, UserID: claims.UserID, Expires: time.Now().Add(checkoutCompletionTTL).Unix(),
	})
	s.writeCheckoutSession(w, r, claims.UserID,
		s.cfg.PublicURL+checkoutLinkReturnPath+"?completion="+completion+"&session_id="+checkoutSessionID)
}

// handleCheckoutLinkReturn is where a checkout opened from a link ends. Its
// completion token names the account the checkout must belong to.
func (s *Server) handleCheckoutLinkReturn(w http.ResponseWriter, r *http.Request) {
	if s.embedded() == nil {
		renderCheckoutPage(w, r, http.StatusNotFound, dashboardView{Error: "Paying on the web is not available."})
		return
	}
	claims, ok := s.readCheckoutToken(r.URL.Query().Get("completion"), purposeComplete)
	if !ok {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return
	}
	outcome, err := s.completeCheckout(r.Context(), claims.UserID, r.URL.Query().Get("session_id"))
	switch {
	case errors.Is(err, billing.ErrCheckoutForeign):
		renderCheckoutPage(w, r, http.StatusForbidden, dashboardView{Error: "That checkout belongs to another account."})
		return
	case errors.Is(err, sql.ErrNoRows):
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return
	case err != nil:
		dashboardServerError(w, r, "confirming checkout", err)
		return
	}
	v := dashboardView{}
	switch outcome {
	case checkoutEntitled:
		v.Notice = "Thank you. Your subscription is active. You can return to the app."
	case checkoutProcessing:
		v.Notice = "Your payment is processing. The app unlocks sync once it clears."
		v.PendingURL = r.URL.RequestURI()
	case checkoutOpen:
		v.Error = "The payment did not go through. Start again from the app."
	}
	renderCheckoutPage(w, r, http.StatusOK, v)
}
