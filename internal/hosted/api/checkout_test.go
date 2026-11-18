package api_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// checkoutCSP is what Stripe documents for Stripe.js with the Payment
// Element and 3D Secure, and nothing more.
const checkoutCSP = "default-src 'none'; " +
	"script-src 'self' https://js.stripe.com https://*.js.stripe.com; " +
	"style-src 'self'; img-src 'self'; font-src 'self'; " +
	"connect-src 'self' https://api.stripe.com; " +
	"frame-src https://js.stripe.com https://*.js.stripe.com https://hooks.stripe.com; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

const testPublishableKey = "pk_test_custom"

var checkoutTokenKey = bytes.Repeat([]byte{0x24}, 32)

// fakeCustom is a web provider whose checkout is a page of ours, as
// Stripe's is: it opens sessions, on our page or its own, which the test
// completes. US and CA pay on our page; RU and CN cannot pay; the rest pay
// on the provider's page.
type fakeCustom struct {
	mu       sync.Mutex
	next     int
	plan     billing.Plan
	planErr  error
	sessions map[string]*fakeCheckout
	subs     map[string]billing.Event
}

type fakeCheckout struct {
	subject string
	// country is the billing country chosen for our page; billed the
	// card's, once paid.
	country, billed string
	returnURL       string
	secret          string
	complete        bool
	subID           string
	// merchant marks a session on the provider's page, with its URLs.
	merchant              bool
	successURL, cancelURL string
}

// merchantPage prefixes the provider's checkout pages.
const merchantPage = "https://checkout.stripe.test/pay/"

func (f *fakeCustom) Route(country string) billing.CheckoutRoute {
	switch country {
	case "US", "CA":
		return billing.RouteSelf
	case "RU", "CN":
		return billing.RouteUnavailable
	}
	return billing.RouteMerchantOfRecord
}

func (f *fakeCustom) StartMerchantCheckout(_ context.Context, c billing.Customer, successURL, cancelURL string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Subject == "" {
		return "", fmt.Errorf("checkout for %+v without a subject", c)
	}
	f.next++
	id := fmt.Sprintf("cs_test_%d", f.next)
	f.sessions[id] = &fakeCheckout{subject: c.Subject, merchant: true, successURL: successURL, cancelURL: cancelURL}
	return merchantPage + id, nil
}

// opened counts the checkouts opened, by either route.
func (f *fakeCustom) opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func newFakeCustom() *fakeCustom {
	return &fakeCustom{
		plan:     billing.Plan{Amount: 500, Currency: "usd", Interval: "month", IntervalCount: 1},
		sessions: map[string]*fakeCheckout{}, subs: map[string]billing.Event{},
	}
}

func (f *fakeCustom) Plan(context.Context) (billing.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plan, f.planErr
}

func (f *fakeCustom) Source() billing.Source { return billing.SourceWeb }
func (f *fakeCustom) PublishableKey() string { return testPublishableKey }

func (f *fakeCustom) CheckoutURL(context.Context, billing.Customer) (string, error) {
	return "", billing.ErrNotSupported
}

func (f *fakeCustom) PortalURL(_ context.Context, c billing.Customer) (string, error) {
	return "https://portal.test/" + c.Subject, nil
}

func (f *fakeCustom) StartCheckout(_ context.Context, c billing.Customer, billingCountry, returnURL string) (billing.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Subject == "" {
		return billing.Checkout{}, fmt.Errorf("checkout for %+v without a subject", c)
	}
	f.next++
	id := fmt.Sprintf("cs_test_%d", f.next)
	s := &fakeCheckout{subject: c.Subject, country: billingCountry, returnURL: returnURL, secret: id + "_secret_MUST-NOT-BE-LOGGED"}
	f.sessions[id] = s
	return billing.Checkout{ID: id, ClientSecret: s.secret}, nil
}

// pay completes checkout id, creating its subscription.
func (f *fakeCustom) pay(id string, status billing.Status) {
	f.payFrom(id, "", status)
}

// payFrom completes checkout id with a card billed in billed ("" for the
// country chosen), creating its subscription.
func (f *fakeCustom) payFrom(id, billed string, status billing.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	s.complete = true
	s.billed = cmp.Or(billed, s.country)
	s.subID = "sub_" + id
	f.subs[s.subID] = billing.Event{
		Source: billing.SourceWeb, ExternalID: s.subID, Subject: s.subject, ProductID: "prod_notes", Status: status,
		WillRenew: true, ExpiresAt: new(time.Now().Add(30 * 24 * time.Hour)), Environment: billing.EnvironmentSandbox,
		EventAt: time.Now(), Raw: []byte(`{"fake":true}`),
	}
}

