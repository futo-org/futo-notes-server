package stripe_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/stripe"
)

const (
	secretKey      = "sk_test_fake"
	publishableKey = "pk_test_fake"
	webhookSecret  = "whsec_test_secret"
	priceID        = "price_test_plan"
	productID      = "prod_test_plan"
	publicURL      = "https://notes.example.test"
)

// fakeStripe is the slice of Stripe's API the provider calls, in memory.
type fakeStripe struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	next          int
	customers     map[string]*fakeCustomer
	idempotent    map[string]string
	sessions      map[string]map[string]any
	subscriptions map[string]map[string]any
	// invoices are each subscription's invoices; intents and charges the
	// PaymentIntents paying them and their charges; refunded the
	// PaymentIntents refunded.
	invoices map[string][]map[string]any
	intents  map[string]map[string]any
	charges  map[string]map[string]any
	refunded map[string]bool
	// tokens are the ConfirmationTokens the browser made.
	tokens map[string]*fakeToken
	// responses are earlier answers by idempotency key; conflicts the
	// number of 409s still to answer under a key, as Stripe does while a
	// request under it is in flight.
	responses map[string]any
	conflicts map[string]int
	requests  []fakeRequest
}

// fakeToken is a ConfirmationToken: the billing country its form (or
// saved card) gave, the card's issuing country, and how its payment goes.
type fakeToken struct {
	billed, issued string
	// saved is the Customer whose saved card it uses, "" for a new card.
	saved string
	// outcome is "succeeded", "requires_action" (3-D Secure) or "declined".
	outcome string
}

type fakeCustomer struct {
	ID       string
	Email    string
	Created  int64
	Metadata map[string]string
}

func (c *fakeCustomer) object() map[string]any {
	return map[string]any{"id": c.ID, "object": "customer", "email": nilIfEmpty(c.Email), "created": c.Created, "metadata": c.Metadata}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type fakeRequest struct {
	Method, Path   string
	Form           url.Values
	IdempotencyKey string
}

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	f := &fakeStripe{
		t: t, customers: map[string]*fakeCustomer{}, idempotent: map[string]string{},
		sessions: map[string]map[string]any{}, subscriptions: map[string]map[string]any{},
		invoices: map[string][]map[string]any{}, intents: map[string]map[string]any{}, charges: map[string]map[string]any{},
		refunded: map[string]bool{}, tokens: map[string]*fakeToken{},
		responses: map[string]any{}, conflicts: map[string]int{},
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripe) provider() *stripe.Provider {
	return stripe.New(stripe.Config{
		SecretKey: secretKey, PublishableKey: publishableKey, WebhookSecret: []byte(webhookSecret),
		PriceID: priceID, PublicURL: publicURL, APIURL: f.srv.URL,
	})
}

func (f *fakeStripe) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s_%d", prefix, f.next)
}

// addCustomer stores a customer as if created earlier.
func (f *fakeStripe) addCustomer(sub, email string, created int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeCustomer{ID: f.id("cus"), Email: email, Created: created, Metadata: map[string]string{}}
	if sub != "" {
		c.Metadata["sub"] = sub
	}
	f.customers[c.ID] = c
	return c.ID
}

// addSubscription stores a subscription of customer to the plan, with its
// own metadata.sub when sub is set.
func (f *fakeStripe) addSubscription(customer, sub, status string, periodEnd int64, change func(map[string]any)) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	metadata := map[string]any{}
	if sub != "" {
		metadata["sub"] = sub
	}
	s := map[string]any{
		"id": f.id("sub"), "object": "subscription", "status": status, "cancel_at_period_end": false, "cancel_at": nil,
		"livemode": false, "metadata": metadata, "customer": customer, "created": time.Now().Unix(),
		"items": map[string]any{"object": "list", "data": []any{map[string]any{
			"id": "si_1", "current_period_end": periodEnd, "current_period_start": periodEnd - 30*86400,
			"price": map[string]any{"id": priceID, "product": productID},
		}}},
	}
	if change != nil {
		change(s)
	}
	f.subscriptions[s["id"].(string)] = s
	return s["id"].(string)
}

