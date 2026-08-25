// Package events publishes and fans out lossy collection-change notifications.
package events

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"futo-notes-server/internal/db"
)

const Channel = "notes_changes"

// Notification is the payload sent through Postgres. UserID is used only for
// routing and is omitted from the client-facing SSE event by the HTTP handler.
type Notification struct {
	UserID         string `json:"userId"`
	CollectionID   string `json:"collectionId"`
	CurrentVersion int64  `json:"currentVersion"`
}

// Change is the tenant-safe part of a Notification delivered to a subscriber.
type Change struct {
	CollectionID   string `json:"collectionId"`
	CurrentVersion int64  `json:"currentVersion"`
}

// Publisher queues a sync doorbell in the mutation transaction. Both
// implementations deliver only after the corresponding work commits.
type Publisher interface {
	Publish(context.Context, *db.Tx, string, string, int64) error
}

func NewPublisher(dialect db.Dialect, hub *Hub) Publisher {
	if dialect.Engine() == db.SQLite {
		return SQLitePublisher{Hub: hub}
	}
	return PostgresPublisher{}
}

type PostgresPublisher struct{}

func (PostgresPublisher) Publish(ctx context.Context, tx *db.Tx, userID, collectionID string, currentVersion int64) error {
	payload, err := json.Marshal(Notification{
		UserID:         userID,
		CollectionID:   collectionID,
		CurrentVersion: currentVersion,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `SELECT pg_notify('`+Channel+`', $1)`, string(payload))
	return err
}

type SQLitePublisher struct{ Hub *Hub }

func (p SQLitePublisher) Publish(_ context.Context, tx *db.Tx, userID, collectionID string, currentVersion int64) error {
	notification := Notification{UserID: userID, CollectionID: collectionID, CurrentVersion: currentVersion}
	tx.AfterCommit(func() { p.Hub.Publish(notification) })
	return nil
}

// Hub routes notifications to subscribers for the owning user.
type Hub struct {
	mu          sync.Mutex
	subscribers map[string]map[*Subscription]struct{}
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[string]map[*Subscription]struct{})}
}

// Subscription coalesces pending changes by collection. The wake channel has
// capacity one because it signals state, rather than carrying every event.
type Subscription struct {
	mu      sync.Mutex
	userID  string
	pending map[string]int64
	wake    chan struct{}
	closed  bool
}

func (h *Hub) Subscribe(userID string) *Subscription {
	sub := &Subscription{
		userID:  userID,
		pending: make(map[string]int64),
		wake:    make(chan struct{}, 1),
	}
	h.mu.Lock()
	if h.subscribers[userID] == nil {
		h.subscribers[userID] = make(map[*Subscription]struct{})
	}
	h.subscribers[userID][sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

// Unsubscribe is idempotent.
func (h *Hub) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	if subscribers := h.subscribers[sub.userID]; subscribers != nil {
		delete(subscribers, sub)
		if len(subscribers) == 0 {
			delete(h.subscribers, sub.userID)
		}
	}
	h.mu.Unlock()
	sub.close()
}

// Publish fans a notification out only to subscribers for its owning user.
func (h *Hub) Publish(notification Notification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subscribers[notification.UserID] {
		sub.enqueue(notification.CollectionID, notification.CurrentVersion)
	}
}

// CloseAll terminates all current subscriptions. The listener calls it again
// after reconnecting so subscriptions opened during backoff cannot survive a
// notification gap.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	all := h.subscribers
	h.subscribers = make(map[string]map[*Subscription]struct{})
	h.mu.Unlock()

	for _, subscribers := range all {
		for sub := range subscribers {
			sub.close()
		}
	}
}

func (s *Subscription) enqueue(collectionID string, currentVersion int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if previous, ok := s.pending[collectionID]; !ok || currentVersion > previous {
		s.pending[collectionID] = currentVersion
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Subscription) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.wake)
}

// Wake is signaled when pending state changes and closed when the subscription
// must end.
func (s *Subscription) Wake() <-chan struct{} { return s.wake }

// Drain snapshots and clears the pending changes. The bool is false after the
// listener or subscriber has closed, in which case the caller should exit.
func (s *Subscription) Drain() ([]Change, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	changes := make([]Change, 0, len(s.pending))
	for collectionID, currentVersion := range s.pending {
		changes = append(changes, Change{CollectionID: collectionID, CurrentVersion: currentVersion})
	}
	clear(s.pending)
	sort.Slice(changes, func(i, j int) bool { return changes[i].CollectionID < changes[j].CollectionID })
	return changes, true
}