func (f *fakeCustom) CompleteCheckout(_ context.Context, c billing.Customer, id string) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok || s.subject != c.Subject {
		return billing.Event{}, billing.ErrCheckoutForeign
	}
	if !s.complete {
		return billing.Event{}, billing.ErrCheckoutIncomplete
	}
	if !s.merchant && s.billed != s.country {
		return billing.Event{}, &billing.CountryMismatchError{Chosen: s.country, Billed: s.billed}
	}
	ev := f.subs[s.subID]
	ev.EventAt = time.Now()
	return ev, nil
}

func (f *fakeCustom) Verify(context.Context, billing.Delivery) (string, []billing.Event, error) {
	return "", nil, billing.ErrInvalidDelivery
}

func (f *fakeCustom) Refresh(_ context.Context, id string) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[id], nil
}

func (f *fakeCustom) Cancel(context.Context, string) error { return nil }

func (f *fakeCustom) session(id string) fakeCheckout {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.sessions[id]
}

type customHarness struct {
	*harness
	web *fakeCustom
}

func newCustomHarness(t *testing.T) *customHarness {
	t.Helper()
	e := &customHarness{web: newFakeCustom()}
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

// linkPath is the path of a new checkout link for app's account.
func linkPath(t *testing.T, h *customHarness, app *client) string {
	t.Helper()
	var link struct {
		URL string `json:"url"`
	}
	app.expect(http.MethodPost, "/api/billing/checkout", nil, http.StatusOK).json(t, &link)
	if !strings.HasPrefix(link.URL, h.api.URL+"/billing/checkout/") {
		t.Fatalf("checkout link %q", link.URL)
	}
	return strings.TrimPrefix(link.URL, h.api.URL)
}

// chooseCountry posts a link's country step as a browser form.
func chooseCountry(browser *client, link, country string) response {
	return browser.postForm(link, url.Values{"country": {country}})
}

// payPath posts a self-payment country to a link's country step and
// returns where it sends the browser: our checkout page.
func payPath(t *testing.T, browser *client, link string) string {
	t.Helper()
	res := chooseCountry(browser, link, "US")
	pay := res.header.Get("Location")
	if res.status != http.StatusSeeOther || !strings.HasPrefix(pay, "/billing/checkout/") || pay == link {
		t.Fatalf("Checkout for US = %d to %q %s", res.status, pay, res.body)
	}
	return pay
}

func TestCheckoutLinkFromTheApp(t *testing.T) {
	h := newCustomHarness(t)
	app, tokens := h.appLogin(h.newSub())
	link := linkPath(t, h, app)

	// The system browser has no session: the link alone opens the page,
	// which asks for the billing country first, prefilled from the
	// browser's languages. It mounts nothing, so it keeps the strict
	// policy.
	browser := h.client()
	step := browser.do(http.MethodGet, link, nil, "Accept-Language", "en-CA,en;q=0.9")
	if step.status != http.StatusOK || step.header.Get("Content-Security-Policy") != dashboardCSP {
		t.Fatalf("country step = %d, CSP %q", step.status, step.header.Get("Content-Security-Policy"))
	}
	expectContains(t, step, `data-country-step`, `action="`+link+`" hx-post="`+link+`"`, "FUTO Notes Sync",
		`<label class="field-label" for="billing-country">Billing country</label>`,
		`<option value="CA" data-route="self" selected>Canada</option>`,
		`<option value="GB" data-route="merchant">United Kingdom</option>`,
		`<option value="RU" data-route="unavailable">Russia</option>`,
		`data-country-submit>Checkout</button>`, `<a href="/billing/checkout/cancelled">Cancel</a>`)
	expectLacks(t, step, "js.stripe.com", "data-checkout ", `name="csrf_token"`)
	// Opening it again, or with no language, does not use the link.
	expectContains(t, browser.expect(http.MethodGet, link, nil, http.StatusOK), `<option value="" selected disabled>Choose your country</option>`)

	// Checkout for a self-payment country uses the link and sends the
	// browser to our checkout page.
	pay := payPath(t, browser, link)
	expectContains(t, browser.expect(http.MethodGet, link, nil, http.StatusGone), "expired or was already used")
	if res := chooseCountry(browser, link, "US"); res.status != http.StatusGone {
		t.Fatalf("second Checkout = %d", res.status)
	}

	res := browser.expect(http.MethodGet, pay, nil, http.StatusOK)
	if got := res.header.Get("Content-Security-Policy"); got != checkoutCSP {
		t.Fatalf("checkout page CSP = %q", got)
	}
	expectContains(t, res, `<script src="`+stripe.JSURL+`" defer></script>`, `data-checkout`,
		`data-publishable-key="`+testPublishableKey+`"`, `data-session-url="`+pay+`/session"`,
		// Our page: the plan, the account it is for, our button, the
		// legal line, and a way out.
		"FUTO Notes Sync", `<p class="field-value" aria-labelledby="checkout-email">`+tokens.User.Email+`</p>`,
		`data-checkout-submit disabled>`, "Renews monthly until you cancel",
		`href="https://docs.futo.cloud/legal/terms"`, `href="https://docs.futo.cloud/legal/privacy"`,
		`<a href="/billing/checkout/cancelled">Cancel</a>`)
	if tokens.User.Email == "" {
		t.Fatal("the test account has no email")
	}
	// Opening the page twice does not use it.
	browser.expect(http.MethodGet, pay, nil, http.StatusOK)
	if h.web.opened() != 0 {
		t.Fatalf("%d checkouts opened before the page asked", h.web.opened())
	}

	var session sessionBody
	browser.expect(http.MethodPost, pay+"/session", nil, http.StatusOK).json(t, &session)
	id := checkoutID(t, session.ReturnURL)
	if session.ClientSecret != h.web.session(id).secret || !strings.HasPrefix(session.ReturnURL, h.api.URL+"/billing/checkout/return?completion=") {
		t.Fatalf("session = %+v", session)
	}
	if want := strings.Replace(session.ReturnURL, id, "{CHECKOUT_SESSION_ID}", 1); h.web.session(id).returnURL != want {
		t.Fatalf("provider return URL = %q, want %q", h.web.session(id).returnURL, want)
	}

	// Used once: it no longer opens a checkout, nor its page. The link
	// itself never opens one.
	expectHostedError(t, browser.do(http.MethodPost, pay+"/session", nil), http.StatusGone, "checkout_link_expired")
	expectContains(t, browser.expect(http.MethodGet, pay, nil, http.StatusGone), "expired or was already used")
	expectHostedError(t, browser.do(http.MethodPost, link+"/session", nil), http.StatusGone, "checkout_link_expired")

	// Returning before paying says so; after paying, the account is
	// entitled at once, without any webhook.
	expectContains(t, browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK), "did not go through")
	if app.billing().Entitled {
		t.Fatal("entitled before paying")
	}
	h.web.pay(id, billing.StatusActive)
	returned := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, returned, "Your subscription is active", `class="success"`, "Return to the FUTO Notes app")
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

