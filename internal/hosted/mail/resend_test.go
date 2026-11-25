package mail_test

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"futo-notes-server/internal/hosted/mail"
	"futo-notes-server/internal/mockresend"
)

var testMessage = mail.Message{To: "reader@example.test", Subject: "Hello", HTML: "<p>Hi</p>", Text: "Hi", Category: "lapsed"}

func TestNewWithoutKeyIsDisabled(t *testing.T) {
	m := mail.New(mail.Config{APIURL: "http://127.0.0.1:1", From: "a@example.test"})
	if _, ok := m.(mail.Disabled); !ok {
		t.Fatalf("New without a key = %T, want Disabled", m)
	}
	if err := m.Send(t.Context(), testMessage); err != nil {
		t.Fatalf("Disabled.Send = %v", err)
	}
	if _, ok := mail.New(mail.Config{APIKey: "re_x", APIURL: "http://127.0.0.1:1"}).(*mail.Resend); !ok {
		t.Fatal("New with a key is not the Resend driver")
	}
}

func TestResendSendsTheEmail(t *testing.T) {
	var got struct {
		method, path, auth, contentType string
		body                            map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &got.body); err != nil {
			t.Errorf("body %s: %v", data, err)
		}
		_, _ = io.WriteString(w, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
	}))
	defer server.Close()

	resend := mail.NewResend(mail.Config{APIKey: "re_test", APIURL: server.URL + "/", From: "FUTO Notes <noreply@example.test>"})
	if err := resend.Send(t.Context(), testMessage); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/emails" || got.auth != "Bearer re_test" || got.contentType != "application/json" {
		t.Fatalf("request = %s %s, auth %q, type %q", got.method, got.path, got.auth, got.contentType)
	}
	want := map[string]any{
		"from":    "FUTO Notes <noreply@example.test>",
		"to":      []any{"reader@example.test"},
		"subject": "Hello",
		"html":    "<p>Hi</p>",
		"text":    "Hi",
		"tags":    []any{map[string]any{"name": "category", "value": "lapsed"}},
	}
	gotJSON, _ := json.Marshal(got.body)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("body = %s\nwant   %s", gotJSON, wantJSON)
	}
}

func TestResendSendsTheIdempotencyKey(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		_, _ = io.WriteString(w, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
	}))
	defer server.Close()

	resend := mail.NewResend(mail.Config{APIKey: "re_test", APIURL: server.URL, From: "noreply@example.test"})
	keyed := testMessage
	keyed.IdempotencyKey = "lapse-email/u/lapsed/1"
	for _, m := range []mail.Message{testMessage, keyed} {
		if err := resend.Send(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] != "" || keys[1] != "lapse-email/u/lapsed/1" {
		t.Fatalf("Idempotency-Key headers = %q, want none then the message's key", keys)
	}
}

func TestResendMapsErrorResponses(t *testing.T) {
	for name, tc := range map[string]struct {
		status        int
		body          string
		wantName, msg string
	}{
		"resend error": {http.StatusUnprocessableEntity, `{"statusCode":422,"name":"validation_error","message":"Invalid ` + "`to`" + ` field."}`,
			"validation_error", "Invalid `to` field."},
		"rate limit":     {http.StatusTooManyRequests, `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests."}`, "rate_limit_exceeded", "Too many requests."},
		"not json":       {http.StatusBadGateway, "<html>bad gateway</html>", "Bad Gateway", "<html>bad gateway</html>"},
		"json, no name":  {http.StatusInternalServerError, `{"error":"boom"}`, "Internal Server Error", `{"error":"boom"}`},
		"empty response": {http.StatusServiceUnavailable, "", "Service Unavailable", ""},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			err := mail.NewResend(mail.Config{APIKey: "re_test", APIURL: server.URL}).Send(t.Context(), testMessage)
			var resendErr *mail.ResendError
			if !errors.As(err, &resendErr) {
				t.Fatalf("Send = %v, want a *ResendError", err)
			}
			if resendErr.StatusCode != tc.status || resendErr.Name != tc.wantName || resendErr.Message != tc.msg {
				t.Fatalf("error = %#v", resendErr)
			}
		})
	}
}

func TestResendRejectsAnAcceptanceWithoutID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	if err := mail.NewResend(mail.Config{APIKey: "re_test", APIURL: server.URL}).Send(t.Context(), testMessage); err == nil {
		t.Fatal("Send succeeded on a response without an id")
	}
}

func TestResendUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	err = mail.NewResend(mail.Config{APIKey: "re_test", APIURL: "http://" + addr}).Send(t.Context(), testMessage)
	var resendErr *mail.ResendError
	if err == nil || errors.As(err, &resendErr) {
		t.Fatalf("Send to a closed port = %v, want a transport error", err)
	}
}

// The mock answers in Resend's error shape, which the driver maps like the
// real thing's.
func TestResendAgainstTheMock(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedSMTP := listener.Addr().String()
	listener.Close()
	server := httptest.NewServer(mockresend.Handler(closedSMTP))
	defer server.Close()

	for name, tc := range map[string]struct {
		message mail.Message
		status  int
		errName string
	}{
		"invalid recipient": {mail.Message{To: "not an address", Subject: "s", Text: "t"}, http.StatusUnprocessableEntity, "validation_error"},
		"invalid tag":       {mail.Message{To: "a@example.test", Subject: "s", Text: "t", Category: "has space"}, http.StatusUnprocessableEntity, "validation_error"},
		"smtp down":         {testMessage, http.StatusInternalServerError, "application_error"},
	} {
		t.Run(name, func(t *testing.T) {
			err := mail.NewResend(mail.Config{APIKey: "re_test", APIURL: server.URL, From: "a@example.test"}).Send(t.Context(), tc.message)
			var resendErr *mail.ResendError
			if !errors.As(err, &resendErr) || resendErr.StatusCode != tc.status || resendErr.Name != tc.errName {
				t.Fatalf("Send = %v, want %d %s", err, tc.status, tc.errName)
			}
		})
	}
}
