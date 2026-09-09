package polar_test

import (
	"testing"
	"time"

	"futo-notes-server/internal/polar"
)

// A subscription.active delivery, trimmed to the fields this server reads.
// The shape is Polar's: the subscription is the event's data, and the
// customer is embedded in it carrying the external id we set at checkout.
const activeDelivery = `{
  "type": "subscription.active",
  "data": {
    "id": "1a4f0f6e-0000-4000-8000-000000000001",
    "status": "active",
    "current_period_end": "2026-10-09T12:00:00Z",
    "customer_id": "1a4f0f6e-0000-4000-8000-000000000002",
    "product_id": "1a4f0f6e-0000-4000-8000-000000000003",
    "customer": {
      "id": "1a4f0f6e-0000-4000-8000-000000000002",
      "external_id": "zitadel-sub-1",
      "email": "person@futo.org"
    }
  }
}`

func TestParseEventReadsTheSubscriptionAndItsCustomer(t *testing.T) {
	event, err := polar.ParseEvent([]byte(activeDelivery))
	if err != nil {
		t.Fatalf("ParseEvent() error = %v", err)
	}

	if event.Type != "subscription.active" {
		t.Errorf("Type = %q", event.Type)
	}
	if !event.IsSubscription() {
		t.Error("IsSubscription() = false for subscription.active")
	}
	if event.Subscription.ID != "1a4f0f6e-0000-4000-8000-000000000001" {
		t.Errorf("Subscription.ID = %q", event.Subscription.ID)
	}
	if event.Subscription.Status != "active" {
		t.Errorf("Subscription.Status = %q", event.Subscription.Status)
	}
	if event.Subscription.CustomerID != "1a4f0f6e-0000-4000-8000-000000000002" {
		t.Errorf("Subscription.CustomerID = %q", event.Subscription.CustomerID)
	}
	if event.Subscription.ExternalCustomerID != "zitadel-sub-1" {
		t.Errorf("Subscription.ExternalCustomerID = %q, want the account's sub", event.Subscription.ExternalCustomerID)
	}
	want := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if event.Subscription.CurrentPeriodEnd == nil || !event.Subscription.CurrentPeriodEnd.Equal(want) {
		t.Errorf("Subscription.CurrentPeriodEnd = %v, want %v", event.Subscription.CurrentPeriodEnd, want)
	}
}

func TestParseEventRecognisesTheSubscriptionLifecycle(t *testing.T) {
	subscription := []string{
		"subscription.created", "subscription.active", "subscription.updated",
		"subscription.past_due", "subscription.canceled", "subscription.revoked",
	}
	for _, eventType := range subscription {
		event, err := polar.ParseEvent([]byte(`{"type":"` + eventType + `","data":{"status":"active"}}`))
		if err != nil {
			t.Fatalf("ParseEvent(%s) error = %v", eventType, err)
		}
		if !event.IsSubscription() {
			t.Errorf("IsSubscription() = false for %s", eventType)
		}
	}

	// Everything else is a type this server acknowledges and ignores.
	for _, eventType := range []string{"order.paid", "benefit_grant.created", "checkout.created", ""} {
		event, err := polar.ParseEvent([]byte(`{"type":"` + eventType + `","data":{}}`))
		if err != nil {
			t.Fatalf("ParseEvent(%q) error = %v", eventType, err)
		}
		if event.IsSubscription() {
			t.Errorf("IsSubscription() = true for %q", eventType)
		}
	}
}

func TestParseEventLeavesAnAbsentPeriodEndUnset(t *testing.T) {
	event, err := polar.ParseEvent([]byte(
		`{"type":"subscription.canceled","data":{"status":"canceled","current_period_end":null}}`))
	if err != nil {
		t.Fatalf("ParseEvent() error = %v", err)
	}
	if event.Subscription.CurrentPeriodEnd != nil {
		t.Errorf("CurrentPeriodEnd = %v, want unset", event.Subscription.CurrentPeriodEnd)
	}
}

func TestParseEventRejectsABodyThatIsNotAnEvent(t *testing.T) {
	if _, err := polar.ParseEvent([]byte(`not json`)); err == nil {
		t.Fatal("ParseEvent() error = nil for a body that is not JSON")
	}
}