// Everywhere outside the self-payment countries, Checkout sends the
// browser to the provider's own page, where it is merchant of record, and
// the return confirms that checkout like ours.
func TestCheckoutLinkMerchantOfRecord(t *testing.T) {
	h := newCustomHarness(t)
	sub := h.newSub()
	app, _ := h.appLogin(sub)
	link := linkPath(t, h, app)
	browser := h.client()
	expectContains(t, browser.do(http.MethodGet, link, nil, "Accept-Language", "de-DE,de;q=0.9,en;q=0.8"),
		`<option value="DE" data-route="merchant" selected>Germany</option>`)

	// htmx follows the provider's page with a full load.
	res := browser.postForm(link, url.Values{"country": {"de"}}, "HX-Request", "true")
	target := res.header.Get("HX-Redirect")
	if res.status != http.StatusNoContent || !strings.HasPrefix(target, merchantPage) {
		t.Fatalf("Checkout for DE = %d, HX-Redirect %q %s", res.status, target, res.body)
	}
	id := strings.TrimPrefix(target, merchantPage)
	session := h.web.session(id)
	if !session.merchant || session.subject != sub || session.cancelURL != h.api.URL+"/billing/checkout/cancelled" ||
		!strings.HasPrefix(session.successURL, h.api.URL+"/billing/checkout/return?completion=") ||
		!strings.HasSuffix(session.successURL, "&session_id={CHECKOUT_SESSION_ID}") {
		t.Fatalf("merchant session = %+v", session)
	}
	// The link is used.
	if res := chooseCountry(browser, link, "DE"); res.status != http.StatusGone {
		t.Fatalf("second Checkout = %d", res.status)
	}

	// Back from the provider's page: the same confirmation as ours.
	returnURL := strings.Replace(session.successURL, "{CHECKOUT_SESSION_ID}", id, 1)
	expectContains(t, browser.expect(http.MethodGet, returnURL, nil, http.StatusOK), "did not go through")
	h.web.pay(id, billing.StatusActive)
	expectContains(t, browser.expect(http.MethodGet, returnURL, nil, http.StatusOK), "Your subscription is active")
	if !app.billing().Entitled {
		t.Fatal("a merchant checkout did not entitle the account")
	}

	// A plain form post gets a 303 to the provider's page instead.
	other, _ := h.appLogin(h.newSub())
	plain := chooseCountry(h.client(), linkPath(t, h, other), "GB")
	if plain.status != http.StatusSeeOther || !strings.HasPrefix(plain.header.Get("Location"), merchantPage) {
		t.Fatalf("plain Checkout for GB = %d to %q", plain.status, plain.header.Get("Location"))
	}
}

