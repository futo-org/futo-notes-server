package main

import (
	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/mockweb"
	"futo-notes-server/internal/hosted/config"
)

// billingProviders builds the payment providers cfg configures. With none,
// billing is off.
func billingProviders(cfg config.Config) billing.Providers {
	providers := billing.Providers{Stores: map[billing.Source]billing.StoreProvider{}}
	// Real web providers add their own branch here once one is chosen.
	if cfg.WebBilling.Provider == config.WebProviderMock {
		providers.Web = mockweb.New(cfg.PublicURL, cfg.WebBilling.MockWebhookSecret)
	}
	// The App Store and Play providers (launch plan S5b) register here, as
	// providers.Stores[billing.SourceAppStore] and [billing.SourcePlay],
	// when their APPLE_* and PLAY_* configuration is set.
	return providers
}
