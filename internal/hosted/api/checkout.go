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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/stripe"
	"futo-notes-server/internal/hosted/country"
	"futo-notes-server/internal/httputil"
)

// Custom checkout: a provider whose checkout is a page of ours
// (billing.CustomCheckout, i.e. Stripe). It starts by asking for the
// buyer's billing country, which decides the route: in the self-payment
// countries our page shows the plan and the account, the provider's own
// fields (Stripe's Payment Element, in its frames) take the card, and our
// button posts what they collected to the server, which pays; everywhere
// else the browser is sent to the provider's own page, where it sells as
// merchant of record; a few countries cannot pay on the web. The dashboard
// asks in place; the apps get a one-time link to a page of ours that needs
// no dashboard session. Once a payment goes through, by either route, the
// server applies its subscription at once, so entitlement never waits for
// the webhook, which stays the source of every later change.
// Our page's payment is bound to the country chosen: the card form starts
// in it and holds the button while it names another, the provider refuses
// to pay with a billing address elsewhere before charging anything, and a
// payment billed elsewhere anyway is cancelled and refunded.
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
	// checkoutLinkCancelledPath is where a checkout opened from a link is
	// abandoned.
	checkoutLinkCancelledPath = checkoutLinkPath + "/cancelled"
	// checkoutSessionID is the provider's placeholder for the session id in
	// its page's return URL.
	checkoutSessionID = "{CHECKOUT_SESSION_ID}"
	// payPath is our dashboard panel's Pay.
	payPath = dashboardPath + "/billing/pay"

	// checkoutLinkTTL is how long an app's checkout link may be opened.
	checkoutLinkTTL = 15 * time.Minute
	// checkoutCompletionTTL is how long a link checkout's return URL can
	// still confirm it: past a payment that takes a while.
	checkoutCompletionTTL = 24 * time.Hour

	// checkoutCSP is dashboardCSP widened by what Stripe documents for
	// Stripe.js (docs.stripe.com/security/guide, "Content Security
	// Policy"), for the pages that mount checkout and only them. Stripe.js
	// must load from js.stripe.com and talks to api.stripe.com; the Payment
	// Element runs in js.stripe.com frames; 3D Secure uses hooks.stripe.com.
	// Nothing else is needed: Link, wallets and the Address Element are off.
	checkoutCSP = "default-src 'none'; " +
		"script-src 'self' https://js.stripe.com https://*.js.stripe.com; " +
		"style-src 'self'; img-src 'self'; font-src 'self'; " +
		"connect-src 'self' https://api.stripe.com; " +
		"frame-src https://js.stripe.com https://*.js.stripe.com https://hooks.stripe.com; " +
		"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// The purposes of a checkout token.
const (
	// purposeOpen opens one checkout for its user: an app's link. It is
	// used when its country step is posted.
	purposeOpen = "open"
	// purposePay pays once on our page for its user and billing country:
	// what an app's link becomes once a self-payment country is chosen. It
	// is used by the payment that goes through, so a declined card, or the
	// page reloaded, can try again under it.
	purposePay = "pay"
	// purposeComplete confirms a checkout opened from a link.
	purposeComplete = "complete"
)

// The country step's messages.
const (
	chooseCountryMessage = "Choose your billing country from the list."
	countryUnavailable   = "Paying on the web isn't available in your country yet."
)

// custom returns the web provider when its checkout is a page of ours.
func (s *Server) custom() billing.CustomCheckout {
	c, _ := s.providers.Web.(billing.CustomCheckout)
	return c
}

// checkoutClaims are what a checkout token says, under the server's HMAC.
type checkoutClaims struct {
	Purpose string `json:"p"`
	UserID  string `json:"u"`
	Expires int64  `json:"e"`
	// Nonce identifies an open or pay token, so it is used once.
	Nonce string `json:"n,omitempty"`
	// Country is a pay token's billing country.
	Country string `json:"c,omitempty"`
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
		(purpose != purposeComplete && c.Nonce == "") || (purpose == purposePay && c.Country == "") {
		return checkoutClaims{}, false
	}
	return c, true
}

// oneTimeToken signs a token for purpose, userID and billing country
// (pay tokens only) that can be used once within checkoutLinkTTL.
func (s *Server) oneTimeToken(purpose, userID, billingCountry string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return s.signCheckoutToken(checkoutClaims{
		Purpose: purpose, UserID: userID, Expires: time.Now().Add(checkoutLinkTTL).Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), Country: billingCountry,
	}), nil
}