// A country that cannot pay on the web, or is not a country, is said so
// and opens nothing, whatever is posted, and leaves the link unused.
func TestCheckoutLinkCountryRefused(t *testing.T) {
	h := newCustomHarness(t)
	app, _ := h.appLogin(h.newSub())
	link := linkPath(t, h, app)
	browser := h.client()

	prefilled := browser.do(http.MethodGet, link, nil, "Accept-Language", "ru-RU")
	expectContains(t, prefilled, `<option value="RU" data-route="unavailable" selected>Russia</option>`,
		`<p class="notice" role="status" data-route-note="unavailable">Paying on the web isn't available in your country yet.</p>`,
		`data-country-submit hidden>Checkout</button>`)

	for _, country := range []string{"RU", "cn"} {
		res := chooseCountry(browser, link, country)
		if res.status != http.StatusOK {
			t.Fatalf("Checkout for %s = %d", country, res.status)
		}
		expectContains(t, res, "Paying on the web isn't available in your country yet.", "data-country-submit hidden")
	}
	for _, country := range []string{"", "ZZ", "UK", "USA", "EU", "<b>"} {
		res := chooseCountry(browser, link, country)
		if res.status != http.StatusBadRequest {
			t.Fatalf("Checkout for %q = %d", country, res.status)
		}
		expectContains(t, res, "Choose your billing country from the list.", `data-country-step`)
	}
	// htmx gets the step as a fragment.
	fragment := browser.postForm(link, url.Values{"country": {"RU"}}, "HX-Request", "true")
	expectContains(t, fragment, "isn't available in your country yet")
	expectLacks(t, fragment, "<!doctype html>")
	if h.web.opened() != 0 {
		t.Fatalf("refused countries opened %d checkouts", h.web.opened())
	}
	// None of that used the link.
	payPath(t, browser, link)
}

