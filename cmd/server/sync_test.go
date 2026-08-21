package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/events"
)

func TestSyncEventsReadyChangeHeadersAndListenerLoss(t *testing.T) {
	hub := events.NewHub()
	server := newSyncTestServer(hub, time.Hour)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}

	reader := bufio.NewReader(response.Body)
	if got := readSSEEvent(t, reader); got != "event: ready\ndata:\n\n" {
		t.Fatalf("ready event = %q", got)
	}

	// Another tenant's collection ID must never appear on this stream.
	hub.Publish(events.Notification{UserID: "other-user", CollectionID: "secret", CurrentVersion: 99})
	hub.Publish(events.Notification{UserID: testUser.ID, CollectionID: testCollection.ID, CurrentVersion: 42})
	want := "event: change\ndata: {\"collectionId\":\"" + testCollection.ID + "\",\"currentVersion\":42}\n\n"
	if got := readSSEEvent(t, reader); got != want {
		t.Fatalf("change event = %q, want %q", got, want)
	}

	// Listener loss closes the response rather than leaving a heartbeat-only
	// stream that looks healthy.
	hub.CloseAll()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading closed stream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream remained open after hub.CloseAll")
	}
}

func TestSyncEventsPing(t *testing.T) {
	hub := events.NewHub()
	server := newSyncTestServer(hub, 5*time.Millisecond)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	readSSEEvent(t, reader)
	if got := readSSEEvent(t, reader); got != "event: ping\ndata:\n\n" {
		t.Fatalf("ping event = %q", got)
	}
}

func newSyncTestServer(hub *events.Hub, pingInterval time.Duration) *httptest.Server {
	handler := handleSyncEventsWithInterval(hub, pingInterval)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := &auth.Session{ID: "session", User: testUser}
		ctx := context.WithValue(r.Context(), sessionCtxKey{}, session)
		handler(w, r.WithContext(ctx))
	}))
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var event strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE event: %v", err)
		}
		event.WriteString(line)
		if line == "\n" {
			return event.String()
		}
	}
}
