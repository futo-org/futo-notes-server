package main

import (
	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/appstore"
	"futo-notes-server/internal/hosted/billing/mockweb"
	"futo-notes-server/internal/hosted/billing/play"
	"futo-notes-server/internal/hosted/billing/stripe"
	"futo-notes-server/internal/hosted/config"
)

// billingProviders builds the payment providers cfg configures. With none,
// billing is off.
func billingProviders(cfg config.Config) billing.Providers {
	providers := billing.Providers{Stores: map[billing.Source]billing.StoreProvider{}}
	switch cfg.WebBilling.Provider {
	case config.WebProviderMock:
		providers.Web = mockweb.New(cfg.PublicURL, cfg.WebBilling.MockWebhookSecret)
	case config.WebProviderStripe:
		providers.Web = stripe.New(*cfg.WebBilling.Stripe)
	}
	if cfg.AppStore != nil {
		providers.Stores[billing.SourceAppStore] = appstore.New(*cfg.AppStore)
	}
	if cfg.Play != nil {
		providers.Stores[billing.SourcePlay] = play.New(*cfg.Play)
	}
	return providers
}