// completeSession marks a Checkout Session on Stripe's page paid, creating
// its subscription, with the session's subscription metadata, and its paid
// first invoice, billed in country billed.
func (f *fakeStripe) completeSession(id, billed string, periodEnd int64) string {
	f.mu.Lock()
	s := f.sessions[id]
	customer, _ := s["customer"].(string)
	sub, _ := s["client_reference_id"].(string)
	metadata := s["subscription_metadata"].(map[string]any)
	f.mu.Unlock()
	subID := f.addSubscription(customer, sub, "active", periodEnd, func(sub map[string]any) { sub["metadata"] = metadata })
	f.mu.Lock()
	defer f.mu.Unlock()
	s["status"] = "complete"
	s["subscription"] = subID
	f.payInvoice(f.newInvoice(subID), billed)
	return subID
}

// paidSubscription is an active subscription of customer, for sub, sold
// on our page for chosen, whose first payment was charged with a billing
// address in billed: what the check before payment never lets through,
// for the checks after it.
func (f *fakeStripe) paidSubscription(customer, sub, chosen, billed string) string {
	subID := f.addSubscription(customer, sub, "active", time.Now().Add(30*24*time.Hour).Unix(), func(s map[string]any) {
		s["metadata"].(map[string]any)["billing_country"] = chosen
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payInvoice(f.newInvoice(subID), billed)
	return subID
}

// newInvoice opens subscription subID's first invoice, with its default
// payment's PaymentIntent, and makes it the subscription's latest.
func (f *fakeStripe) newInvoice(subID string) map[string]any {
	intent := f.id("pi")
	f.intents[intent] = map[string]any{
		"id": intent, "object": "payment_intent", "status": "requires_payment_method", "client_secret": intent + "_secret_fake",
		"last_payment_error": nil, "latest_charge": nil,
	}
	invoice := map[string]any{
		"id": f.id("in"), "object": "invoice", "status": "open", "billing_reason": "subscription_create", "subscription": subID,
		"payments": map[string]any{"object": "list", "data": []any{map[string]any{
			"id": f.id("inpay"), "object": "invoice_payment", "status": "open", "is_default": true,
			"payment": map[string]any{"type": "payment_intent", "payment_intent": intent},
		}}},
	}
	f.invoices[subID] = append(f.invoices[subID], invoice)
	if s, ok := f.subscriptions[subID]; ok {
		s["latest_invoice"] = invoice["id"]
	}
	return invoice
}

// payInvoice charges invoice's PaymentIntent with a billing address in
// billed, marking it and its payment paid and its subscription active.
func (f *fakeStripe) payInvoice(invoice map[string]any, billed string) {
	payment := invoice["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)
	intent := f.intents[payment["payment"].(map[string]any)["payment_intent"].(string)]
	charge := f.id("ch")
	f.charges[charge] = map[string]any{
		"id": charge, "object": "charge", "paid": true,
		"billing_details": map[string]any{"address": map[string]any{"country": nilIfEmpty(billed), "postal_code": nil}},
	}
	intent["status"] = "succeeded"
	intent["latest_charge"] = charge
	payment["status"] = "paid"
	invoice["status"] = "paid"
	if s := f.subscriptions[invoice["subscription"].(string)]; s["status"] == "incomplete" {
		s["status"] = "active"
	}
}

// token makes a ConfirmationToken for a new card billed in billed (and
// issued in the same country) whose payment ends as outcome.
func (f *fakeStripe) token(billed, outcome string) string {
	return f.addToken(&fakeToken{billed: billed, issued: billed, outcome: outcome})
}

func (f *fakeStripe) addToken(t *fakeToken) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id("ctoken")
	f.tokens[id] = t
	return id
}

// authenticate ends the 3-D Secure step of subscription subID's payment:
// passed, the card is charged; failed, the payment needs another card.
func (f *fakeStripe) authenticate(subID string, passed bool, billed string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	invoice := f.invoices[subID][0]
	if passed {
		f.payInvoice(invoice, billed)
		return
	}
	payment := invoice["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)
	intent := f.intents[payment["payment"].(map[string]any)["payment_intent"].(string)]
	intent["status"] = "requires_payment_method"
	intent["last_payment_error"] = map[string]any{"type": "card_error", "code": "authentication_required",
		"message": "We are unable to authenticate your payment method."}
}

// subscriptionList is every subscription the fake holds.
func (f *fakeStripe) subscriptionList() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, s := range f.subscriptions {
		out = append(out, maps.Clone(s))
	}
	return out
}

// subscriptionStatus is a subscription's status as the fake holds it.
func (f *fakeStripe) subscriptionStatus(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscriptions[id]["status"].(string)
}

// refunds lists the payment intents refunded on subscription id's invoices.
func (f *fakeStripe) refunds(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, invoice := range f.invoices[id] {
		for _, payment := range invoice["payments"].(map[string]any)["data"].([]any) {
			intent := payment.(map[string]any)["payment"].(map[string]any)["payment_intent"].(string)
			if f.refunded[intent] {
				out = append(out, intent)
			}
		}
	}
	return out
}

func (f *fakeStripe) calls(method, path string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+secretKey {
		writeJSON(w, http.StatusUnauthorized, stripeError("invalid_request_error", "", "Invalid API Key provided"))
		return
	}
	if got := r.Header.Get("Stripe-Version"); got != stripe.APIVersion {
		f.t.Errorf("%s %s with Stripe-Version %q", r.Method, r.URL.Path, got)
	}
	form := r.URL.Query()
	if r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			f.t.Errorf("POST %s with Content-Type %q", r.URL.Path, ct)
		}
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("POST %s: %v", r.URL.Path, err)
		}
		form = r.PostForm
	}
	f.requests = append(f.requests, fakeRequest{Method: r.Method, Path: r.URL.Path, Form: form, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	expand := form["expand[]"]
	expandCustomer := slices.Contains(expand, "customer")
	key := r.Header.Get("Idempotency-Key")
	if f.conflicts[key] > 0 {
		f.conflicts[key]--
		writeJSON(w, http.StatusConflict, stripeError("idempotency_error", "idempotency_key_in_use", "There is currently another in-progress request using this Idempotent Key."))
		return
	}
	if earlier, ok := f.responses[key]; ok && key != "" && r.Method != http.MethodGet && path != "/customers" {
		writeJSON(w, http.StatusOK, earlier)
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/customers/search":
		f.search(w, form.Get("query"))
	case r.Method == http.MethodPost && path == "/customers":
		if id, ok := f.idempotent[r.Header.Get("Idempotency-Key")]; ok && r.Header.Get("Idempotency-Key") != "" {
			writeJSON(w, http.StatusOK, f.customers[id].object())
			return
		}
		c := &fakeCustomer{ID: f.id("cus"), Email: form.Get("email"), Created: time.Now().Unix(), Metadata: map[string]string{}}
		for key := range form {
			if name, ok := strings.CutPrefix(key, "metadata["); ok {
				c.Metadata[strings.TrimSuffix(name, "]")] = form.Get(key)
			}
		}
		f.customers[c.ID] = c
		f.idempotent[r.Header.Get("Idempotency-Key")] = c.ID
		writeJSON(w, http.StatusOK, c.object())
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/customers/"):
		c, ok := f.customers[strings.TrimPrefix(path, "/customers/")]
		if !ok {
			writeJSON(w, http.StatusNotFound, stripeError("invalid_request_error", "resource_missing", "No such customer"))
			return
		}
		if form.Has("email") {
			c.Email = form.Get("email")
		}
		writeJSON(w, http.StatusOK, c.object())
	case r.Method == http.MethodPost && path == "/checkout/sessions":
		id := f.id("cs_test")
		metadata, subscriptionMetadata := map[string]any{}, map[string]any{}
		for key := range form {
			if name, ok := strings.CutPrefix(key, "metadata["); ok {
				metadata[strings.TrimSuffix(name, "]")] = form.Get(key)
			}
			if name, ok := strings.CutPrefix(key, "subscription_data[metadata]["); ok {
				subscriptionMetadata[strings.TrimSuffix(name, "]")] = form.Get(key)
			}
		}
		f.sessions[id] = map[string]any{
			"id": id, "object": "checkout.session", "client_secret": id + "_secret_fake", "status": "open",
			"mode": form.Get("mode"), "ui_mode": form.Get("ui_mode"), "customer": form.Get("customer"),
			"client_reference_id": nilIfEmpty(form.Get("client_reference_id")), "subscription": nil, "url": nil,
			"metadata": metadata, "subscription_metadata": subscriptionMetadata, "customer_details": nil,
		}
		if form.Get("ui_mode") == "hosted_page" {
			f.sessions[id]["client_secret"] = nil
			f.sessions[id]["url"] = "https://checkout.stripe.com/c/pay/" + id
		}
		writeJSON(w, http.StatusOK, f.sessions[id])
	case r.Method == http.MethodGet && path == "/invoices":
		if form.Get("status") != "paid" || form.Get("expand[]") != "data.payments" {
			f.t.Errorf("invoices listed with %v", form)
		}
		data := []any{}
		for _, invoice := range slices.Backward(f.invoices[form.Get("subscription")]) {
			if invoice["status"] == "paid" {
				data = append(data, invoice)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	case r.Method == http.MethodPost && path == "/customer_sessions":
		if _, ok := f.customers[form.Get("customer")]; !ok {
			writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "resource_missing", "No such customer"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "customer_session", "client_secret": f.id("cuss") + "_secret_MUST-NOT-BE-LOGGED", "customer": form.Get("customer"),
		})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/confirmation_tokens/"):
		id := strings.TrimPrefix(path, "/confirmation_tokens/")
		token, ok := f.tokens[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, stripeError("invalid_request_error", "resource_missing", "No such confirmationtoken"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "confirmation_token", "setup_future_usage": "off_session", "payment_intent": nil,
			"payment_method_preview": map[string]any{
				"type": "card", "customer": nilIfEmpty(token.saved),
				"billing_details": map[string]any{"address": map[string]any{"country": nilIfEmpty(token.billed), "postal_code": nil}},
				"card":            map[string]any{"brand": "visa", "country": token.issued, "last4": "4242"},
			},
		})
	case r.Method == http.MethodPost && path == "/subscriptions":
		if _, ok := f.customers[form.Get("customer")]; !ok {
			writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "resource_missing", "No such customer"))
			return
		}
		metadata := map[string]any{}
		for key := range form {
			if name, ok := strings.CutPrefix(key, "metadata["); ok {
				metadata[strings.TrimSuffix(name, "]")] = form.Get(key)
			}
		}
		id := f.id("sub")
		end := time.Now().Add(30 * 24 * time.Hour).Unix()
		f.subscriptions[id] = map[string]any{
			"id": id, "object": "subscription", "status": "incomplete", "cancel_at_period_end": false, "cancel_at": nil,
			"livemode": false, "metadata": metadata, "customer": form.Get("customer"), "created": time.Now().Unix(),
			"items": map[string]any{"object": "list", "data": []any{map[string]any{
				"id": "si_1", "current_period_end": end, "current_period_start": end - 30*86400,
				"price": map[string]any{"id": form.Get("items[0][price]"), "product": productID},
			}}},
		}
		f.newInvoice(id)
		created := f.subscriptionObject(f.subscriptions[id], expand)
		f.responses[key] = created
		writeJSON(w, http.StatusOK, created)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/payment_intents/") && strings.HasSuffix(path, "/confirm"):
		intent, ok := f.intents[strings.TrimSuffix(strings.TrimPrefix(path, "/payment_intents/"), "/confirm")]
		token, known := f.tokens[form.Get("confirmation_token")]
		if !ok || !known {
			writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "resource_missing", "No such payment_intent or confirmation token"))
			return
		}
		if intent["status"] != "requires_payment_method" {
			writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "payment_intent_unexpected_state",
				"This PaymentIntent's status is "+intent["status"].(string)+"."))
			return
		}
		invoice := f.invoiceOf(intent["id"].(string))
		switch token.outcome {
		case "succeeded":
			f.payInvoice(invoice, token.billed)
		case "requires_action":
			intent["status"] = "requires_action"
			intent["next_action"] = map[string]any{"type": "use_stripe_sdk"}
		case "declined":
			intent["last_payment_error"] = map[string]any{"type": "card_error", "code": "card_declined", "message": "Your card was declined."}
			writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": map[string]any{
				"type": "card_error", "code": "card_declined", "decline_code": "generic_decline", "message": "Your card was declined.",
			}})
			return
		}
		f.responses[key] = maps.Clone(intent)
		writeJSON(w, http.StatusOK, intent)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/payment_intents/"):
		intent, ok := f.intents[strings.TrimPrefix(path, "/payment_intents/")]
		if !ok {
			writeJSON(w, http.StatusNotFound, stripeError("invalid_request_error", "resource_missing", "No such payment_intent"))
			return
		}
		out := maps.Clone(intent)
		if charge, ok := out["latest_charge"].(string); ok && slices.Contains(expand, "latest_charge") {
			out["latest_charge"] = f.charges[charge]
		}
		writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodPost && path == "/refunds":
		intent := form.Get("payment_intent")
		if f.refunded[intent] {
			writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "charge_already_refunded", "Charge has already been refunded."))
			return
		}
		f.refunded[intent] = true
		refund := map[string]any{"id": f.id("re"), "object": "refund", "payment_intent": intent, "status": "succeeded"}
		f.responses[key] = refund
		writeJSON(w, http.StatusOK, refund)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/checkout/sessions/"):
		s, ok := f.sessions[strings.TrimPrefix(path, "/checkout/sessions/")]
		if !ok {
			writeJSON(w, http.StatusNotFound, stripeError("invalid_request_error", "resource_missing", "No such checkout.session"))
			return
		}
		writeJSON(w, http.StatusOK, f.withCustomer(s, expandCustomer))
	case r.Method == http.MethodGet && path == "/subscriptions":
		// status=all: every subscription of the customer, ended ones too.
		var data []any
		for _, s := range f.subscriptions {
			if s["customer"] == r.URL.Query().Get("customer") {
				data = append(data, f.subscriptionObject(s, nil))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	case strings.HasPrefix(path, "/subscriptions/"):
		s, ok := f.subscriptions[strings.TrimPrefix(path, "/subscriptions/")]
		if !ok {
			writeJSON(w, http.StatusNotFound, stripeError("invalid_request_error", "resource_missing", "No such subscription"))
			return
		}
		if r.Method == http.MethodDelete {
			if s["status"] == "canceled" || s["status"] == "incomplete_expired" {
				writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "", "A canceled subscription can only update its cancellation_details and metadata."))
				return
			}
			// As the sandbox does: cancelling an unpaid subscription expires it.
			if s["status"] == "incomplete" {
				s["status"] = "incomplete_expired"
			} else {
				s["status"] = "canceled"
			}
		}
		if r.Method == http.MethodPost {
			if s["status"] == "canceled" {
				writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "", "A canceled subscription can only update its cancellation_details and metadata."))
				return
			}
			if form.Get("cancel_at_period_end") == "true" {
				s["cancel_at_period_end"] = true
				s["cancel_at"] = s["items"].(map[string]any)["data"].([]any)[0].(map[string]any)["current_period_end"]
			}
		}
		writeJSON(w, http.StatusOK, f.subscriptionObject(s, expand))
	case r.Method == http.MethodGet && path == "/prices/"+priceID:
		writeJSON(w, http.StatusOK, map[string]any{
			"id": priceID, "object": "price", "currency": "usd", "unit_amount": 700, "product": productID,
			"recurring": map[string]any{"interval": "year", "interval_count": 1},
		})
	case r.Method == http.MethodPost && path == "/billing_portal/sessions":
		writeJSON(w, http.StatusOK, map[string]any{"id": f.id("bps"), "url": "https://billing.stripe.com/p/session/test_" + form.Get("customer")})
	default:
		writeJSON(w, http.StatusNotFound, stripeError("invalid_request_error", "", "Unrecognized request URL"))
	}
}

