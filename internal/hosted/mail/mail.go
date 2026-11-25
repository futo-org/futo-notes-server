// Package mail sends the hosted service's transactional email: a Mailer, its
// Resend driver, the embedded templates, and the per-user stamps that keep a
// retried job from sending an email twice. Modelled on Backups' email
// repository.
package mail

import (
	"context"

	"github.com/rs/zerolog/log"
)

// Message is one email to one recipient. Category tags it at the provider,
// for filtering and metrics there.
type Message struct {
	To       string
	Subject  string
	HTML     string
	Text     string
	Category string
	// IdempotencyKey, when set, makes the provider accept the same message
	// only once: a send repeated with the same key, within the provider's
	// window, is not delivered again.
	IdempotencyKey string
}

// Mailer sends email. A nil error means the provider accepted the message.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Config is where and as whom email is sent.
type Config struct {
	// APIKey is the Resend API key (RESEND_API_KEY); empty disables sending.
	APIKey string
	// APIURL is Resend's base URL (RESEND_API_URL), without a trailing slash.
	APIURL string
	// From is the From header (EMAIL_FROM_ADDRESS).
	From string
}

// New returns the Resend driver for cfg, or Disabled when it has no API key.
func New(cfg Config) Mailer {
	if cfg.APIKey == "" {
		return Disabled{}
	}
	return NewResend(cfg)
}

// Disabled logs and skips every send, so environments without a Resend key
// keep working.
type Disabled struct{}

// Send logs m's category and subject and reports success. The address is
// left out of the log.
func (Disabled) Send(_ context.Context, m Message) error {
	log.Info().Str("category", m.Category).Str("subject", m.Subject).Msg("RESEND_API_KEY is not set; skipping email send")
	return nil
}