func TestCheckoutLinkTokens(t *testing.T) {
	h := newCustomHarness(t)
	app, tokens := h.appLogin(h.newSub())
	browser := h.client()
	valid := func() string {
		return strings.TrimPrefix(linkPath(t, h, app), "/billing/checkout/")
	}
	claims := func(purpose string, expires time.Time) map[string]any {
		return map[string]any{"p": purpose, "u": tokens.User.ID, "e": expires.Unix(), "n": "nonce-" + purpose + expires.String(), "c": "US"}
	}
	token := valid()
	payload, sig, _ := strings.Cut(token, ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(payload)
	other, _ := h.appLogin(h.newSub())
	otherID := other.account().User.ID
	retargeted := base64.RawURLEncoding.EncodeToString(bytes.Replace(decoded, []byte(tokens.User.ID), []byte(otherID), 1))
	pay := strings.TrimPrefix(payPath(t, browser, "/billing/checkout/"+valid()), "/billing/checkout/")
	payPayload, paySig, _ := strings.Cut(pay, ".")
	payDecoded, _ := base64.RawURLEncoding.DecodeString(payPayload)
	payCountry := base64.RawURLEncoding.EncodeToString(bytes.Replace(payDecoded, []byte(`"c":"US"`), []byte(`"c":"DE"`), 1))

	for name, bad := range map[string]string{
		"payload re-pointed at another user": retargeted + "." + sig,
		"signature changed":                  payload + "." + base64.RawURLEncoding.EncodeToString([]byte("not the signature at all, 32 b")),
		"no signature":                       payload,
		"garbage":                            "garbage",
		"expired":                            signToken(checkoutTokenKey, claims("open", time.Now().Add(-time.Second))),
		"another key":                        signToken(bytes.Repeat([]byte{1}, 32), claims("open", time.Now().Add(time.Hour))),
		"a completion token":                 signToken(checkoutTokenKey, claims("complete", time.Now().Add(time.Hour))),
		"an expired pay token":               signToken(checkoutTokenKey, claims("pay", time.Now().Add(-time.Second))),
		"a pay token for another country":    payCountry + "." + paySig,
	} {
		expectHostedError(t, browser.do(http.MethodPost, "/billing/checkout/"+bad+"/session", nil), http.StatusGone, "checkout_link_expired")
		if res := browser.do(http.MethodGet, "/billing/checkout/"+url.PathEscape(bad), nil); res.status != http.StatusGone {
			t.Errorf("checkout page with %s = %d", name, res.status)
		}
		if res := chooseCountry(browser, "/billing/checkout/"+url.PathEscape(bad), "US"); res.status != http.StatusGone {
			t.Errorf("Checkout with %s = %d", name, res.status)
		}
	}
	// A pay token is not a link: its Checkout is refused.
	if res := chooseCountry(browser, "/billing/checkout/"+pay, "US"); res.status != http.StatusGone {
		t.Errorf("Checkout with a pay token = %d", res.status)
	}
	// A pay token for a country that no longer pays on our page opens
	// nothing.
	signedDE := signToken(checkoutTokenKey, map[string]any{"p": "pay", "u": tokens.User.ID, "e": time.Now().Add(time.Hour).Unix(), "n": "nonce-de", "c": "DE"})
	expectHostedError(t, browser.do(http.MethodPost, "/billing/checkout/"+signedDE+"/session", nil), http.StatusGone, "checkout_link_expired")
	if res := browser.do(http.MethodGet, "/billing/checkout/"+signedDE, nil); res.status != http.StatusGone {
		t.Errorf("checkout page for a DE pay token = %d", res.status)
	}
	if opened := h.web.opened(); opened != 0 {
		t.Fatalf("bad tokens opened %d checkouts", opened)
	}

	// Reuse is refused even when requests race for one link, or one pay
	// token.
	race := func(name string, send func() int, ok int) {
		results := make(chan int, 4)
		for range 4 {
			go func() { results <- send() }()
		}
		n := 0
		for range 4 {
			if <-results == ok {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d of 4 racing uses of %s went through, want 1", n, name)
		}
	}
	token = valid()
	race("a link", func() int { return chooseCountry(h.client(), "/billing/checkout/"+token, "US").status }, http.StatusSeeOther)
	pay = payPath(t, browser, "/billing/checkout/"+valid())
	race("a pay token", func() int { return h.client().do(http.MethodPost, pay+"/session", nil).status }, http.StatusOK)

	// A link whose account was deleted opens nothing.
	token = valid()
	pay = payPath(t, browser, "/billing/checkout/"+valid())
	h.exec(`DELETE FROM users WHERE id = $1`, tokens.User.ID)
	if res := chooseCountry(browser, "/billing/checkout/"+token, "US"); res.status != http.StatusGone {
		t.Errorf("Checkout for a deleted account = %d", res.status)
	}
	expectHostedError(t, browser.do(http.MethodPost, pay+"/session", nil), http.StatusGone, "checkout_link_expired")
}

func TestCheckoutLinkReturnRefusesAnotherAccountsSession(t *testing.T) {
	h := newCustomHarness(t)
	open := func(app *client) sessionBody {
		pay := payPath(t, h.client(), linkPath(t, h, app))
		var session sessionBody
		h.client().expect(http.MethodPost, pay+"/session", nil, http.StatusOK).json(t, &session)
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

func TestDashboardCustomCheckout(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&logs))
	t.Cleanup(func() { log.Logger = previous })

	h := newCustomHarness(t)
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

	// Subscribe asks for the billing country first, prefilled from the
	// browser's languages; a self-payment country opens the panel.
	step := browser.do(http.MethodGet, "/dashboard/billing/subscribe", nil, "HX-Request", "true", "Accept-Language", "en-US,en;q=0.5")
	expectContains(t, step, `data-country-step`, `hx-post="/dashboard/billing/subscribe"`,
		`<option value="US" data-route="self" selected>United States</option>`, `data-country-submit>Checkout</button>`,
		`<a href="/dashboard" hx-get="/dashboard" hx-push-url="true">Cancel</a>`)
	expectLacks(t, step, `data-checkout `)
	if res := browser.postForm("/dashboard/billing/subscribe", url.Values{"country": {"US"}}, "HX-Request", "true"); res.status != http.StatusForbidden {
		t.Fatalf("Checkout without CSRF = %d", res.status)
	}
	panel := browser.hxSubmit("/dashboard/billing/subscribe", url.Values{"country": {"us"}})
	if panel.header.Get("HX-Push-Url") != "/dashboard/billing/subscribe?country=US" {
		t.Fatalf("Checkout for US pushed %q", panel.header.Get("HX-Push-Url"))
	}
	expectRedirect(t, browser.submit("/dashboard/billing/subscribe", url.Values{"country": {"US"}}), "/dashboard/billing/subscribe?country=US")
	reloaded := browser.do(http.MethodGet, "/dashboard/billing/subscribe?country=US", nil, "HX-Request", "true")
	if reloaded.body != panel.body {
		t.Fatalf("the panel's page differs from Checkout's:\n%s\n%s", reloaded.body, panel.body)
	}
	expectContains(t, panel, `data-checkout`, `data-session-url="/dashboard/billing/checkout?country=US"`, `data-country="US"`,
		`<p class="alert" data-checkout-country role="alert" hidden>The billing country must be United States, the one you chose. `+
			`<a href="/dashboard/billing/subscribe" hx-get="/dashboard/billing/subscribe" hx-push-url="true">Choose another country</a></p>`,
		`data-page-url="/dashboard/billing/subscribe?country=US"`, `data-publishable-key="`+testPublishableKey+`"`,
		`<p class="field-value" aria-labelledby="checkout-email">`+browser.account().User.Email+`</p>`,
		`<a href="/dashboard" hx-get="/dashboard" hx-push-url="true">Cancel</a>`)
	if panel.status != http.StatusOK || panel.header.Get("Content-Security-Policy") != checkoutCSP {
		t.Fatalf("subscribe fragment = %d %v", panel.status, panel.header)
	}
	asset := browser.expect(http.MethodGet, "/dashboard/static/checkout.js", nil, http.StatusOK)
	expectContains(t, asset, "initCheckoutElementsSdk", "createPaymentElement", "changeAppearance", "prefers-color-scheme",
		`wallets: { applePay: "never", googlePay: "never", link: "never" }`, "adaptivePricing: { allowed: false }",
		`actions.confirm({ redirect: "if_required" })`, "defaultValues: { billingAddress: { address: { country: chosen } } }")
	expectLacks(t, asset, "createEmbeddedCheckoutPage")

	// Opening the session needs the CSRF token, and a self-payment
	// country.
	if res := browser.do(http.MethodPost, "/dashboard/billing/checkout?country=US", nil); res.status != http.StatusForbidden {
		t.Fatalf("session without CSRF = %d", res.status)
	}
	for _, country := range []string{"", "DE", "RU", "ZZ"} {
		expectHostedError(t, browser.do(http.MethodPost, "/dashboard/billing/checkout?country="+country, nil, "X-CSRF-Token", browser.csrf()),
			http.StatusBadRequest, "invalid_request")
	}
	if h.web.opened() != 0 {
		t.Fatalf("%d checkouts opened for countries that do not pay on our page", h.web.opened())
	}
	var session sessionBody
	res := browser.do(http.MethodPost, "/dashboard/billing/checkout?country=US", nil, "X-CSRF-Token", browser.csrf())
	if res.status != http.StatusOK {
		t.Fatalf("session = %d %s", res.status, res.body)
	}
	res.json(t, &session)
	id := checkoutID(t, session.ReturnURL)
	if !strings.HasPrefix(session.ReturnURL, h.api.URL+"/dashboard/billing/return?session_id=") || session.ClientSecret != h.web.session(id).secret {
		t.Fatalf("session = %+v", session)
	}

	// Back before paying: the country step again. Someone else's
	// session: refused.
	notPaid := browser.do(http.MethodGet, session.ReturnURL, nil, "HX-Request", "true")
	expectContains(t, notPaid, "did not go through", `data-country-step`)
	h.web.pay(id, billing.StatusActive)
	stranger := h.regionBrowser()
	expectRenderedError(t, stranger.do(http.MethodGet, session.ReturnURL, nil), http.StatusForbidden)
	if stranger.billing().Entitled {
		t.Fatal("another account's checkout entitled a stranger")
	}

	// Paid: entitled immediately, before any webhook.
	done := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, done, "Your subscription is active", `class="success"`, `data-billing-status="active"`)
	expectLacks(t, done, "js.stripe.com", `hx-get="/dashboard/billing/subscribe"`)
	if done.header.Get("Content-Security-Policy") != dashboardCSP {
		t.Fatalf("an entitled overview keeps the checkout policy: %q", done.header.Get("Content-Security-Policy"))
	}
	if !browser.billing().Entitled {
		t.Fatal("checkout did not entitle the account")
	}
	if res := browser.do(http.MethodPost, "/dashboard/billing/checkout?country=US", nil, "X-CSRF-Token", browser.csrf()); res.status != http.StatusConflict {
		t.Fatalf("second checkout = %d %s", res.status, res.body)
	}
	expectRenderedError(t, browser.submit("/dashboard/billing/subscribe", url.Values{"country": {"DE"}}), http.StatusConflict)

	if strings.Contains(logs.String(), "_secret_") {
		t.Fatalf("a client secret was logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "/dashboard/billing/checkout") {
		t.Fatal("the test captured no request logs")
	}
}

func TestCustomCheckoutWaitsForAPaymentToClear(t *testing.T) {
	h := newCustomHarness(t)
	browser := h.regionBrowser()
	var session sessionBody
	browser.do(http.MethodPost, "/dashboard/billing/checkout?country=US", nil, "X-CSRF-Token", browser.csrf()).json(t, &session)
	h.web.pay(checkoutID(t, session.ReturnURL), billing.StatusOnHold)
	pending := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, pending, "payment is processing", `hx-get="`+strings.TrimPrefix(session.ReturnURL, h.api.URL))
	if browser.billing().Entitled {
		t.Fatal("entitled while the payment is on hold")
	}
}

func TestSubscribeWithoutCustomCheckout(t *testing.T) {
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
		t.Fatalf("checkout link page without a custom checkout provider = %d", res.status)
	}
}