// checkoutLink is a one-time link to the checkout page for userID.
func (s *Server) checkoutLink(userID string) (string, error) {
	token, err := s.oneTimeToken(purposeOpen, userID, "")
	if err != nil {
		return "", err
	}
	return s.cfg.PublicURL + checkoutLinkPath + "/" + token, nil
}

// completionToken names the account a link checkout's return confirms.
func (s *Server) completionToken(userID string) string {
	return s.signCheckoutToken(checkoutClaims{
		Purpose: purposeComplete, UserID: userID, Expires: time.Now().Add(checkoutCompletionTTL).Unix(),
	})
}

// checkoutLinkUsed reports whether an open or pay token has been used.
func (s *Server) checkoutLinkUsed(ctx context.Context, c checkoutClaims) (bool, error) {
	var used bool
	err := s.database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM checkout_token_uses WHERE nonce = $1)`, c.Nonce).Scan(&used)
	return used, err
}

// useCheckoutLink records that an open or pay token was used, and reports
// false if it already had been. Uses are kept until the token would have
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

// paymentSession is what a checkout panel mounts its card fields with:
// the provider's client secret for the account's saved cards, which is for
// the browser alone and never logged.
type paymentSession struct {
	ClientSecret string `json:"client_secret"`
}

// writePaymentSession answers with the payment session for userID's panel.
func (s *Server) writePaymentSession(w http.ResponseWriter, r *http.Request, userID string) {
	err := s.checkoutAllowed(r.Context(), userID)
	var secret string
	if err == nil {
		var customer billing.Customer
		if customer, err = s.billing.Customer(r.Context(), userID); err == nil {
			secret, err = s.custom().StartPayment(r.Context(), customer)
		}
	}
	if err != nil {
		writeBillingURLError(w, r, "opening payment", err)
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, paymentSession{ClientSecret: secret})
}

// payRequest is what our page posts to pay: the ConfirmationToken its card
// fields made for this attempt, or, once the payment's next action (3-D
// Secure) is handled, the subscription it was for.
type payRequest struct {
	ConfirmationToken string `json:"confirmation_token"`
	Subscription      string `json:"subscription"`
}

// payResponse tells our page what comes next: "requires_action", with the
// client secret that handles it and the subscription to post back, or
// "complete", with the page that shows the result. The client secret is
// for the browser alone and never logged.
type payResponse struct {
	Status       string `json:"status"`
	ClientSecret string `json:"client_secret,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	ReturnURL    string `json:"return_url,omitempty"`
}

// readPayRequest decodes a payRequest naming exactly one of its fields,
// answering 400 otherwise.
func readPayRequest(w http.ResponseWriter, r *http.Request) (payRequest, bool) {
	var req payRequest
	if !decodeJSON(w, r, &req) {
		return payRequest{}, false
	}
	if (req.ConfirmationToken == "") == (req.Subscription == "") {
		badRequest(w, r, "Send confirmation_token or subscription.")
		return payRequest{}, false
	}
	return req, true
}

// pay takes one step of a payment on our page for userID, who chose
// billingCountry: paying with a ConfirmationToken, refused while the
// account is entitled, or finishing a payment after its next action. A
// payment that went through is applied at once. Steps for one account run
// one at a time, on every replica, so two tabs cannot both pay.
func (s *Server) pay(ctx context.Context, userID, billingCountry string, req payRequest) (billing.PaymentResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return billing.PaymentResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "notes-api pay: "+userID); err != nil {
		return billing.PaymentResult{}, err
	}
	customer, err := s.billing.Customer(ctx, userID)
	if err != nil {
		return billing.PaymentResult{}, err
	}
	var result billing.PaymentResult
	if req.Subscription != "" {
		result, err = s.custom().FinishPayment(ctx, customer, req.Subscription)
	} else {
		if err := s.checkoutAllowed(ctx, userID); err != nil {
			return billing.PaymentResult{}, err
		}
		result, err = s.custom().Pay(ctx, customer, billing.Payment{BillingCountry: billingCountry, ConfirmationToken: req.ConfirmationToken})
	}
	if err != nil || result.ActionSecret != "" {
		return result, err
	}
	_, err = s.billing.Apply(ctx, result.Event)
	return result, err
}

