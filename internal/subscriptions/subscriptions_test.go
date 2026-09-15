package subscriptions_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/polar"
	"futo-notes-server/internal/subscriptions"
)

// newDatabase is a migrated SQLite database in a temp directory, so these
// tests run without Postgres.
func newDatabase(t *testing.T) *db.DB {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"),
		BlobDir:     filepath.Join(root, "blobs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

const accountSub = "zitadel-sub-1"

func newAccount(t *testing.T, database *db.DB) string {
	t.Helper()
	user, err := auth.UpsertUserBySub(context.Background(), database, accountSub, "person@futo.org", "Person")
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

var noon = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// periodEnd is the current period end every delivery in these tests carries.
var periodEnd = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// apply builds a Polar-shaped delivery for the account's sub and applies it,
// returning whether an account matched.
func apply(t *testing.T, database *db.DB, eventType, status, externalID string, at time.Time) bool {
	t.Helper()
	event := polar.Event{
		Type: eventType,
		Subscription: polar.Subscription{
			ID:                 "sub_1",
			Status:             status,
			CustomerID:         "cus_1",
			ExternalCustomerID: externalID,
			CurrentPeriodEnd:   &periodEnd,
		},
	}
	body, err := polar.Delivery(event)
	if err != nil {
		t.Fatal(err)
	}
	matched, err := subscriptions.Apply(context.Background(), database, event, body, at)
	if err != nil {
		t.Fatal(err)
	}
	return matched
}

// storedPeriodEnd reads current_period_end straight off the row. Issue #10
// asks for the column and Apply writes it, but nothing in the server reads it
// back, so this is what keeps the write honest.
func storedPeriodEnd(t *testing.T, database *db.DB, userID string) db.NullTime {
	t.Helper()
	var stored db.NullTime
	if err := database.QueryRowContext(context.Background(),
		`SELECT current_period_end FROM subscriptions WHERE user_id = $1`, userID).
		Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func recordOf(t *testing.T, database *db.DB, userID string) subscriptions.Record {
	t.Helper()
	record, err := subscriptions.Of(context.Background(), database, userID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAnAccountWithNoSubscriptionIsNotEntitled(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)

	record := recordOf(t, database, userID)
	if record.State != subscriptions.StateNone {
		t.Errorf("State = %q, want %q for an account that never subscribed",
			record.State, subscriptions.StateNone)
	}
	if record.Entitled(noon) {
		t.Error("Entitled() = true with no subscription record")
	}
}

func TestAnActiveOrTrialingSubscriptionIsEntitled(t *testing.T) {
	for _, status := range []string{"active", "trialing"} {
		t.Run(status, func(t *testing.T) {
			database := newDatabase(t)
			userID := newAccount(t, database)

			if !apply(t, database, "subscription.active", status, accountSub, noon) {
				t.Fatal("Apply() matched no account for the sub it was addressed to")
			}
			record := recordOf(t, database, userID)
			if record.State != status {
				t.Errorf("State = %q, want %q", record.State, status)
			}
			if !record.Entitled(noon) {
				t.Errorf("Entitled() = false for %s", status)
			}
			if stored := storedPeriodEnd(t, database, userID); !stored.Valid || !stored.Time.Equal(periodEnd) {
				t.Errorf("current_period_end = %v, want %v", stored, periodEnd)
			}
			if record.GraceUntil != nil {
				t.Errorf("GraceUntil = %v, want unset while paid up", record.GraceUntil)
			}
		})
	}
}

func TestAFailedPaymentStartsASevenDayGracePeriod(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)
	apply(t, database, "subscription.active", "active", accountSub, noon)

	failedAt := noon.Add(30 * 24 * time.Hour)
	apply(t, database, "subscription.past_due", "past_due", accountSub, failedAt)

	record := recordOf(t, database, userID)
	if record.State != "past_due" {
		t.Fatalf("State = %q, want past_due", record.State)
	}
	wantGrace := failedAt.Add(7 * 24 * time.Hour)
	if record.GraceUntil == nil || !record.GraceUntil.Equal(wantGrace) {
		t.Fatalf("GraceUntil = %v, want the failure plus seven days (%v)", record.GraceUntil, wantGrace)
	}
	if !record.Entitled(failedAt.Add(6*24*time.Hour + 23*time.Hour)) {
		t.Error("Entitled() = false one hour before the Grace Period ends")
	}
	if record.Entitled(wantGrace.Add(time.Second)) {
		t.Error("Entitled() = true after the Grace Period ended")
	}
}

func TestFurtherPastDueEventsDoNotExtendTheGracePeriod(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)
	failedAt := noon
	apply(t, database, "subscription.past_due", "past_due", accountSub, failedAt)
	first := recordOf(t, database, userID).GraceUntil

	// A retry three days later, and the same delivery replayed, both leave
	// the deadline where the first failure put it.
	apply(t, database, "subscription.past_due", "past_due", accountSub, failedAt.Add(3*24*time.Hour))
	apply(t, database, "subscription.updated", "past_due", accountSub, failedAt.Add(3*24*time.Hour))

	again := recordOf(t, database, userID).GraceUntil
	if first == nil || again == nil || !first.Equal(*again) {
		t.Fatalf("GraceUntil moved from %v to %v", first, again)
	}
}

func TestRecoveringFromAFailedPaymentClearsTheGracePeriod(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)
	apply(t, database, "subscription.past_due", "past_due", accountSub, noon)
	apply(t, database, "subscription.active", "active", accountSub, noon.Add(24*time.Hour))

	record := recordOf(t, database, userID)
	if record.GraceUntil != nil {
		t.Fatalf("GraceUntil = %v, want cleared once payment recovered", record.GraceUntil)
	}
	if !record.Entitled(noon.Add(30 * 24 * time.Hour)) {
		t.Error("Entitled() = false long after payment recovered")
	}
}

func TestACanceledOrRevokedSubscriptionIsNotEntitled(t *testing.T) {
	for _, test := range []struct{ eventType, status string }{
		{"subscription.canceled", "canceled"},
		{"subscription.revoked", "canceled"},
		{"subscription.updated", "unpaid"},
		{"subscription.created", "incomplete"},
	} {
		t.Run(test.eventType+"/"+test.status, func(t *testing.T) {
			database := newDatabase(t)
			userID := newAccount(t, database)
			apply(t, database, "subscription.active", "active", accountSub, noon)
			apply(t, database, test.eventType, test.status, accountSub, noon.Add(time.Hour))

			record := recordOf(t, database, userID)
			if record.State != test.status {
				t.Errorf("State = %q, want %q", record.State, test.status)
			}
			if record.Entitled(noon.Add(time.Hour)) {
				t.Errorf("Entitled() = true for %s", test.status)
			}
		})
	}
}

func TestAnEventForAnUnknownCustomerMatchesNoAccount(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)

	if apply(t, database, "subscription.active", "active", "somebody-else", noon) {
		t.Fatal("Apply() matched an account for an external customer id nobody has")
	}
	if recordOf(t, database, userID).State != subscriptions.StateNone {
		t.Fatal("an event for another customer wrote this account's record")
	}
}

func TestTheLastEventIsKeptAsDelivered(t *testing.T) {
	database := newDatabase(t)
	userID := newAccount(t, database)
	apply(t, database, "subscription.past_due", "past_due", accountSub, noon)

	record := recordOf(t, database, userID)
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(record.LastEvent), &event); err != nil {
		t.Fatalf("stored last event is not the delivered JSON: %v", err)
	}
	if event.Type != "subscription.past_due" {
		t.Errorf("last event type = %q", event.Type)
	}
	if !record.UpdatedAt.Equal(noon) {
		t.Errorf("UpdatedAt = %v, want %v", record.UpdatedAt, noon)
	}
}
