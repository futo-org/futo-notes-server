package api_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/stripe"
	"futo-notes-server/internal/hosted/config"
)

const checkoutCSP = "default-src 'none'; " +
	"script-src 'self' https://js.stripe.com https://*.js.stripe.com https://checkout.stripe.com; " +
	"style-src 'self'; img-src 'self' https://*.stripe.com; font-src 'self'; " +
	"connect-src 'self' https://api.stripe.com https://checkout.stripe.com; " +
	"frame-src https://js.stripe.com https://*.js.stripe.com https://hooks.stripe.com https://checkout.stripe.com; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

const testPublishableKey = "pk_test_embedded"

var checkoutTokenKey = bytes.Repeat([]byte{0x24}, 32)

// fakeEmbedded is a web provider whose checkout is embedded, as Stripe's
// is: it opens sessions, which the test completes.
type fakeEmbedded struct {
	mu       sync.Mutex
	next     int
	sessions map[string]*fakeCheckout
	subs     map[string]billing.Event
}

type fakeCheckout struct {
	subject   string
	returnURL string
	secret    string
	complete  bool
	subID     string
}

func newFakeEmbedded() *fakeEmbedded {
	return &fakeEmbedded{sessions: map[string]*fakeCheckout{}, subs: map[string]billing.Event{}}
}

func (f *fakeEmbedded) Source() billing.Source { return billing.SourceWeb }
func (f *fakeEmbedded) PublishableKey() string { return testPublishableKey }

func (f *fakeEmbedded) CheckoutURL(context.Context, billing.Customer) (string, error) {
	return "", billing.ErrNotSupported
}

func (f *fakeEmbedded) PortalURL(_ context.Context, c billing.Customer) (string, error) {
	return "https://portal.test/" + c.Subject, nil
}

func (f *fakeEmbedded) StartCheckout(_ context.Context, c billing.Customer, returnURL string) (billing.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Subject == "" {
		return billing.Checkout{}, fmt.Errorf("checkout for %+v without a subject", c)
	}
	f.next++
	id := fmt.Sprintf("cs_test_%d", f.next)
	s := &fakeCheckout{subject: c.Subject, returnURL: returnURL, secret: id + "_secret_MUST-NOT-BE-LOGGED"}
	f.sessions[id] = s
	return billing.Checkout{ID: id, ClientSecret: s.secret}, nil
}

// pay completes checkout id, creating its subscription.
func (f *fakeEmbedded) pay(id string, status billing.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	s.complete = true
	s.subID = "sub_" + id
	f.subs[s.subID] = billing.Event{
		Source: billing.SourceWeb, ExternalID: s.subID, Subject: s.subject, ProductID: "prod_notes", Status: status,
		WillRenew: true, ExpiresAt: new(time.Now().Add(30 * 24 * time.Hour)), Environment: billing.EnvironmentSandbox,
		EventAt: time.Now(), Raw: []byte(`{"fake":true}`),
	}
}

func (f *fakeEmbedded) CompleteCheckout(_ context.Context, c billing.Customer, id string) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok || s.subject != c.Subject {
		return billing.Event{}, billing.ErrCheckoutForeign
	}
	if !s.complete {
		return billing.Event{}, billing.ErrCheckoutIncomplete
	}
	ev := f.subs[s.subID]
	ev.EventAt = time.Now()
	return ev, nil
}

func (f *fakeEmbedded) Verify(context.Context, billing.Delivery) (string, []billing.Event, error) {
	return "", nil, billing.ErrInvalidDelivery
}

func (f *fakeEmbedded) Refresh(_ context.Context, id string) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[id], nil
}

func (f *fakeEmbedded) Cancel(context.Context, string) error { return nil }

func (f *fakeEmbedded) session(id string) fakeCheckout {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.sessions[id]
}

type embeddedHarness struct {
	*harness
	web *fakeEmbedded
}

func newEmbeddedHarness(t *testing.T) *embeddedHarness {
	t.Helper()
	e := &embeddedHarness{web: newFakeEmbedded()}
	e.harness = newHarnessWithBilling(t, func(config.Config) billing.Providers {
		return billing.Providers{Web: e.web}
	}, func(c *config.Config) { c.WebBilling.CheckoutTokenKey = checkoutTokenKey })
	return e
}

type sessionBody struct {
	ClientSecret string `json:"client_secret"`
	ReturnURL    string `json:"return_url"`
}

