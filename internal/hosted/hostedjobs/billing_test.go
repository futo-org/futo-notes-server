package hostedjobs_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/hosted/mail"
)

// fakeWeb is a web provider whose subscriptions the test sets.
type fakeWeb struct {
	mu        sync.Mutex
	subs      map[string]billing.Event
	refreshed []string
	cancelled []string
	failing   map[string]bool
}

func newFakeWeb() *fakeWeb {
	return &fakeWeb{subs: map[string]billing.Event{}, failing: map[string]bool{}}
}

func (f *fakeWeb) Source() billing.Source { return billing.SourceWeb }

func (f *fakeWeb) Verify(context.Context, billing.Delivery) (string, []billing.Event, error) {
	return "", nil, billing.ErrInvalidDelivery
}

func (f *fakeWeb) Refresh(_ context.Context, id string) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, id)
	if f.failing[id] {
		return billing.Event{}, errors.New("provider down")
	}
	ev, ok := f.subs[id]
	if !ok {
		return billing.Event{}, errors.New("unknown subscription")
	}
	return ev, nil
}

func (f *fakeWeb) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing[id] {
		return errors.New("provider down")
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeWeb) CheckoutURL(context.Context, billing.Customer) (string, error) { return "", nil }
func (f *fakeWeb) PortalURL(context.Context, billing.Customer) (string, error)   { return "", nil }

func (f *fakeWeb) set(ev billing.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[ev.ExternalID] = ev
}

// recordingMailer records each email it accepts, and refuses them while fail
// is set.
type recordingMailer struct {
	mu   sync.Mutex
	sent []mail.Message
	fail error
}

func (m *recordingMailer) Send(_ context.Context, message mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, message)
	return nil
}

// to lists the categories of the emails sent to address, in order.
func (m *recordingMailer) to(address string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var categories []string
	for _, message := range m.sent {
		if message.To == address {
			categories = append(categories, message.Category)
		}
	}
	return categories
}

type billingEnv struct {
	t        *testing.T
	database *db.DB
	web      *fakeWeb
	mailer   *recordingMailer
	b        hostedjobs.Billing
}

func newBillingEnv(t *testing.T) *billingEnv {
	t.Helper()
	database := hostedtest.Postgres(t)
	e := &billingEnv{t: t, database: database, web: newFakeWeb(), mailer: &recordingMailer{}}
	e.b = hostedjobs.Billing{
		Database: database, Store: billing.NewStore(database), Providers: billing.Providers{Web: e.web},
		Notifier: mail.NewNotifier(database, e.mailer, "https://notes.example.test"),
	}
	return e
}

// user inserts a user without a region (so a purge needs no storage
// service) and returns its id and email.
func (e *billingEnv) user(email string) string {
	e.t.Helper()
	id := uuid.NewV7().String()
	if _, err := e.database.ExecContext(e.t.Context(), `INSERT INTO users (id, sub, name, email) VALUES ($1, $2, 'jobs billing', $3)`,
		id, "jobs-billing-"+id, email); err != nil {
		e.t.Fatal(err)
	}
	dbtest.CleanupExec(e.t, e.database, `DELETE FROM users WHERE id = $1`, id)
	return id
}

// subscribe gives userID a web subscription in status until expires,
// reported at eventAt, known to the fake provider too.
func (e *billingEnv) subscribe(userID string, status billing.Status, expires, eventAt time.Time) billing.Event {
	e.t.Helper()
	ev := billing.Event{
		Source: billing.SourceWeb, ExternalID: "sub_" + uuid.NewV7().String(), UserID: userID, ProductID: "monthly",
		Status: status, WillRenew: status == billing.StatusActive, ExpiresAt: &expires,
		Environment: billing.EnvironmentProduction, EventAt: eventAt,
	}
	if _, err := e.b.Store.Apply(e.t.Context(), ev); err != nil {
		e.t.Fatal(err)
	}
	e.web.set(ev)
	return ev
}

