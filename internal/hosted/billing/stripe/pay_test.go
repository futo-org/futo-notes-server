package stripe_test

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
)

var me = billing.Customer{Subject: subject, Email: "me@example.test"}

// pay pays as me, who chose chosen, with token.
func pay(t *testing.T, f *fakeStripe, chosen, token string) (billing.PaymentResult, error) {
	t.Helper()
	return f.provider().Pay(t.Context(), me, billing.Payment{BillingCountry: chosen, ConfirmationToken: token})
}

// nothingCreated fails unless no subscription exists and no payment was
// confirmed.
func nothingCreated(t *testing.T, f *fakeStripe) {
	t.Helper()
	if subs := f.subscriptionList(); len(subs) != 0 {
		t.Fatalf("subscriptions created: %v", subs)
	}
	if n := len(f.calls(http.MethodPost, "/v1/subscriptions")); n != 0 {
		t.Fatalf("%d subscription creations", n)
	}
	for _, r := range f.requests {
		if strings.HasPrefix(r.Path, "/v1/payment_intents/") {
			t.Fatalf("a payment was touched: %s %s", r.Method, r.Path)
		}
	}
}

// refusals counts the cancellations and refunds sent.
func refusals(f *fakeStripe, subID string) (cancels, refunds int) {
	return len(f.calls(http.MethodDelete, "/v1/subscriptions/"+subID)), len(f.calls(http.MethodPost, "/v1/refunds"))
}

func TestStartPayment(t *testing.T) {
	f := newFakeStripe(t)
	secret, err := f.provider().StartPayment(t.Context(), me)
	if err != nil || !strings.Contains(secret, "_secret_") {
		t.Fatalf("StartPayment = %q, %v", secret, err)
	}
	sessions := f.calls(http.MethodPost, "/v1/customer_sessions")
	if len(sessions) != 1 {
		t.Fatalf("customer sessions = %+v", sessions)
	}
	form := sessions[0].Form
	// Offer to save the card, show the cards saved that way, never remove
	// one (it may pay a subscription): all per session, nothing left to
	// the account's settings.
	want := map[string]string{
		"components[payment_element][enabled]":                             "true",
		"components[payment_element][features][payment_method_save]":       "enabled",
		"components[payment_element][features][payment_method_save_usage]": "off_session",
		"components[payment_element][features][payment_method_redisplay]":  "enabled",
	}
	for key, value := range want {
		if form.Get(key) != value {
			t.Errorf("customer session %s = %q, want %q", key, form.Get(key), value)
		}
	}
	for key := range form {
		if _, ok := want[key]; !ok && key != "customer" {
			t.Errorf("customer session sends %s=%q", key, form.Get(key))
		}
	}
	// The customer is the one found or created by sub, and no other.
	created := f.calls(http.MethodPost, "/v1/customers")
	if len(created) != 1 || created[0].Form.Get("metadata[sub]") != subject {
		t.Fatalf("customers created = %+v", created)
	}
	if !strings.HasPrefix(form.Get("customer"), "cus_") {
		t.Fatalf("customer session for %q", form.Get("customer"))
	}
	if _, err := f.provider().StartPayment(t.Context(), billing.Customer{Email: "x@example.test"}); err == nil {
		t.Fatal("a payment session without a sub was opened")
	}
}

