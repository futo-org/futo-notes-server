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
	run := uuid.NewV7().String()
	subject := "s7-sandbox-" + run
	me := billing.Customer{Subject: subject, Email: "sandbox-test+" + run + "@example.com"}
	var customer string
	var subscriptions []string
	t.Cleanup(func() {
		for _, id := range subscriptions {
			var current struct {
				Status string `json:"status"`
			}
			s.call(http.MethodGet, "/subscriptions/"+id, nil, &current)
			if current.Status != "canceled" && current.Status != "incomplete_expired" {
				s.call(http.MethodDelete, "/subscriptions/"+id, nil, nil)
			}
		}
		if customer != "" {
			s.call(http.MethodDelete, "/customers/"+customer, nil, nil)
		}
	})

	// Find-or-create by sub is idempotent: two payment sessions, one
	// customer (listed by its unique email: search lags).
	for range 2 {
		secret, err := p.StartPayment(t.Context(), me)
		if err != nil || secret == "" {
			t.Fatalf("StartPayment: secret set %t, %v", secret != "", err)
		}
	}
	var found struct {
		Data []struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		} `json:"data"`
	}
	s.call(http.MethodGet, "/customers", url.Values{"email": {me.Email}}, &found)
	if len(found.Data) != 1 || found.Data[0].Metadata["sub"] != subject {
		t.Fatalf("two payment sessions for one sub made customers %+v", found.Data)
	}
	customer = found.Data[0].ID
	plan, err := p.Plan(t.Context())
	if err != nil || plan.Currency != "usd" || plan.Amount != 500 || plan.Interval != "month" || plan.IntervalCount != 1 {
		t.Fatalf("Plan = %+v, %v", plan, err)
	}

	// token is a test ConfirmationToken for a new card from cardToken,
	// billed in country, as the Payment Element makes in subscription mode.
	token := func(cardToken, country string) string {
		var ct struct {
			ID string `json:"id"`
		}
		s.call(http.MethodPost, "/test_helpers/confirmation_tokens", url.Values{
			"payment_method_data[type]":                                  {"card"},
			"payment_method_data[card][token]":                           {cardToken},
			"payment_method_data[billing_details][address][country]":     {country},
			"payment_method_data[billing_details][address][postal_code]": {"10001"},
			"return_url":         {"https://notes.example.test/billing/checkout/x"},
			"setup_future_usage": {"off_session"},
		}, &ct)
		return ct.ID
	}
	// mine lists the customer's subscriptions, by status.
	mine := func() map[string]string {
		var list struct {
			Data []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"data"`
		}
		s.call(http.MethodGet, "/subscriptions", url.Values{"customer": {customer}, "status": {"all"}}, &list)
		out := map[string]string{}
		for _, sub := range list.Data {
			out[sub.ID] = sub.Status
		}
		return out
	}
	pay := func(country, ct string) (billing.PaymentResult, error) {
		result, err := p.Pay(t.Context(), me, billing.Payment{BillingCountry: country, ConfirmationToken: ct})
		if result.SubscriptionID != "" {
			subscriptions = append(subscriptions, result.SubscriptionID)
		}
		return result, err
	}

	// Billed in another country: refused before anything exists.
	var mismatch *billing.CountryMismatchError
	if _, err := pay("US", token("tok_visa", "CA")); !errors.As(err, &mismatch) || mismatch.Billed != "CA" || mismatch.Refunded {
		t.Fatalf("Pay billed in CA = %v", err)
	}
	if subs := mine(); len(subs) != 0 {
		t.Fatalf("a refused payment created %v", subs)
	}

	// Declined: the subscription it was for is cancelled at once.
	var declined *billing.PaymentDeclinedError
	if _, err := pay("US", token("tok_chargeDeclined", "US")); !errors.As(err, &declined) || declined.Message == "" {
		t.Fatalf("Pay with a declined card = %v", err)
	}
	// 3-D Secure: the page's step, until the buyer answers.
	secure, err := pay("US", token("tok_threeDSecure2Required", "US"))
	if err != nil || !strings.Contains(secure.ActionSecret, "_secret_") {
		t.Fatalf("Pay with a 3-D Secure card = %+v (secret set %t), %v", secure.SubscriptionID, secure.ActionSecret != "", err)
	}
	if again, err := p.FinishPayment(t.Context(), me, secure.SubscriptionID); err != nil || again.ActionSecret == "" {
		t.Fatalf("FinishPayment before 3-D Secure: secret set %t, %v", again.ActionSecret != "", err)
	}

	// Billed in the country chosen: paid, and active at once.
	paid, err := pay("US", token("tok_visa", "US"))
	if err != nil || paid.Event.Status != billing.StatusActive || paid.Event.Subject != subject {
		t.Fatalf("Pay billed in US = %+v, %v", paid.Event, err)
	}
	subs := mine()
	for id, status := range subs {
		want := map[string]string{paid.SubscriptionID: "active", secure.SubscriptionID: "incomplete"}[id]
		if want == "" {
			want = "incomplete_expired" // the declined one, cancelled unpaid
		}
		if status != want {
			t.Fatalf("subscription %s is %s, want %s (all: %v)", id, status, want, subs)
		}
	}
	if len(subs) != 3 {
		t.Fatalf("subscriptions = %v", subs)
	}
	sub := struct{ ID string }{paid.SubscriptionID}

	// A subscription sold on our page for US but charged with a CA
	// billing address (which Pay never lets through) is refused and
	// refunded by the checks after payment, read from the charge.
	var pm struct {
		ID string `json:"id"`
	}
	s.call(http.MethodPost, "/payment_methods", url.Values{
		"type": {"card"}, "card[token]": {"tok_visa"}, "billing_details[address][country]": {"CA"},
	}, &pm)
	s.call(http.MethodPost, "/payment_methods/"+pm.ID+"/attach", url.Values{"customer": {customer}}, nil)
	var stray struct {
		ID string `json:"id"`
	}
	s.call(http.MethodPost, "/subscriptions", url.Values{
		"customer": {customer}, "items[0][price]": {price}, "default_payment_method": {pm.ID},
		"metadata[sub]": {subject}, "metadata[billing_country]": {"US"},
	}, &stray)
	subscriptions = append(subscriptions, stray.ID)
	if ev, err := p.Refresh(t.Context(), stray.ID); err != nil || ev.Status != billing.StatusExpired {
		t.Fatalf("Refresh of a subscription billed in CA = %+v, %v", ev, err)
	}
	var refunds struct {
		Data []struct {
			Status   string            `json:"status"`
			Metadata map[string]string `json:"metadata"`
		} `json:"data"`
	}
	var invoices struct {
		Data []paidInvoiceJSON `json:"data"`
	}
	s.call(http.MethodGet, "/invoices", url.Values{"subscription": {stray.ID}, "expand[]": {"data.payments"}}, &invoices)
	if len(invoices.Data) != 1 || len(invoices.Data[0].Payments.Data) != 1 {
		t.Fatalf("invoices of the stray subscription = %+v", invoices)
	}
	s.call(http.MethodGet, "/refunds", url.Values{"payment_intent": {invoices.Data[0].Payments.Data[0].Payment.PaymentIntent}}, &refunds)
	if len(refunds.Data) != 1 || refunds.Data[0].Metadata["reason"] != "billing_country_mismatch" {
		t.Fatalf("refunds of the stray subscription = %+v", refunds)
	}

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
	if ev, err = p.Refresh(t.Context(), sub.ID); err != nil || ev.Status != billing.StatusExpired || ev.WillRenew {
		t.Fatalf("Refresh after deletion = %+v, %v", ev, err)
	}
}

// paidInvoiceJSON is an invoice with its payments expanded.
type paidInvoiceJSON struct {
	Payments struct {
		Data []struct {
			Payment struct {
				PaymentIntent string `json:"payment_intent"`
			} `json:"payment"`
		} `json:"data"`
	} `json:"payments"`
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

// TestSandboxManagedPayments opens a Managed Payments checkout in the
// sandbox, which needs Managed Payments activated on the account and an
// eligible tax code on the dev product. It needs STRIPE_TEST_SECRET_KEY_FILE
// like TestSandbox, and expires the session and deletes the customer when
// done.
func TestSandboxManagedPayments(t *testing.T) {
	key := readKeyFile(t, "STRIPE_TEST_SECRET_KEY_FILE")
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("STRIPE_TEST_SECRET_KEY_FILE does not hold a test-mode secret key")
	}
	s := sandbox{t: t, key: key}
	_, price := s.plan()
	p := stripe.New(stripe.Config{
		SecretKey: key, PublishableKey: "pk_test_unset", WebhookSecret: []byte("whsec_unused"), PriceID: price,
		PublicURL: "https://notes.example.test",
	})
	subject := "s11-sandbox-" + uuid.NewV7().String()
	target, err := p.StartMerchantCheckout(t.Context(), billing.Customer{Subject: subject, Email: "sandbox-test@example.com"},
		"https://notes.example.test/dashboard/billing/return?session_id={CHECKOUT_SESSION_ID}", "https://notes.example.test/dashboard/billing/subscribe")
	if err != nil {
		t.Fatalf("StartMerchantCheckout: %v (is Managed Payments activated, and does the dev product have an eligible tax code?)", err)
	}
	id := strings.TrimPrefix(target, "https://checkout.stripe.com/c/pay/")
	id, _, _ = strings.Cut(id, "#")
	if !strings.HasPrefix(id, "cs_test_") {
		t.Fatalf("hosted checkout URL %q", target)
	}
	var session struct {
		Customer          string            `json:"customer"`
		ClientReferenceID string            `json:"client_reference_id"`
		UIMode            string            `json:"ui_mode"`
		Mode              string            `json:"mode"`
		Status            string            `json:"status"`
		Metadata          map[string]string `json:"metadata"`
		Currency          string            `json:"currency"`
		ManagedPayments   struct {
			Enabled bool `json:"enabled"`
		} `json:"managed_payments"`
		AdaptivePricing struct {
			Enabled bool `json:"enabled"`
		} `json:"adaptive_pricing"`
	}
	s.call(http.MethodGet, "/checkout/sessions/"+id, nil, &session)
	t.Cleanup(func() {
		s.call(http.MethodPost, "/checkout/sessions/"+id+"/expire", nil, nil)
		s.call(http.MethodDelete, "/customers/"+session.Customer, nil, nil)
	})
	if !session.ManagedPayments.Enabled || session.UIMode != "hosted_page" || session.Mode != "subscription" || session.Status != "open" ||
		session.ClientReferenceID != subject || session.Metadata["sub"] != subject || !session.AdaptivePricing.Enabled || session.Currency != "usd" {
		t.Fatalf("session %s = %+v", id, session)
	}
	var cus struct {
		Metadata map[string]string `json:"metadata"`
	}
	s.call(http.MethodGet, "/customers/"+session.Customer, nil, &cus)
	if cus.Metadata["sub"] != subject {
		t.Fatalf("customer = %+v", cus)
	}
	if _, err := p.CompleteCheckout(t.Context(), billing.Customer{Subject: subject}, id); !errors.Is(err, billing.ErrCheckoutIncomplete) {
		t.Fatalf("CompleteCheckout of an open Managed Payments session = %v", err)
	}
}
