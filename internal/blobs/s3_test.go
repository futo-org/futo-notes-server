package blobs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fakeS3(t *testing.T, handler http.HandlerFunc) *S3Store {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewS3Store(S3Config{Endpoint: server.URL, Bucket: "blobs", AccessKeyID: "GK", SecretAccessKey: "secret", Region: "us-east-1"})
}

func TestS3OpenOnlyReportsAMissingKeyAsNotFound(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		notFound bool
	}{
		{"NoSuchKey", http.StatusNotFound, `<Error><Code>NoSuchKey</Code></Error>`, true},
		{"bare 404", http.StatusNotFound, "", true},
		{"access denied", http.StatusForbidden, `<Error><Code>AccessDenied</Code></Error>`, false},
		{"server error", http.StatusInternalServerError, `<Error><Code>InternalError</Code></Error>`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := fakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(test.status)
				w.Write([]byte(test.body))
			})
			_, _, err := store.Open(context.Background(), "user/blob")
			if err == nil {
				t.Fatal("Open succeeded")
			}
			if errors.Is(err, ErrNotFound) != test.notFound {
				t.Fatalf("Open = %v, want ErrNotFound %t", err, test.notFound)
			}
		})
	}
}

func TestS3OpenRefusesABodyWithNoLength(t *testing.T) {
	store := fakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		// Flushing before the body forces chunked encoding, so the
		// response carries no Content-Length.
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		w.Write([]byte("ciphertext"))
	})
	body, size, err := store.Open(context.Background(), "user/blob")
	if err == nil {
		body.Close()
		t.Fatalf("Open = size %d, want an error", size)
	}
}

func TestS3StalledServerFailsInsteadOfHanging(t *testing.T) {
	defer func(timeout time.Duration) { s3ResponseHeaderTimeout = timeout }(s3ResponseHeaderTimeout)
	s3ResponseHeaderTimeout = 100 * time.Millisecond

	release := make(chan struct{})
	store := fakeS3(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)

	start := time.Now()
	_, _, err := store.Open(context.Background(), "user/blob")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Open = %v, want a timeout", err)
	}
	// The SDK retries, so allow for its backoff, but not for a hang.
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("Open took %s", elapsed)
	}
}