// writePayResult answers our page's Pay: what comes next, or why the
// payment was refused. returnURL shows the result of a payment that went
// through.
func writePayResult(w http.ResponseWriter, r *http.Request, result billing.PaymentResult, err error, returnURL string) {
	mismatch := (*billing.CountryMismatchError)(nil)
	declined := (*billing.PaymentDeclinedError)(nil)
	switch {
	case errors.As(err, &mismatch):
		httputil.WriteError(w, r, http.StatusUnprocessableEntity, codeBillingCountryMismatch, countryMismatchMessage(mismatch))
	case errors.As(err, &declined):
		httputil.WriteError(w, r, http.StatusPaymentRequired, codePaymentDeclined, declined.Message)
	case errors.Is(err, billing.ErrSubscribed):
		httputil.WriteError(w, r, http.StatusConflict, codeAlreadySubscribed, "You already have an active subscription. Nothing was charged.")
	case errors.Is(err, billing.ErrPaymentPending):
		httputil.WriteError(w, r, http.StatusConflict, codePaymentPending,
			"A payment is waiting for your bank's confirmation in another window. Finish it there, or try again in 15 minutes. Nothing was charged.")
	case errors.Is(err, billing.ErrCheckoutForeign):
		badRequest(w, r, "That payment is unknown or belongs to another account.")
	case err != nil:
		writeBillingURLError(w, r, "paying", err)
	case result.ActionSecret != "":
		httputil.WriteJSON(w, r, http.StatusOK, payResponse{Status: "requires_action", ClientSecret: result.ActionSecret, Subscription: result.SubscriptionID})
	default:
		httputil.WriteJSON(w, r, http.StatusOK, payResponse{Status: "complete", Subscription: result.SubscriptionID, ReturnURL: returnURL})
	}
}

