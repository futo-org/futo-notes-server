package events

import (
	"context"
	"testing"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
)

func TestListenerTransactionalDeliveryAndReconnect(t *testing.T) {
	databaseURL := dbtest.PostgresURL(t, "EVENTS_TEST_DATABASE_URL")
	database := dbtest.Postgres(t, "EVENTS_TEST_DATABASE_URL")

	hub := NewHub()
	ctx, cancel := context.WithCancel(t.Context())
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

	sub := hub.Subscribe("events-user", "events-session")
	defer hub.Unsubscribe(sub)
	if _, err := database.ExecContext(t.Context(), `SELECT pg_notify($1, 'not-json')`, Channel); err != nil {
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

	tx, err := database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := (PostgresPublisher{}).Publish(t.Context(), tx, "events-user", "collection-1", 8); err != nil {
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

	if _, err := database.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
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
	duringBackoff := hub.Subscribe("events-user", "events-session")
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
	recovered := hub.Subscribe("events-user", "events-session")
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

func TestListenerEndsTheStreamsASessionEndNames(t *testing.T) {
	databaseURL := dbtest.PostgresURL(t, "EVENTS_TEST_DATABASE_URL")
	database := dbtest.Postgres(t, "EVENTS_TEST_DATABASE_URL")

	hub := NewHub()
	ctx, cancel := context.WithCancel(t.Context())
	listenerDone := make(chan struct{})
	appName := "futo-notes-events-session-ends"
	go func() {
		listen(ctx, databaseURL, hub, appName)
		close(listenerDone)
	}()
	defer func() {
		cancel()
		<-listenerDone
	}()
	waitForListener(t, database, appName)

	revoked := hub.Subscribe("ends-user", "revoked-session")
	kept := hub.Subscribe("ends-user", "kept-session")
	other := hub.Subscribe("other-user", "revoked-session")
	defer hub.Unsubscribe(kept)
	defer hub.Unsubscribe(other)
	notifyEnd := func(payload string) {
		t.Helper()
		if _, err := database.ExecContext(t.Context(), `SELECT pg_notify($1, $2)`, SessionsChannel, payload); err != nil {
			t.Fatal(err)
		}
	}

	notifyEnd(`not-json`)
	notifyEnd(`{"userId":"ends-user","sessionId":"revoked-session"}`)
	waitForClose(t, revoked)
	for _, sub := range []*Subscription{kept, other} {
		if _, open := sub.Drain(); !open {
			t.Fatal("a session end closed another session's or user's stream")
		}
	}

	notifyEnd(`{"userId":"ends-user"}`)
	waitForClose(t, kept)
	if _, open := other.Drain(); !open {
		t.Fatal("a user's end closed another user's stream")
	}
}

func waitForClose(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case _, open := <-sub.Wake():
		if open {
			t.Fatal("subscription woke rather than closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscription was not closed")
	}
}

func waitForListener(t *testing.T, database *db.DB, appName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ready bool
		err := database.QueryRowContext(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE application_name = $1 AND query = $2
		)`, appName, "LISTEN "+SessionsChannel).Scan(&ready)
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

func publishAndCommit(t *testing.T, database *db.DB, notification Notification) {
	t.Helper()
	tx, err := database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Discard()
	if err := (PostgresPublisher{}).Publish(t.Context(), tx, notification.UserID, notification.CollectionID, notification.CurrentVersion); err != nil {
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