func (e *billingEnv) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.database.ExecContext(e.t.Context(), query, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *billingEnv) exists(userID string) bool {
	e.t.Helper()
	var n int
	if err := e.database.QueryRowContext(e.t.Context(), `SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

func (e *billingEnv) lapses() hostedjobs.LapseResult {
	e.t.Helper()
	result, err := hostedjobs.ProcessLapses(e.t.Context(), e.b)
	if err != nil {
		e.t.Fatalf("ProcessLapses = %+v, %v", result, err)
	}
	return result
}

// age moves userID's lapse days into the past.
func (e *billingEnv) age(userID string, days int) {
	e.t.Helper()
	e.exec(`UPDATE users SET lapsed_at = lapsed_at - make_interval(days => $2) WHERE id = $1`, userID, days)
}

func TestLapseEmailsThenPurge(t *testing.T) {
	e := newBillingEnv(t)
	now := time.Now()
	email := "lapse-" + uuid.NewV7().String() + "@example.test"
	userID := e.user(email)
	sub := e.subscribe(userID, billing.StatusActive, now.Add(-time.Minute), now.Add(-time.Hour))
	collectionID := uuid.NewV7().String()
	e.exec(`INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, userID)

	e.lapses()
	if got := e.mailer.to(email); !slices.Equal(got, []string{"lapsed"}) {
		t.Fatalf("after the lapse: %v", got)
	}
	e.lapses()
	if got := e.mailer.to(email); len(got) != 1 {
		t.Fatalf("a second run sent again: %v", got)
	}

	e.age(userID, 83)
	e.lapses()
	if got := e.mailer.to(email); !slices.Equal(got, []string{"lapsed", "purge_warning"}) {
		t.Fatalf("83 days after the lapse: %v", got)
	}
	if !e.exists(userID) {
		t.Fatal("purged at 83 days")
	}

	e.age(userID, 7)
	result := e.lapses()
	if result.Purged != 1 || e.exists(userID) {
		t.Fatalf("90 days after the lapse: %+v, user exists %t", result, e.exists(userID))
	}
	if got := e.mailer.to(email); !slices.Equal(got, []string{"lapsed", "purge_warning", "purged"}) {
		t.Fatalf("emails = %v, want lapsed, purge_warning, purged", got)
	}
	// Cancelled first: the row said it would renew.
	if !slices.Contains(e.web.cancelled, sub.ExternalID) {
		t.Fatalf("cancelled %v, want %s", e.web.cancelled, sub.ExternalID)
	}
	// All three emails of one lapse share its purge date and carry keys.
	for _, m := range e.mailer.sent {
		if m.To == email && m.IdempotencyKey == "" {
			t.Fatalf("%s email has no idempotency key", m.Category)
		}
	}
}

func TestLapseHeldByAFailedSendAndEndedByResubscribing(t *testing.T) {
	e := newBillingEnv(t)
	now := time.Now()
	email := "held-" + uuid.NewV7().String() + "@example.test"
	userID := e.user(email)
	e.subscribe(userID, billing.StatusExpired, now.Add(-time.Hour), now.Add(-time.Hour))
	e.lapses()
	e.age(userID, 95)

	// The warning fails to send: no purge until it went out.
	e.mailer.fail = errors.New("resend down")
	result, err := hostedjobs.ProcessLapses(t.Context(), e.b)
	if err == nil || result.Failed != 1 || result.Purged != 0 || !e.exists(userID) {
		t.Fatalf("with the mailer failing: %+v, %v", result, err)
	}
	e.mailer.fail = nil

	// Resubscribing before the next run ends the lapse and its stamps.
	e.subscribe(userID, billing.StatusActive, now.Add(30*24*time.Hour), now)
	if result := e.lapses(); result.Purged != 0 || !e.exists(userID) {
		t.Fatalf("after resubscribing: %+v", result)
	}
	var lapsed, stamped bool
	if err := e.database.QueryRowContext(t.Context(), `SELECT lapsed_at IS NOT NULL, lapse_notified_at IS NOT NULL FROM users WHERE id = $1`,
		userID).Scan(&lapsed, &stamped); err != nil || lapsed || stamped {
		t.Fatalf("lapse %t, stamp %t (%v); want both cleared", lapsed, stamped, err)
	}
}

func TestLapsePurgeWithoutAnAddress(t *testing.T) {
	e := newBillingEnv(t)
	now := time.Now()
	userID := e.user("")
	e.subscribe(userID, billing.StatusRevoked, now.Add(time.Hour), now.Add(-time.Hour))
	e.lapses()
	e.age(userID, 90)
	if result := e.lapses(); result.Purged != 1 || e.exists(userID) {
		t.Fatalf("lapse purge of a user without an address = %+v", result)
	}
}

func TestLapseSkipsAccountsPendingDeletion(t *testing.T) {
	e := newBillingEnv(t)
	now := time.Now()
	email := "deleting-" + uuid.NewV7().String() + "@example.test"
	userID := e.user(email)
	e.subscribe(userID, billing.StatusExpired, now.Add(-time.Hour), now.Add(-time.Hour))
	e.exec(`UPDATE users SET deletion_requested_at = now(), purge_at = now() + interval '30 days' WHERE id = $1`, userID)
	e.lapses()
	e.age(userID, 100)
	e.lapses()
	if got := e.mailer.to(email); len(got) != 0 || !e.exists(userID) {
		t.Fatalf("an account pending deletion got %v, exists %t", got, e.exists(userID))
	}
}