// merchantCheckout opens a checkout for userID on the provider's own page,
// as merchant of record, and returns the URL to send the browser to.
func (s *Server) merchantCheckout(ctx context.Context, userID, successURL, cancelURL string) (string, error) {
	if err := s.checkoutAllowed(ctx, userID); err != nil {
		return "", err
	}
	customer, err := s.billing.Customer(ctx, userID)
	if err != nil {
		return "", err
	}
	return s.custom().StartMerchantCheckout(ctx, customer, successURL, cancelURL)
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

// completeCheckout confirms what a return page names: a payment on our
// page (?subscription=) or a checkout on the provider's (?session_id=). It
// fetches it from the provider and applies the subscription, as a refresh
// does. It returns billing.ErrCheckoutForeign for another account's, and
// the provider's *billing.CountryMismatchError for one it refused.
func (s *Server) completeCheckout(ctx context.Context, userID string, query url.Values) (checkoutOutcome, error) {
	customer, err := s.billing.Customer(ctx, userID)
	if err != nil {
		return 0, err
	}
	var ev billing.Event
	if id := query.Get("subscription"); id != "" {
		var result billing.PaymentResult
		result, err = s.custom().FinishPayment(ctx, customer, id)
		if result.ActionSecret != "" || errors.As(err, new(*billing.PaymentDeclinedError)) {
			return checkoutOpen, nil
		}
		ev = result.Event
	} else {
		ev, err = s.custom().CompleteCheckout(ctx, customer, query.Get("session_id"))
		if errors.Is(err, billing.ErrCheckoutIncomplete) {
			return checkoutOpen, nil
		}
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

// checkoutPanel is the custom checkout as a page shows it.
type checkoutPanel struct {
	PublishableKey string
	// SessionURL is POSTed for the paymentSession to mount; PayURL for each
	// step of a payment (payRequest).
	SessionURL, PayURL string
	// PageURL is a page that hosts checkout, for a panel that landed in
	// a document without checkoutCSP.
	PageURL string
	Plan    checkoutPlan
	// Amount and Currency are the plan's, which the card fields are made
	// for: what the first payment charges, in Currency's minor units.
	Amount   int64
	Currency string
	// Email is the account's, which the subscription's receipts go to.
	Email string
	// Country is the billing country chosen, which the card form starts in
	// and must keep; CountryName is its name.
	Country, CountryName string
	// ChangeURL goes back to the country step, where the flow has one; HX
	// loads it with htmx.
	ChangeURL string
	ChangeHX  bool
	// CancelURL leaves checkout; CancelHX loads it with htmx.
	CancelURL string
	CancelHX  bool
}

// checkoutPlan is a billing.Plan as the page words it.
type checkoutPlan struct {
	// Price is the amount charged each period, "$5.00".
	Price string
	// Period follows a slash: "month", or "3 months".
	Period string
	// Renews says how often: "monthly", or "every 3 months".
	Renews string
}

func planView(p billing.Plan) checkoutPlan {
	major, minor := p.Amount/100, p.Amount%100
	price := fmt.Sprintf("%d.%02d %s", major, minor, strings.ToUpper(p.Currency))
	if p.Currency == "usd" {
		price = fmt.Sprintf("$%d.%02d", major, minor)
	}
	if p.IntervalCount > 1 {
		period := fmt.Sprintf("%d %ss", p.IntervalCount, p.Interval)
		return checkoutPlan{Price: price, Period: period, Renews: "every " + period}
	}
	renews := map[string]string{"day": "daily", "week": "weekly", "month": "monthly", "year": "yearly"}[p.Interval]
	return checkoutPlan{Price: price, Period: p.Interval, Renews: renews}
}

// newCheckoutPanel is the panel for an account with email paying from
// billingCountry, its plan read from the provider.
func (s *Server) newCheckoutPanel(ctx context.Context, email, billingCountry, sessionURL, payURL, pageURL string) (*checkoutPanel, error) {
	plan, err := s.custom().Plan(ctx)
	if err != nil {
		return nil, err
	}
	return &checkoutPanel{
		PublishableKey: s.custom().PublishableKey(), SessionURL: sessionURL, PayURL: payURL, PageURL: pageURL,
		Plan: planView(plan), Amount: plan.Amount, Currency: plan.Currency,
		Email: email, Country: billingCountry, CountryName: country.Name(billingCountry),
	}, nil
}

// countryStep asks for the buyer's billing country before checkout, the
// list prefilled from the browser's languages. Each option carries its
// route, so the page can say before posting that a country cannot pay on
// the web, or that tax is added on the provider's page.
type countryStep struct {
	// Action is where Checkout posts the country.
	Action string
	// CSRF is the dashboard session's token; empty on an app's link page,
	// whose token authorizes the post.
	CSRF      string
	Plan      checkoutPlan
	Countries []countryOption
	// Selected is the country chosen, or prefilled; "" for none.
	Selected string
	// Route is Selected's route, as routeName names it.
	Route string
	// Error says why a post was refused.
	Error string
	// CancelURL leaves checkout; CancelHX loads it with htmx.
	CancelURL string
	CancelHX  bool
}

// countryOption is a country in the step's list.
type countryOption struct {
	Code, Name, Route string
}

// routeName is how the page names a billing.CheckoutRoute.
func routeName(r billing.CheckoutRoute) string {
	switch r {
	case billing.RouteSelf:
		return "self"
	case billing.RouteMerchantOfRecord:
		return "merchant"
	}
	return "unavailable"
}

// newCountryStep is the country step with selected chosen, its plan read
// from the provider.
func (s *Server) newCountryStep(ctx context.Context, action, selected string) (*countryStep, error) {
	plan, err := s.custom().Plan(ctx)
	if err != nil {
		return nil, err
	}
	step := &countryStep{Action: action, Plan: planView(plan), Selected: selected}
	for _, c := range country.All() {
		step.Countries = append(step.Countries, countryOption{Code: c.Code, Name: c.Name, Route: routeName(s.custom().Route(c.Code))})
	}
	if selected != "" {
		step.Route = routeName(s.custom().Route(selected))
	}
	return step, nil
}

// browserCountry is the billing country the request's Accept-Language
// suggests, to prefill the country step; "" when it names none.
func browserCountry(r *http.Request) string {
	return country.FromAcceptLanguage(r.Header.Get("Accept-Language"))
}

// checkoutHost is what a page that hosts custom checkout loads.
type checkoutHost struct {
	ScriptURL string
}

// hostCheckout makes v a page that can mount custom checkout.
func hostCheckout(v *dashboardView) {
	v.CheckoutHost = &checkoutHost{ScriptURL: stripe.JSURL}
}

// handleDashboardSubscribe shows the overview with the next checkout step
// open: given a billing country (?country=) that pays on our page, the
// checkout panel; otherwise the country step, with that country chosen or
// the browser's prefilled.
func (s *Server) handleDashboardSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		redirectDashboard(w, r, dashboardPath)
		return
	}
	code, ok := country.Parse(r.URL.Query().Get("country"))
	if ok && s.custom().Route(code) == billing.RouteSelf {
		s.showDashboardPanel(w, r, code)
		return
	}
	if !ok {
		code = browserCountry(r)
	}
	s.showDashboardCountryStep(w, r, code, "")
}

// showDashboardPanel shows the overview with the checkout panel for a
// self-payment billing country.
func (s *Server) showDashboardPanel(w http.ResponseWriter, r *http.Request, billingCountry string) {
	panel, err := s.dashboardPanel(r, billingCountry)
	if err != nil {
		dashboardServerError(w, r, "reading plan", err)
		return
	}
	s.overviewWith(w, r, func(v *dashboardView) {
		if !v.Billing.Entitled {
			v.Checkout = panel
		}
	})
}

// showDashboardCountryStep shows the overview with the country step,
// selected chosen and notice above it.
func (s *Server) showDashboardCountryStep(w http.ResponseWriter, r *http.Request, selected, notice string) {
	step, err := s.dashboardCountryStep(r, selected)
	if err != nil {
		dashboardServerError(w, r, "reading plan", err)
		return
	}
	s.overviewWith(w, r, func(v *dashboardView) {
		if !v.Billing.Entitled {
			v.CountryStep = step
			v.Notice = notice
		}
	})
}

func (s *Server) dashboardCountryStep(r *http.Request, selected string) (*countryStep, error) {
	step, err := s.newCountryStep(r.Context(), subscribePath, selected)
	if err != nil {
		return nil, err
	}
	step.CSRF, _ = r.Context().Value(csrfCtxKey{}).(string)
	step.CancelURL, step.CancelHX = dashboardPath, true
	return step, nil
}

func (s *Server) dashboardPanel(r *http.Request, billingCountry string) (*checkoutPanel, error) {
	query := "?country=" + billingCountry
	panel, err := s.newCheckoutPanel(r.Context(), principalFrom(r).Session.User.Email, billingCountry,
		dashboardPath+"/billing/checkout"+query, payPath+query, subscribePath+query)
	if err != nil {
		return nil, err
	}
	panel.ChangeURL, panel.ChangeHX = subscribePath, true
	panel.CancelURL, panel.CancelHX = dashboardPath, true
	return panel, nil
}

// handleDashboardChooseCountry is the country step's Checkout, the
// dashboard's POST /api/billing/checkout for a billing country: our panel
// for a self-payment country, the provider's page for the rest, and the
// step again, saying so, for a country that cannot pay on the web.
func (s *Server) handleDashboardChooseCountry(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		redirectDashboard(w, r, dashboardPath)
		return
	}
	code, ok := country.Parse(r.PostFormValue("country"))
	if !ok {
		renderDashboardError(w, r, http.StatusBadRequest, chooseCountryMessage)
		return
	}
	switch s.custom().Route(code) {
	case billing.RouteSelf:
		afterAction(w, r, subscribePath+"?country="+code, func() { s.showDashboardPanel(w, r, code) })
	case billing.RouteMerchantOfRecord:
		target, err := s.merchantCheckout(r.Context(), principalFrom(r).Session.User.ID,
			s.cfg.PublicURL+checkoutReturnPath+"?session_id="+checkoutSessionID, s.cfg.PublicURL+subscribePath+"?country="+code)
		if err != nil {
			dashboardBillingError(w, r, "creating checkout", err)
			return
		}
		redirectAway(w, r, target)
	default:
		s.showDashboardCountryStep(w, r, code, "")
	}
}

