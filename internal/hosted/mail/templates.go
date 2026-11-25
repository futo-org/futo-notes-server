package mail

import (
	"bytes"
	"embed"
	"errors"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"time"
)

//go:embed templates
var templateFS embed.FS

// Template names an email. Its name is also the email's category tag.
type Template string

// The lapse emails (launch plan C5).
const (
	// Lapsed is sent when the subscription lapses and the account turns
	// read-only.
	Lapsed Template = "lapsed"
	// PurgeWarning is sent 7 days before the lapsed account is purged.
	PurgeWarning Template = "purge_warning"
	// Purged is sent once the purge is done.
	Purged Template = "purged"
)

// Templates lists every email, in the order an account receives them.
var Templates = []Template{Lapsed, PurgeWarning, Purged}

// ErrUnknownTemplate reports a Template that names no email.
var ErrUnknownTemplate = errors.New("unknown email template")

// Data is what the lapse emails show.
type Data struct {
	// Email is the recipient: the account's address.
	Email string
	// PurgeAt is when the account is, or was, purged.
	PurgeAt time.Time
	// DashboardURL links to the account dashboard, <publicUrl>/dashboard.
	DashboardURL string
}

type parsed struct {
	html *htmltemplate.Template
	text *texttemplate.Template
}

var funcs = map[string]any{
	"date": func(t time.Time) string { return t.UTC().Format("January 2, 2006") },
}

var parsedTemplates = parseTemplates()

// parseTemplates parses each email once: <name>.html inside layout.html, and
// <name>.txt, whose "subject" template is the subject line.
func parseTemplates() map[Template]parsed {
	out := make(map[Template]parsed, len(Templates))
	for _, t := range Templates {
		name := string(t)
		html := htmltemplate.Must(htmltemplate.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
		text := texttemplate.Must(texttemplate.New(name+".txt").Funcs(funcs).
			ParseFS(templateFS, "templates/"+name+".txt"))
		if text.Lookup("subject") == nil {
			panic("mail: templates/" + name + ".txt defines no subject")
		}
		out[t] = parsed{html: html, text: text}
	}
	return out
}

// Render renders email t for d, addressed to d.Email.
func Render(t Template, d Data) (Message, error) {
	p, ok := parsedTemplates[t]
	if !ok {
		return Message{}, ErrUnknownTemplate
	}
	var subject, text, html bytes.Buffer
	if err := p.text.ExecuteTemplate(&subject, "subject", d); err != nil {
		return Message{}, err
	}
	if err := p.text.Execute(&text, d); err != nil {
		return Message{}, err
	}
	layout := struct {
		Subject string
		Data
	}{Subject: subject.String(), Data: d}
	if err := p.html.Execute(&html, layout); err != nil {
		return Message{}, err
	}
	return Message{
		To:       d.Email,
		Subject:  strings.TrimSpace(subject.String()),
		HTML:     html.String(),
		Text:     strings.TrimSpace(text.String()) + "\n",
		Category: string(t),
	}, nil
}