func TestAccountPurgeCancelsRenewalsFirst(t *testing.T) {
	e := newBillingEnv(t)
	now := time.Now()
	userID := e.user("purge-" + uuid.NewV7().String() + "@example.test")
	sub := e.subscribe(userID, billing.StatusActive, now.Add(time.Hour), now)
	e.exec(`UPDATE users SET deletion_requested_at = now(), purge_at = now() - interval '1 minute' WHERE id = $1`, userID)

	e.web.failing[sub.ExternalID] = true
	if result, err := hostedjobs.PurgeAccounts(t.Context(), e.b); err == nil || result.Failed < 1 || !e.exists(userID) {
		t.Fatalf("purge with cancellation failing = %+v, %v; want the account kept", result, err)
	}
	e.web.failing[sub.ExternalID] = false
	if result, err := hostedjobs.PurgeAccounts(t.Context(), e.b); err != nil || e.exists(userID) {
		t.Fatalf("purge = %+v, %v", result, err)
	}
	if !slices.Contains(e.web.cancelled, sub.ExternalID) {
		t.Fatalf("cancelled %v", e.web.cancelled)
	}
	// A deletion the user asked for sends no lapse email.
	if len(e.mailer.sent) != 0 {
		t.Fatalf("sent %v", e.mailer.sent)
	}
}

func TestReconcileEntitlements(t *testing.T) {
	e := newBillingEnv(t)
	now := time.Now()
	userID := e.user("reconcile@example.test")

	ending := e.subscribe(userID, billing.StatusActive, now.Add(time.Hour), now.Add(-time.Hour))
	renewed := ending
	renewed.ExpiresAt = new(now.Add(30 * 24 * time.Hour))
	renewed.EventAt = now
	e.web.set(renewed)

	quiet := e.subscribe(userID, billing.StatusActive, now.Add(20*24*time.Hour), now.Add(-8*24*time.Hour))
	e.exec(`UPDATE entitlements SET updated_at = now() - interval '8 days' WHERE external_id = $1`, quiet.ExternalID)
	fresh := e.subscribe(userID, billing.StatusActive, now.Add(20*24*time.Hour), now)
	expired := e.subscribe(userID, billing.StatusExpired, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour))
	e.exec(`UPDATE entitlements SET updated_at = now() - interval '20 days' WHERE external_id = $1`, expired.ExternalID)
	broken := e.subscribe(userID, billing.StatusActive, now.Add(time.Hour), now)
	e.web.failing[broken.ExternalID] = true

	result, err := hostedjobs.ReconcileEntitlements(t.Context(), e.b.Store, e.b.Providers)
	if err == nil || result.Failed != 1 {
		t.Fatalf("reconciliation = %+v, %v; want the broken one failed", result, err)
	}
	for _, id := range []string{ending.ExternalID, quiet.ExternalID, broken.ExternalID} {
		if !slices.Contains(e.web.refreshed, id) {
			t.Errorf("%s was not refreshed", id)
		}
	}
	for _, id := range []string{fresh.ExternalID, expired.ExternalID} {
		if slices.Contains(e.web.refreshed, id) {
			t.Errorf("%s was refreshed", id)
		}
	}
	var expires time.Time
	if err := e.database.QueryRowContext(t.Context(), `SELECT expires_at FROM entitlements WHERE external_id = $1`, ending.ExternalID).Scan(&expires); err != nil ||
		!expires.Equal(renewed.ExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("expires_at = %v (%v), want the renewal's %v", expires, err, renewed.ExpiresAt)
	}

	// The quiet one had nothing newer; it is not asked again for a week.
	e.web.refreshed = nil
	e.web.failing[broken.ExternalID] = false
	if _, err := hostedjobs.ReconcileEntitlements(t.Context(), e.b.Store, e.b.Providers); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.web.refreshed, quiet.ExternalID) {
		t.Fatal("a subscription with nothing newer was asked again at once")
	}

	// A source without a provider is skipped.
	unconfigured, err := hostedjobs.ReconcileEntitlements(t.Context(), e.b.Store, billing.Providers{})
	if err != nil || unconfigured.Skipped != unconfigured.Checked {
		t.Fatalf("without providers = %+v, %v", unconfigured, err)
	}
}

func TestBillingJobsInTheMaintenanceGroup(t *testing.T) {
	e := newBillingEnv(t)
	var names []string
	for _, job := range hostedjobs.Maintenance(e.b, nil, nil) {
		names = append(names, job.Name)
	}
	want := []string{"entitlement reconciliation", "subscription lapses", "account purge", "billing delivery pruning"}
	at := -1
	for _, name := range want {
		i := slices.Index(names, name)
		if i <= at {
			t.Fatalf("maintenance jobs = %v, want %v in that order", names, want)
		}
		at = i
	}
}