// checkoutID is the session id a return URL names.
func checkoutID(t *testing.T, returnURL string) string {
	t.Helper()
	u, err := url.Parse(returnURL)
	if err != nil || u.Query().Get("session_id") == "" {
		t.Fatalf("return URL %q (%v)", returnURL, err)
	}
	return u.Query().Get("session_id")
}

// signToken makes a checkout token as the server does, under key.
func signToken(key []byte, claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestCheckoutLinkFromTheApp(t *testing.T) {
	h := newEmbeddedHarness(t)
	app, _ := h.appLogin(h.newSub())
	var link struct {
		URL string `json:"url"`
	}
	app.expect(http.MethodPost, "/api/billing/checkout", nil, http.StatusOK).json(t, &link)
	page, err := url.Parse(link.URL)
	if err != nil || !strings.HasPrefix(link.URL, h.api.URL+"/billing/checkout/") {
		t.Fatalf("checkout link %q (%v)", link.URL, err)
	}

	// The system browser has no session: the link alone opens the page.
	browser := h.client()
	res := browser.expect(http.MethodGet, page.Path, nil, http.StatusOK)
	if got := res.header.Get("Content-Security-Policy"); got != checkoutCSP {
		t.Fatalf("checkout page CSP = %q", got)
	}
	expectContains(t, res, `<script src="`+stripe.JSURL+`" defer></script>`, `data-checkout`,
		`data-publishable-key="`+testPublishableKey+`"`, `data-session-url="`+page.Path+`/session"`)
	// Opening the page twice does not use the link.
	browser.expect(http.MethodGet, page.Path, nil, http.StatusOK)

	var session sessionBody
	browser.expect(http.MethodPost, page.Path+"/session", nil, http.StatusOK).json(t, &session)
	id := checkoutID(t, session.ReturnURL)
	if session.ClientSecret != h.web.session(id).secret || !strings.HasPrefix(session.ReturnURL, h.api.URL+"/billing/checkout/return?completion=") {
		t.Fatalf("session = %+v", session)
	}
	if want := strings.Replace(session.ReturnURL, id, "{CHECKOUT_SESSION_ID}", 1); h.web.session(id).returnURL != want {
		t.Fatalf("provider return URL = %q, want %q", h.web.session(id).returnURL, want)
	}

	// Used once: the link no longer opens a checkout, nor its page.
	expectHostedError(t, browser.do(http.MethodPost, page.Path+"/session", nil), http.StatusGone, "checkout_link_expired")
	expectContains(t, browser.expect(http.MethodGet, page.Path, nil, http.StatusGone), "expired or was already used")

	// Returning before paying says so; after paying, the account is
	// entitled at once, without any webhook.
	expectContains(t, browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK), "did not go through")
	if app.billing().Entitled {
		t.Fatal("entitled before paying")
	}
	h.web.pay(id, billing.StatusActive)
	returned := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, returned, "Your subscription is active")
	// The return page mounts nothing, so it keeps the strict policy.
	if returned.header.Get("Content-Security-Policy") != dashboardCSP {
		t.Fatalf("return page CSP = %q", returned.header.Get("Content-Security-Policy"))
	}
	expectLacks(t, returned, "js.stripe.com")
	if b := app.billing(); !b.Entitled || str(b.Source) != "web" {
		t.Fatalf("billing after checkout = %+v", b)
	}
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/checkout", nil), http.StatusConflict, "already_subscribed")
}

