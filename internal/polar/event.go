package polar

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// The subscription lifecycle events this server acts on. Polar sends more
// types than these — orders, benefit grants, checkouts — and the route
// acknowledges those without reading them, so a webhook endpoint subscribed
// to everything does not produce retries.
var subscriptionEventTypes = []string{
	"subscription.created",
	"subscription.active",
	"subscription.updated",
	"subscription.past_due",
	"subscription.canceled",
	"subscription.revoked",
}

// Subscription is the part of Polar's subscription object this server keeps.
//
// Status is Polar's own vocabulary — incomplete, incomplete_expired,
// trialing, active, past_due, canceled, unpaid, paused — and it is what
// decides Entitlement, not the event type. A subscription canceled to run
// out at the end of its period keeps status "active" until Polar revokes it,
// which is what lets a person who cancels keep writing until the period they
// paid for ends.
type Subscription struct {
	ID                 string
	Status             string
	CustomerID         string
	ExternalCustomerID string
	CurrentPeriodEnd   *time.Time
}

// Event is one webhook delivery.
type Event struct {
	Type         string
	Subscription Subscription
}

// IsSubscription reports whether this event moves a subscription's state.
func (e Event) IsSubscription() bool {
	return slices.Contains(subscriptionEventTypes, e.Type)
}

// wireEvent is the delivery as Polar sends it: the subscription is the
// event's data, and the customer is embedded in it.
type wireEvent struct {
	Type string `json:"type"`
	Data struct {
		ID               string     `json:"id"`
		Status           string     `json:"status"`
		CustomerID       string     `json:"customer_id"`
		CurrentPeriodEnd *time.Time `json:"current_period_end"`
		Customer         struct {
			ExternalID string `json:"external_id"`
		} `json:"customer"`
	} `json:"data"`
}

// ParseEvent reads a delivery body. Verify it first: this does no checking
// of where the bytes came from.
func ParseEvent(body []byte) (Event, error) {
	var wire wireEvent
	if err := json.Unmarshal(body, &wire); err != nil {
		return Event{}, fmt.Errorf("decoding webhook body: %w", err)
	}
	return Event{
		Type: wire.Type,
		Subscription: Subscription{
			ID:                 wire.Data.ID,
			Status:             wire.Data.Status,
			CustomerID:         wire.Data.CustomerID,
			ExternalCustomerID: wire.Data.Customer.ExternalID,
			CurrentPeriodEnd:   wire.Data.CurrentPeriodEnd,
		},
	}, nil
}