// The panel's price and period are the provider's plan, never fixed text.
func TestCheckoutPanelShowsThePlan(t *testing.T) {
	h := newCustomHarness(t)
	browser := h.regionBrowser()
	for _, tc := range []struct {
		plan                  billing.Plan
		price, period, renews string
	}{
		{billing.Plan{Amount: 500, Currency: "usd", Interval: "month", IntervalCount: 1}, "$5.00", "month", "monthly"},
		{billing.Plan{Amount: 4999, Currency: "usd", Interval: "year", IntervalCount: 1}, "$49.99", "year", "yearly"},
		{billing.Plan{Amount: 1200, Currency: "usd", Interval: "month", IntervalCount: 3}, "$12.00", "3 months", "every 3 months"},
		{billing.Plan{Amount: 450, Currency: "eur", Interval: "week", IntervalCount: 1}, "4.50 EUR", "week", "weekly"},
	} {
		h.web.mu.Lock()
		h.web.plan = tc.plan
		h.web.mu.Unlock()
		step := browser.expect(http.MethodGet, "/dashboard/billing/subscribe", nil, http.StatusOK)
		expectContains(t, step, `<p class="plan-price"><strong>`+tc.price+`</strong><span class="muted"> / `+tc.period+`<span data-route-note="merchant" hidden> plus tax</span>`)
		panel := browser.expect(http.MethodGet, "/dashboard/billing/subscribe?country=US", nil, http.StatusOK)
		expectContains(t, panel,
			`<strong data-checkout-amount>`+tc.price+`</strong>`,
			`Subscribe · <span data-checkout-amount>`+tc.price+`</span>/<span data-checkout-period>`+tc.period+`</span>`,
			"Renews "+tc.renews+" until you cancel")
	}

	// Without a plan there is no checkout to show.
	h.web.mu.Lock()
	h.web.planErr = errors.New("stripe is down")
	h.web.mu.Unlock()
	expectRenderedError(t, browser.do(http.MethodGet, "/dashboard/billing/subscribe", nil), http.StatusInternalServerError)
	expectRenderedError(t, browser.do(http.MethodGet, "/dashboard/billing/subscribe?country=US", nil), http.StatusInternalServerError)
	app, _ := h.appLogin(h.newSub())
	link := linkPath(t, h, app)
	if res := h.client().do(http.MethodGet, link, nil); res.status != http.StatusInternalServerError {
		t.Fatalf("checkout page without a plan = %d", res.status)
	}
}