func TestCheckoutLinkTokens(t *testing.T) {
	h := newEmbeddedHarness(t)
	app, tokens := h.appLogin(h.newSub())
	browser := h.client()
	valid := func() string {
		var link struct {
			URL string `json:"url"`
		}
		app.expect(http.MethodPost, "/api/billing/checkout", nil, http.StatusOK).json(t, &link)
		return strings.TrimPrefix(link.URL, h.api.URL+"/billing/checkout/")
	}
	claims := func(purpose string, expires time.Time) map[string]any {
		return map[string]any{"p": purpose, "u": tokens.User.ID, "e": expires.Unix(), "n": "nonce-" + purpose + expires.String()}
	}
	token := valid()
	payload, sig, _ := strings.Cut(token, ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(payload)
	other, _ := h.appLogin(h.newSub())
	otherID := other.account().User.ID
	retargeted := base64.RawURLEncoding.EncodeToString(bytes.Replace(decoded, []byte(tokens.User.ID), []byte(otherID), 1))

	for name, bad := range map[string]string{
		"payload re-pointed at another user": retargeted + "." + sig,
		"signature changed":                  payload + "." + base64.RawURLEncoding.EncodeToString([]byte("not the signature at all, 32 b")),
		"no signature":                       payload,
		"garbage":                            "garbage",
		"expired":                            signToken(checkoutTokenKey, claims("open", time.Now().Add(-time.Second))),
		"another key":                        signToken(bytes.Repeat([]byte{1}, 32), claims("open", time.Now().Add(time.Hour))),
		"a completion token":                 signToken(checkoutTokenKey, claims("complete", time.Now().Add(time.Hour))),
	} {
		expectHostedError(t, browser.do(http.MethodPost, "/billing/checkout/"+bad+"/session", nil), http.StatusGone, "checkout_link_expired")
		if res := browser.do(http.MethodGet, "/billing/checkout/"+url.PathEscape(bad), nil); res.status != http.StatusGone {
			t.Errorf("checkout page with %s = %d", name, res.status)
		}
	}
	h.web.mu.Lock()
	opened := len(h.web.sessions)
	h.web.mu.Unlock()
	if opened != 0 {
		t.Fatalf("bad tokens opened %d checkouts", opened)
	}

	// Reuse is refused even when two requests race for one link.
	token = valid()
	results := make(chan int, 4)
	for range 4 {
		go func() {
			res := h.client().do(http.MethodPost, "/billing/checkout/"+token+"/session", nil)
			results <- res.status
		}()
	}
	ok := 0
	for range 4 {
		if <-results == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 4 racing uses opened a checkout, want 1", ok)
	}

	// A link whose account was deleted opens nothing.
	token = valid()
	h.exec(`DELETE FROM users WHERE id = $1`, tokens.User.ID)
	expectHostedError(t, browser.do(http.MethodPost, "/billing/checkout/"+token+"/session", nil), http.StatusGone, "checkout_link_expired")
}

func TestCheckoutLinkReturnRefusesAnotherAccountsSession(t *testing.T) {
	h := newEmbeddedHarness(t)
	open := func(app *client) sessionBody {
		var link struct {
			URL string `json:"url"`
		}
		app.expect(http.MethodPost, "/api/billing/checkout", nil, http.StatusOK).json(t, &link)
		var session sessionBody
		h.client().expect(http.MethodPost, strings.TrimPrefix(link.URL, h.api.URL)+"/session", nil, http.StatusOK).json(t, &session)
		return session
	}
	victim, _ := h.appLogin(h.newSub())
	attacker, _ := h.appLogin(h.newSub())
	victimSession := open(victim)
	attackerSession := open(attacker)
	h.web.pay(checkoutID(t, victimSession.ReturnURL), billing.StatusActive)

	// The attacker's completion token with the victim's session id.
	u, _ := url.Parse(attackerSession.ReturnURL)
	q := u.Query()
	q.Set("session_id", checkoutID(t, victimSession.ReturnURL))
	u.RawQuery = q.Encode()
	expectContains(t, h.client().expect(http.MethodGet, u.String(), nil, http.StatusForbidden), "belongs to another account")
	if attacker.billing().Entitled {
		t.Fatal("another account's checkout entitled the attacker")
	}
	expectContains(t, h.client().expect(http.MethodGet, strings.Replace(victimSession.ReturnURL, "completion=", "completion=x", 1), nil, http.StatusGone), "expired")
}

func TestDashboardEmbeddedCheckout(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&logs))
	t.Cleanup(func() { log.Logger = previous })

	h := newEmbeddedHarness(t)
	browser := h.regionBrowser()

	// The overview hosts checkout: Stripe.js and its policy, a Subscribe
	// that opens the panel in place. Other pages keep the strict policy.
	overview := browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)
	if got := overview.header.Get("Content-Security-Policy"); got != checkoutCSP {
		t.Fatalf("overview CSP = %q", got)
	}
	expectContains(t, overview, `<script src="`+stripe.JSURL+`" defer></script>`, `<meta name="checkout-host"`,
		`hx-get="/dashboard/billing/subscribe"`, `<script src="/dashboard/static/checkout.js" defer></script>`)
	expectLacks(t, overview, `data-checkout `)
	devices := browser.expect(http.MethodGet, "/dashboard/devices", nil, http.StatusOK)
	if got := devices.header.Get("Content-Security-Policy"); got != dashboardCSP {
		t.Fatalf("devices CSP = %q", got)
	}
	expectLacks(t, devices, "js.stripe.com", `name="checkout-host"`)

	panel := browser.do(http.MethodGet, "/dashboard/billing/subscribe", nil, "HX-Request", "true")
	expectContains(t, panel, `data-checkout`, `data-session-url="/dashboard/billing/checkout"`,
		`data-page-url="/dashboard/billing/subscribe"`, `data-publishable-key="`+testPublishableKey+`"`)
	if panel.status != http.StatusOK || panel.header.Get("Content-Security-Policy") != checkoutCSP {
		t.Fatalf("subscribe fragment = %d %v", panel.status, panel.header)
	}
	asset := browser.expect(http.MethodGet, "/dashboard/static/checkout.js", nil, http.StatusOK)
	expectContains(t, asset, "createEmbeddedCheckoutPage", "onComplete")

	// Opening the session needs the CSRF token.
	if res := browser.do(http.MethodPost, "/dashboard/billing/checkout", nil); res.status != http.StatusForbidden {
		t.Fatalf("session without CSRF = %d", res.status)
	}
	var session sessionBody
	res := browser.do(http.MethodPost, "/dashboard/billing/checkout", nil, "X-CSRF-Token", browser.csrf())
	if res.status != http.StatusOK {
		t.Fatalf("session = %d %s", res.status, res.body)
	}
	res.json(t, &session)
	id := checkoutID(t, session.ReturnURL)
	if !strings.HasPrefix(session.ReturnURL, h.api.URL+"/dashboard/billing/return?session_id=") || session.ClientSecret != h.web.session(id).secret {
		t.Fatalf("session = %+v", session)
	}

	// Back before paying: the panel again. Someone else's session: refused.
	notPaid := browser.do(http.MethodGet, session.ReturnURL, nil, "HX-Request", "true")
	expectContains(t, notPaid, "did not go through", `data-checkout`)
	h.web.pay(id, billing.StatusActive)
	stranger := h.regionBrowser()
	expectRenderedError(t, stranger.do(http.MethodGet, session.ReturnURL, nil), http.StatusForbidden)
	if stranger.billing().Entitled {
		t.Fatal("another account's checkout entitled a stranger")
	}

	// Paid: entitled immediately, before any webhook.
	done := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, done, "Your subscription is active", `data-billing-status="active"`)
	expectLacks(t, done, "js.stripe.com", `hx-get="/dashboard/billing/subscribe"`)
	if done.header.Get("Content-Security-Policy") != dashboardCSP {
		t.Fatalf("an entitled overview keeps the checkout policy: %q", done.header.Get("Content-Security-Policy"))
	}
	if !browser.billing().Entitled {
		t.Fatal("checkout did not entitle the account")
	}
	if res := browser.do(http.MethodPost, "/dashboard/billing/checkout", nil, "X-CSRF-Token", browser.csrf()); res.status != http.StatusConflict {
		t.Fatalf("second checkout = %d %s", res.status, res.body)
	}

	if strings.Contains(logs.String(), "_secret_") {
		t.Fatalf("a client secret was logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "/dashboard/billing/checkout") {
		t.Fatal("the test captured no request logs")
	}
}

