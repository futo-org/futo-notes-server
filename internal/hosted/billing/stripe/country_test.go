package stripe_test

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
)

// paidCheckout is a checkout on our page for chosen, paid with a card
// billed in billed; it returns the session and subscription ids.
func paidCheckout(t *testing.T, f *fakeStripe, chosen, billed string) (string, string) {
	t.Helper()
	checkout, err := f.provider().StartCheckout(t.Context(), billing.Customer{Subject: subject}, chosen, publicURL+"/return")
	if err != nil {
		t.Fatal(err)
	}
	return checkout.ID, f.completeSession(checkout.ID, billed, time.Now().Add(30*24*time.Hour).Unix())
}

// refusals counts the cancellations and refunds sent.
func refusals(f *fakeStripe, subID string) (cancels, refunds int) {
	return len(f.calls(http.MethodDelete, "/v1/subscriptions/"+subID)), len(f.calls(http.MethodPost, "/v1/refunds"))
}

func TestCountryMatchEntitles(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	id, subID := paidCheckout(t, f, "US", "US")

	ev, err := p.CompleteCheckout(t.Context(), billing.Customer{Subject: subject}, id)
	if err != nil || ev.ExternalID != subID || ev.Status != billing.StatusActive {
		t.Fatalf("CompleteCheckout = %+v, %v", ev, err)
	}
	_, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", subID))
	if err != nil || len(events) != 1 || events[0].Status != billing.StatusActive {
		t.Fatalf("Verify = %+v, %v", events, err)
	}
	if ev, err := p.Refresh(t.Context(), subID); err != nil || ev.Status != billing.StatusActive {
		t.Fatalf("Refresh = %+v, %v", ev, err)
	}
	if cancels, refunds := refusals(f, subID); cancels != 0 || refunds != 0 || f.subscriptionStatus(subID) != "active" {
		t.Fatalf("a matching payment was refused: %d cancels, %d refunds, %s", cancels, refunds, f.subscriptionStatus(subID))
	}
	// The match is read from the session that created the subscription.
	if listed := f.calls(http.MethodGet, "/v1/checkout/sessions"); len(listed) != 2 || listed[0].Form.Get("subscription") != subID {
		t.Fatalf("session lookups = %+v", listed)
	}
}

func TestCountryMismatchOnTheReturnPage(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	id, subID := paidCheckout(t, f, "US", "CA")
	me := billing.Customer{Subject: subject}

	_, err := p.CompleteCheckout(t.Context(), me, id)
	var mismatch *billing.CountryMismatchError
	if !errors.As(err, &mismatch) || *mismatch != (billing.CountryMismatchError{Chosen: "US", Billed: "CA"}) {
		t.Fatalf("CompleteCheckout = %v, want a US/CA mismatch", err)
	}
	if f.subscriptionStatus(subID) != "canceled" {
		t.Fatalf("subscription is %s, want cancelled at once", f.subscriptionStatus(subID))
	}
	cancel := f.calls(http.MethodDelete, "/v1/subscriptions/"+subID)
	refund := f.calls(http.MethodPost, "/v1/refunds")
	intents := f.refunds(subID)
	if len(cancel) != 1 || cancel[0].IdempotencyKey != "refuse-"+subID || len(refund) != 1 || len(intents) != 1 ||
		refund[0].Form.Get("payment_intent") != intents[0] || refund[0].IdempotencyKey != "refund-"+intents[0] {
		t.Fatalf("cancels %+v, refunds %+v, refunded %v", cancel, refund, intents)
	}

	// Again, and through every other path: still refused, nothing reported,
	// nothing cancelled or refunded twice.
	if _, err := p.CompleteCheckout(t.Context(), me, id); !errors.As(err, &mismatch) {
		t.Fatalf("CompleteCheckout again = %v", err)
	}
	for _, kind := range []string{"customer.subscription.created", "customer.subscription.deleted"} {
		if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_"+kind, kind, "subscription", subID)); err != nil || len(events) != 0 {
			t.Fatalf("Verify %s = %+v, %v; want nothing to apply", kind, events, err)
		}
	}
	// Reconciliation sees it over.
	if ev, err := p.Refresh(t.Context(), subID); err != nil || ev.Status != billing.StatusExpired || ev.WillRenew {
		t.Fatalf("Refresh = %+v, %v", ev, err)
	}
	if cancels, _ := refusals(f, subID); cancels != 1 || len(f.refunds(subID)) != 1 {
		t.Fatalf("%d cancels, refunded %v", cancels, f.refunds(subID))
	}
}

