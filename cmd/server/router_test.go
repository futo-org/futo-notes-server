package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
)

func TestRoutesLogEachRequestWithItsID(t *testing.T) {
	logs := captureLogs(t)
	database := dbtest.SQLite(t)
	cfg := config.Config{AuthMode: config.AuthModeDev, DevUI: true, BlobDir: t.TempDir()}
	hub := events.NewHub()
	handler := routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub, events.NewPublisher(database.Dialect(), hub))

	for _, tt := range []struct {
		method, path string
		want         []string
	}{
		{http.MethodGet, "/health", []string{
			`"message":"request"`, `"status":200`, `"method":"GET"`, `"path":"/health"`, `"route":"/health"`,
		}},
		{http.MethodGet, "/api/collections/x", []string{
			`"message":"request"`, `"status":401`, `"path":"/api/collections/x"`,
		}},
		{http.MethodPost, "/dev/panic", []string{
			`"message":"panic"`, `"error":"intentional dev panic"`, `"message":"request"`, `"status":500`,
		}},
	} {
		logs.Reset()
		request := httptest.NewRequest(tt.method, tt.path, nil)
		request.Header.Set("User-Agent", "router-test")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		id := response.Header().Get("X-Request-Id")
		if id == "" {
			t.Fatalf("%s %s: no X-Request-Id", tt.method, tt.path)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
			for _, field := range []string{`"request_id":"` + id + `"`, `"remote_ip":"192.0.2.1:1234"`, `"user_agent":"router-test"`} {
				if !strings.Contains(line, field) {
					t.Errorf("%s %s: log line %s lacks %s", tt.method, tt.path, line, field)
				}
			}
		}
		for _, want := range tt.want {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("%s %s: logs %s lack %s", tt.method, tt.path, logs.String(), want)
			}
		}
	}
}
