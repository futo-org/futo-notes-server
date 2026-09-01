package events

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"futo-notes-server/internal/db"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestListenerTransactionalDeliveryAndReconnect(t *testing.T) {
	databaseURL := os.Getenv("EVENTS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("EVENTS_TEST_DATABASE_URL is not set")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}

	hub := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	listenerDone := make(chan struct{})
	appName := "futo-notes-events-integration"
	go func() {
		listen(ctx, databaseURL, hub, appName)
		close(listenerDone)
	}()
	defer func() {
		cancel()
		select {
		case <-listenerDone:
		case <-time.After(2 * time.Second):
			t.Error("listener did not stop after cancellation")
		}
	}()
	waitForListener(t, database, appName)

	sub := hub.Subscribe("events-user")
	defer hub.Unsubscribe(sub)
	if _, err := database.Exec(`SELECT pg_notify($1, 'not-json')`, Channel); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Wake():
		t.Fatal("invalid notification payload was delivered")
	case <-time.After(100 * time.Millisecond):
	}
	publishAndCommit(t, database, Notification{
		UserID:         "events-user",
		CollectionID:   "collection-1",
		CurrentVersion: 7,
	})
	change := waitForChange(t, sub, 2*time.Second)
	if change.CollectionID != "collection-1" || change.CurrentVersion != 7 {
		t.Fatalf("change = %#v", change)
	}

	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := db.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := (PostgresPublisher{}).Publish(context.Background(), db.WrapTx(tx, dialect), "events-user", "collection-1", 8); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Wake():
		t.Fatal("rolled-back notification was delivered")
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := database.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND pid <> pg_backend_pid()`, appName); err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-sub.Wake():
		if open {
			t.Fatal("listener loss woke rather than closed the subscription")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener loss did not close the subscription")
	}

	// A stream opened during the reconnect backoff was deaf to notifications
	// committed before LISTEN came back. It must close at reconnect so the
	// client pulls again instead of trusting that gap.
	duringBackoff := hub.Subscribe("events-user")
	defer hub.Unsubscribe(duringBackoff)
	publishAndCommit(t, database, Notification{
		UserID:         "events-user",
		CollectionID:   "collection-1",
		CurrentVersion: 9,
	})
	select {
	case <-duringBackoff.Wake():
		t.Fatal("backoff subscription received a notification without a listener")
	case <-time.After(100 * time.Millisecond):
	}
	waitForListener(t, database, appName)
	select {
	case _, open := <-duringBackoff.Wake():
		if open {
			t.Fatal("backoff subscription woke rather than closed at reconnect")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backoff subscription survived listener reconnect")
	}

	// A stream opened after LISTEN is restored receives later notifications.
	recovered := hub.Subscribe("events-user")
	defer hub.Unsubscribe(recovered)
	publishAndCommit(t, database, Notification{
		UserID:         "events-user",
		CollectionID:   "collection-1",
		CurrentVersion: 10,
	})
	change = waitForChange(t, recovered, 2*time.Second)
	if change.CollectionID != "collection-1" || change.CurrentVersion != 10 {
		t.Fatalf("recovered change = %#v", change)
	}
}

func waitForListener(t *testing.T, database *sql.DB, appName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ready bool
		err := database.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE application_name = $1 AND query = $2
		)`, appName, "LISTEN "+Channel).Scan(&ready)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listener did not connect")
}

func publishAndCommit(t *testing.T, database *sql.DB, notification Notification) {
	t.Helper()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	dialect, err := db.ParseDialect(os.Getenv("EVENTS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := db.WrapTx(tx, dialect)
	if err := (PostgresPublisher{}).Publish(context.Background(), wrapped, notification.UserID, notification.CollectionID, notification.CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Commit(); err != nil {
		t.Fatal(err)
	}
}

func waitForChange(t *testing.T, sub *Subscription, timeout time.Duration) Change {
	t.Helper()
	select {
	case _, open := <-sub.Wake():
		if !open {
			t.Fatal("subscription closed")
		}
		changes, open := sub.Drain()
		if !open || len(changes) != 1 {
			t.Fatalf("Drain() = %#v, %v", changes, open)
		}
		return changes[0]
	case <-time.After(timeout):
		t.Fatal("timed out waiting for change")
		return Change{}
	}
}
