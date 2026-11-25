package stripe_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/stripe"
)

const subject = "oidc-sub-1"

func TestVerifySignatures(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	customer := f.addCustomer(subject, "", 1)
	subID := f.addSubscription(customer, subject, "active", time.Now().Add(30*24*time.Hour).Unix(), nil)
	now := time.Now()
	good := delivery(now, "evt_1", "customer.subscription.updated", "subscription", subID)

	id, events, err := p.Verify(t.Context(), good)
	if err != nil || id != "evt_1" || len(events) != 1 || events[0].ExternalID != subID {
		t.Fatalf("Verify of a good delivery = %q, %+v, %v", id, events, err)
	}

	tampered := good
	tampered.Body = []byte(strings.Replace(string(good.Body), "evt_1", "evt_2", 1))
	stale := delivery(now.Add(-stripe.SignatureTolerance-time.Minute), "evt_1", "customer.subscription.updated", "subscription", subID)
	future := delivery(now.Add(stripe.SignatureTolerance+time.Minute), "evt_1", "customer.subscription.updated", "subscription", subID)
	wrongSecret := good
	wrongSecret.Header = http.Header{"Stripe-Signature": {sign("whsec_another", now, good.Body)}}
	v0Only := good
	v0Only.Header = http.Header{"Stripe-Signature": {"t=" + itoa(now) + ",v0=" + v1(webhookSecret, now, good.Body)}}
	for name, d := range map[string]billing.Delivery{
		"tampered body": tampered,
		"stale":         stale,
		"future":        future,
		"wrong secret":  wrongSecret,
		"v0 only":       v0Only,
		"no header":     {Header: http.Header{}, Body: good.Body},
		"garbage":       {Header: http.Header{"Stripe-Signature": {"t=abc,v1=zz"}}, Body: good.Body},
	} {
		if _, _, err := p.Verify(t.Context(), d); !errors.Is(err, billing.ErrInvalidDelivery) {
			t.Errorf("Verify (%s) = %v, want ErrInvalidDelivery", name, err)
		}
	}

	// While a secret is rolled, the header carries a v1 per secret; one
	// match is enough, wherever it is.
	for _, header := range []string{
		"t=" + itoa(now) + ",v1=" + v1("whsec_old", now, good.Body) + ",v1=" + v1(webhookSecret, now, good.Body),
		"t=" + itoa(now) + ",v1=" + v1(webhookSecret, now, good.Body) + ",v1=" + v1("whsec_old", now, good.Body) + ",v0=00",
	} {
		d := billing.Delivery{Header: http.Header{"Stripe-Signature": {header}}, Body: good.Body}
		if _, events, err := p.Verify(t.Context(), d); err != nil || len(events) != 1 {
			t.Errorf("Verify with several v1 (%s) = %v, %v", header, events, err)
		}
	}
}