// handleDashboardPay is the custom checkout panel's Pay, for a
// self-payment billing country (?country=): each step of a payment
// (payRequest), answered as JSON.
func (s *Server) handleDashboardPay(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not found")
		return
	}
	code, ok := s.selfPaymentCountry(w, r)
	if !ok {
		return
	}
	req, ok := readPayRequest(w, r)
	if !ok {
		return
	}
	result, err := s.pay(r.Context(), principalFrom(r).Session.User.ID, code, req)
	writePayResult(w, r, result, err, s.cfg.PublicURL+checkoutReturnPath+"?subscription="+url.QueryEscape(result.SubscriptionID))
}

// handleDashboardCheckoutReturn is where a dashboard checkout ends, by
// either route: Stripe sends the browser here after a payment on its page,
// and our panel swaps it in once its payment went through. It confirms the
// payment and shows the overview.
func (s *Server) handleDashboardCheckoutReturn(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		redirectDashboard(w, r, dashboardPath)
		return
	}
	outcome, err := s.completeCheckout(r.Context(), principalFrom(r).Session.User.ID, r.URL.Query())
	if mismatch := (*billing.CountryMismatchError)(nil); errors.As(err, &mismatch) {
		selected := mismatch.Billed
		if selected == "" {
			selected = browserCountry(r)
		}
		s.showDashboardCountryStep(w, r, selected, countryMismatchMessage(mismatch)+" Choose your billing country to try again.")
		return
	}
	if errors.Is(err, billing.ErrCheckoutForeign) {
		renderDashboardError(w, r, http.StatusForbidden, "That checkout belongs to another account.")
		return
	}
	if err != nil {
		dashboardServerError(w, r, "confirming checkout", err)
		return
	}
	if outcome == checkoutOpen {
		s.showDashboardCountryStep(w, r, browserCountry(r), "The payment did not go through. You can try again.")
		return
	}
	s.overviewWith(w, r, func(v *dashboardView) {
		switch outcome {
		case checkoutEntitled:
			v.Subscribed = "Thank you. Your subscription is active, and your devices can sync."
		case checkoutProcessing:
			v.Notice = "Your payment is processing. This page updates when it clears."
			v.PendingURL = r.URL.RequestURI()
		}
	})
}

