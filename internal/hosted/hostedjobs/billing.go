package hostedjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/mail"
	"futo-notes-server/internal/hosted/storageclient"
	"futo-notes-server/internal/jobs"
)

// Billing is what the account purge and the billing jobs act through.
type Billing struct {
	Database  *db.DB
	Storage   *storageclient.Client
	Store     *billing.Store
	Providers billing.Providers
	Notifier  *mail.Notifier
}

const (
	// refreshSoon and refreshQuiet pick the subscriptions reconciliation
	// asks their provider about: ending within a day, or silent for a week.
	refreshSoon  = 24 * time.Hour
	refreshQuiet = 7 * 24 * time.Hour
	// deliveryRetention is how long a webhook delivery's id is kept to
	// recognize the provider retrying it.
	deliveryRetention = 30 * 24 * time.Hour
)

// ReconcileEntitlementsResult reports what ReconcileEntitlements did.
type ReconcileEntitlementsResult struct {
	Checked   int `json:"checked"`
	Refreshed int `json:"refreshed"`
	// Skipped counts subscriptions whose provider is not configured.
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// ReconcileEntitlements asks providers for the current state of every
// subscription that ends within a day or has had no news for a week, and
// applies it, in case a notification was missed.
func ReconcileEntitlements(ctx context.Context, store *billing.Store, providers billing.Providers) (ReconcileEntitlementsResult, error) {
	var result ReconcileEntitlementsResult
	refs, err := store.Stale(ctx, refreshSoon, refreshQuiet)
	if err != nil {
		return result, err
	}
	var errs []error
	for _, ref := range refs {
		result.Checked++
		provider := providers.Provider(ref.Source)
		if provider == nil {
			result.Skipped++
			continue
		}
		if err := refresh(ctx, store, provider, ref); err != nil {
			result.Failed++
			errs = append(errs, fmt.Errorf("refreshing %s subscription %s: %w", ref.Source, ref.ExternalID, err))
			continue
		}
		result.Refreshed++
	}
	return result, errors.Join(errs...)
}

func refresh(ctx context.Context, store *billing.Store, provider billing.Provider, ref billing.Ref) error {
	ev, err := provider.Refresh(ctx, ref.ExternalID)
	if err != nil {
		return err
	}
	if ev.Source != ref.Source || ev.ExternalID != ref.ExternalID {
		return fmt.Errorf("the provider answered about %s subscription %s", ev.Source, ev.ExternalID)
	}
	applied, err := store.Apply(ctx, ev)
	if err != nil {
		return err
	}
	if applied.Stale {
		return store.Touch(ctx, ref)
	}
	return nil
}

// LapseResult reports what ProcessLapses did.
type LapseResult struct {
	// Resubscribed counts lapses ended by a new subscription, Lapsed new
	// lapses.
	Resubscribed int64 `json:"resubscribed"`
	Lapsed       int64 `json:"lapsed"`
	Notified     int   `json:"notified"`
	Warned       int   `json:"warned"`
	Purged       int   `json:"purged"`
	Failed       int   `json:"failed"`
}

// ProcessLapses moves lapsed accounts along launch plan C5: it records new
// lapses and ends those a resubscription ended, then, for each lapsed
// account, emails that it lapsed, warns PurgeWarningAfter it lapsed, and
// purges it PurgeAfter it lapsed. The emails go out in that order and each
// at most once per lapse; a failed send holds the account's later steps for
// the next run, so nobody is purged unwarned. An account without an email
// address is purged without the emails.
func ProcessLapses(ctx context.Context, b Billing) (LapseResult, error) {
	var result LapseResult
	var err error
	if result.Resubscribed, err = b.Store.ClearResubscribed(ctx); err != nil {
		return result, err
	}
	if result.Lapsed, err = b.Store.MarkLapsed(ctx); err != nil {
		return result, err
	}
	lapses, err := b.Store.Lapses(ctx)
	if err != nil {
		return result, err
	}
	var errs []error
	for _, l := range lapses {
		if err := processLapse(ctx, b, l, &result); err != nil {
			result.Failed++
			errs = append(errs, fmt.Errorf("lapse of user %s: %w", l.UserID, err))
		}
	}
	return result, errors.Join(errs...)
}

func processLapse(ctx context.Context, b Billing, l billing.Lapse, result *LapseResult) error {
	send := func(t mail.Template, done bool, count *int) (bool, error) {
		if done {
			return true, nil
		}
		sent, err := b.Notifier.SendOnce(ctx, l.UserID, t, l.PurgeAt())
		if sent {
			*count++
		}
		if errors.Is(err, mail.ErrNoAddress) {
			log.Ctx(ctx).Warn().Str("user", l.UserID).Str("template", string(t)).Msg("lapsed account has no email address; not emailed")
			return true, nil
		}
		return err == nil, err
	}
	if ok, err := send(mail.Lapsed, l.Notified, &result.Notified); !ok {
		return err
	}
	if !l.WarningDue {
		return nil
	}
	if ok, err := send(mail.PurgeWarning, l.Warned, &result.Warned); !ok {
		return err
	}
	if !l.PurgeDue {
		return nil
	}
	claimed, err := b.Store.ClaimLapsePurge(ctx, l.UserID)
	if err != nil || !claimed {
		return err
	}
	_, purged, err := purgeAccount(ctx, b, l.UserID)
	if purged {
		result.Purged++
	}
	return err
}

func entitlementReconciliation(b Billing, metrics *Metrics) jobs.Job {
	const name = "entitlement reconciliation"
	return jobs.Job{Name: name, Run: func(ctx context.Context) (string, error) {
		result, err := ReconcileEntitlements(ctx, b.Store, b.Providers)
		metrics.add(ctx, name, map[string]int{itemsProcessed: result.Checked, itemsFailed: result.Failed})
		return fmt.Sprintf("refreshed %d of %d subscriptions, %d skipped, %d failed",
			result.Refreshed, result.Checked, result.Skipped, result.Failed), err
	}}
}

func subscriptionLapses(b Billing, metrics *Metrics) jobs.Job {
	const name = "subscription lapses"
	return jobs.Job{Name: name, Run: func(ctx context.Context) (string, error) {
		result, err := ProcessLapses(ctx, b)
		metrics.add(ctx, name, map[string]int{
			itemsProcessed: int(result.Lapsed), itemsDeleted: result.Purged, itemsFailed: result.Failed,
		})
		return fmt.Sprintf("%d lapsed, %d resubscribed, %d notified, %d warned, %d purged, %d failed",
			result.Lapsed, result.Resubscribed, result.Notified, result.Warned, result.Purged, result.Failed), err
	}}
}

func deliveryPruning(b Billing, metrics *Metrics) jobs.Job {
	const name = "billing delivery pruning"
	return jobs.Job{Name: name, Run: func(ctx context.Context) (string, error) {
		deleted, err := b.Store.PruneDeliveries(ctx, deliveryRetention)
		metrics.add(ctx, name, map[string]int{itemsDeleted: int(deleted)})
		return fmt.Sprintf("deleted %d", deleted), err
	}}
}
