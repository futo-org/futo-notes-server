package standins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"futo-notes-server/internal/polar"
)

// deliveryTimeout caps one webhook delivery from a completed checkout. The
// browser fetching the checkout URL is waiting on it, so a server that has
// wedged has to become a readable error rather than a hung request.
const deliveryTimeout = 10 * time.Second

// deliveryClient is the transport a completed checkout delivers over.
var deliveryClient = &http.Client{Timeout: deliveryTimeout}

// Webhook is where a completed stand-in checkout delivers its subscription
// event, and the Standard Webhooks secret it signs with. It is the server's
// own webhook route: the delivery goes in the front door and through
// signature verification like any other.
type Webhook struct {
	URL    string
	Secret string
}

// Polar stands in for Polar's API. It answers the three calls the server
// makes and keeps every request body, so a caller can assert what the server
// actually sent — that a checkout carried POLAR_DISCOUNT_ID, and that its
// success URL points at the server's own "go back to the app" page.
//
// It refuses a second customer for an external id it has already seen, the
// way Polar does, so the lazy-registration path is exercised honestly.
//
// Fetching the checkout URL it hands back stands in for a person paying:
// it signs a subscription event for that checkout's customer, delivers it to
// the server's webhook, and only then sends the browser to the success page.
// Firing before redirecting is what lets a test read the checkout URL and
// find the account entitled on the very next request.
type Polar struct {
	// Origin is where it is listening, which is what the server is given as
	// POLAR_BASE_URL. Set before StartPolar returns and never written again.
	Origin string

	accessToken string
	webhook     Webhook

	mu       sync.Mutex
	requests map[string][]map[string]any
	// customers maps an external id (the account's OIDC sub) to the Polar
	// customer id handed back for it.
	customers map[string]string
	// checkouts maps an opened checkout's id to what completing it needs.
	checkouts map[string]checkout
}

// checkout is one opened checkout: who it is for, and where the browser goes
// once it is paid.
type checkout struct {
	customerID string
	externalID string
	successURL string
}

// StartPolar starts the stand-in on a loopback port. webhook is where a
// completed checkout delivers its subscription event.
func StartPolar(accessToken string, webhook Webhook) (*Polar, func(), error) {
	standin := &Polar{
		accessToken: accessToken,
		webhook:     webhook,
		requests:    map[string][]map[string]any{},
		customers:   map[string]string{},
		checkouts:   map[string]checkout{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/customers/", standin.createCustomer)
	mux.HandleFunc("POST /v1/checkouts/", standin.createCheckout)
	mux.HandleFunc("POST /v1/customer-sessions/", standin.createCustomerSession)
	mux.HandleFunc("GET /checkout/{id}", standin.completeCheckout)

	origin, stop, err := serve(mux)
	if err != nil {
		return nil, nil, err
	}
	standin.Origin = origin
	return standin, stop, nil
}

// read returns the request body decoded, after checking the access token and
// recording the call. A false second return means it has answered already.
func (f *Polar) read(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.Header.Get("Authorization") != "Bearer "+f.accessToken {
		http.Error(w, `{"detail":"invalid token"}`, http.StatusUnauthorized)
		return nil, false
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"detail":"invalid json"}`, http.StatusBadRequest)
		return nil, false
	}
	f.mu.Lock()
	f.requests[r.URL.Path] = append(f.requests[r.URL.Path], body)
	f.mu.Unlock()
	return body, true
}

// LastRequest returns the most recent body sent to path.
func (f *Polar) LastRequest(path string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.requests[path]
	if len(calls) == 0 {
		return nil, fmt.Errorf("the server never called Polar %s", path)
	}
	return calls[len(calls)-1], nil
}

// CallCount reports how many times path was called, which is how a caller
// checks that a customer is registered at most once per account.
func (f *Polar) CallCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests[path])
}

func (f *Polar) createCustomer(w http.ResponseWriter, r *http.Request) {
	body, ok := f.read(w, r)
	if !ok {
		return
	}
	externalID, _ := body["external_id"].(string)

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.customers[externalID]; taken {
		// Polar's own refusal: an external id belongs to one customer.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"detail": []map[string]string{{"msg": "external_id is already used"}},
		})
		return
	}
	customerID := fmt.Sprintf("polar_cus_%d", len(f.customers)+1)
	f.customers[externalID] = customerID
	writeJSON(w, http.StatusCreated, map[string]string{"id": customerID})
}

func (f *Polar) createCheckout(w http.ResponseWriter, r *http.Request) {
	body, ok := f.read(w, r)
	if !ok {
		return
	}
	customerID, _ := body["customer_id"].(string)
	successURL, _ := body["success_url"].(string)

	id := randomToken()
	f.mu.Lock()
	f.checkouts[id] = checkout{
		customerID: customerID,
		externalID: f.externalIDLocked(customerID),
		successURL: successURL,
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"url": f.Origin + "/checkout/" + id})
}

// externalIDLocked returns the external id a customer was registered under.
// Call it holding the lock.
func (f *Polar) externalIDLocked(customerID string) string {
	for externalID, known := range f.customers {
		if known == customerID {
			return externalID
		}
	}
	return ""
}

// completeCheckout stands in for a person paying: it delivers the
// subscription event Polar would send and then sends the browser on to the
// success page. A delivery the server refuses is reported here rather than
// swallowed, because an account that silently failed to become entitled is
// the hardest possible failure to read from the far end.
func (f *Polar) completeCheckout(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	opened, known := f.checkouts[r.PathValue("id")]
	f.mu.Unlock()
	if !known {
		http.Error(w, "unknown checkout", http.StatusNotFound)
		return
	}
	if err := f.deliver(r.PathValue("id"), opened); err != nil {
		http.Error(w, "delivering the subscription event: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, opened.successURL, http.StatusFound)
}

// deliver signs one subscription.created the way Polar would and posts it to
// the server's webhook route.
func (f *Polar) deliver(checkoutID string, opened checkout) error {
	now := time.Now()
	periodEnd := now.AddDate(0, 1, 0).UTC()
	delivery, err := polar.Delivery(polar.Event{
		Type: "subscription.created",
		Subscription: polar.Subscription{
			ID:                 "polar_sub_" + checkoutID,
			Status:             "active",
			CustomerID:         opened.customerID,
			ExternalCustomerID: opened.externalID,
			CurrentPeriodEnd:   &periodEnd,
		},
	})
	if err != nil {
		return err
	}
	header, err := polar.Sign(f.webhook.Secret, "standin_"+checkoutID, now, delivery)
	if err != nil {
		return err
	}

	request, err := http.NewRequest(http.MethodPost, f.webhook.URL, bytes.NewReader(delivery))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := deliveryClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the webhook answered %d: %s", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"applied"`)) {
		return fmt.Errorf("the webhook answered %s, want applied: the delivery reached no account", body)
	}
	return nil
}

func (f *Polar) createCustomerSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.read(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"customer_portal_url": f.Origin + "/portal/" + randomToken(),
	})
}
