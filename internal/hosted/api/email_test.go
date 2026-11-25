package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/mail"
)

// fakeResend records the emails posted to it and answers with status.
type fakeResend struct {
	*httptest.Server
	mu     sync.Mutex
	emails []map[string]any
	status int
}

func newFakeResend(t *testing.T) *fakeResend {
	f := &fakeResend{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"statusCode":422,"name":"validation_error","message":"refused"}`)
			return
		}
		var email map[string]any
		if err := json.NewDecoder(r.Body).Decode(&email); err != nil {
			t.Errorf("decoding email: %v", err)
		}
		f.emails = append(f.emails, email)
		_, _ = io.WriteString(w, `{"id":"fake"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

// sent returns the emails recorded so far.
func (f *fakeResend) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.emails...)
}

// refuse makes every later send fail with a 422.
func (f *fakeResend) refuse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = http.StatusUnprocessableEntity
}

func TestDevEmailSendsToTheSignedInUser(t *testing.T) {
	resend := newFakeResend(t)
	h := newHarnessWith(t, func(c *config.Config) {
		c.DevUI = true
		c.Email = mail.Config{APIKey: "re_test", APIURL: resend.URL, From: "FUTO Notes <noreply@notes.futo.test>"}
	})
	sub := h.newSub()
	app, _ := h.appLogin(sub)

	for _, tmpl := range mail.Templates {
		var sent struct {
			Template  string `json:"template"`
			To        string `json:"to"`
			Subject   string `json:"subject"`
			Delivered bool   `json:"delivered"`
		}
		app.expect(http.MethodPost, "/dev/email/"+string(tmpl), nil, http.StatusOK).json(t, &sent)
		if sent.Template != string(tmpl) || sent.To != sub+"@example.test" || sent.Subject == "" || !sent.Delivered {
			t.Fatalf("%s: response = %+v", tmpl, sent)
		}
	}
	emails := resend.sent()
	if len(emails) != len(mail.Templates) {
		t.Fatalf("resend got %d emails, want %d", len(emails), len(mail.Templates))
	}
	for i, email := range emails {
		to, _ := email["to"].([]any)
		tags, _ := json.Marshal(email["tags"])
		if len(to) != 1 || to[0] != sub+"@example.test" || email["from"] != "FUTO Notes <noreply@notes.futo.test>" ||
			string(tags) != `[{"name":"category","value":"`+string(mail.Templates[i])+`"}]` {
			t.Fatalf("email %d = %v", i, email)
		}
	}

	// The dev sends leave the stamps alone.
	var stamped bool
	if err := h.database.QueryRowContext(t.Context(), `SELECT lapse_notified_at IS NOT NULL OR purge_warned_at IS NOT NULL
		OR purge_notified_at IS NOT NULL FROM users WHERE sub = $1`, sub).Scan(&stamped); err != nil || stamped {
		t.Fatalf("stamped = %v, %v; want no stamps", stamped, err)
	}

	expectHostedError(t, app.do(http.MethodPost, "/dev/email/welcome", nil), http.StatusNotFound, "not_found")
	expectHostedError(t, h.client().do(http.MethodPost, "/dev/email/lapsed", nil), http.StatusUnauthorized, "unauthorized")
	resend.refuse()
	expectHostedError(t, app.do(http.MethodPost, "/dev/email/lapsed", nil), http.StatusBadGateway, "internal_error")
}

func TestDevEmailWithoutAKeyOnlyLogs(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.DevUI = true })
	app, _ := h.appLogin(h.newSub())
	var sent struct {
		Delivered bool `json:"delivered"`
	}
	app.expect(http.MethodPost, "/dev/email/purged", nil, http.StatusOK).json(t, &sent)
	if sent.Delivered {
		t.Fatal("delivered = true without RESEND_API_KEY")
	}
}

// The lapse emails go to the address the latest sign-in's ID token carried.
func TestSignInRefreshesTheEmail(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	first := h.dashboardLogin(sub)

	second := h.client()
	start := second.do(http.MethodGet, "/api/auth/oidc/start", nil)
	callback := second.authorizeWith(start, sub, url.Values{"email": {"moved-" + sub + "@example.test"}})
	if res := second.do(http.MethodGet, callback, nil); res.status != http.StatusSeeOther {
		t.Fatalf("callback = %d %s", res.status, res.body)
	}

	for _, c := range []*client{first, second} {
		var account accountBody
		c.expect(http.MethodGet, "/api/account", nil, http.StatusOK).json(t, &account)
		if account.User.Email != "moved-"+sub+"@example.test" {
			t.Fatalf("email after signing in with a new one = %q", account.User.Email)
		}
	}
}