func TestCountryMismatchWebhookFirst(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	id, subID := paidCheckout(t, f, "US", "CA")

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
	var mismatch *billing.CountryMismatchError
	if _, err := p.CompleteCheckout(t.Context(), billing.Customer{Subject: subject}, id); !errors.As(err, &mismatch) || mismatch.Billed != "CA" {
		t.Fatalf("CompleteCheckout after the webhook = %v", err)
	}
	if len(f.refunds(subID)) != 1 {
		t.Fatalf("refunded %v", f.refunds(subID))
	}
}

func TestCountryCheckWaitsForTheSession(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	id, subID := paidCheckout(t, f, "US", "CA")
	f.mu.Lock()
	f.sessions[id]["status"] = "open"
	f.mu.Unlock()

	// An active subscription whose session has not completed cannot be
	// judged yet: the webhook is retried, never applied.
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", subID)); err == nil {
		t.Fatalf("Verify before the session completed = %+v, want an error", events)
	}
	// One that cannot entitle yet is reported as it is.
	f.mu.Lock()
	f.subscriptions[subID]["status"] = "incomplete"
	f.mu.Unlock()
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_2", "customer.subscription.created", "subscription", subID)); err != nil ||
		len(events) != 1 || events[0].Status != billing.StatusOnHold {
		t.Fatalf("Verify of an incomplete subscription = %+v, %v", events, err)
	}
	if cancels, refunds := refusals(f, subID); cancels != 0 || refunds != 0 {
		t.Fatalf("%d cancels, %d refunds before the session completed", cancels, refunds)
	}
}

func TestCountryMissingIsRefused(t *testing.T) {
	f := newFakeStripe(t)
	id, subID := paidCheckout(t, f, "US", "")
	_, err := f.provider().CompleteCheckout(t.Context(), billing.Customer{Subject: subject}, id)
	var mismatch *billing.CountryMismatchError
	if !errors.As(err, &mismatch) || mismatch.Billed != "" || f.subscriptionStatus(subID) != "canceled" || len(f.refunds(subID)) != 1 {
		t.Fatalf("CompleteCheckout without a billed country = %v, subscription %s", err, f.subscriptionStatus(subID))
	}
}

func TestCountryRefusalRacesItself(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	id, subID := paidCheckout(t, f, "US", "CA")
	intent := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.invoices[subID][0]["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)["payment"].(map[string]any)["payment_intent"].(string)
	}()
	// The other request under each key is still in flight at first.
	f.mu.Lock()
	f.conflicts["refuse-"+subID] = 1
	f.conflicts["refund-"+intent] = 1
	f.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() { _, errs[0] = p.CompleteCheckout(t.Context(), billing.Customer{Subject: subject}, id) })
	wg.Go(func() {
		_, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", subID))
		if err == nil && len(events) != 0 {
			err = errors.New("the refused subscription was reported")
		}
		errs[1] = err
	})
	wg.Wait()
	var mismatch *billing.CountryMismatchError
	if !errors.As(errs[0], &mismatch) || errs[1] != nil {
		t.Fatalf("return page %v, webhook %v", errs[0], errs[1])
	}
	if f.subscriptionStatus(subID) != "canceled" || len(f.refunds(subID)) != 1 {
		t.Fatalf("subscription %s, refunded %v", f.subscriptionStatus(subID), f.refunds(subID))
	}
}

func TestMerchantCheckoutHasNoCountryCheck(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	target, err := p.StartMerchantCheckout(t.Context(), billing.Customer{Subject: subject}, publicURL+"/return", publicURL+"/cancel")
	if err != nil {
		t.Fatal(err)
	}
	form := f.calls(http.MethodPost, "/v1/checkout/sessions")[0].Form
	if form.Has("metadata[billing_country]") || form.Has("subscription_data[metadata][billing_country]") {
		t.Fatalf("merchant checkout records a billing country: %v", form)
	}
	id := target[len("https://checkout.stripe.com/c/pay/"):]
	subID := f.completeSession(id, "CA", time.Now().Add(30*24*time.Hour).Unix())
	if ev, err := p.CompleteCheckout(t.Context(), billing.Customer{Subject: subject}, id); err != nil || ev.Status != billing.StatusActive {
		t.Fatalf("CompleteCheckout = %+v, %v", ev, err)
	}
	if _, events, err := p.Verify(t.Context(), delivery(time.Now(), "evt_1", "customer.subscription.created", "subscription", subID)); err != nil || len(events) != 1 {
		t.Fatalf("Verify = %+v, %v", events, err)
	}
	if cancels, refunds := refusals(f, subID); cancels != 0 || refunds != 0 || len(f.calls(http.MethodGet, "/v1/checkout/sessions")) != 0 {
		t.Fatalf("merchant checkout was checked: %d cancels, %d refunds", cancels, refunds)
	}
}