func TestPayMatchEntitles(t *testing.T) {
	f := newFakeStripe(t)
	token := f.token("US", "succeeded")
	result, err := pay(t, f, "US", token)
	if err != nil || result.ActionSecret != "" || result.Event.Status != billing.StatusActive || result.Event.Subject != subject ||
		result.Event.ExternalID != result.SubscriptionID || !result.Event.WillRenew {
		t.Fatalf("Pay = %+v, %v", result, err)
	}

	// Created incomplete, for the plan, recording the sub and the country,
	// then its first payment confirmed with the token: both under the
	// token, which is this attempt's.
	creates := f.calls(http.MethodPost, "/v1/subscriptions")
	if len(creates) != 1 {
		t.Fatalf("subscription creations = %+v", creates)
	}
	form := creates[0].Form
	want := map[string]string{
		"items[0][price]": priceID, "items[0][quantity]": "1", "payment_behavior": "default_incomplete",
		"payment_settings[save_default_payment_method]": "on_subscription", "payment_settings[payment_method_types][0]": "card",
		"metadata[sub]": subject, "metadata[billing_country]": "US", "expand[]": "latest_invoice.payments",
	}
	for key, value := range want {
		if form.Get(key) != value {
			t.Errorf("subscription %s = %q, want %q", key, form.Get(key), value)
		}
	}
	for key := range form {
		if _, ok := want[key]; !ok && key != "customer" {
			t.Errorf("subscription sends %s=%q", key, form.Get(key))
		}
	}
	if creates[0].IdempotencyKey != "pay-"+token {
		t.Errorf("subscription idempotency key %q", creates[0].IdempotencyKey)
	}
	f.mu.Lock()
	intent := f.invoices[result.SubscriptionID][0]["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)["payment"].(map[string]any)["payment_intent"].(string)
	f.mu.Unlock()
	confirms := f.calls(http.MethodPost, "/v1/payment_intents/"+intent+"/confirm")
	if len(confirms) != 1 || confirms[0].Form.Get("confirmation_token") != token || len(confirms[0].Form) != 1 ||
		confirms[0].IdempotencyKey != "confirm-"+token {
		t.Fatalf("confirmations = %+v", confirms)
	}

	// The webhook and reconciliation agree, and nothing is refused.
	p := f.provider()
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.updated", "subscription", result.SubscriptionID)); err != nil ||
		len(events) != 1 || events[0].Status != billing.StatusActive {
		t.Fatalf("Verify = %+v, %v", events, err)
	}
	if ev, err := p.Refresh(t.Context(), result.SubscriptionID); err != nil || ev.Status != billing.StatusActive {
		t.Fatalf("Refresh = %+v, %v", ev, err)
	}
	if cancels, refunds := refusals(f, result.SubscriptionID); cancels != 0 || refunds != 0 {
		t.Fatalf("a matching payment was refused: %d cancels, %d refunds", cancels, refunds)
	}
}

// Only the billing country typed in the form counts: a card issued in
// another country pays, and one issued at home but billed abroad does not.
func TestPayIgnoresTheIssuingCountry(t *testing.T) {
	f := newFakeStripe(t)
	if _, err := pay(t, f, "US", f.addToken(&fakeToken{billed: "US", issued: "GB", outcome: "succeeded"})); err != nil {
		t.Fatalf("a card issued abroad, billed at home: %v", err)
	}
	g := newFakeStripe(t)
	_, err := pay(t, g, "US", g.addToken(&fakeToken{billed: "CA", issued: "US", outcome: "succeeded"}))
	if _, ok := errors.AsType[*billing.CountryMismatchError](err); !ok {
		t.Fatalf("a card issued at home, billed abroad: %v", err)
	}
}

func TestPayMismatchCreatesNothing(t *testing.T) {
	for name, billed := range map[string]string{"another country": "CA", "no country": ""} {
		t.Run(name, func(t *testing.T) {
			f := newFakeStripe(t)
			_, err := pay(t, f, "US", f.token(billed, "succeeded"))
			var mismatch *billing.CountryMismatchError
			if !errors.As(err, &mismatch) || *mismatch != (billing.CountryMismatchError{Chosen: "US", Billed: billed}) {
				t.Fatalf("Pay = %v, want an unrefunded US/%q mismatch", err, billed)
			}
			nothingCreated(t, f)
			if n := len(f.calls(http.MethodGet, "/v1/confirmation_tokens/"+"ctoken_1")); n != 1 {
				t.Fatalf("the token was read %d times", n)
			}
		})
	}
}

func TestPayRefusesUnknownTokens(t *testing.T) {
	f := newFakeStripe(t)
	for _, token := range []string{"ctoken_missing", "pm_card_visa", ""} {
		if _, err := pay(t, f, "US", token); !errors.Is(err, billing.ErrCheckoutForeign) {
			t.Errorf("Pay with %q = %v, want ErrCheckoutForeign", token, err)
		}
	}
	if _, err := pay(t, f, "", f.token("US", "succeeded")); !errors.Is(err, billing.ErrCheckoutForeign) {
		t.Errorf("Pay without a country = %v", err)
	}
	nothingCreated(t, f)
}

// A card saved by an earlier purchase is vetted the same way, by the
// billing address it was saved with.
func TestPaySavedCard(t *testing.T) {
	f := newFakeStripe(t)
	customer := f.addCustomer(subject, me.Email, 1)
	_, err := pay(t, f, "US", f.addToken(&fakeToken{billed: "CA", issued: "CA", saved: customer, outcome: "succeeded"}))
	var mismatch *billing.CountryMismatchError
	if !errors.As(err, &mismatch) || mismatch.Billed != "CA" || mismatch.Refunded {
		t.Fatalf("Pay with a card saved in CA = %v", err)
	}
	nothingCreated(t, f)

	result, err := pay(t, f, "US", f.addToken(&fakeToken{billed: "US", issued: "US", saved: customer, outcome: "succeeded"}))
	if err != nil || result.Event.Status != billing.StatusActive {
		t.Fatalf("Pay with a card saved in US = %+v, %v", result, err)
	}
	if got := f.calls(http.MethodPost, "/v1/subscriptions")[0].Form.Get("customer"); got != customer {
		t.Fatalf("paid as customer %q, want %q", got, customer)
	}
}

func TestPayThreeDSecure(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	result, err := pay(t, f, "US", f.token("US", "requires_action"))
	if err != nil || !strings.Contains(result.ActionSecret, "_secret_") || result.SubscriptionID == "" || result.Event.ExternalID != "" {
		t.Fatalf("Pay = %+v, %v", result, err)
	}
	// Until the bank answers, nothing entitles, and the webhook says so.
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", result.SubscriptionID)); err != nil ||
		len(events) != 1 || events[0].Status != billing.StatusOnHold {
		t.Fatalf("Verify before 3-D Secure = %+v, %v", events, err)
	}
	// Asked again before then, the step is the same.
	again, err := p.FinishPayment(t.Context(), me, result.SubscriptionID)
	if err != nil || again.ActionSecret != result.ActionSecret {
		t.Fatalf("FinishPayment before 3-D Secure = %+v, %v", again, err)
	}

	f.authenticate(result.SubscriptionID, true, "US")
	done, err := p.FinishPayment(t.Context(), me, result.SubscriptionID)
	if err != nil || done.ActionSecret != "" || done.Event.Status != billing.StatusActive || done.SubscriptionID != result.SubscriptionID {
		t.Fatalf("FinishPayment = %+v, %v", done, err)
	}
	// Only the subscription's owner can finish it.
	for name, c := range map[string]billing.Customer{"someone else": {Subject: "another-sub"}, "nobody": {}} {
		if _, err := p.FinishPayment(t.Context(), c, result.SubscriptionID); !errors.Is(err, billing.ErrCheckoutForeign) {
			t.Errorf("FinishPayment as %s = %v", name, err)
		}
	}
	for _, id := range []string{"sub_missing", "cs_test_1", ""} {
		if _, err := p.FinishPayment(t.Context(), me, id); !errors.Is(err, billing.ErrCheckoutForeign) {
			t.Errorf("FinishPayment of %q = %v", id, err)
		}
	}
}

func TestPayThreeDSecureFailed(t *testing.T) {
	f := newFakeStripe(t)
	result, err := pay(t, f, "US", f.token("US", "requires_action"))
	if err != nil {
		t.Fatal(err)
	}
	f.authenticate(result.SubscriptionID, false, "")
	_, err = f.provider().FinishPayment(t.Context(), me, result.SubscriptionID)
	var declined *billing.PaymentDeclinedError
	if !errors.As(err, &declined) || declined.Message != "We are unable to authenticate your payment method." {
		t.Fatalf("FinishPayment after a failed 3-D Secure = %v", err)
	}
	if f.subscriptionStatus(result.SubscriptionID) != "incomplete_expired" {
		t.Fatalf("subscription %s, want cancelled (incomplete_expired)", f.subscriptionStatus(result.SubscriptionID))
	}
	// Settled again (the page retried): still declined, nothing more sent.
	if _, err := f.provider().FinishPayment(t.Context(), me, result.SubscriptionID); !errors.As(err, &declined) {
		t.Fatalf("FinishPayment again = %v", err)
	}
	if cancels, _ := refusals(f, result.SubscriptionID); cancels != 1 {
		t.Fatalf("%d cancellations", cancels)
	}
}

func TestPayDeclinedThenRetried(t *testing.T) {
	f := newFakeStripe(t)
	_, err := pay(t, f, "US", f.token("US", "declined"))
	var declined *billing.PaymentDeclinedError
	if !errors.As(err, &declined) || declined.Message != "Your card was declined." {
		t.Fatalf("Pay with a declined card = %v", err)
	}
	subs := f.subscriptionList()
	if len(subs) != 1 || subs[0]["status"] != "incomplete_expired" {
		t.Fatalf("after a decline: %v", subs)
	}
	if len(f.refunds(subs[0]["id"].(string))) != 0 {
		t.Fatal("a declined payment was refunded")
	}

	// Another card, another token: another subscription, which pays.
	result, err := pay(t, f, "US", f.token("US", "succeeded"))
	if err != nil || result.Event.Status != billing.StatusActive || result.SubscriptionID == subs[0]["id"] {
		t.Fatalf("Pay after a decline = %+v, %v", result, err)
	}
}

// The same token twice (a retried request) creates and confirms once.
func TestPayIdempotentPerToken(t *testing.T) {
	f := newFakeStripe(t)
	token := f.token("US", "succeeded")
	first, err := pay(t, f, "US", token)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pay(t, f, "US", token)
	if err != nil || second.SubscriptionID != first.SubscriptionID || second.Event.Status != billing.StatusActive {
		t.Fatalf("Pay again = %+v, %v (first %s)", second, err, first.SubscriptionID)
	}
	if subs := f.subscriptionList(); len(subs) != 1 {
		t.Fatalf("%d subscriptions for one token", len(subs))
	}
	for _, r := range f.calls(http.MethodPost, "/v1/subscriptions") {
		if r.IdempotencyKey != "pay-"+token {
			t.Fatalf("subscription created under %q", r.IdempotencyKey)
		}
	}

	// A declined token retried stays declined, with one subscription.
	g := newFakeStripe(t)
	declinedToken := g.token("US", "declined")
	var declined *billing.PaymentDeclinedError
	for range 2 {
		if _, err := pay(t, g, "US", declinedToken); !errors.As(err, &declined) {
			t.Fatalf("Pay with a declined token = %v", err)
		}
	}
	if subs := g.subscriptionList(); len(subs) != 1 || subs[0]["status"] != "incomplete_expired" {
		t.Fatalf("subscriptions = %v", subs)
	}
}

// The checks after payment: a subscription sold on our page whose first
// charge was billed elsewhere (which Pay's own check never lets through)
// is refused wherever it could entitle.
func TestCountryMismatchAfterPaymentIsRefunded(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	customer := f.addCustomer(subject, "", 1)
	subID := f.paidSubscription(customer, subject, "US", "CA")

	created := delivery(time.Now(), "evt_created", "customer.subscription.created", "subscription", subID)
	for range 2 { // and Stripe's retry
		eventID, events, err := p.Verify(t.Context(), created)
		if err != nil || eventID != "evt_created" || len(events) != 0 {
			t.Fatalf("Verify = %q, %+v, %v; want acknowledged with nothing to apply", eventID, events, err)
		}
	}
	if f.subscriptionStatus(subID) != "canceled" || len(f.refunds(subID)) != 1 {
		t.Fatalf("subscription %s, refunded %v", f.subscriptionStatus(subID), f.refunds(subID))
	}
	// Each check refunds again under the same key, in case a step failed
	// before; Stripe answers the first refund.
	for _, refund := range f.calls(http.MethodPost, "/v1/refunds") {
		if refund.Form.Get("payment_intent") != f.refunds(subID)[0] || refund.IdempotencyKey != "refund-"+f.refunds(subID)[0] ||
			refund.Form.Get("metadata[reason]") != "billing_country_mismatch" {
			t.Fatalf("refund %+v", refund)
		}
	}
	// Read from the first payment's charge.
	if reads := f.calls(http.MethodGet, "/v1/payment_intents/"+f.refunds(subID)[0]); len(reads) == 0 || reads[0].Form.Get("expand[]") != "latest_charge" {
		t.Fatalf("payment intent reads = %+v", reads)
	}

	// Reconciliation sees it over; finishing it on the page says why.
	if ev, err := p.Refresh(t.Context(), subID); err != nil || ev.Status != billing.StatusExpired || ev.WillRenew {
		t.Fatalf("Refresh = %+v, %v", ev, err)
	}
	f.mu.Lock()
	f.subscriptions[subID]["latest_invoice"] = f.invoices[subID][0]["id"]
	f.mu.Unlock()
	_, err := p.FinishPayment(t.Context(), me, subID)
	var mismatch *billing.CountryMismatchError
	if !errors.As(err, &mismatch) || *mismatch != (billing.CountryMismatchError{Chosen: "US", Billed: "CA", Refunded: true}) {
		t.Fatalf("FinishPayment = %v", err)
	}
	if cancels, _ := refusals(f, subID); cancels != 1 || len(f.refunds(subID)) != 1 {
		t.Fatalf("%d cancels, refunded %v", cancels, f.refunds(subID))
	}
}

func TestCountryMissingAfterPaymentIsRefunded(t *testing.T) {
	f := newFakeStripe(t)
	subID := f.paidSubscription(f.addCustomer(subject, "", 1), subject, "US", "")
	_, err := f.provider().Refresh(t.Context(), subID)
	if err != nil || f.subscriptionStatus(subID) != "canceled" || len(f.refunds(subID)) != 1 {
		t.Fatalf("Refresh = %v, subscription %s, refunded %v", err, f.subscriptionStatus(subID), f.refunds(subID))
	}
}

// A webhook for an entitling subscription whose first charge is not there
// yet cannot be judged: it is retried, never applied.
func TestCountryCheckWaitsForTheCharge(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	subID := f.addSubscription(f.addCustomer(subject, "", 1), subject, "active", time.Now().Add(time.Hour).Unix(), func(s map[string]any) {
		s["metadata"].(map[string]any)["billing_country"] = "US"
	})
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.updated", "subscription", subID)); err == nil {
		t.Fatalf("Verify before the charge = %+v, want an error", events)
	}
	if cancels, refunds := refusals(f, subID); cancels != 0 || refunds != 0 {
		t.Fatalf("%d cancels, %d refunds before the charge", cancels, refunds)
	}
}

