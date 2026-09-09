package subscriptions

import (
	"context"
	"time"

	"futo-notes-server/internal/db"
)

// Customer is the account's billing identity: how Polar names it back to this
// server, and Polar's own id for it once one exists.
//
// ExternalID is the account's OIDC sub. It is what the checkout registers as
// Polar's external customer id and what the webhook matches an incoming
// subscription against, so the two ends of the round trip agree.
type Customer struct {
	ExternalID string
	Email      string

	// ID is Polar's customer id, empty until a checkout or portal request
	// has created one.
	ID string
}

// CustomerOf reads what the checkout and portal routes need about an account
// in one query: its identity at Polar, and whether Polar already knows it.
func CustomerOf(ctx context.Context, database *db.DB, userID string) (Customer, error) {
	var customer Customer
	err := database.QueryRowContext(ctx,
		`SELECT users.sub, users.email, coalesce(subscriptions.polar_customer_id, '')
		 FROM users LEFT JOIN subscriptions ON subscriptions.user_id = users.id
		 WHERE users.id = $1`, userID).
		Scan(&customer.ExternalID, &customer.Email, &customer.ID)
	if err != nil {
		return Customer{}, err
	}
	return customer, nil
}

// SaveCustomerID records the Polar customer just created for the account.
//
// It writes only the customer id. An account that already has a subscription
// keeps its state, its period, its Grace Period, and the delivery it was last
// written from — opening the customer portal must not roll a live
// subscription back to unsubscribed.
func SaveCustomerID(ctx context.Context, database *db.DB, userID, customerID string, now time.Time) error {
	_, err := database.ExecContext(ctx,
		`INSERT INTO subscriptions (user_id, polar_customer_id, state, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id) DO UPDATE SET
		   polar_customer_id = EXCLUDED.polar_customer_id`,
		userID, customerID, StateNone, db.Timestamp(now))
	return err
}
