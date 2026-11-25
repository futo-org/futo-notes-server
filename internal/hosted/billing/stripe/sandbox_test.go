package stripe_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/stripe"
)

// The dev plan the sandbox test sells, found by its price's lookup key.
const (
	sandboxLookupKey   = "futo-notes-sync-dev-monthly"
	sandboxProductName = "FUTO Notes Sync (dev)"
)

// sandbox calls a Stripe sandbox with a test secret key.
type sandbox struct {
	t   *testing.T
	key string
}

// readKeyFile reads a key from the file env names, or skips the test.
func readKeyFile(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s is not set", env)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", env, err)
	}
	return strings.TrimSpace(string(data))
}

// call sends one request and decodes the response into out; it never
// includes the key in a failure.
func (s sandbox) call(method, path string, form url.Values, out any) {
	s.t.Helper()
	target := stripe.APIURL + "/v1" + path
	var body io.Reader
	if method == http.MethodGet {
		target += "?" + form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}
	// Cleanup runs after t.Context is cancelled.
	req, err := http.NewRequestWithContext(context.WithoutCancel(s.t.Context()), method, target, body)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Stripe-Version", stripe.APIVersion)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: request failed", method, path)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		s.t.Fatalf("%s %s = %d %s", method, path, res.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			s.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

// plan returns the dev plan's product and price, creating them once.
func (s sandbox) plan() (product, price string) {
	var prices struct {
		Data []struct {
			ID      string `json:"id"`
			Product string `json:"product"`
		} `json:"data"`
	}
	s.call(http.MethodGet, "/prices", url.Values{"lookup_keys[]": {sandboxLookupKey}, "active": {"true"}}, &prices)
	if len(prices.Data) > 0 {
		return prices.Data[0].Product, prices.Data[0].ID
	}
	var created struct {
		ID           string `json:"id"`
		DefaultPrice string `json:"default_price"`
	}
	s.call(http.MethodPost, "/products", url.Values{
		"name": {sandboxProductName}, "default_price_data[currency]": {"usd"},
		"default_price_data[unit_amount]": {"500"}, "default_price_data[recurring][interval]": {"month"},
	}, &created)
	s.call(http.MethodPost, "/prices/"+created.DefaultPrice, url.Values{"lookup_key": {sandboxLookupKey}}, nil)
	return created.ID, created.DefaultPrice
}

// TestSandbox runs the provider against a real Stripe sandbox. It needs
// STRIPE_TEST_SECRET_KEY_FILE, a file holding a test-mode secret key, and
// reads STRIPE_TEST_PUBLISHABLE_KEY_FILE when set. It creates customers and
// a subscription tagged with a fresh sub and deletes them when done.
func TestSandbox(t *testing.T) {
	key := readKeyFile(t, "STRIPE_TEST_SECRET_KEY_FILE")
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("STRIPE_TEST_SECRET_KEY_FILE does not hold a test-mode secret key")
	}
	publishable := "pk_test_unset"
	if os.Getenv("STRIPE_TEST_PUBLISHABLE_KEY_FILE") != "" {
		publishable = readKeyFile(t, "STRIPE_TEST_PUBLISHABLE_KEY_FILE")
		if !strings.HasPrefix(publishable, "pk_test_") {
			t.Fatal("STRIPE_TEST_PUBLISHABLE_KEY_FILE does not hold a test-mode publishable key")
		}
	}
	s := sandbox{t: t, key: key}
	product, price := s.plan()
	t.Logf("dev plan: product %s, price %s (lookup key %s)", product, price, sandboxLookupKey)

	webhookSecret := "whsec_sandbox_test_" + uuid.NewV7().String()
	p := stripe.New(stripe.Config{
		SecretKey: key, PublishableKey: publishable, WebhookSecret: []byte(webhookSecret), PriceID: price,
		PublicURL: "https://notes.example.test",
	})
	subject := "s7-sandbox-" + uuid.NewV7().String()
	me := billing.Customer{Subject: subject, Email: "sandbox-test@example.com"}
	customers := map[string]bool{}
	var subscriptions []string
	t.Cleanup(func() {
		for _, id := range subscriptions {
			s.call(http.MethodDelete, "/subscriptions/"+id, nil, nil)
		}
		for id := range customers {
			s.call(http.MethodDelete, "/customers/"+id, nil, nil)
		}
	})

	// Find-or-create by sub is idempotent: two checkouts, one customer.
	var sessions []billing.Checkout
	for range 2 {
		checkout, err := p.StartCheckout(t.Context(), me, "https://notes.example.test/dashboard/billing/return?session_id={CHECKOUT_SESSION_ID}")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(checkout.ID, "cs_test_") || checkout.ClientSecret == "" {
			t.Fatalf("checkout id %q, client secret set %t", checkout.ID, checkout.ClientSecret != "")
		}
		sessions = append(sessions, checkout)
	}
	var session struct {
		Customer          string            `json:"customer"`
		ClientReferenceID string            `json:"client_reference_id"`
		UIMode            string            `json:"ui_mode"`
		Mode              string            `json:"mode"`
		Status            string            `json:"status"`
		Metadata          map[string]string `json:"metadata"`
		Currency          string            `json:"currency"`
		AmountTotal       int64             `json:"amount_total"`
		AdaptivePricing   struct {
			Enabled bool `json:"enabled"`
		} `json:"adaptive_pricing"`
		PaymentMethodTypes []string `json:"payment_method_types"`
		WalletOptions      struct {
			Link struct {
				Display string `json:"display"`
			} `json:"link"`
		} `json:"wallet_options"`
	}
	plan, err := p.Plan(t.Context())
	if err != nil || plan.Currency != "usd" || plan.Amount != 500 || plan.Interval != "month" || plan.IntervalCount != 1 {
		t.Fatalf("Plan = %+v, %v", plan, err)
	}
	var customer string
	for _, checkout := range sessions {
		s.call(http.MethodGet, "/checkout/sessions/"+checkout.ID, nil, &session)
		customers[session.Customer] = true
		customer = session.Customer
		if session.ClientReferenceID != subject || session.UIMode != "elements" || session.Mode != "subscription" ||
			session.Status != "open" || session.Metadata["sub"] != subject {
			t.Fatalf("session %s = %+v", checkout.ID, session)
		}
		// The price's own amount and currency, by card alone.
		if session.AdaptivePricing.Enabled || session.Currency != plan.Currency || session.AmountTotal != plan.Amount ||
			len(session.PaymentMethodTypes) != 1 || session.PaymentMethodTypes[0] != "card" || session.WalletOptions.Link.Display != "never" {
			t.Fatalf("session %s = %+v", checkout.ID, session)
		}
	}
	if len(customers) != 1 {
		t.Fatalf("two checkouts for one sub made customers %v", customers)
	}
	var cus struct {
		Email    string            `json:"email"`
		Metadata map[string]string `json:"metadata"`
	}
	s.call(http.MethodGet, "/customers/"+customer, nil, &cus)
	if cus.Metadata["sub"] != subject || cus.Email != me.Email {
		t.Fatalf("customer = %+v", cus)
	}

	// An open session is not complete, and is nobody else's.
	if _, err := p.CompleteCheckout(t.Context(), me, sessions[0].ID); !errors.Is(err, billing.ErrCheckoutIncomplete) {
		t.Fatalf("CompleteCheckout of an open session = %v", err)
	}
	if _, err := p.CompleteCheckout(t.Context(), billing.Customer{Subject: "someone-else"}, sessions[0].ID); !errors.Is(err, billing.ErrCheckoutForeign) {
		t.Fatalf("CompleteCheckout as someone else = %v", err)
	}

	// A subscription paid with the test card, as checkout would make it.
	var pm struct {
		ID string `json:"id"`
	}
	s.call(http.MethodPost, "/payment_methods/pm_card_visa/attach", url.Values{"customer": {customer}}, &pm)
	var sub struct {
		ID string `json:"id"`
	}
	s.call(http.MethodPost, "/subscriptions", url.Values{
		"customer": {customer}, "items[0][price]": {price}, "default_payment_method": {pm.ID},
		"metadata[sub]": {subject},
	}, &sub)
	subscriptions = append(subscriptions, sub.ID)

	ev, err := p.Refresh(t.Context(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != billing.StatusActive || !ev.WillRenew || ev.Subject != subject || ev.ProductID != product ||
		ev.Environment != billing.EnvironmentSandbox || ev.ExpiresAt == nil || time.Until(*ev.ExpiresAt) < 27*24*time.Hour {
		t.Fatalf("Refresh = %+v", ev)
	}
	if err := p.Cancel(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if ev, err = p.Refresh(t.Context(), sub.ID); err != nil || ev.WillRenew || ev.Status != billing.StatusActive {
		t.Fatalf("Refresh after Cancel = %+v, %v", ev, err)
	}
	var raw struct {
		CancelAtPeriodEnd bool `json:"cancel_at_period_end"`
	}
	s.call(http.MethodGet, "/subscriptions/"+sub.ID, nil, &raw)
	if !raw.CancelAtPeriodEnd {
		t.Fatal("Cancel did not set cancel_at_period_end")
	}

	// A real event, signed as Stripe would sign it, verifies and names
	// the subscription and its account.
	event := sandboxEvent(t, s, sub.ID)
	now := time.Now()
	delivery := billing.Delivery{Header: http.Header{"Stripe-Signature": {sign(webhookSecret, now, event)}}, Body: event}
	id, events, err := p.Verify(t.Context(), delivery)
	if err != nil || !strings.HasPrefix(id, "evt_") || len(events) != 1 || events[0].ExternalID != sub.ID || events[0].Subject != subject || events[0].WillRenew {
		t.Fatalf("Verify of a real event = %q, %+v, %v", id, events, err)
	}
	delivery.Header.Set("Stripe-Signature", sign(webhookSecret, now.Add(-10*time.Minute), event))
	if _, _, err := p.Verify(t.Context(), delivery); !errors.Is(err, billing.ErrInvalidDelivery) {
		t.Fatalf("Verify of a stale signature = %v", err)
	}

	// Ended at once, it maps as expired.
	s.call(http.MethodDelete, "/subscriptions/"+sub.ID, nil, nil)
	subscriptions = nil
	if ev, err = p.Refresh(t.Context(), sub.ID); err != nil || ev.Status != billing.StatusExpired || ev.WillRenew {
		t.Fatalf("Refresh after deletion = %+v, %v", ev, err)
	}
}

// sandboxEvent waits for a customer.subscription.updated event about subID
// and returns its JSON.
func sandboxEvent(t *testing.T, s sandbox, subID string) []byte {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var events struct {
			Data []json.RawMessage `json:"data"`
		}
		s.call(http.MethodGet, "/events", url.Values{"type": {"customer.subscription.updated"}, "limit": {"20"}}, &events)
		for _, raw := range events.Data {
			var e struct {
				Data struct {
					Object struct {
						ID string `json:"id"`
					} `json:"object"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Data.Object.ID == subID {
				return raw
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no customer.subscription.updated event for %s", subID)
		}
		time.Sleep(2 * time.Second)
	}
}
