package mail_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/mail"
)

var testData = mail.Data{
	Email:        "reader@example.test",
	PurgeAt:      time.Date(2027, time.March, 3, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600)),
	DashboardURL: "https://notes.example.test/dashboard",
}

func TestRenderEveryTemplate(t *testing.T) {
	subjects := map[mail.Template]string{
		mail.Lapsed:       "Your FUTO Notes subscription has ended",
		mail.PurgeWarning: "Your FUTO Notes account will be deleted on March 4, 2027",
		mail.Purged:       "Your FUTO Notes account has been deleted",
	}
	for _, tmpl := range mail.Templates {
		t.Run(string(tmpl), func(t *testing.T) {
			m, err := mail.Render(tmpl, testData)
			if err != nil {
				t.Fatal(err)
			}
			if m.To != testData.Email || m.Category != string(tmpl) || m.Subject != subjects[tmpl] {
				t.Fatalf("message = to %q, category %q, subject %q", m.To, m.Category, m.Subject)
			}
			// The purge date is shown in UTC, whatever zone it arrives in.
			for name, body := range map[string]string{"text": m.Text, "html": m.HTML} {
				for _, want := range []string{testData.Email, "March 4, 2027", testData.DashboardURL} {
					if !strings.Contains(body, want) {
						t.Errorf("%s body lacks %q:\n%s", name, want, body)
					}
				}
			}
			if strings.HasPrefix(m.Text, "\n") || !strings.HasSuffix(m.Text, "\n") || strings.HasSuffix(m.Text, "\n\n") {
				t.Errorf("text body is not trimmed to end in one newline: %q", m.Text)
			}
			if !strings.Contains(m.HTML, "<title>"+m.Subject+"</title>") {
				t.Errorf("html lacks the subject as its title:\n%s", m.HTML)
			}
		})
	}
}

func TestRenderButtonLabels(t *testing.T) {
	for tmpl, want := range map[mail.Template]string{mail.Lapsed: ">Open your account</a>", mail.Purged: ">Sign in</a>"} {
		m, err := mail.Render(tmpl, testData)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(m.HTML, want) {
			t.Errorf("%s html lacks %q", tmpl, want)
		}
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	data := testData
	data.Email = `"<b>x</b>"@example.test`
	m, err := mail.Render(mail.Lapsed, data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.HTML, "<b>x</b>") || !strings.Contains(m.HTML, "&lt;b&gt;x&lt;/b&gt;") {
		t.Fatalf("html does not escape the address:\n%s", m.HTML)
	}
	if !strings.Contains(m.Text, data.Email) {
		t.Fatalf("text body lacks the address verbatim:\n%s", m.Text)
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	if _, err := mail.Render("welcome", testData); !errors.Is(err, mail.ErrUnknownTemplate) {
		t.Fatalf("Render = %v, want ErrUnknownTemplate", err)
	}
}
