package main

import (
	"testing"

	"golang.org/x/oauth2/jwt"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/appstore"
	"futo-notes-server/internal/hosted/billing/play"
	"futo-notes-server/internal/hosted/config"
)

func TestBillingProvidersRegistersConfiguredStores(t *testing.T) {
	if p := billingProviders(config.Config{}); p.Web != nil || len(p.Stores) != 0 {
		t.Fatalf("no billing config = %+v, want no providers", p)
	}
	p := billingProviders(config.Config{
		AppStore: &appstore.Config{BundleID: "tech.futo.notes"},
		Play:     &play.Config{PackageName: "com.futo.notes", ServiceAccount: &jwt.Config{}},
	})
	for _, source := range []billing.Source{billing.SourceAppStore, billing.SourcePlay} {
		if got := p.Stores[source]; got == nil || got.Source() != source {
			t.Errorf("Stores[%s] = %v", source, got)
		}
	}
}
