package config

import (
	"errors"
	"fmt"
	"strings"
)

// The web payment providers BILLING_WEB_PROVIDER can name. Real providers
// (FUTOpay, Polar or Stripe) join this list once one is chosen.
const (
	// WebProviderMock is the dev and test-stack mock (internal/hosted/billing/mockweb).
	WebProviderMock = "mock"
)

// minMockWebhookSecret is the shortest BILLING_MOCK_WEBHOOK_SECRET accepted.
const minMockWebhookSecret = 16

// WebBilling is the web payment provider's configuration.
type WebBilling struct {
	// Provider is BILLING_WEB_PROVIDER: "" for none, or WebProviderMock.
	Provider string
	// MockWebhookSecret signs the mock's webhook deliveries
	// (BILLING_MOCK_WEBHOOK_SECRET).
	MockWebhookSecret []byte
}

// loadWebBilling reads BILLING_WEB_PROVIDER and its settings. The mock needs
// DEV_UI, which serves its pages, so it can never run in production.
func loadWebBilling(getenv Getenv, devUI bool) (WebBilling, error) {
	cfg := WebBilling{Provider: strings.TrimSpace(getenv("BILLING_WEB_PROVIDER"))}
	switch cfg.Provider {
	case "":
		return cfg, nil
	case WebProviderMock:
		if !devUI {
			return WebBilling{}, errors.New("BILLING_WEB_PROVIDER=mock needs DEV_UI=true: the mock is for development and tests only")
		}
		secret := getenv("BILLING_MOCK_WEBHOOK_SECRET")
		if len(secret) < minMockWebhookSecret {
			return WebBilling{}, fmt.Errorf("BILLING_MOCK_WEBHOOK_SECRET must be at least %d characters with BILLING_WEB_PROVIDER=mock", minMockWebhookSecret)
		}
		cfg.MockWebhookSecret = []byte(secret)
		return cfg, nil
	}
	return WebBilling{}, fmt.Errorf("BILLING_WEB_PROVIDER: unknown provider %q (want empty or %q)", cfg.Provider, WebProviderMock)
}
