package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The identifiers the hosted test harness is configured with, so an
// assertion can say what the server was supposed to send Polar.
const (
	testPolarProductID  = "prod_test"
	testPolarDiscountID = "disc_test"
	testPolarCustomerID = "cus_created"
	// The harness's OIDC redirect URI is on this origin, and the checkout's
	// success page is derived from it.
	testServerOrigin = "http://app.test"
)

// polarCall is one request the server made to Polar.
type polarCall struct {
	Path string
	Auth string
	Body map[string]any
}

// fakePolar stands in for Polar's API in the route tests. It answers the
// three calls sign-up makes and keeps them, so a test can check that the
// server asked for the right product, discount, customer, and success URL
// rather than only that a URL came back.
type fakePolar struct {
	*httptest.Server
	mu    sync.Mutex
	calls []polarCall
}

func newFakePolar(t *testing.T) *fakePolar {
	t.Helper()
	fake := &fakePolar{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := polarCall{Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if err := json.NewDecoder(r.Body).Decode(&call.Body); err != nil {
			t.Errorf("fake Polar could not decode the %s body: %v", r.URL.Path, err)
		}
		fake.mu.Lock()
		fake.calls = append(fake.calls, call)
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		switch r.URL.Path {
		case "/v1/customers/":
			_, _ = w.Write([]byte(`{"id":"` + testPolarCustomerID + `"}`))
		case "/v1/checkouts/":
			_, _ = w.Write([]byte(`{"id":"checkout_1","url":"https://polar.test/checkout/checkout_1"}`))
		case "/v1/customer-sessions/":
			_, _ = w.Write([]byte(`{"id":"session_1","customer_portal_url":"https://polar.test/portal/session_1"}`))
		default:
			t.Errorf("fake Polar received an unexpected call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(fake.Close)
	return fake
}

// callsTo returns every request the server made to one Polar endpoint.
func (f *fakePolar) callsTo(path string) []polarCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []polarCall
	for _, call := range f.calls {
		if call.Path == path {
			matched = append(matched, call)
		}
	}
	return matched
}

// one requires exactly one call to a Polar endpoint and returns it.
func (f *fakePolar) one(t *testing.T, path string) polarCall {
	t.Helper()
	calls := f.callsTo(path)
	if len(calls) != 1 {
		t.Fatalf("the server made %d calls to %s, want 1: %+v", len(calls), path, calls)
	}
	return calls[0]
}

// billingURL reads the {"url": ...} body the checkout and portal routes
// answer with.
func billingURL(t *testing.T, response routeTestResponse) string {
	t.Helper()
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body.URL
}

func TestCheckoutRegistersTheAccountWithPolarAndReturnsACheckoutURL(t *testing.T) {
	h := newHostedHarness(t)

	response := h.do(t, http.MethodPost, "/api/billing/checkout", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/billing/checkout = %d %q", response.StatusCode, response.Body)
	}
	if url := billingURL(t, response); url != "https://polar.test/checkout/checkout_1" {
		t.Errorf("url = %q, want the URL Polar answered with", url)
	}

	// The account is registered under its sub, which is the external
	// customer id the webhook will name it by when the subscription lands.
	customer := h.polar.one(t, "/v1/customers/")
	if customer.Auth != "Bearer polar_oat_test" {
		t.Errorf("Authorization = %q, want POLAR_ACCESS_TOKEN as a bearer", customer.Auth)
	}
	if customer.Body["external_id"] != h.claims.Sub {
		t.Errorf("external_id = %v, want the account's sub %q", customer.Body["external_id"], h.claims.Sub)
	}
	if customer.Body["email"] != h.claims.Email {
		t.Errorf("email = %v, want the account's email %q", customer.Body["email"], h.claims.Email)
	}

	checkout := h.polar.one(t, "/v1/checkouts/")
	products, ok := checkout.Body["products"].([]any)
	if !ok || len(products) != 1 || products[0] != testPolarProductID {
		t.Errorf("products = %v, want POLAR_PRODUCT_ID", checkout.Body["products"])
	}
	if checkout.Body["discount_id"] != testPolarDiscountID {
		t.Errorf("discount_id = %v, want POLAR_DISCOUNT_ID", checkout.Body["discount_id"])
	}
	if checkout.Body["customer_id"] != testPolarCustomerID {
		t.Errorf("customer_id = %v, want the customer just created", checkout.Body["customer_id"])
	}
	if checkout.Body["success_url"] != testServerOrigin+"/billing/done" {
		t.Errorf("success_url = %v, want this server's go-back-to-the-app page", checkout.Body["success_url"])
	}
}

func TestASecondCheckoutReusesTheStoredPolarCustomer(t *testing.T) {
	h := newHostedHarness(t)

	for attempt := range 2 {
		if response := h.do(t, http.MethodPost, "/api/billing/checkout", ""); response.StatusCode != http.StatusOK {
			t.Fatalf("checkout %d = %d %q", attempt+1, response.StatusCode, response.Body)
		}
	}

	// Polar rejects a second customer carrying an external id it has
	// already seen, so abandoning a checkout and starting another must not
	// try to register the account twice.
	if calls := h.polar.callsTo("/v1/customers/"); len(calls) != 1 {
		t.Errorf("two checkouts created %d Polar customers, want 1", len(calls))
	}
	if calls := h.polar.callsTo("/v1/checkouts/"); len(calls) != 2 {
		t.Errorf("two checkouts opened %d Polar checkouts, want 2", len(calls))
	}
}

func TestCheckoutOnAnEntitledAccountAnswersTheBillingStatusInstead(t *testing.T) {
	h := newHostedHarness(t)
	h.deliver(t, "msg_active", "subscription.active", "active")

	response := h.do(t, http.MethodPost, "/api/billing/checkout", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("checkout while entitled = %d %q", response.StatusCode, response.Body)
	}
	if strings.Contains(response.Body, `"url"`) {
		t.Errorf("checkout while entitled returned a URL: %q", response.Body)
	}

	var status struct {
		Entitled bool   `json:"entitled"`
		State    string `json:"state"`
		Plan     struct {
			StorageQuotaBytes int64 `json:"storage_quota_bytes"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(response.Body), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Entitled || status.State != "active" || status.Plan.StorageQuotaBytes == 0 {
		t.Errorf("checkout while entitled = %+v, want the same body as GET /api/billing", status)
	}
	if calls := h.polar.callsTo("/v1/checkouts/"); len(calls) != 0 {
		t.Errorf("an entitled account was sent to Polar anyway: %+v", calls)
	}
}

func TestThePortalReturnsAPolarCustomerPortalURL(t *testing.T) {
	h := newHostedHarness(t)

	response := h.do(t, http.MethodGet, "/api/billing/portal", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/billing/portal = %d %q", response.StatusCode, response.Body)
	}
	if url := billingURL(t, response); url != "https://polar.test/portal/session_1" {
		t.Errorf("url = %q, want the portal URL Polar answered with", url)
	}

	// Reaching the portal before ever checking out still needs a customer,
	// so the portal creates one the same way the checkout does.
	if customer := h.polar.one(t, "/v1/customers/"); customer.Body["external_id"] != h.claims.Sub {
		t.Errorf("external_id = %v, want the account's sub", customer.Body["external_id"])
	}
	if session := h.polar.one(t, "/v1/customer-sessions/"); session.Body["customer_id"] != testPolarCustomerID {
		t.Errorf("customer_id = %v, want the account's Polar customer", session.Body["customer_id"])
	}
}

func TestThePortalReusesTheCustomerACheckoutAlreadyCreated(t *testing.T) {
	h := newHostedHarness(t)

	if response := h.do(t, http.MethodPost, "/api/billing/checkout", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("checkout = %d %q", response.StatusCode, response.Body)
	}
	if response := h.do(t, http.MethodGet, "/api/billing/portal", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("portal = %d %q", response.StatusCode, response.Body)
	}

	if calls := h.polar.callsTo("/v1/customers/"); len(calls) != 1 {
		t.Errorf("checkout then portal created %d Polar customers, want 1", len(calls))
	}
}

func TestTheSuccessPageTellsThePersonToGoBackToTheApp(t *testing.T) {
	h := newHostedHarness(t)

	// Polar returns the browser here, which holds no session, so the page
	// has to render for anyone who lands on it.
	response := doRequest(t, h.client, http.MethodGet, h.url+"/billing/done", "", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /billing/done = %d %q", response.StatusCode, response.Body)
	}
	if !strings.Contains(response.Body, "FUTO Notes") || !strings.Contains(response.Body, "Go back") {
		t.Errorf("the success page does not tell the person to go back to the app: %q", response.Body)
	}
}

func TestASelfHostedServerSellsNothing(t *testing.T) {
	server, client := newSelfHostedServer(t)
	token := devLoginToken(t, client, server.URL)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/billing/checkout"},
		{http.MethodGet, "/api/billing/portal"},
		{http.MethodGet, "/billing/done"},
	} {
		response := doRequest(t, client, route.method, server.URL+route.path, "", token, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s off the hosted service = %d %q, want 404",
				route.method, route.path, response.StatusCode, response.Body)
		}
	}
}

func TestCheckoutAndPortalNeedASession(t *testing.T) {
	h := newHostedHarness(t)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/billing/checkout"},
		{http.MethodGet, "/api/billing/portal"},
	} {
		response := doRequest(t, h.client, route.method, h.url+route.path, "", "", nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d %q, want 401",
				route.method, route.path, response.StatusCode, response.Body)
		}
	}
	if calls := h.polar.callsTo("/v1/customers/"); len(calls) != 0 {
		t.Errorf("an anonymous request reached Polar: %+v", calls)
	}
}
