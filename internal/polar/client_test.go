package polar_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"futo-notes-server/internal/polar"
)

// recordedRequest is one call the fake Polar received.
type recordedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

// fakePolar answers the three calls the checkout and portal routes make, and
// keeps what it was asked so a test can check the request shape rather than
// only the result.
type fakePolar struct {
	*httptest.Server
	requests []recordedRequest
	status   int
	body     string
}

func newFakePolar(t *testing.T) *fakePolar {
	t.Helper()
	fake := &fakePolar{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := recordedRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if err := json.NewDecoder(r.Body).Decode(&recorded.Body); err != nil {
			t.Errorf("fake Polar could not decode the %s %s body: %v", r.Method, r.URL.Path, err)
		}
		fake.requests = append(fake.requests, recorded)

		if fake.status != 0 {
			w.WriteHeader(fake.status)
			_, _ = w.Write([]byte(fake.body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		switch r.URL.Path {
		case "/v1/customers/":
			_, _ = w.Write([]byte(`{"id":"cus_created","email":"person@futo.org"}`))
		case "/v1/checkouts/":
			_, _ = w.Write([]byte(`{"id":"checkout_1","url":"https://polar.test/checkout/checkout_1"}`))
		case "/v1/customer-sessions/":
			_, _ = w.Write([]byte(`{"id":"session_1","token":"tok","customer_portal_url":"https://polar.test/portal/tok"}`))
		default:
			t.Errorf("fake Polar received an unexpected call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakePolar) client() *polar.Client {
	return &polar.Client{BaseURL: f.URL, AccessToken: "polar_oat_test"}
}

func (f *fakePolar) only(t *testing.T) recordedRequest {
	t.Helper()
	if len(f.requests) != 1 {
		t.Fatalf("fake Polar received %d requests, want 1: %+v", len(f.requests), f.requests)
	}
	return f.requests[0]
}

func TestCreateCustomerSendsTheAccountsSubAsPolarsExternalID(t *testing.T) {
	fake := newFakePolar(t)

	customerID, err := fake.client().CreateCustomer(context.Background(), "zitadel-sub-1", "person@futo.org")
	if err != nil {
		t.Fatalf("CreateCustomer() error = %v", err)
	}
	if customerID != "cus_created" {
		t.Errorf("CreateCustomer() = %q, want the id Polar answered with", customerID)
	}

	request := fake.only(t)
	if request.Method != http.MethodPost || request.Path != "/v1/customers/" {
		t.Errorf("CreateCustomer called %s %s, want POST /v1/customers/", request.Method, request.Path)
	}
	if request.Auth != "Bearer polar_oat_test" {
		t.Errorf("Authorization = %q, want the access token as a bearer", request.Auth)
	}
	if request.Body["external_id"] != "zitadel-sub-1" {
		t.Errorf("external_id = %v, want the account's sub", request.Body["external_id"])
	}
	if request.Body["email"] != "person@futo.org" {
		t.Errorf("email = %v, want the account's email", request.Body["email"])
	}
}

func TestCreateCheckoutSendsTheProductDiscountCustomerAndSuccessURL(t *testing.T) {
	fake := newFakePolar(t)

	checkoutURL, err := fake.client().CreateCheckout(context.Background(), polar.CheckoutRequest{
		ProductID:  "prod_1",
		DiscountID: "disc_1",
		CustomerID: "cus_created",
		SuccessURL: "https://notes.futo.test/billing/done",
	})
	if err != nil {
		t.Fatalf("CreateCheckout() error = %v", err)
	}
	if checkoutURL != "https://polar.test/checkout/checkout_1" {
		t.Errorf("CreateCheckout() = %q, want the URL Polar answered with", checkoutURL)
	}

	request := fake.only(t)
	if request.Method != http.MethodPost || request.Path != "/v1/checkouts/" {
		t.Errorf("CreateCheckout called %s %s, want POST /v1/checkouts/", request.Method, request.Path)
	}
	products, ok := request.Body["products"].([]any)
	if !ok || len(products) != 1 || products[0] != "prod_1" {
		t.Errorf("products = %v, want the one configured product", request.Body["products"])
	}
	if request.Body["discount_id"] != "disc_1" {
		t.Errorf("discount_id = %v", request.Body["discount_id"])
	}
	if request.Body["customer_id"] != "cus_created" {
		t.Errorf("customer_id = %v", request.Body["customer_id"])
	}
	if request.Body["success_url"] != "https://notes.futo.test/billing/done" {
		t.Errorf("success_url = %v", request.Body["success_url"])
	}
}

func TestACheckoutWithNoDiscountOmitsTheField(t *testing.T) {
	fake := newFakePolar(t)

	if _, err := fake.client().CreateCheckout(context.Background(), polar.CheckoutRequest{
		ProductID:  "prod_1",
		CustomerID: "cus_created",
		SuccessURL: "https://notes.futo.test/billing/done",
	}); err != nil {
		t.Fatalf("CreateCheckout() error = %v", err)
	}

	// Sending discount_id: null would be Polar's own "no discount", but the
	// launch flip is dropping POLAR_DISCOUNT_ID, so the unset case has to
	// look to Polar exactly like a checkout that never mentioned a discount.
	if _, present := fake.only(t).Body["discount_id"]; present {
		t.Errorf("discount_id was sent with no POLAR_DISCOUNT_ID configured: %+v", fake.only(t).Body)
	}
}

func TestCreateCustomerSessionAsksForOneCustomersPortal(t *testing.T) {
	fake := newFakePolar(t)

	portalURL, err := fake.client().CreateCustomerSession(context.Background(), "cus_created")
	if err != nil {
		t.Fatalf("CreateCustomerSession() error = %v", err)
	}
	if portalURL != "https://polar.test/portal/tok" {
		t.Errorf("CreateCustomerSession() = %q, want the portal URL Polar answered with", portalURL)
	}

	request := fake.only(t)
	if request.Method != http.MethodPost || request.Path != "/v1/customer-sessions/" {
		t.Errorf("CreateCustomerSession called %s %s, want POST /v1/customer-sessions/", request.Method, request.Path)
	}
	if request.Body["customer_id"] != "cus_created" {
		t.Errorf("customer_id = %v", request.Body["customer_id"])
	}
}

func TestARefusalFromPolarIsAnErrorNamingTheStatusAndBody(t *testing.T) {
	fake := newFakePolar(t)
	fake.status = http.StatusUnprocessableEntity
	fake.body = `{"detail":[{"msg":"external_id is already used"}]}`

	_, err := fake.client().CreateCustomer(context.Background(), "zitadel-sub-1", "person@futo.org")
	if err == nil {
		t.Fatal("CreateCustomer() error = nil for a 422")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "external_id is already used") {
		t.Errorf("error = %q, want the status and what Polar said", err)
	}
}