func itoa(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestVerifyEvents(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	customer := f.addCustomer(subject, "", 1)
	periodEnd := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	subID := f.addSubscription(customer, subject, "canceled", periodEnd.Unix(), nil)
	created := time.Now().Add(-time.Minute).Truncate(time.Second)

	for _, kind := range []string{"customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted"} {
		id, events, err := p.Verify(t.Context(), delivery(created, "evt_"+kind, kind, "subscription", subID))
		if err != nil || id != "evt_"+kind || len(events) != 1 {
			t.Fatalf("Verify %s = %q, %+v, %v", kind, id, events, err)
		}
		// The state is fetched, never read from the payload.
		ev := events[0]
		if ev.Source != billing.SourceWeb || ev.ExternalID != subID || ev.Subject != subject || ev.Status != billing.StatusExpired ||
			ev.WillRenew || !ev.EventAt.Equal(created) || ev.ExpiresAt == nil || !ev.ExpiresAt.Equal(periodEnd) ||
			ev.ProductID != productID || ev.Environment != billing.EnvironmentSandbox || !strings.Contains(string(ev.Raw), subID) {
			t.Fatalf("event from %s = %+v", kind, ev)
		}
	}
	if fetched := f.calls(http.MethodGet, "/v1/subscriptions/"+subID); len(fetched) != 3 || fetched[0].Form.Get("expand[]") != "customer" {
		t.Fatalf("subscription fetches = %+v", fetched)
	}

	// Other events and vanished subscriptions verify with nothing to apply.
	for name, d := range map[string]billing.Delivery{
		"invoice":   delivery(created, "evt_inv", "invoice.paid", "invoice", "in_1"),
		"vanished":  delivery(created, "evt_gone", "customer.subscription.updated", "subscription", "sub_missing"),
		"test hook": delivery(created, "evt_ping", "customer.created", "customer", customer),
	} {
		id, events, err := p.Verify(t.Context(), d)
		if err != nil || id == "" || len(events) != 0 {
			t.Errorf("Verify (%s) = %q, %+v, %v; want an id and no events", name, id, events, err)
		}
	}
	if _, _, err := p.Verify(t.Context(), delivery(created, "evt_bad", "customer.subscription.updated", "invoice", "in_1")); !errors.Is(err, billing.ErrInvalidDelivery) {
		t.Fatalf("subscription event about an invoice = %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	customer := f.addCustomer(subject, "", 1)
	periodEnd := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	for _, c := range []struct {
		status     string
		want       billing.Status
		grace      bool
		willRenew  bool
		cancelling bool
	}{
		{"active", billing.StatusActive, false, true, false},
		{"trialing", billing.StatusActive, false, true, false},
		{"past_due", billing.StatusGrace, true, true, false},
		{"unpaid", billing.StatusOnHold, false, true, false},
		{"paused", billing.StatusOnHold, false, true, false},
		{"incomplete", billing.StatusOnHold, false, true, false},
		{"canceled", billing.StatusExpired, false, false, false},
		{"incomplete_expired", billing.StatusExpired, false, false, false},
		{"active", billing.StatusActive, false, false, true},
	} {
		subID := f.addSubscription(customer, subject, c.status, periodEnd.Unix(), func(s map[string]any) {
			if c.cancelling {
				s["cancel_at_period_end"] = true
			}
		})
		ev, err := p.Refresh(t.Context(), subID)
		if err != nil {
			t.Fatalf("Refresh %s: %v", c.status, err)
		}
		if ev.Status != c.want || ev.WillRenew != c.willRenew || ev.ExpiresAt == nil || !ev.ExpiresAt.Equal(periodEnd) {
			t.Errorf("%s (cancelling %t) = %s, renews %t, expires %v", c.status, c.cancelling, ev.Status, ev.WillRenew, ev.ExpiresAt)
		}
		if c.grace != (ev.GraceUntil != nil) || (c.grace && !ev.GraceUntil.Equal(periodEnd)) {
			t.Errorf("%s grace = %v", c.status, ev.GraceUntil)
		}
		if ev.EventAt.IsZero() || time.Since(ev.EventAt) > time.Minute {
			t.Errorf("%s refreshed at %v", c.status, ev.EventAt)
		}
	}

	// A cancellation scheduled past this period still renews once.
	later := f.addSubscription(customer, subject, "active", periodEnd.Unix(), func(s map[string]any) { s["cancel_at"] = periodEnd.Add(60 * 24 * time.Hour).Unix() })
	soon := f.addSubscription(customer, subject, "active", periodEnd.Unix(), func(s map[string]any) { s["cancel_at"] = periodEnd.Unix() })
	if ev, _ := p.Refresh(t.Context(), later); !ev.WillRenew {
		t.Error("a cancellation after this period stops the renewal")
	}
	if ev, _ := p.Refresh(t.Context(), soon); ev.WillRenew {
		t.Error("a cancellation at the period's end renews")
	}

	unknown := f.addSubscription(customer, subject, "something_new", periodEnd.Unix(), nil)
	if _, err := p.Refresh(t.Context(), unknown); err == nil {
		t.Error("an unknown status mapped")
	}
}

func TestSubjectFromSubscriptionOrCustomer(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	end := time.Now().Add(time.Hour).Unix()
	tagged := f.addCustomer("customer-sub", "", 1)
	untagged := f.addCustomer("", "same@example.test", 1)

	for name, c := range map[string]struct {
		id   string
		want string
	}{
		"subscription's own":          {f.addSubscription(tagged, "subscription-sub", "active", end, nil), "subscription-sub"},
		"customer's when it has none": {f.addSubscription(tagged, "", "active", end, nil), "customer-sub"},
		"neither, never the email":    {f.addSubscription(untagged, "", "active", end, nil), ""},
		"deleted customer":            {f.addSubscription("cus_deleted", "", "active", end, nil), ""},
	} {
		ev, err := p.Refresh(t.Context(), c.id)
		if err != nil || ev.Subject != c.want {
			t.Errorf("%s: subject %q, %v; want %q", name, ev.Subject, err, c.want)
		}
	}
}

func TestCustomerFoundOrCreatedBySub(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	// Another account's customer with the same email is never matched.
	stranger := f.addCustomer("someone-else", "shared@example.test", 1)

	if _, err := p.StartCheckout(t.Context(), billing.Customer{Subject: subject, Email: "shared@example.test"}, publicURL+"/return"); err != nil {
		t.Fatal(err)
	}
	created := f.calls(http.MethodPost, "/v1/customers")
	if len(created) != 1 || created[0].Form.Get("metadata[sub]") != subject || created[0].Form.Has("email") || created[0].IdempotencyKey == "" {
		t.Fatalf("customer creation = %+v", created)
	}
	sessions := f.calls(http.MethodPost, "/v1/checkout/sessions")
	first := sessions[0].Form.Get("customer")
	if first == stranger {
		t.Fatal("checkout reused another account's customer by email")
	}
	if updates := f.calls(http.MethodPost, "/v1/customers/"+first); len(updates) != 1 || updates[0].Form.Get("email") != "shared@example.test" {
		t.Fatalf("email update = %+v", updates)
	}

	// The next checkout finds it by sub; an unchanged email is not sent again.
	if _, err := p.StartCheckout(t.Context(), billing.Customer{Subject: subject, Email: "shared@example.test"}, publicURL+"/return"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(http.MethodPost, "/v1/customers")); n != 1 {
		t.Fatalf("created %d customers for one sub", n)
	}
	if n := len(f.calls(http.MethodPost, "/v1/customers/"+first)); n != 1 {
		t.Fatalf("updated an unchanged email (%d updates)", n)
	}
	if got := f.calls(http.MethodPost, "/v1/checkout/sessions")[1].Form.Get("customer"); got != first {
		t.Fatalf("second checkout used %s, want %s", got, first)
	}
	searches := f.calls(http.MethodGet, "/v1/customers/search")
	for _, s := range searches {
		if strings.Contains(s.Form.Get("query"), "email") {
			t.Fatalf("searched by email: %q", s.Form.Get("query"))
		}
	}

	// Two customers with the sub (a race before search caught up): the
	// oldest wins. A sub with a quote is quoted, not injected.
	older := f.addCustomer("o'brien\\x", "", 5)
	f.addCustomer("o'brien\\x", "", 9)
	if _, err := p.StartCheckout(t.Context(), billing.Customer{Subject: "o'brien\\x"}, publicURL+"/return"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls(http.MethodPost, "/v1/checkout/sessions")[2].Form.Get("customer"); got != older {
		t.Fatalf("checkout used %s, want the oldest %s", got, older)
	}
	if _, err := p.StartCheckout(t.Context(), billing.Customer{Email: "x@example.test"}, publicURL+"/return"); err == nil {
		t.Fatal("a customer without a sub was accepted")
	}
}

func TestCheckoutParameters(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	returnURL := publicURL + "/dashboard/billing/return?session_id={CHECKOUT_SESSION_ID}"
	checkout, err := p.StartCheckout(t.Context(), billing.Customer{UserID: "user-1", Subject: subject, Email: "a@example.test"}, returnURL)
	if err != nil || !strings.HasPrefix(checkout.ID, "cs_test") || !strings.Contains(checkout.ClientSecret, "_secret_") {
		t.Fatalf("StartCheckout = %+v, %v", checkout, err)
	}
	form := f.calls(http.MethodPost, "/v1/checkout/sessions")[0].Form
	for key, want := range map[string]string{
		"mode": "subscription", "ui_mode": "embedded_page", "line_items[0][price]": priceID, "line_items[0][quantity]": "1",
		"client_reference_id": subject, "metadata[sub]": subject, "subscription_data[metadata][sub]": subject,
		"return_url": returnURL, "redirect_on_completion": "if_required",
	} {
		if got := form.Get(key); got != want {
			t.Errorf("checkout %s = %q, want %q", key, got, want)
		}
	}
	for key := range form {
		if strings.Contains(key, "email") || key == "success_url" || key == "cancel_url" {
			t.Errorf("checkout sends %s", key)
		}
	}
	if !strings.HasPrefix(form.Get("customer"), "cus_") {
		t.Errorf("checkout customer = %q", form.Get("customer"))
	}
	if p.PublishableKey() != publishableKey {
		t.Errorf("PublishableKey = %q", p.PublishableKey())
	}
	if _, err := p.CheckoutURL(t.Context(), billing.Customer{Subject: subject}); !errors.Is(err, billing.ErrNotSupported) {
		t.Errorf("CheckoutURL = %v, want ErrNotSupported (checkout is embedded)", err)
	}
}

func TestCompleteCheckout(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	me := billing.Customer{Subject: subject}
	checkout, err := p.StartCheckout(t.Context(), me, publicURL+"/return")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CompleteCheckout(t.Context(), me, checkout.ID); !errors.Is(err, billing.ErrCheckoutIncomplete) {
		t.Fatalf("an open session = %v, want ErrCheckoutIncomplete", err)
	}
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	subID := f.completeSession(checkout.ID, periodEnd.Unix())

	someoneElse := billing.Customer{Subject: "another-sub"}
	for name, id := range map[string]string{"another's": checkout.ID, "unknown": "cs_test_missing", "not a session": "sub_1"} {
		if _, err := p.CompleteCheckout(t.Context(), someoneElse, id); !errors.Is(err, billing.ErrCheckoutForeign) {
			t.Errorf("CompleteCheckout of %s session = %v, want ErrCheckoutForeign", name, err)
		}
	}
	ev, err := p.CompleteCheckout(t.Context(), me, checkout.ID)
	if err != nil || ev.ExternalID != subID || ev.Subject != subject || ev.Status != billing.StatusActive || !ev.WillRenew ||
		ev.ExpiresAt == nil || !ev.ExpiresAt.Equal(periodEnd) {
		t.Fatalf("CompleteCheckout = %+v, %v", ev, err)
	}

	// A session whose reference names someone else is refused even if its
	// customer is mine.
	f.mu.Lock()
	f.sessions[checkout.ID]["client_reference_id"] = "another-sub"
	f.mu.Unlock()
	if _, err := p.CompleteCheckout(t.Context(), me, checkout.ID); !errors.Is(err, billing.ErrCheckoutForeign) {
		t.Fatalf("a session referencing another sub = %v", err)
	}
}

func TestPortal(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	customer := f.addCustomer(subject, "", 1)
	link, err := p.PortalURL(t.Context(), billing.Customer{Subject: subject})
	if err != nil || !strings.HasPrefix(link, "https://billing.stripe.com/") {
		t.Fatalf("PortalURL = %q, %v", link, err)
	}
	form := f.calls(http.MethodPost, "/v1/billing_portal/sessions")[0].Form
	if form.Get("customer") != customer || form.Get("return_url") != publicURL+"/dashboard" {
		t.Fatalf("portal session = %v", form)
	}
}

func TestRefreshAndCancel(t *testing.T) {
	f := newFakeStripe(t)
	p := f.provider()
	customer := f.addCustomer(subject, "", 1)
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	subID := f.addSubscription(customer, subject, "active", periodEnd.Unix(), func(s map[string]any) { s["livemode"] = true })

	ev, err := p.Refresh(t.Context(), subID)
	if err != nil || !ev.WillRenew || ev.Environment != billing.EnvironmentProduction {
		t.Fatalf("Refresh = %+v, %v", ev, err)
	}
	if err := p.Cancel(t.Context(), subID); err != nil {
		t.Fatal(err)
	}
	updates := f.calls(http.MethodPost, "/v1/subscriptions/"+subID)
	if len(updates) != 1 || updates[0].Form.Get("cancel_at_period_end") != "true" || len(updates[0].Form) != 1 {
		t.Fatalf("cancel = %+v", updates)
	}
	if ev, _ := p.Refresh(t.Context(), subID); ev.WillRenew || ev.Status != billing.StatusActive {
		t.Fatalf("after Cancel: %+v", ev)
	}
	// Cancelling again, or cancelling an ended subscription, asks nothing
	// of Stripe and succeeds.
	ended := f.addSubscription(customer, subject, "canceled", periodEnd.Unix(), nil)
	for _, id := range []string{subID, ended} {
		if err := p.Cancel(t.Context(), id); err != nil {
			t.Fatalf("Cancel %s: %v", id, err)
		}
	}
	if n := len(f.calls(http.MethodPost, "/v1/subscriptions/"+subID)) + len(f.calls(http.MethodPost, "/v1/subscriptions/"+ended)); n != 1 {
		t.Fatalf("%d cancellation calls, want 1", n)
	}
	if _, err := p.Refresh(t.Context(), "sub_missing"); err == nil {
		t.Fatal("Refresh of a missing subscription succeeded")
	}
	if err := p.Cancel(t.Context(), "sub_missing"); err == nil {
		t.Fatal("Cancel of a missing subscription succeeded")
	}
}

func TestAPIErrorsAndKey(t *testing.T) {
	f := newFakeStripe(t)
	p := stripe.New(stripe.Config{SecretKey: "sk_test_wrong", PriceID: priceID, APIURL: f.srv.URL})
	_, err := p.Refresh(t.Context(), "sub_1")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "sk_test_wrong") {
		t.Fatalf("Refresh with a bad key = %v", err)
	}
}