// checkoutPageView is the signed-out checkout page.
func checkoutPageView() dashboardView {
	v := dashboardView{}
	hostCheckout(&v)
	return v
}

// renderCheckoutPage answers with the signed-out checkout page, or for htmx
// just the part inside #page.
func renderCheckoutPage(w http.ResponseWriter, r *http.Request, status int, v dashboardView) {
	if v.CheckoutHost != nil {
		w.Header().Set("Content-Security-Policy", checkoutCSP)
	}
	name := "layout"
	if isHTMX(r) {
		name = "fragment"
	}
	writeTemplate(w, r, status, "checkout", name, v)
}

const checkoutLinkExpiredMessage = "This checkout link has expired or was already used. Start again from the app."

// countryMismatchMessage says why a payment was refused for its billing
// country, and what became of it.
func countryMismatchMessage(m *billing.CountryMismatchError) string {
	billed := "another country"
	if m.Billed != "" {
		billed = country.Name(m.Billed)
	}
	if !m.Refunded {
		return fmt.Sprintf("Your card's billing address is in %s, but you chose %s as your billing country. "+
			"Nothing was charged.", billed, country.Name(m.Chosen))
	}
	return fmt.Sprintf("Your card's billing address was in %s, but you chose %s as your billing country. "+
		"We cancelled that subscription and refunded the payment.", billed, country.Name(m.Chosen))
}

// linkAccount checks an app's checkout link: a token signed for purpose,
// not yet used, for an account that still exists and is not already
// subscribed. Otherwise it answers with the page that says so and returns
// false.
func (s *Server) linkAccount(w http.ResponseWriter, r *http.Request, token, purpose string) (checkoutClaims, billing.Customer, bool) {
	claims, ok := s.readCheckoutToken(token, purpose)
	used := false
	if ok {
		var err error
		if used, err = s.checkoutLinkUsed(r.Context(), claims); err != nil {
			dashboardServerError(w, r, "reading checkout link", err)
			return checkoutClaims{}, billing.Customer{}, false
		}
	}
	if !ok || used {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return checkoutClaims{}, billing.Customer{}, false
	}
	err := s.checkoutAllowed(r.Context(), claims.UserID)
	var customer billing.Customer
	if err == nil {
		customer, err = s.billing.Customer(r.Context(), claims.UserID)
	}
	if errors.Is(err, errAlreadySubscribed) {
		renderCheckoutPage(w, r, http.StatusOK, dashboardView{Notice: "You already have an active subscription. You can return to the app."})
		return checkoutClaims{}, billing.Customer{}, false
	}
	if errors.Is(err, sql.ErrNoRows) {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return checkoutClaims{}, billing.Customer{}, false
	}
	if err != nil {
		dashboardServerError(w, r, "reading checkout account", err)
		return checkoutClaims{}, billing.Customer{}, false
	}
	return claims, customer, true
}

