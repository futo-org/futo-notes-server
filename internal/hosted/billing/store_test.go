package billing_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/hostedtest"
)

// newUser inserts a user and removes it when the test ends. It returns the
// user's id and store account token.
func newUser(t *testing.T, database *db.DB) (string, string) {
	t.Helper()
	id := uuid.NewV7().String()
	var token string
	if err := database.QueryRowContext(t.Context(), `INSERT INTO users (id, sub, name, email) VALUES ($1, $2, 'billing test', $3)
		RETURNING store_account_token`, id, subjectOf(id), id+"@example.test").Scan(&token); err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, id)
	return id, token
}

// subjectOf is the OIDC sub newUser gives userID.
func subjectOf(userID string) string {
	if userID == "" {
		return ""
	}
	return "billing-test-" + userID
}

// webEvent is a web subscription event for userID's sub at eventAt.
func webEvent(userID, id string, status billing.Status, expires, eventAt time.Time) billing.Event {
	return billing.Event{
		Source: billing.SourceWeb, ExternalID: id, Subject: subjectOf(userID), ProductID: "monthly", Status: status,
		WillRenew: status == billing.StatusActive, ExpiresAt: new(expires), Environment: billing.EnvironmentProduction,
		EventAt: eventAt, Raw: []byte(`{"test":true}`),
	}
}

// storeEvent is an App Store subscription event carrying accountToken.
func storeEvent(accountToken, id string, status billing.Status, expires, eventAt time.Time) billing.Event {
	return billing.Event{
		Source: billing.SourceAppStore, ExternalID: id, AccountToken: accountToken, ProductID: "monthly",
		Status: status, WillRenew: true, ExpiresAt: new(expires), Environment: billing.EnvironmentSandbox, EventAt: eventAt,
	}
}