// search answers a customer search, which must be by metadata['sub'] only.
func (f *fakeStripe) search(w http.ResponseWriter, query string) {
	value, ok := strings.CutPrefix(query, "metadata['sub']:'")
	if !ok || !strings.HasSuffix(value, "'") {
		f.t.Errorf("customer search %q is not by metadata['sub']", query)
		writeJSON(w, http.StatusBadRequest, stripeError("invalid_request_error", "", "bad query"))
		return
	}
	value = strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(strings.TrimSuffix(value, "'"))
	var data []any
	for _, c := range f.customers {
		if c.Metadata["sub"] == value {
			data = append(data, c.object())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "search_result", "data": data, "has_more": false})
}

// subscriptionObject is subscription s as Stripe answers it, with expand.
func (f *fakeStripe) subscriptionObject(s map[string]any, expand []string) map[string]any {
	out := f.withCustomer(s, slices.Contains(expand, "customer"))
	if id, ok := out["latest_invoice"].(string); ok && slices.Contains(expand, "latest_invoice.payments") {
		for _, invoice := range f.invoices[s["id"].(string)] {
			if invoice["id"] == id {
				out["latest_invoice"] = invoice
			}
		}
	}
	return out
}

// invoiceOf is the invoice PaymentIntent intent pays.
func (f *fakeStripe) invoiceOf(intent string) map[string]any {
	for _, invoices := range f.invoices {
		for _, invoice := range invoices {
			for _, payment := range invoice["payments"].(map[string]any)["data"].([]any) {
				if payment.(map[string]any)["payment"].(map[string]any)["payment_intent"] == intent {
					return invoice
				}
			}
		}
	}
	return nil
}

func (f *fakeStripe) withCustomer(obj map[string]any, expand bool) map[string]any {
	out := maps.Clone(obj)
	if id, ok := out["customer"].(string); ok && expand {
		if c, ok := f.customers[id]; ok {
			out["customer"] = c.object()
		} else {
			out["customer"] = map[string]any{"id": id, "object": "customer", "deleted": true}
		}
	}
	return out
}

func stripeError(kind, code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"type": kind, "code": nilIfEmpty(code), "message": message}}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// sign is the Stripe-Signature header for body at t under secret.
func sign(secret string, t time.Time, body []byte) string {
	return "t=" + strconv.FormatInt(t.Unix(), 10) + ",v1=" + v1(secret, t, body)
}

func v1(secret string, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t.Unix())
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// delivery is a signed delivery of a Stripe event about object.
func delivery(t time.Time, eventID, kind, objectType, objectID string) billing.Delivery {
	body, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "type": kind, "created": t.Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{"id": objectID, "object": objectType, "status": "this payload is never trusted"}},
	})
	return billing.Delivery{Header: http.Header{"Stripe-Signature": {sign(webhookSecret, t, body)}}, Body: body}
}
