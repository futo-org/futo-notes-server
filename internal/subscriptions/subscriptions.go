// Package subscriptions owns the account's subscription record and answers
// the Entitlement question: may this account write right now.
//
// It is the one seam between Polar and everything that gates a write. The
// record is written only by the webhook handler and read by the gate, so a
// sync request never makes an outbound call — a Polar outage cannot stop
// people syncing, and Polar's latency is never on the write path.
package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/polar"
)

// GracePeriod is how long an account stays entitled after Polar reports a
// failed payment. A lapsed card should not strand a device mid-sync.
const GracePeriod = 7 * 24 * time.Hour

// The Polar subscription statuses this package reasons about.
const (
	statusActive   = "active"
	statusTrialing = "trialing"
	statusPastDue  = "past_due"

	// StateNone is the state of an account that has never subscribed. It is
	// not one of Polar's statuses — Polar has never heard of the account —
	// but it keeps the field a string, so a client has no null to handle.
	StateNone = "none"
)

// Record is one account's subscription as the last webhook left it.
type Record struct {
	// State is Polar's subscription status as delivered, or StateNone for
	// an account that has never subscribed.
	State            string
	CurrentPeriodEnd *time.Time
	GraceUntil       *time.Time
	LastEvent        string
	UpdatedAt        time.Time
}

// Entitled reports whether the account may write. Active and trialing pay
// for it outright; a past-due account keeps writing until its Grace Period
// ends. Everything else — canceled, revoked, unpaid, incomplete, paused, and
// having never subscribed at all — does not.
func (r Record) Entitled(now time.Time) bool {
	switch r.State {
	case statusActive, statusTrialing:
		return true
	case statusPastDue:
		return r.GraceUntil != nil && now.Before(*r.GraceUntil)
	default:
		return false
	}
}

// Of reads the account's subscription record. An account with no record
// reads as StateNone rather than an error: not subscribing is the normal
// state before checkout, not a failure.
//
// This is the one read the Entitlement gate takes per request, and it is a
// primary-key lookup.
func Of(ctx context.Context, database *db.DB, userID string) (Record, error) {
	var record Record
	var currentPeriodEnd, graceUntil db.NullTime
	var updatedAt db.Time
	// last_event is null on a row a checkout created before any delivery
	// arrived, and empty is how the rest of the server already spells "no
	// webhook received yet".
	var lastEvent sql.NullString
	err := database.QueryRowContext(ctx,
		`SELECT state, current_period_end, grace_until, last_event, updated_at
		 FROM subscriptions WHERE user_id = $1`, userID).
		Scan(&record.State, &currentPeriodEnd, &graceUntil, &lastEvent, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{State: StateNone}, nil
	}
	if err != nil {
		return Record{}, err
	}
	record.CurrentPeriodEnd = nullableTime(currentPeriodEnd)
	record.GraceUntil = nullableTime(graceUntil)
	record.LastEvent = lastEvent.String
	record.UpdatedAt = updatedAt.Time
	return record, nil
}

func nullableTime(value db.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

// timestamp renders a nullable instant for storage, keeping SQL NULL as nil.
func timestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return db.Timestamp(*value)
}

// Apply records a verified subscription event against the account Polar's
// external customer id names, which is the account's sub. It reports whether
// an account matched; an event for a customer this server does not know is
// not an error, because the endpoint is shared with the rest of the
// organization's Polar traffic.
//
// Applying the same delivery twice leaves the same row: every stored field
// is derived from the event, and the Grace Period deadline is set only when
// the account enters past-due, so a replay cannot push it further out.
func Apply(ctx context.Context, database *db.DB, event polar.Event, delivery []byte, now time.Time) (bool, error) {
	if event.Subscription.ExternalCustomerID == "" {
		return false, nil
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var userID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE sub = $1`,
		event.Subscription.ExternalCustomerID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// The record is read under its own lock so two deliveries landing
	// together cannot both decide they are the first past-due event.
	var previousState string
	var previousGrace db.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT state, grace_until FROM subscriptions WHERE user_id = $1`+database.Dialect().ForUpdate(),
		userID).Scan(&previousState, &previousGrace)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	graceUntil := graceDeadline(event.Subscription.Status, previousState, nullableTime(previousGrace), now)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO subscriptions
		   (user_id, polar_customer_id, polar_subscription_id, state,
		    current_period_end, grace_until, last_event, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7`+database.Dialect().JSONCast()+`, $8)
		 ON CONFLICT (user_id) DO UPDATE SET
		   polar_customer_id = EXCLUDED.polar_customer_id,
		   polar_subscription_id = EXCLUDED.polar_subscription_id,
		   state = EXCLUDED.state,
		   current_period_end = EXCLUDED.current_period_end,
		   grace_until = EXCLUDED.grace_until,
		   last_event = EXCLUDED.last_event,
		   updated_at = EXCLUDED.updated_at`,
		userID,
		event.Subscription.CustomerID,
		event.Subscription.ID,
		event.Subscription.Status,
		timestamp(event.Subscription.CurrentPeriodEnd),
		timestamp(graceUntil),
		string(delivery),
		db.Timestamp(now),
	); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// graceDeadline decides where the Grace Period ends after this event. The
// window opens on the first event that puts the account past due and is not
// pushed out by the ones that follow, so a run of retries — or the same
// delivery arriving twice — buys no extra time. Recovering, or lapsing for
// any other reason, closes it.
func graceDeadline(status, previousState string, previousGrace *time.Time, now time.Time) *time.Time {
	if status != statusPastDue {
		return nil
	}
	if previousState == statusPastDue && previousGrace != nil {
		return previousGrace
	}
	deadline := now.Add(GracePeriod)
	return &deadline
}
