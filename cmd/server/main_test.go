package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/syncapi"
)

// captureLogs sends log output to the returned buffer. Requests log through
// the logger in their context, which routes() puts there from log.Logger.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := log.Logger
	t.Cleanup(func() { log.Logger = previous })

	var output bytes.Buffer
	log.Logger = zerolog.New(&output)
	return &output
}

// Golden bytes for the health document; internal/syncapi pins the rest of
// the wire format.
func TestHealthWireFormat(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   healthStatus
		want   string
	}{
		{"health ok", 200, healthStatus{Status: "ok", DB: "connected"}, `{"status":"ok","db":"connected"}`},
		{"health degraded", 503, healthStatus{Status: "degraded", DB: "unreachable"}, `{"status":"degraded","db":"unreachable"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			syncapi.WriteJSON(w, tc.status, tc.body)
			if got := w.Body.String(); got != tc.want+"\n" {
				t.Errorf("body mismatch\n got: %s\nwant: %s\n", got, tc.want)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestLoginBodyDecoding(t *testing.T) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(strings.NewReader(`{"password":"hunter2"}`)).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Password != "hunter2" {
		t.Fatalf("Password = %q, want hunter2", body.Password)
	}

	// An absent password is a 400 from the empty-string check, not a decode
	// error.
	body.Password = ""
	if err := json.NewDecoder(strings.NewReader(`{}`)).Decode(&body); err != nil {
		t.Fatalf("empty object: %v", err)
	}
	if body.Password != "" {
		t.Fatalf("Password = %q, want empty", body.Password)
	}
}

// TestServeShutdownEndsSyncStreams checks that a shutdown signal ends open
// sync event streams, so graceful shutdown does not wait out its timeout.
func TestServeShutdownEndsSyncStreams(t *testing.T) {
	hub := events.NewHub()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	stream := syncapi.HandleSyncEvents(hub)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := &auth.Session{ID: "session", User: auth.User{ID: "0195e2a1-0000-7000-8000-000000000001"}}
		stream(w, r.WithContext(syncapi.WithSession(r.Context(), session)))
	})
	served := make(chan error, 1)
	go func() { served <- serve(ctx, listener, handler, hub.CloseAll) }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: ready\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}

	shutdown()
	if err := <-served; err != nil {
		t.Fatalf("serve() = %v, want nil after shutdown", err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("reading stream after shutdown: %v", err)
	}
}
