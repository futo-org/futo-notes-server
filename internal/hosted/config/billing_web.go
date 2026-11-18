package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"futo-notes-server/internal/hosted/billing/stripe"
	"futo-notes-server/internal/hosted/country"
)

// The web payment providers BILLING_WEB_PROVIDER can name.
const (
	// WebProviderMock is the dev and test-stack mock (internal/hosted/billing/mockweb).
	WebProviderMock = "mock"
	// WebProviderStripe is Stripe (internal/hosted/billing/stripe).
	WebProviderStripe = "stripe"
)

// minMockWebhookSecret is the shortest BILLING_MOCK_WEBHOOK_SECRET accepted.
const minMockWebhookSecret = 16

// WebBilling is the web payment provider's configuration.
type WebBilling struct {
	// Provider is BILLING_WEB_PROVIDER: "" for none, WebProviderMock or
	// WebProviderStripe.
	Provider string
	// MockWebhookSecret signs the mock's webhook deliveries
	// (BILLING_MOCK_WEBHOOK_SECRET).
	MockWebhookSecret []byte
	// Stripe is the Stripe account, with WebProviderStripe.
	Stripe *stripe.Config
	// CheckoutTokenKey signs the checkout links POST /api/billing/checkout
	// gives the apps, with a provider whose checkout is embedded. It is
	// derived from the provider's secret key, so it needs no setting of its
	// own and rotates with that key.
	CheckoutTokenKey []byte
}

// loadWebBilling reads BILLING_WEB_PROVIDER and its settings. The mock needs
// DEV_UI, which serves its pages, so it can never run in production; for the
// same reason a live Stripe key is refused with DEV_UI on.
func loadWebBilling(getenv Getenv, devUI bool, publicURL string) (WebBilling, error) {
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
	case WebProviderStripe:
		s, err := loadStripe(getenv, devUI, publicURL)
		if err != nil {
			return WebBilling{}, err
		}
		cfg.Stripe = &s
		mac := hmac.New(sha256.New, []byte(s.SecretKey))
		mac.Write([]byte("futo-notes checkout token key"))
		cfg.CheckoutTokenKey = mac.Sum(nil)
		return cfg, nil
	}
	return WebBilling{}, fmt.Errorf("BILLING_WEB_PROVIDER: unknown provider %q (want empty, %q or %q)", cfg.Provider, WebProviderMock, WebProviderStripe)
}

// stripeMode is "live" or "test", from a Stripe key's prefix, or "" when
// the key is not one of kinds.
func stripeMode(key string, kinds ...string) string {
	for _, kind := range kinds {
		for _, mode := range []string{"live", "test"} {
			if strings.HasPrefix(key, kind+"_"+mode+"_") && len(key) > len(kind)+len(mode)+2 {
				return mode
			}
		}
	}
	return ""
}

func loadStripe(getenv Getenv, devUI bool, publicURL string) (stripe.Config, error) {
	cfg := stripe.Config{
		SecretKey:      strings.TrimSpace(getenv("STRIPE_SECRET_KEY")),
		PublishableKey: strings.TrimSpace(getenv("STRIPE_PUBLISHABLE_KEY")),
		WebhookSecret:  []byte(strings.TrimSpace(getenv("STRIPE_WEBHOOK_SECRET"))),
		PriceID:        strings.TrimSpace(getenv("STRIPE_PRICE_ID")),
		PublicURL:      publicURL,
		APIURL:         strings.TrimSpace(getenv("STRIPE_API_URL")),
	}
	mode := stripeMode(cfg.SecretKey, "sk", "rk")
	switch {
	case mode == "":
		return stripe.Config{}, errors.New("STRIPE_SECRET_KEY must be a Stripe secret (sk_) or restricted (rk_) key with BILLING_WEB_PROVIDER=stripe")
	case mode == "live" && devUI:
		return stripe.Config{}, errors.New("STRIPE_SECRET_KEY is a live key, which DEV_UI=true refuses: use a test-mode or sandbox key in development")
	case stripeMode(cfg.PublishableKey, "pk") != mode:
		return stripe.Config{}, fmt.Errorf("STRIPE_PUBLISHABLE_KEY must be a %s-mode publishable key (pk_%s_), like STRIPE_SECRET_KEY", mode, mode)
	case !strings.HasPrefix(string(cfg.WebhookSecret), "whsec_"):
		return stripe.Config{}, errors.New("STRIPE_WEBHOOK_SECRET must be the webhook endpoint's signing secret (whsec_...)")
	case !strings.HasPrefix(cfg.PriceID, "price_"):
		return stripe.Config{}, errors.New("STRIPE_PRICE_ID must be the plan's price id (price_...)")
	}
	for item := range strings.SplitSeq(getenv("STRIPE_SELF_PAYMENT_COUNTRIES"), ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		code, ok := country.Parse(item)
		if !ok {
			return stripe.Config{}, fmt.Errorf("STRIPE_SELF_PAYMENT_COUNTRIES: %q is not an ISO 3166-1 alpha-2 country code", strings.TrimSpace(item))
		}
		if !slices.Contains(cfg.SelfPaymentCountries, code) {
			cfg.SelfPaymentCountries = append(cfg.SelfPaymentCountries, code)
		}
	}
	if cfg.APIURL != "" {
		if u, err := url.Parse(cfg.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return stripe.Config{}, fmt.Errorf("STRIPE_API_URL %q is not an http(s) URL", cfg.APIURL)
		}
	}
	return cfg, nil
}