func TestCheckoutLinkCancelled(t *testing.T) {
	h := newCustomHarness(t)
	res := h.client().expect(http.MethodGet, "/billing/checkout/cancelled", nil, http.StatusOK)
	expectContains(t, res, "No payment was taken")
	expectLacks(t, res, "js.stripe.com", "data-checkout")
	if got := res.header.Get("Content-Security-Policy"); got != dashboardCSP {
		t.Fatalf("cancelled page CSP = %q", got)
	}
}

// On the dashboard too, Checkout routes by billing country: the
// provider's page outside the self-payment countries, and nothing where
// the web cannot take payment.
func TestDashboardCountryRoutes(t *testing.T) {
	h := newCustomHarness(t)
	browser := h.regionBrowser()

	// Choosing a country by URL shows it chosen, and opens nothing.
	expectContains(t, browser.expect(http.MethodGet, "/dashboard/billing/subscribe?country=DE", nil, http.StatusOK),
		`<option value="DE" data-route="merchant" selected>Germany</option>`, `data-route-note="merchant"> plus tax</span>`,
		"adds any sales tax or VAT due in your country")
	unavailable := browser.expect(http.MethodGet, "/dashboard/billing/subscribe?country=RU", nil, http.StatusOK)
	expectContains(t, unavailable, `<option value="RU" data-route="unavailable" selected>Russia</option>`,
		"Paying on the web isn't available in your country yet.", "data-country-submit hidden")
	expectLacks(t, unavailable, "data-checkout ")

	// Posting a country that cannot pay, or no country, opens nothing.
	refused := browser.hxSubmit("/dashboard/billing/subscribe", url.Values{"country": {"CN"}})
	if refused.status != http.StatusOK {
		t.Fatalf("Checkout for CN = %d", refused.status)
	}
	expectContains(t, refused, "Paying on the web isn't available in your country yet.", "data-country-submit hidden")
	expectLacks(t, refused, "data-checkout ")
	for _, country := range []string{"", "ZZ", "GBR"} {
		res := browser.hxSubmit("/dashboard/billing/subscribe", url.Values{"country": {country}})
		if res.status != http.StatusBadRequest || res.header.Get("HX-Retarget") != "#alert" {
			t.Fatalf("Checkout for %q = %d", country, res.status)
		}
		expectContains(t, res, "Choose your billing country from the list.")
	}
	if h.web.opened() != 0 {
		t.Fatalf("refused countries opened %d checkouts", h.web.opened())
	}

	// Germany pays on the provider's page: a full load there for htmx, a
	// 303 without it.
	res := browser.hxSubmit("/dashboard/billing/subscribe", url.Values{"country": {"DE"}})
	target := res.header.Get("HX-Redirect")
	if res.status != http.StatusNoContent || !strings.HasPrefix(target, merchantPage) {
		t.Fatalf("Checkout for DE = %d, HX-Redirect %q %s", res.status, target, res.body)
	}
	plain := browser.submit("/dashboard/billing/subscribe", url.Values{"country": {"GB"}})
	if plain.status != http.StatusSeeOther || !strings.HasPrefix(plain.header.Get("Location"), merchantPage) {
		t.Fatalf("plain Checkout for GB = %d to %q", plain.status, plain.header.Get("Location"))
	}
	id := strings.TrimPrefix(target, merchantPage)
	session := h.web.session(id)
	if !session.merchant || session.successURL != h.api.URL+"/dashboard/billing/return?session_id={CHECKOUT_SESSION_ID}" ||
		session.cancelURL != h.api.URL+"/dashboard/billing/subscribe?country=DE" {
		t.Fatalf("merchant session = %+v", session)
	}

	// Back from the provider's page: confirmed at once, like ours.
	h.web.pay(id, billing.StatusActive)
	done := browser.expect(http.MethodGet, strings.Replace(session.successURL, "{CHECKOUT_SESSION_ID}", id, 1), nil, http.StatusOK)
	expectContains(t, done, "Your subscription is active", `data-billing-status="active"`)
	if !browser.billing().Entitled {
		t.Fatal("a merchant checkout did not entitle the account")
	}
	expectLacks(t, browser.expect(http.MethodGet, "/dashboard/billing/subscribe", nil, http.StatusOK), "data-country-step")
}

