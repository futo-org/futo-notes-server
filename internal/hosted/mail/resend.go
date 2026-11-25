package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sendTimeout bounds one call to Resend, so a hung provider cannot stall the
// job that sends.
const sendTimeout = 10 * time.Second

// maxResponseBytes caps how much of a Resend response is read.
const maxResponseBytes = 64 << 10

// Resend sends email through Resend's POST /emails.
type Resend struct {
	client *http.Client
	url    string
	apiKey string
	from   string
}

// NewResend returns a driver for cfg, which must carry an API key.
func NewResend(cfg Config) *Resend {
	return &Resend{
		client: &http.Client{Timeout: sendTimeout},
		url:    strings.TrimSuffix(cfg.APIURL, "/") + "/emails",
		apiKey: cfg.APIKey,
		from:   cfg.From,
	}
}

// ResendError is an error response from Resend: its HTTP status and the
// name and message of its body, such as 422 validation_error.
type ResendError struct {
	StatusCode int
	Name       string
	Message    string
}

func (e *ResendError) Error() string {
	return fmt.Sprintf("resend: %d %s: %s", e.StatusCode, e.Name, e.Message)
}

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendEmail struct {
	From    string      `json:"from"`
	To      []string    `json:"to"`
	Subject string      `json:"subject"`
	HTML    string      `json:"html,omitempty"`
	Text    string      `json:"text,omitempty"`
	Tags    []resendTag `json:"tags,omitempty"`
}

// Send posts m from the configured From address, tagged with its category,
// with m's idempotency key as Resend's Idempotency-Key (honoured for 24
// hours). It returns a *ResendError when Resend refuses it.
func (r *Resend) Send(ctx context.Context, m Message) error {
	email := resendEmail{From: r.from, To: []string{m.To}, Subject: m.Subject, HTML: m.HTML, Text: m.Text}
	if m.Category != "" {
		email.Tags = []resendTag{{Name: "category", Value: m.Category}}
	}
	body, err := json.Marshal(email)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("resend: reading response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return responseError(res.StatusCode, data)
	}
	var sent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &sent); err != nil || sent.ID == "" {
		return errors.New("resend: accepted the email without an id")
	}
	return nil
}

// responseError reads Resend's error body, { statusCode, name, message },
// falling back to the raw body when it is something else, such as a proxy's
// error page.
func responseError(status int, data []byte) *ResendError {
	var body struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Name == "" {
		message := strings.TrimSpace(string(data))
		if len(message) > 200 {
			message = message[:200]
		}
		return &ResendError{StatusCode: status, Name: http.StatusText(status), Message: message}
	}
	return &ResendError{StatusCode: status, Name: body.Name, Message: body.Message}
}
