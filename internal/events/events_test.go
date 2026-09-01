package events

import (
	"testing"
	"time"
)

func TestHubCoalescesNewestVersionPerCollection(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe("user-1")
	defer hub.Unsubscribe(sub)

	hub.Publish(Notification{UserID: "user-1", CollectionID: "collection-b", CurrentVersion: 1})
	hub.Publish(Notification{UserID: "user-1", CollectionID: "collection-a", CurrentVersion: 2})
	hub.Publish(Notification{UserID: "user-1", CollectionID: "collection-a", CurrentVersion: 3})
	hub.Publish(Notification{UserID: "user-1", CollectionID: "collection-a", CurrentVersion: 1})

	select {
	case <-sub.Wake():
	case <-time.After(time.Second):
		t.Fatal("subscription was not woken")
	}
	changes, open := sub.Drain()
	if !open {
		t.Fatal("subscription unexpectedly closed")
	}
	want := []Change{
		{CollectionID: "collection-a", CurrentVersion: 3},
		{CollectionID: "collection-b", CurrentVersion: 1},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %#v, want %#v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Fatalf("changes = %#v, want %#v", changes, want)
		}
	}

	select {
	case <-sub.Wake():
		t.Fatal("coalesced notifications left more than one wake signal")
	default:
	}
}

func TestHubIsolatesUsers(t *testing.T) {
	hub := NewHub()
	first := hub.Subscribe("user-1")
	second := hub.Subscribe("user-2")
	defer hub.Unsubscribe(first)
	defer hub.Unsubscribe(second)

	hub.Publish(Notification{UserID: "user-1", CollectionID: "secret", CurrentVersion: 1})
	select {
	case <-first.Wake():
	case <-time.After(time.Second):
		t.Fatal("owner was not woken")
	}
	select {
	case <-second.Wake():
		t.Fatal("notification leaked to another user")
	default:
	}
}

func TestHubCloseAllEndsCurrentSubscriptionsOnly(t *testing.T) {
	hub := NewHub()
	old := hub.Subscribe("user-1")
	hub.CloseAll()

	select {
	case _, open := <-old.Wake():
		if open {
			t.Fatal("old subscription wake channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("old subscription was not closed")
	}
	if changes, open := old.Drain(); open || changes != nil {
		t.Fatalf("Drain() = %#v, %v after close", changes, open)
	}

	fresh := hub.Subscribe("user-1")
	defer hub.Unsubscribe(fresh)
	hub.Publish(Notification{UserID: "user-1", CollectionID: "collection", CurrentVersion: 4})
	select {
	case <-fresh.Wake():
	case <-time.After(time.Second):
		t.Fatal("fresh subscription did not receive notifications")
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe("user-1")
	hub.Unsubscribe(sub)
	hub.Unsubscribe(sub)
	hub.Publish(Notification{UserID: "user-1", CollectionID: "collection", CurrentVersion: 1})
}
