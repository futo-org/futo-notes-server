package polar_test

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/polar"
)

// A Standard Webhooks secret: the whsec_ prefix over a base64 key.
var testSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("polar-webhook-key"))

// signedAt is the moment the fixtures below were signed at.
var signedAt = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

const testBody = `{"type":"subscription.active","data":{"id":"sub_1"}}`

// The expected signature was computed outside this package from the Standard
// Webhooks definition — base64(HMAC-SHA256(key, "<id>.<timestamp>.<body>")) —
// over key "polar-webhook-key", id "msg_1", timestamp 1788955200, and testBody.
// It is a fixed literal so the test can disagree with the implementation.
const knownGoodSignature = "v1,Avvi+7RIYyJOByUsE9KwkDhkkxVoUbQSbS8slafy98E="

func signedHeader(t *testing.T, body string) http.Header {
	t.Helper()
	header, err := polar.Sign(testSecret, "msg_1", signedAt, []byte(body))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	return header
}

func TestVerifyAcceptsASignatureFromTheSharedSecret(t *testing.T) {
	header := signedHeader(t, testBody)

	if got := header.Get("webhook-signature"); got != knownGoodSignature {
		t.Fatalf("webhook-signature = %q, want the independently computed %q", got, knownGoodSignature)
	}
	if err := polar.Verify(testSecret, header, []byte(testBody), signedAt); err != nil {
		t.Fatalf("Verify() error = %v, want a signature this secret produced to be accepted", err)
	}
}

func TestVerifyRejectsTamperingAndTheWrongSecret(t *testing.T) {
	other := "whsec_" + base64.StdEncoding.EncodeToString([]byte("someone-elses-key"))

	tests := []struct {
		name   string
		secret string
		header func(http.Header)
		body   string
	}{
		{name: "a body edited after signing", secret: testSecret,
			body: `{"type":"subscription.active","data":{"id":"sub_2"}}`},
		{name: "a signature from another secret", secret: other},
		{name: "a replaced signature", secret: testSecret,
			header: func(h http.Header) {
				h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(make([]byte, 32)))
			}},
		{name: "a swapped message id", secret: testSecret,
			header: func(h http.Header) { h.Set("webhook-id", "msg_2") }},
		{name: "a swapped timestamp", secret: testSecret,
			header: func(h http.Header) { h.Set("webhook-timestamp", "1788955201") }},
		{name: "no signature header", secret: testSecret,
			header: func(h http.Header) { h.Del("webhook-signature") }},
		{name: "no id header", secret: testSecret,
			header: func(h http.Header) { h.Del("webhook-id") }},
		{name: "no timestamp header", secret: testSecret,
			header: func(h http.Header) { h.Del("webhook-timestamp") }},
		{name: "an unparseable timestamp", secret: testSecret,
			header: func(h http.Header) { h.Set("webhook-timestamp", "yesterday") }},
		{name: "an unknown signature version", secret: testSecret,
			header: func(h http.Header) {
				h.Set("webhook-signature", strings.Replace(h.Get("webhook-signature"), "v1,", "v9,", 1))
			}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := testBody
			if test.body != "" {
				body = test.body
			}
			header := signedHeader(t, testBody)
			if test.header != nil {
				test.header(header)
			}
			if err := polar.Verify(test.secret, header, []byte(body), signedAt); err == nil {
				t.Fatal("Verify() error = nil, want the delivery rejected")
			}
		})
	}
}

func TestVerifyRejectsADeliveryReplayedOutsideTheTolerance(t *testing.T) {
	header := signedHeader(t, testBody)

	if err := polar.Verify(testSecret, header, []byte(testBody), signedAt.Add(4*time.Minute)); err != nil {
		t.Fatalf("Verify() four minutes late error = %v, want it inside the tolerance", err)
	}
	if err := polar.Verify(testSecret, header, []byte(testBody), signedAt.Add(6*time.Minute)); err == nil {
		t.Fatal("Verify() six minutes late error = nil, want a captured delivery to stop replaying")
	}
	if err := polar.Verify(testSecret, header, []byte(testBody), signedAt.Add(-6*time.Minute)); err == nil {
		t.Fatal("Verify() six minutes early error = nil, want a far-future timestamp rejected")
	}
}

func TestVerifyAcceptsOneOfSeveralSignaturesDuringKeyRotation(t *testing.T) {
	header := signedHeader(t, testBody)
	stale := "v1," + base64.StdEncoding.EncodeToString(make([]byte, 32))
	header.Set("webhook-signature", stale+" "+header.Get("webhook-signature"))

	if err := polar.Verify(testSecret, header, []byte(testBody), signedAt); err != nil {
		t.Fatalf("Verify() error = %v, want the matching entry of a rotated pair accepted", err)
	}
}

func TestSignRejectsASecretThatIsNotBase64(t *testing.T) {
	if _, err := polar.Sign("whsec_not base64 at all", "msg_1", signedAt, []byte(testBody)); err == nil {
		t.Fatal("Sign() error = nil, want a malformed POLAR_WEBHOOK_SECRET reported")
	}
	if err := polar.Verify("whsec_not base64 at all", signedHeader(t, testBody), []byte(testBody), signedAt); err == nil {
		t.Fatal("Verify() error = nil, want a malformed POLAR_WEBHOOK_SECRET reported")
	}
}

// TestVerifyOverTheWire checks a delivery the way it actually arrives: an
// http.Request built by the net/http client and read by a real server, so
// header canonicalization sits between Sign and Verify rather than being
// skipped by handing the same http.Header to both.
func TestVerifyOverTheWire(t *testing.T) {
	var verifyErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		verifyErr = polar.Verify(testSecret, r.Header, body, signedAt)
		if verifyErr != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	send := func(t *testing.T, header http.Header, body string) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range header {
			request.Header[name] = values
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}

	if status := send(t, signedHeader(t, testBody), testBody); status != http.StatusOK {
		t.Fatalf("a signed delivery over the wire = %d, want 200 (%v)", status, verifyErr)
	}

	edited := `{"type":"subscription.revoked","data":{"id":"sub_1"}}`
	if status := send(t, signedHeader(t, testBody), edited); status != http.StatusUnauthorized {
		t.Fatalf("a delivery edited after signing = %d, want 401", status)
	}
}