func TestEmbeddedCheckoutWaitsForAPaymentToClear(t *testing.T) {
	h := newEmbeddedHarness(t)
	browser := h.regionBrowser()
	var session sessionBody
	browser.do(http.MethodPost, "/dashboard/billing/checkout", nil, "X-CSRF-Token", browser.csrf()).json(t, &session)
	h.web.pay(checkoutID(t, session.ReturnURL), billing.StatusOnHold)
	pending := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, pending, "payment is processing", `hx-get="`+strings.TrimPrefix(session.ReturnURL, h.api.URL))
	if browser.billing().Entitled {
		t.Fatal("entitled while the payment is on hold")
	}
}

func TestSubscribeWithoutEmbeddedCheckout(t *testing.T) {
	h := newBillingHarness(t)
	browser := h.regionBrowser()
	expectRedirect(t, browser.do(http.MethodGet, "/dashboard/billing/subscribe", nil), "/dashboard")
	expectRedirect(t, browser.do(http.MethodGet, "/dashboard/billing/return?session_id=cs_1", nil), "/dashboard")
	overview := browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)
	if overview.header.Get("Content-Security-Policy") != dashboardCSP {
		t.Fatal("the mock provider's overview loosens the policy")
	}
	expectLacks(t, overview, "js.stripe.com")
	if res := h.client().do(http.MethodGet, "/billing/checkout/anything", nil); res.status != http.StatusNotFound {
		t.Fatalf("checkout link page without an embedded provider = %d", res.status)
	}
}
