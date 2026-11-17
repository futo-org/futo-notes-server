// Package mockresend is a stand-in for Resend's POST /emails, for development
// and tests: it accepts what Resend accepts, answers in Resend's shapes, and
// relays each email over SMTP to an inbox such as Mailpit instead of sending
// it, so nothing leaves the machine. Tags become Mailpit tags (X-Tags).
package mockresend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/rs/zerolog/hlog"
)

// maxBodyBytes caps a request body. Resend allows 40 MB with attachments,
// which the mock does not take.
const maxBodyBytes = 1 << 20

// maxRecipients is Resend's limit on to.
const maxRecipients = 50

// tagPattern is what Resend allows in a tag's name and value.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// Handler serves GET /health and POST /emails, relaying accepted emails to
// the SMTP server at smtpAddr (host:port, no auth, no TLS).
func Handler(smtpAddr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /emails", func(w http.ResponseWriter, r *http.Request) {
		handleSend(w, r, smtpAddr)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("No such endpoint: %s %s", r.Method, r.URL.Path))
	})
	return mux
}

// email is the subset of Resend's send-email body the mock understands.
type email struct {
	From    string     `json:"from"`
	To      recipients `json:"to"`
	Subject string     `json:"subject"`
	HTML    string     `json:"html"`
	Text    string     `json:"text"`
	Tags    []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"tags"`
}

// recipients is Resend's to: one address or a list of them.
type recipients []string

func (r *recipients) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*r = recipients{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*r = many
	return nil
}

func handleSend(w http.ResponseWriter, r *http.Request, smtpAddr string) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeError(w, http.StatusUnauthorized, "missing_api_key", "Missing API key in the authorization header.")
		return
	}
	var e email
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&e); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "invalid JSON body: "+err.Error())
		return
	}
	from, to, err := validate(e)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	id := uuid.NewV7().String()
	message, err := compose(id, e, to)
	if err == nil {
		envelope := make([]string, len(to))
		for i, addr := range to {
			envelope[i] = addr.Address
		}
		err = smtp.SendMail(smtpAddr, nil, from.Address, envelope, message)
	}
	if err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("relaying email over SMTP")
		writeError(w, http.StatusInternalServerError, "application_error", "relaying the email failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

// validate checks e as Resend would and returns its parsed addresses.
func validate(e email) (*mail.Address, []*mail.Address, error) {
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid `from` field: %w", err)
	}
	if len(e.To) == 0 || len(e.To) > maxRecipients {
		return nil, nil, fmt.Errorf("`to` must name 1 to %d recipients", maxRecipients)
	}
	to := make([]*mail.Address, len(e.To))
	for i, addr := range e.To {
		if to[i], err = mail.ParseAddress(addr); err != nil {
			return nil, nil, fmt.Errorf("invalid `to` field: %w", err)
		}
	}
	if e.Subject == "" {
		return nil, nil, errors.New("missing `subject` field")
	}
	if e.HTML == "" && e.Text == "" {
		return nil, nil, errors.New("missing `html` or `text` field")
	}
	for _, tag := range e.Tags {
		if !tagPattern.MatchString(tag.Name) || !tagPattern.MatchString(tag.Value) {
			return nil, nil, errors.New("tags should only contain ASCII letters, numbers, underscores, or dashes")
		}
	}
	return from, to, nil
}

// compose builds the RFC 5322 message for e: a text and an HTML
// alternative, quoted-printable.
func compose(id string, e email, to []*mail.Address) ([]byte, error) {
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for _, part := range []struct{ contentType, content string }{
		{"text/plain", e.Text},
		{"text/html", e.HTML},
	} {
		if part.content == "" {
			continue
		}
		w, err := parts.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := io.WriteString(qp, part.content); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := parts.Close(); err != nil {
		return nil, err
	}

	addresses := make([]string, len(to))
	for i, addr := range to {
		addresses[i] = addr.String()
	}
	from, _ := mail.ParseAddress(e.From)
	var message bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&message, "%s: %s\r\n", name, value) }
	header("From", from.String())
	header("To", strings.Join(addresses, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", e.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+id+"@mock-resend>")
	if len(e.Tags) > 0 {
		values := make([]string, len(e.Tags))
		for i, tag := range e.Tags {
			values[i] = tag.Value
		}
		header("X-Tags", strings.Join(values, ","))
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "multipart/alternative; boundary="+parts.Boundary())
	message.WriteString("\r\n")
	message.Write(body.Bytes())
	return message.Bytes(), nil
}

// writeError answers in Resend's error shape.
func writeError(w http.ResponseWriter, status int, name, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		StatusCode int    `json:"statusCode"`
		Name       string `json:"name"`
		Message    string `json:"message"`
	}{status, name, message})
}