func state(t *testing.T, store *billing.Store, userID string) billing.State {
	t.Helper()
	s, err := store.State(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestApplyKeepsTheNewestEvent(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	userID, _ := newUser(t, database)
	now := time.Now()
	sub := "sub_" + uuid.NewV7().String()

	if _, err := store.Apply(t.Context(), webEvent(userID, sub, billing.StatusActive, now.Add(time.Hour), now)); err != nil {
		t.Fatal(err)
	}
	if !state(t, store, userID).Entitled {
		t.Fatal("an active subscription does not entitle")
	}
	// A late retry of an older notification changes nothing.
	applied, err := store.Apply(t.Context(), webEvent(userID, sub, billing.StatusExpired, now.Add(-time.Hour), now.Add(-time.Minute)))
	if err != nil || !applied.Stale || applied.UserID != userID {
		t.Fatalf("older event = %+v, %v; want stale", applied, err)
	}
	if s := state(t, store, userID); !s.Entitled || s.Current.Status != billing.StatusActive {
		t.Fatalf("after an older event: %+v", s)
	}
	// A newer one wins.
	if _, err := store.Apply(t.Context(), webEvent(userID, sub, billing.StatusRevoked, now.Add(time.Hour), now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if s := state(t, store, userID); s.Entitled || s.Current.Status != billing.StatusRevoked {
		t.Fatalf("after a refund: %+v", s)
	}
}

func TestEntitlement(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	for name, c := range map[string]struct {
		status   billing.Status
		expires  time.Time
		grace    *time.Time
		entitled bool
	}{
		"active":                    {billing.StatusActive, now.Add(time.Hour), nil, true},
		"active past its end":       {billing.StatusActive, now.Add(-time.Second), nil, false},
		"grace":                     {billing.StatusGrace, now.Add(-time.Hour), new(now.Add(time.Hour)), true},
		"grace past its end":        {billing.StatusGrace, now.Add(-time.Hour), new(now.Add(-time.Second)), false},
		"grace without an end":      {billing.StatusGrace, now.Add(time.Hour), nil, false},
		"on hold":                   {billing.StatusOnHold, now.Add(time.Hour), nil, false},
		"expired":                   {billing.StatusExpired, now.Add(time.Hour), nil, false},
		"revoked before its period": {billing.StatusRevoked, now.Add(time.Hour), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			userID, _ := newUser(t, database)
			ev := webEvent(userID, "sub_"+uuid.NewV7().String(), c.status, c.expires, now)
			ev.GraceUntil = c.grace
			if _, err := store.Apply(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			if got := state(t, store, userID).Entitled; got != c.entitled {
				t.Fatalf("entitled = %t, want %t", got, c.entitled)
			}
		})
	}

	// Any entitling row is enough, and State describes it.
	userID, _ := newUser(t, database)
	if _, err := store.Apply(t.Context(), webEvent(userID, "sub_"+uuid.NewV7().String(), billing.StatusExpired, now.Add(-time.Hour), now)); err != nil {
		t.Fatal(err)
	}
	if s := state(t, store, userID); s.Entitled || s.Current == nil || s.Current.Status != billing.StatusExpired {
		t.Fatalf("with only an expired row: %+v", s)
	}
	var accountToken string
	if err := database.QueryRowContext(t.Context(), `SELECT store_account_token FROM users WHERE id = $1`, userID).Scan(&accountToken); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(t.Context(), storeEvent(accountToken, "orig_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)); err != nil {
		t.Fatal(err)
	}
	if s := state(t, store, userID); !s.Entitled || s.Current.Source != billing.SourceAppStore || s.StoreAccountToken != accountToken {
		t.Fatalf("with an active store row beside an expired web one: %+v", s)
	}
}

func TestStoreBinding(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	owner, ownerToken := newUser(t, database)
	other, otherToken := newUser(t, database)
	original := "orig_" + uuid.NewV7().String()
	bought := storeEvent(ownerToken, original, billing.StatusActive, now.Add(time.Hour), now)

	// The buyer submits it.
	if applied, err := store.Submit(t.Context(), owner, bought); err != nil || applied.UserID != owner {
		t.Fatalf("owner submit = %+v, %v", applied, err)
	}
	// Restoring it from another account changes nothing, by its row or by
	// its token.
	restored := bought
	restored.EventAt = now.Add(time.Minute)
	restored.Status = billing.StatusExpired
	if _, err := store.Submit(t.Context(), other, restored); !errors.Is(err, billing.ErrOwnedElsewhere) {
		t.Fatalf("submit from another account = %v, want ErrOwnedElsewhere", err)
	}
	if !state(t, store, owner).Entitled || state(t, store, other).Entitled {
		t.Fatal("a refused submit moved or changed the subscription")
	}
	unbound := storeEvent(ownerToken, "orig_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)
	if _, err := store.Submit(t.Context(), other, unbound); !errors.Is(err, billing.ErrOwnedElsewhere) {
		t.Fatalf("submit of a new purchase bought under another account = %v, want ErrOwnedElsewhere", err)
	}

	// A notification follows the row, whatever its token says.
	notified := bought
	notified.AccountToken = otherToken
	notified.EventAt = now.Add(2 * time.Minute)
	if applied, err := store.Apply(t.Context(), notified); err != nil || applied.UserID != owner {
		t.Fatalf("notification = %+v, %v; want it applied to the owner", applied, err)
	}

	// A notification for a subscription with no row binds by its token, and
	// one whose token names nobody is unknown.
	fresh := storeEvent(otherToken, "orig_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)
	if applied, err := store.Apply(t.Context(), fresh); err != nil || applied.UserID != other {
		t.Fatalf("notification of a new subscription = %+v, %v", applied, err)
	}
	if _, err := store.Apply(t.Context(), storeEvent(uuid.NewV7().String(), "orig_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)); !errors.Is(err, billing.ErrUnknownUser) {
		t.Fatalf("notification naming no account = %v, want ErrUnknownUser", err)
	}

	// Once the owner is purged, the next account to submit it gets it.
	if _, err := database.ExecContext(t.Context(), `DELETE FROM users WHERE id = $1`, owner); err != nil {
		t.Fatal(err)
	}
	restored.EventAt = now.Add(3 * time.Minute)
	restored.Status = billing.StatusActive
	if applied, err := store.Submit(t.Context(), other, restored); err != nil || applied.UserID != other {
		t.Fatalf("submit after the owner's purge = %+v, %v", applied, err)
	}
}

// A store event without an account token, such as an App Store offer-code
// redemption, binds by its row, or else to the account submitting it.
func TestStoreBindingWithoutAccountToken(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	owner, _ := newUser(t, database)
	other, _ := newUser(t, database)
	redeemed := storeEvent("", "orig_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)

	// With no row, it binds to the submitter.
	if applied, err := store.Submit(t.Context(), owner, redeemed); err != nil || applied.UserID != owner {
		t.Fatalf("tokenless submit with no row = %+v, %v; want it bound to the submitter", applied, err)
	}
	if !state(t, store, owner).Entitled {
		t.Fatal("a tokenless submit did not entitle the submitter")
	}
	// Its owner submits it again.
	again := redeemed
	again.EventAt = now.Add(time.Minute)
	if applied, err := store.Submit(t.Context(), owner, again); err != nil || applied.UserID != owner {
		t.Fatalf("tokenless submit by the row's owner = %+v, %v", applied, err)
	}
	// Another account cannot take it.
	taken := redeemed
	taken.EventAt = now.Add(2 * time.Minute)
	taken.Status = billing.StatusExpired
	if _, err := store.Submit(t.Context(), other, taken); !errors.Is(err, billing.ErrOwnedElsewhere) {
		t.Fatalf("tokenless submit from another account = %v, want ErrOwnedElsewhere", err)
	}
	if !state(t, store, owner).Entitled || state(t, store, other).Entitled {
		t.Fatal("a refused tokenless submit moved or changed the subscription")
	}

	// A tokenless notification follows the row; with no row it names no
	// account, and a delivery acknowledges it as unknown.
	notified := redeemed
	notified.EventAt = now.Add(3 * time.Minute)
	notified.Status = billing.StatusRevoked
	if applied, err := store.Apply(t.Context(), notified); err != nil || applied.UserID != owner {
		t.Fatalf("tokenless notification = %+v, %v; want it applied to the owner", applied, err)
	}
	if state(t, store, owner).Entitled {
		t.Fatal("the tokenless revocation was not applied")
	}
	orphan := storeEvent("", "orig_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)
	if _, err := store.Apply(t.Context(), orphan); !errors.Is(err, billing.ErrUnknownUser) {
		t.Fatalf("tokenless notification with no row = %v, want ErrUnknownUser", err)
	}
	duplicate, unknown, err := store.ApplyDelivery(t.Context(), billing.SourceAppStore, "dlv_"+uuid.NewV7().String(), []billing.Event{orphan})
	if err != nil || duplicate || len(unknown) != 1 || unknown[0].ExternalID != orphan.ExternalID {
		t.Fatalf("tokenless delivery with no row = %t, %v, %v; want it acknowledged as unknown", duplicate, unknown, err)
	}
}

func TestWebEventNeedsItsUser(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	for _, userID := range []string{"", "not-a-uuid", uuid.NewV7().String()} {
		if _, err := store.Apply(t.Context(), webEvent(userID, "sub_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)); !errors.Is(err, billing.ErrUnknownUser) {
			t.Errorf("web event for user %q = %v, want ErrUnknownUser", userID, err)
		}
	}
	owner, _ := newUser(t, database)
	other, _ := newUser(t, database)
	// The sub is the only key: the account's id or email names nobody.
	for _, subject := range []string{owner, owner + "@example.test"} {
		ev := webEvent(owner, "sub_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)
		ev.Subject = subject
		if _, err := store.Apply(t.Context(), ev); !errors.Is(err, billing.ErrUnknownUser) {
			t.Errorf("web event with subject %q = %v, want ErrUnknownUser", subject, err)
		}
	}
	sub := "sub_" + uuid.NewV7().String()
	if _, err := store.Apply(t.Context(), webEvent(owner, sub, billing.StatusActive, now.Add(time.Hour), now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(t.Context(), webEvent(other, sub, billing.StatusActive, now.Add(time.Hour), now.Add(time.Second))); err == nil {
		t.Fatal("a web subscription moved to another account")
	}
}

func TestApplyDeliveryDeduplicates(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	userID, _ := newUser(t, database)
	delivery := "dlv_" + uuid.NewV7().String()
	events := []billing.Event{
		webEvent(userID, "sub_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now),
		webEvent(uuid.NewV7().String(), "sub_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now),
	}
	duplicate, unknown, err := store.ApplyDelivery(t.Context(), billing.SourceWeb, delivery, events)
	if err != nil || duplicate || len(unknown) != 1 || unknown[0].ExternalID != events[1].ExternalID {
		t.Fatalf("first delivery = %t, %v, %v", duplicate, unknown, err)
	}
	if !state(t, store, userID).Entitled {
		t.Fatal("the delivery was not applied")
	}
	revoked := webEvent(userID, events[0].ExternalID, billing.StatusRevoked, now.Add(time.Hour), now.Add(time.Minute))
	if duplicate, _, err := store.ApplyDelivery(t.Context(), billing.SourceWeb, delivery, []billing.Event{revoked}); err != nil || !duplicate {
		t.Fatalf("same delivery again = %t, %v; want a duplicate", duplicate, err)
	}
	if !state(t, store, userID).Entitled {
		t.Fatal("a duplicate delivery was applied")
	}
	// Ids are per source.
	if duplicate, _, err := store.ApplyDelivery(t.Context(), billing.SourcePlay, delivery, nil); err != nil || duplicate {
		t.Fatalf("same id from another source = %t, %v", duplicate, err)
	}

	if _, err := database.ExecContext(t.Context(), `UPDATE billing_deliveries SET received_at = now() - interval '31 days'
		WHERE delivery_id = $1 AND source = 'web'`, delivery); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneDeliveries(t.Context(), 30*24*time.Hour)
	if err != nil || pruned < 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}
	var left int
	if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_deliveries WHERE delivery_id = $1`, delivery).Scan(&left); err != nil || left != 1 {
		t.Fatalf("after pruning: %d left (%v), want the play one", left, err)
	}
}

func lapsedAt(t *testing.T, database *db.DB, userID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := database.QueryRowContext(t.Context(), `SELECT lapsed_at FROM users WHERE id = $1`, userID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestLapseTracking(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	userID, _ := newUser(t, database)
	never, _ := newUser(t, database)
	sub := "sub_" + uuid.NewV7().String()
	ended := now.Add(-2 * time.Hour).Truncate(time.Microsecond)
	if _, err := store.Apply(t.Context(), webEvent(userID, sub, billing.StatusActive, ended, now.Add(-3*time.Hour))); err != nil {
		t.Fatal(err)
	}

	if _, err := store.MarkLapsed(t.Context()); err != nil {
		t.Fatal(err)
	}
	if at := lapsedAt(t, database, userID); at == nil || !at.Equal(ended) {
		t.Fatalf("lapsed_at = %v, want when the paid period ended, %v", at, ended)
	}
	if at := lapsedAt(t, database, never); at != nil {
		t.Fatalf("a user who never subscribed lapsed at %v", at)
	}
	lapses, err := store.Lapses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range lapses {
		if l.UserID == userID {
			found = true
			if l.Notified || l.Warned || l.WarningDue || l.PurgeDue || !l.PurgeAt().Equal(ended.Add(billing.PurgeAfter)) {
				t.Fatalf("fresh lapse = %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("the lapse is not listed")
	}

	// Its purge comes 90 days later, and not before.
	if claimed, err := store.ClaimLapsePurge(t.Context(), userID); err != nil || claimed {
		t.Fatalf("early claim = %t, %v", claimed, err)
	}

	// Resubscribing ends the lapse and its email stamps at once.
	if _, err := database.ExecContext(t.Context(), `UPDATE users SET lapse_notified_at = now(), purge_warned_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(t.Context(), webEvent(userID, "sub_"+uuid.NewV7().String(), billing.StatusActive, now.Add(time.Hour), now)); err != nil {
		t.Fatal(err)
	}
	var stamped bool
	if err := database.QueryRowContext(t.Context(), `SELECT lapsed_at IS NOT NULL OR lapse_notified_at IS NOT NULL OR purge_warned_at IS NOT NULL
		FROM users WHERE id = $1`, userID).Scan(&stamped); err != nil || stamped {
		t.Fatalf("after resubscribing: lapse or stamps left (%v)", err)
	}
}

func TestLapseRevokedEndsAtTheRefund(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	now := time.Now()
	userID, _ := newUser(t, database)
	refunded := now.Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := store.Apply(t.Context(), webEvent(userID, "sub_"+uuid.NewV7().String(), billing.StatusRevoked, now.Add(20*24*time.Hour), refunded)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkLapsed(t.Context()); err != nil {
		t.Fatal(err)
	}
	if at := lapsedAt(t, database, userID); at == nil || !at.Equal(refunded) {
		t.Fatalf("lapsed_at = %v, want the refund, %v", at, refunded)
	}
}

func TestClaimLapsePurge(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := billing.NewStore(database)
	due, _ := newUser(t, database)
	deleting, _ := newUser(t, database)
	paying, _ := newUser(t, database)
	for _, userID := range []string{due, deleting, paying} {
		if _, err := database.ExecContext(t.Context(), `UPDATE users SET lapsed_at = now() - interval '91 days' WHERE id = $1`, userID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(t.Context(), `UPDATE users SET deletion_requested_at = now(), purge_at = now() + interval '30 days' WHERE id = $1`, deleting); err != nil {
		t.Fatal(err)
	}
	// Entitled again, before the lapse job noticed.
	if _, err := database.ExecContext(t.Context(), `INSERT INTO entitlements
		(source, external_id, user_id, product_id, status, will_renew, expires_at, environment, event_at, raw)
		VALUES ('web', $1, $2, 'monthly', 'active', true, now() + interval '1 day', 'production', now(), '{}')`,
		"sub_"+uuid.NewV7().String(), paying); err != nil {
		t.Fatal(err)
	}

	lapses, err := store.Lapses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lapses {
		if l.UserID == deleting || l.UserID == paying {
			t.Errorf("%s is listed as lapsed", l.UserID)
		}
		if l.UserID == due && (!l.WarningDue || !l.PurgeDue) {
			t.Errorf("a 91-day lapse = %+v, want warning and purge due", l)
		}
	}
	for userID, want := range map[string]bool{due: true, deleting: false, paying: false} {
		claimed, err := store.ClaimLapsePurge(t.Context(), userID)
		if err != nil || claimed != want {
			t.Errorf("claim of %s = %t, %v; want %t", userID, claimed, err, want)
		}
	}
	if claimed, err := store.ClaimLapsePurge(t.Context(), due); err != nil || claimed {
		t.Fatalf("second claim = %t, %v; want nothing left to claim", claimed, err)
	}
}
