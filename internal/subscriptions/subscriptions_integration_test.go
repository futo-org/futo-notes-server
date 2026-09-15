package subscriptions_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"
	"uuid"

	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/polar"
	"futo-notes-server/internal/subscriptions"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestSubscriptionsOnPostgres runs the record over the engine the hosted
// service deploys on, where the account key is a uuid column, the timestamps
// are timestamptz rather than SQLite's TEXT, and the delivery is stored as
// jsonb.
func TestSubscriptionsOnPostgres(t *testing.T) {
	databaseURL := os.Getenv("SUBSCRIPTIONS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SUBSCRIPTIONS_TEST_DATABASE_URL is not set")
	}
	rawDatabase, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := databasepkg.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := databasepkg.Wrap(rawDatabase, dialect)
	defer database.Close()
	ctx := context.Background()
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}

	userID := uuid.NewV7().String()
	sub := "subscriptions-test-" + userID
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'subscriptions test', $3)`, userID, sub, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)

	applyOnPostgres := func(t *testing.T, eventType, status string, at time.Time) {
		t.Helper()
		event := polar.Event{
			Type: eventType,
			Subscription: polar.Subscription{
				ID:                 "sub_pg_1",
				Status:             status,
				CustomerID:         "cus_pg_1",
				ExternalCustomerID: sub,
				CurrentPeriodEnd:   &periodEnd,
			},
		}
		body, err := polar.Delivery(event)
		if err != nil {
			t.Fatal(err)
		}
		matched, err := subscriptions.Apply(ctx, database, event, body, at)
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Fatalf("Apply(%s) matched no account for sub %q", eventType, sub)
		}
	}

	applyOnPostgres(t, "subscription.active", "active", noon)
	record, err := subscriptions.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Entitled(noon) {
		t.Fatalf("Entitled() = false after an active event; record = %+v", record)
	}
	if stored := storedPeriodEnd(t, database, userID); !stored.Valid || !stored.Time.Equal(periodEnd) {
		t.Fatalf("current_period_end = %v, want %v", stored, periodEnd)
	}

	applyOnPostgres(t, "subscription.past_due", "past_due", noon)
	record, err = subscriptions.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	wantGrace := noon.Add(subscriptions.GracePeriod)
	if record.GraceUntil == nil || !record.GraceUntil.Equal(wantGrace) {
		t.Fatalf("GraceUntil = %v, want %v", record.GraceUntil, wantGrace)
	}
	if !record.Entitled(noon.Add(time.Hour)) || record.Entitled(wantGrace.Add(time.Second)) {
		t.Fatalf("Grace Period is not honored on Postgres; record = %+v", record)
	}

	// jsonb round-trips the delivery as the same event, not the same bytes.
	var stored struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(record.LastEvent), &stored); err != nil {
		t.Fatalf("last_event did not come back as JSON from jsonb: %v", err)
	}
	if stored.Type != "subscription.past_due" {
		t.Errorf("last_event type = %q, want subscription.past_due", stored.Type)
	}

	applyOnPostgres(t, "subscription.revoked", "canceled", noon.Add(time.Hour))
	record, err = subscriptions.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if record.GraceUntil != nil || record.Entitled(noon.Add(time.Hour)) {
		t.Fatalf("a revoked subscription is still entitled on Postgres; record = %+v", record)
	}
}

// TestPolarCustomerOnPostgres covers the customer half of the row on the
// engine the hosted service deploys on, where polar_customer_id sits beside
// a nullable jsonb last_event and the upsert has to leave a live
// subscription alone.
func TestPolarCustomerOnPostgres(t *testing.T) {
	databaseURL := os.Getenv("SUBSCRIPTIONS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SUBSCRIPTIONS_TEST_DATABASE_URL is not set")
	}
	rawDatabase, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := databasepkg.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := databasepkg.Wrap(rawDatabase, dialect)
	defer database.Close()
	ctx := context.Background()
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}

	userID := uuid.NewV7().String()
	sub := "customer-test-" + userID
	email := userID + "@example.invalid"
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'customer test', $3)`, userID, sub, email); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)

	customer, err := subscriptions.CustomerOf(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if customer.ID != "" || customer.ExternalID != sub || customer.Email != email {
		t.Fatalf("CustomerOf() before any checkout = %+v", customer)
	}

	if err := subscriptions.SaveCustomerID(ctx, database, userID, "cus_pg_1", noon); err != nil {
		t.Fatal(err)
	}
	// A row with no delivery yet leaves last_event as SQL NULL, which reads
	// back as the empty string the /dev card treats as "none received".
	record, err := subscriptions.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != subscriptions.StateNone || record.LastEvent != "" || record.Entitled(noon) {
		t.Fatalf("record after only a customer = %+v", record)
	}

	body, err := json.Marshal(map[string]any{
		"type": "subscription.active",
		"data": map[string]any{
			"id": "sub_pg_2", "status": "active", "customer_id": "cus_pg_1",
			"customer": map[string]any{"external_id": sub},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := polar.ParseEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := subscriptions.Apply(ctx, database, event, body, noon); err != nil || !matched {
		t.Fatalf("Apply() = %v, %v", matched, err)
	}
	if err := subscriptions.SaveCustomerID(ctx, database, userID, "cus_pg_1", noon); err != nil {
		t.Fatal(err)
	}
	record, err = subscriptions.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Entitled(noon) || record.LastEvent == "" {
		t.Fatalf("saving a customer id rolled a live subscription back: %+v", record)
	}
}