func TestCountryRefusalRacesItself(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	subID := f.paidSubscription(f.addCustomer(subject, "", 1), subject, "US", "CA")
	f.mu.Lock()
	intent := f.invoices[subID][0]["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)["payment"].(map[string]any)["payment_intent"].(string)
	// The other refund under the key is still in flight at first.
	f.conflicts["refund-"+intent] = 1
	f.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			_, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", subID))
			if err == nil && len(events) != 0 {
				err = errors.New("the refused subscription was reported")
			}
			errs[i] = err
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("webhooks: %v, %v", errs[0], errs[1])
	}
	if f.subscriptionStatus(subID) != "canceled" || len(f.refunds(subID)) != 1 {
		t.Fatalf("subscription %s, refunded %v", f.subscriptionStatus(subID), f.refunds(subID))
	}
}

func TestMerchantCheckoutHasNoCountryCheck(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	target, err := p.StartMerchantCheckout(t.Context(), me, publicURL+"/return", publicURL+"/cancel")
	if err != nil {
		t.Fatal(err)
	}
	form := f.calls(http.MethodPost, "/v1/checkout/sessions")[0].Form
	if form.Has("metadata[billing_country]") || form.Has("subscription_data[metadata][billing_country]") {
		t.Fatalf("merchant checkout records a billing country: %v", form)
	}
	id := target[len("https://checkout.stripe.com/c/pay/"):]
	subID := f.completeSession(id, "CA", time.Now().Add(30*24*time.Hour).Unix())
	if ev, err := p.CompleteCheckout(t.Context(), me, id); err != nil || ev.Status != billing.StatusActive {
		t.Fatalf("CompleteCheckout = %+v, %v", ev, err)
	}
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", subID)); err != nil || len(events) != 1 {
		t.Fatalf("Verify = %+v, %v", events, err)
	}
	if cancels, refunds := refusals(f, subID); cancels != 0 || refunds != 0 ||
		len(f.calls(http.MethodGet, "/v1/invoices")) != 0 || len(f.calls(http.MethodGet, "/v1/confirmation_tokens/")) != 0 {
		t.Fatalf("merchant checkout was checked: %d cancels, %d refunds", cancels, refunds)
	}
	// It is not our page's payment: it cannot be finished there.
	if _, err := p.FinishPayment(t.Context(), me, subID); !errors.Is(err, billing.ErrCheckoutForeign) {
		t.Fatalf("FinishPayment of a merchant subscription = %v", err)
	}
}
