package billing

import (
	"context"
	"errors"
	"fmt"
)

// Providers are the configured payment sources. A source without a provider
// is a rail the deployment does not offer.
type Providers struct {
	// Web is the web provider; nil when none is configured.
	Web WebProvider
	// Stores are the app store providers by their source.
	Stores map[Source]StoreProvider
}

// Provider returns source's provider, or nil when it is not configured.
func (p Providers) Provider(source Source) Provider {
	if source == SourceWeb {
		if p.Web == nil {
			return nil
		}
		return p.Web
	}
	if store, ok := p.Stores[source]; ok {
		return store
	}
	return nil
}

// Store returns source's store provider, or nil when it is not configured.
func (p Providers) Store(source Source) StoreProvider {
	return p.Stores[source]
}

// Any reports whether any provider is configured, which is what the
// capability document's billing says.
func (p Providers) Any() bool {
	return p.Web != nil || len(p.Stores) > 0
}

// CancelRenewals stops every web and Play subscription of userID's from
// renewing, through its provider. The App Store's cannot be cancelled from
// the server and are left alone. It returns the failures, joined; a
// subscription whose provider is not configured is one.
func (p Providers) CancelRenewals(ctx context.Context, store *Store, userID string) error {
	refs, err := store.Cancellable(ctx, userID)
	if err != nil {
		return err
	}
	var errs []error
	for _, ref := range refs {
		provider := p.Provider(ref.Source)
		if provider == nil {
			errs = append(errs, fmt.Errorf("cancelling %s subscription %s: its provider is not configured", ref.Source, ref.ExternalID))
			continue
		}
		if err := provider.Cancel(ctx, ref.ExternalID); err != nil && !errors.Is(err, ErrNotSupported) {
			errs = append(errs, fmt.Errorf("cancelling %s subscription %s: %w", ref.Source, ref.ExternalID, err))
		}
	}
	return errors.Join(errs...)
}
