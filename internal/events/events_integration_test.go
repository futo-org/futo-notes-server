package events

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

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
	if err := Publish(context.Background(), tx, "events-user", "collection-1", 8); err != nil {
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

	// A subscriber opened while the listener reconnects remains useful once
	// LISTEN is active again. Publishes before then are intentionally lossy.
	fresh := hub.Subscribe("events-user")
	defer hub.Unsubscribe(fresh)
	deadline := time.Now().Add(5 * time.Second)
	for version := int64(9); time.Now().Before(deadline); version++ {
		publishAndCommit(t, database, Notification{
			UserID:         "events-user",
			CollectionID:   "collection-1",
			CurrentVersion: version,
		})
		select {
		case _, open := <-fresh.Wake():
			if !open {
				t.Fatal("fresh subscription closed during reconnect")
			}
			changes, open := fresh.Drain()
			if !open || len(changes) != 1 {
				t.Fatalf("reconnected drain = %#v, %v", changes, open)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("listener did not deliver after reconnecting")
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
	if err := Publish(context.Background(), tx, notification.UserID, notification.CollectionID, notification.CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
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