// handleCheckoutLinkPage serves an app's checkout link, without a
// dashboard session: the country step for the link itself, and our
// checkout panel for the pay token a self-payment country turned it into.
// Opening either, or reloading it, does not use it.
func (s *Server) handleCheckoutLinkPage(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		renderCheckoutPage(w, r, http.StatusNotFound, dashboardView{Error: "Paying on the web is not available."})
		return
	}
	token := chi.URLParam(r, "token")
	if _, ok := s.readCheckoutToken(token, purposePay); ok {
		s.checkoutLinkPanel(w, r, token)
		return
	}
	if _, _, ok := s.linkAccount(w, r, token, purposeOpen); !ok {
		return
	}
	s.checkoutLinkCountryStep(w, r, http.StatusOK, token, browserCountry(r), "")
}

// checkoutLinkCountryStep answers with an app link's country step.
func (s *Server) checkoutLinkCountryStep(w http.ResponseWriter, r *http.Request, status int, token, selected, message string) {
	step, err := s.newCountryStep(r.Context(), checkoutLinkPath+"/"+token, selected)
	if err != nil {
		dashboardServerError(w, r, "reading plan", err)
		return
	}
	step.Error = message
	step.CancelURL = checkoutLinkCancelledPath
	renderCheckoutPage(w, r, status, dashboardView{CountryStep: step})
}

// checkoutLinkPanel answers with our checkout page for a pay token.
func (s *Server) checkoutLinkPanel(w http.ResponseWriter, r *http.Request, token string) {
	claims, customer, ok := s.linkAccount(w, r, token, purposePay)
	if !ok {
		return
	}
	if s.custom().Route(claims.Country) != billing.RouteSelf {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return
	}
	v := checkoutPageView()
	var err error
	page := checkoutLinkPath + "/" + token
	v.Checkout, err = s.newCheckoutPanel(r.Context(), customer.Email, claims.Country, page+"/session", page+"/pay", page)
	if err != nil {
		dashboardServerError(w, r, "reading plan", err)
		return
	}
	v.Checkout.CancelURL = checkoutLinkCancelledPath
	renderCheckoutPage(w, r, http.StatusOK, v)
}

