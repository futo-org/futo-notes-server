package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDevUIIncludesRecurringJobPanel(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/dev", nil)
	response := httptest.NewRecorder()
	handleDevUI(response, request)

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
		"Run now",
	} {
		if !strings.Contains(body, text) {
			t.Fatalf("dev UI does not contain %q", text)
		}
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
