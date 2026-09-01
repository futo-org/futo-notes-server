package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	return &output
}

func TestServerError(t *testing.T) {
	logs := captureSlog(t)
	request := httptest.NewRequest(http.MethodPost, "/api/things?cursor=1", nil)
	response := httptest.NewRecorder()

	serverError(response, request, "loading things", errors.New("database unavailable"))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if got := response.Body.String(); got != "{\"error\":\"internal server error\"}\n" {
		t.Fatalf("body = %q", got)
	}
	for _, want := range []string{
		`level=ERROR`,
		`msg="loading things"`,
		`err="database unavailable"`,
		`method=POST`,
		`path=/api/things`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log = %q, want it to contain %q", logs.String(), want)
		}
	}
}

func TestRecoverPanic(t *testing.T) {
	logs := captureSlog(t)
	handler := recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("broken handler")
	}))
	request := httptest.NewRequest(http.MethodPut, "/api/things/1?version=2", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if got := response.Body.String(); got != "{\"error\":\"internal server error\"}\n" {
		t.Fatalf("body = %q", got)
	}
	if got := response.Header().Get("Connection"); got != "close" {
		t.Fatalf("Connection = %q, want close", got)
	}
	for _, want := range []string{
		`level=ERROR`,
		`msg=panic`,
		`err="broken handler"`,
		`method=PUT`,
		`path=/api/things/1`,
		`stack=`,
		`TestRecoverPanic`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log = %q, want it to contain %q", logs.String(), want)
		}
	}
}

func TestRecoverPanicPropagatesAbortHandler(t *testing.T) {
	handler := recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("recovered = %#v, want http.ErrAbortHandler", recovered)
	}
}

func TestRecoverPanicPassesThrough(t *testing.T) {
	handler := recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test", "untouched")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/things", nil))

	if response.Code != http.StatusCreated || response.Body.String() != "created" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Test"); got != "untouched" {
		t.Fatalf("X-Test = %q, want untouched", got)
	}
	if got := response.Header().Get("Connection"); got != "" {
		t.Fatalf("Connection = %q, want empty", got)
	}
}