// handleCheckoutLinkCountry is an app link's Checkout: it uses the link
// and sends the browser on by the billing country's route, to our checkout
// page under a pay token or to the provider's page. A country that cannot
// pay on the web, or is not one, leaves the link unused.
func (s *Server) handleCheckoutLinkCountry(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		renderCheckoutPage(w, r, http.StatusNotFound, dashboardView{Error: "Paying on the web is not available."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	token := chi.URLParam(r, "token")
	claims, _, ok := s.linkAccount(w, r, token, purposeOpen)
	if !ok {
		return
	}
	code, ok := country.Parse(r.PostFormValue("country"))
	if !ok {
		s.checkoutLinkCountryStep(w, r, http.StatusBadRequest, token, "", chooseCountryMessage)
		return
	}
	route := s.custom().Route(code)
	if route == billing.RouteUnavailable {
		s.checkoutLinkCountryStep(w, r, http.StatusOK, token, code, "")
		return
	}
	used, err := s.useCheckoutLink(r.Context(), claims)
	if err != nil {
		dashboardServerError(w, r, "using checkout link", err)
		return
	}
	if !used {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return
	}
	if route == billing.RouteSelf {
		pay, err := s.oneTimeToken(purposePay, claims.UserID, code)
		if err != nil {
			dashboardServerError(w, r, "opening checkout", err)
			return
		}
		redirectAway(w, r, checkoutLinkPath+"/"+pay)
		return
	}
	target, err := s.merchantCheckout(r.Context(), claims.UserID,
		s.cfg.PublicURL+checkoutLinkReturnPath+"?completion="+s.completionToken(claims.UserID)+"&session_id="+checkoutSessionID,
		s.cfg.PublicURL+checkoutLinkCancelledPath)
	switch {
	case errors.Is(err, errAlreadySubscribed):
		renderCheckoutPage(w, r, http.StatusOK, dashboardView{Notice: "You already have an active subscription. You can return to the app."})
	case err != nil:
		dashboardServerError(w, r, "creating checkout", err)
	default:
		redirectAway(w, r, target)
	}
}

// handleCheckoutLinkCancelled is where an app's checkout is abandoned.
func handleCheckoutLinkCancelled(w http.ResponseWriter, r *http.Request) {
	renderCheckoutPage(w, r, http.StatusOK, dashboardView{Notice: "No payment was taken. You can close this page and return to the app."})
}

// payToken reads the pay token of a request from our page under it: one
// for a self-payment country, not yet used, whose account exists.
// Otherwise it answers 410 and returns false.
func (s *Server) payToken(w http.ResponseWriter, r *http.Request) (checkoutClaims, bool) {
	claims, ok := s.readCheckoutToken(chi.URLParam(r, "token"), purposePay)
	ok = ok && s.custom().Route(claims.Country) == billing.RouteSelf
	if ok {
		used, err := s.checkoutLinkUsed(r.Context(), claims)
		if err != nil {
			serverError(w, r, "reading checkout link", err)
			return checkoutClaims{}, false
		}
		ok = !used
	}
	if ok {
		if err := s.database.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, claims.UserID).Scan(&ok); err != nil {
			serverError(w, r, "reading checkout account", err)
			return checkoutClaims{}, false
		}
	}
	if !ok {
		httputil.WriteError(w, r, http.StatusGone, codeCheckoutLinkExpired, checkoutLinkExpiredMessage)
	}
	return claims, ok
}

// handleCheckoutLinkSession answers with the payment session our page
// mounts under a pay token, which it does not use.
func (s *Server) handleCheckoutLinkSession(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not found")
		return
	}
	if claims, ok := s.payToken(w, r); ok {
		s.writePaymentSession(w, r, claims.UserID)
	}
}

// handleCheckoutLinkPay is our page's Pay under a pay token. The payment
// that goes through uses it; until then, the page may try again.
func (s *Server) handleCheckoutLinkPay(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not found")
		return
	}
	claims, ok := s.payToken(w, r)
	if !ok {
		return
	}
	req, ok := readPayRequest(w, r)
	if !ok {
		return
	}
	result, err := s.pay(r.Context(), claims.UserID, claims.Country, req)
	if err == nil && result.ActionSecret == "" {
		if _, err := s.useCheckoutLink(r.Context(), claims); err != nil {
			serverError(w, r, "using checkout link", err)
			return
		}
	}
	writePayResult(w, r, result, err, s.cfg.PublicURL+checkoutLinkReturnPath+"?completion="+s.completionToken(claims.UserID)+
		"&subscription="+url.QueryEscape(result.SubscriptionID))
}

// handleCheckoutLinkReturn is where a checkout opened from a link ends, by
// either route. Its completion token names the account the checkout must
// belong to.
func (s *Server) handleCheckoutLinkReturn(w http.ResponseWriter, r *http.Request) {
	if s.custom() == nil {
		renderCheckoutPage(w, r, http.StatusNotFound, dashboardView{Error: "Paying on the web is not available."})
		return
	}
	claims, ok := s.readCheckoutToken(r.URL.Query().Get("completion"), purposeComplete)
	if !ok {
		renderCheckoutPage(w, r, http.StatusGone, dashboardView{Error: checkoutLinkExpiredMessage})
		return
	}
	outcome, err := s.completeCheckout(r.Context(), claims.UserID, r.URL.Query())
	mismatch := (*billing.CountryMismatchError)(nil)
	switch {
	case errors.As(err, &mismatch):
		renderCheckoutPage(w, r, http.StatusOK, dashboardView{Error: countryMismatchMessage(mismatch) + " Start again from the app."})
		return
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
		v.Subscribed = "Thank you. Your subscription is active. Return to the FUTO Notes app to start syncing; you can close this page."
	case checkoutProcessing:
		v.Notice = "Your payment is processing. The app unlocks sync once it clears."
		v.PendingURL = r.URL.RequestURI()
	case checkoutOpen:
		v.Error = "The payment did not go through. Start again from the app."
	}
	renderCheckoutPage(w, r, http.StatusOK, v)
}
