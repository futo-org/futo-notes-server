package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
)

func TestDevUIIncludesRecurringJobPanel(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/dev", nil)
	response := httptest.NewRecorder()
	dialect, err := db.ParseDialect("sqlite:/var/lib/futo/notes.db")
	if err != nil {
		t.Fatal(err)
	}
	handleDevUI(db.Wrap(nil, dialect), &blobs.FileStore{Dir: "/var/lib/futo/blobs"})(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, text := range []string{
		"/api/auth/dev/login",
		"passwordless login",
		"/dev/panic",
		"Connection",
		"the same recurring jobs used by the hourly and six-hour timers",
		"/dev/jobs/sessions",
		"/dev/jobs/reconciliation",
		"/dev/jobs/mutation-results",
		"/dev/jobs/blob-gc",
		"/dev/jobs/login-handoffs",
		"/dev/jobs/device-pairings",
		"/dev/jobs/usage",
		"/api/pairings",
		"New device: show a code",
		"Unlocked device: send the key",
		"ciphertext it cannot open",
		"/api/auth/handoff",
		"Start hand-off",
		"/api/billing",
		"/dev/plan",
		"Set quota",
		"upload near the limit",
		"/api/billing/polar/webhook",
		"/api/billing/checkout",
		"Open checkout",
		"Open portal",
		"entitlement after paying",
		"/dev/subscription",
		"Send webhook",
		"last webhook received",
		"Run now",
		"sqlite: /var/lib/futo/notes.db",
		"blobs: /var/lib/futo/blobs",
		"/dev/session",
		"app (hand-off)",
		"recovery_encrypted_vault_key",
		"clears the recovery envelope",
	} {
		if !strings.Contains(body, text) {
			t.Fatalf("dev UI does not contain %q", text)
		}
	}
}

func TestDevUIShowsTheS3BucketAndSkipsReconciliation(t *testing.T) {
	dialect, err := db.ParseDialect("sqlite:/var/lib/futo/notes.db")
	if err != nil {
		t.Fatal(err)
	}
	database := db.Wrap(nil, dialect)
	store := blobs.NewS3Store(blobs.S3Config{Endpoint: "http://garage.invalid:3900", Bucket: "notes-blobs", Region: "us-east-1"})

	response := httptest.NewRecorder()
	handleDevUI(database, store)(response, httptest.NewRequest(http.MethodGet, "/dev", nil))
	if body := response.Body.String(); !strings.Contains(body, "blobs: S3 http://garage.invalid:3900, bucket notes-blobs") {
		t.Fatalf("dev UI does not name the bucket")
	}

	// Reconciliation walks BLOB_DIR, so on S3 the button reports that
	// instead of touching the database.
	mux := http.NewServeMux()
	registerDevJobHandlers(mux, database, store)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/dev/jobs/reconciliation", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "does not run in S3 mode") {
		t.Fatalf("reconciliation = %d %q", response.Code, response.Body.String())
	}
}

func TestHandleDevJobReturnsCountsAndErrors(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		handler := handleDevJob(func(context.Context) (map[string]int, error) {
			return map[string]int{"reaped": 3}, nil
		})
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodPost, "/dev/jobs/sessions", nil))
		if response.Code != http.StatusOK || response.Body.String() != "{\"reaped\":3}\n" {
			t.Fatalf("response = %d %q", response.Code, response.Body.String())
		}
	})

	t.Run("error preserves partial counts", func(t *testing.T) {
		handler := handleDevJob(func(context.Context) (map[string]int, error) {
			return map[string]int{"rows_purged": 2}, errors.New("disk unavailable")
		})
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodPost, "/dev/jobs/blob-gc", nil))
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", response.Code)
		}
		body := response.Body.String()
		if !strings.Contains(body, `"rows_purged":2`) || !strings.Contains(body, `"error":"disk unavailable"`) {
			t.Fatalf("body = %q", body)
		}
	})
}