// A checkout on our page paid with a card billed outside the country chosen
// is refused by the provider; the return page says why and starts over at
// the country step, the card's country chosen.
func TestDashboardCheckoutRefusedForItsCountry(t *testing.T) {
	h := newCustomHarness(t)
	browser := h.regionBrowser()
	var session sessionBody
	browser.do(http.MethodPost, "/dashboard/billing/checkout?country=US", nil, "X-CSRF-Token", browser.csrf()).json(t, &session)
	id := checkoutID(t, session.ReturnURL)
	if h.web.session(id).country != "US" {
		t.Fatalf("checkout opened for %q, want US", h.web.session(id).country)
	}
	h.web.payFrom(id, "CA", billing.StatusActive)
	for range 2 {
		refused := browser.do(http.MethodGet, session.ReturnURL, nil, "HX-Request", "true")
		expectContains(t, refused, "Your card&#39;s billing address was in Canada, but you chose United States as your billing country. "+
			"We cancelled that subscription and refunded the payment. Choose your billing country to try again.",
			`data-country-step`, `<option value="CA" data-route="self" selected>Canada</option>`)
		if refused.status != http.StatusOK {
			t.Fatalf("return page = %d", refused.status)
		}
	}
	if browser.billing().Entitled {
		t.Fatal("a payment billed in another country entitled the account")
	}
}

func TestCheckoutLinkRefusedForItsCountry(t *testing.T) {
	h := newCustomHarness(t)
	app, _ := h.appLogin(h.newSub())
	browser := h.client()
	pay := payPath(t, browser, linkPath(t, h, app))
	// No country step to go back to: the link was used choosing it.
	expectContains(t, browser.expect(http.MethodGet, pay, nil, http.StatusOK), `data-country="US"`,
		`<p class="alert" data-checkout-country role="alert" hidden>The billing country must be United States, the one you chose. To pay from another country, start again from the app.</p>`)
	var session sessionBody
	browser.expect(http.MethodPost, pay+"/session", nil, http.StatusOK).json(t, &session)
	id := checkoutID(t, session.ReturnURL)
	if h.web.session(id).country != "US" {
		t.Fatalf("checkout opened for %q, want US", h.web.session(id).country)
	}
	h.web.payFrom(id, "CA", billing.StatusActive)
	refused := browser.expect(http.MethodGet, session.ReturnURL, nil, http.StatusOK)
	expectContains(t, refused, "Your card&#39;s billing address was in Canada, but you chose United States as your billing country. "+
		"We cancelled that subscription and refunded the payment. Start again from the app.")
	expectLacks(t, refused, "Your subscription is active", "js.stripe.com")
	if app.billing().Entitled {
		t.Fatal("a payment billed in another country entitled the account")
	}
	// The pay token stays used; the app's next link starts over.
	expectHostedError(t, browser.do(http.MethodPost, pay+"/session", nil), http.StatusGone, "checkout_link_expired")
	payPath(t, browser, linkPath(t, h, app))
}
