package mail_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/hosted/mail"
	"futo-notes-server/internal/mockresend"
)

// mailpitMessage is a message in Mailpit's REST API, as search and message
// lookups return it.
type mailpitMessage struct {
	ID   string
	From struct{ Name, Address string }
	To   []struct{ Name, Address string }
	Subject,
	Text,
	HTML string
	Tags []string
}

// TestDeliveryThroughMailpit sends every email through the Resend driver to
// the mock Resend, which relays it to Mailpit over SMTP, and reads it back
// through Mailpit's REST API. It runs when MAILPIT_TEST_URL (the REST API,
// e.g. http://localhost:8025) and MAILPIT_TEST_SMTP_ADDR (e.g.
// localhost:1025) are set.
func TestDeliveryThroughMailpit(t *testing.T) {
	apiURL := strings.TrimSuffix(os.Getenv("MAILPIT_TEST_URL"), "/")
	if apiURL == "" {
		t.Skip("MAILPIT_TEST_URL not set")
	}
	smtpAddr := os.Getenv("MAILPIT_TEST_SMTP_ADDR")
	if smtpAddr == "" {
		t.Fatal("MAILPIT_TEST_URL is set but MAILPIT_TEST_SMTP_ADDR is not")
	}
	resend := httptest.NewServer(mockresend.Handler(smtpAddr))
	defer resend.Close()
	mailer := mail.New(mail.Config{APIKey: "re_test", APIURL: resend.URL, From: "FUTO Notes <noreply@notes.futo.test>"})

	for _, tmpl := range mail.Templates {
		t.Run(string(tmpl), func(t *testing.T) {
			to := "mailpit-" + uuid.NewV7().String() + "@example.test"
			data := mail.Data{Email: to, PurgeAt: time.Date(2027, time.March, 3, 12, 0, 0, 0, time.UTC), DashboardURL: "https://notes.example.test/dashboard"}
			message, err := mail.Render(tmpl, data)
			if err != nil {
				t.Fatal(err)
			}
			if err := mailer.Send(t.Context(), message); err != nil {
				t.Fatal(err)
			}

			got := findInMailpit(t, apiURL, to)
			if got.From.Address != "noreply@notes.futo.test" || got.From.Name != "FUTO Notes" ||
				len(got.To) != 1 || got.To[0].Address != to || got.Subject != message.Subject || !slices.Contains(got.Tags, string(tmpl)) {
				t.Fatalf("mailpit has %+v", got)
			}
			// MIME text parts travel with CRLF line endings.
			if text := strings.ReplaceAll(got.Text, "\r\n", "\n"); text != message.Text {
				t.Errorf("text = %q\nwant %q", got.Text, message.Text)
			}
			if !strings.Contains(got.HTML, `href="https://notes.example.test/dashboard"`) || !strings.Contains(got.HTML, "March 3, 2027") {
				t.Errorf("html lacks the link or the date:\n%s", got.HTML)
			}
		})
	}
}

// findInMailpit returns the one message to the address, with its bodies,
// waiting briefly for Mailpit to store it.
func findInMailpit(t *testing.T, apiURL, to string) mailpitMessage {
	t.Helper()
	var search struct{ Messages []mailpitMessage }
	for range 50 {
		getJSON(t, apiURL+"/api/v1/search?query="+url.QueryEscape(fmt.Sprintf("to:%q", to)), &search)
		if len(search.Messages) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(search.Messages) != 1 {
		t.Fatalf("mailpit has %d messages to %s, want 1", len(search.Messages), to)
	}
	var message mailpitMessage
	getJSON(t, apiURL+"/api/v1/message/"+search.Messages[0].ID, &message)
	return message
}

func getJSON(t *testing.T, target string, v any) {
	t.Helper()
	res, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", target, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
