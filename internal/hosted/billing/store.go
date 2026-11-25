package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/ids"
)

// Store failures.
var (
	// ErrOwnedElsewhere reports a store subscription that belongs to another
	// live account. It is never moved: the submitter must sign in to the
	// owning account.
	ErrOwnedElsewhere = errors.New("store subscription belongs to another account")
	// ErrUnknownUser reports an event that names no account: a web event
	// whose subject matches no account, or a store event whose account token matches no
	// account and whose subscription has no row yet.
	ErrUnknownUser = errors.New("billing event names no account")
)

// EntitledSQL is an SQL condition, true while the account whose id is the
// expression userID has a row that entitles it at now(): active until
// expires_at, or in grace until grace_until.
func EntitledSQL(userID string) string {
	return `EXISTS (SELECT 1 FROM entitlements e WHERE e.user_id = ` + userID + ` AND (
		(e.status = 'active' AND e.expires_at > now()) OR (e.status = 'grace' AND e.grace_until > now())))`
}

// entitlementEnd is the SQL for when row e stopped entitling its account,
// for a row that no longer does: its paid period's end, cut short by a
// revocation or hold reported before then.
const entitlementEnd = `CASE e.status
	WHEN 'active' THEN e.expires_at
	WHEN 'grace' THEN e.grace_until
	ELSE least(e.event_at, coalesce(greatest(e.expires_at, e.grace_until), e.event_at)) END`

// Store keeps the entitlement rows and the webhook delivery log.
type Store struct {
	database *db.DB
}

// NewStore returns a Store over database, which must have the hosted
// migrations applied.
func NewStore(database *db.DB) *Store {
	return &Store{database: database}
}

// Applied is what applying an event did.
type Applied struct {
	// UserID owns the subscription.
	UserID string
	// Stale reports an event older than the stored one, which changed
	// nothing.
	Stale bool
}

// Apply records ev, a provider's view of a subscription, and returns its
// owner. A web event belongs to the account whose sub is ev.Subject. A store event belongs to the
// account its row already names, or else to the account ev.AccountToken
// names. An event older than the stored one changes nothing. It returns
// ErrUnknownUser when no account owns the event.
func (s *Store) Apply(ctx context.Context, ev Event) (Applied, error) {
	return s.inTx(ctx, func(tx *db.Tx) (Applied, error) { return apply(ctx, tx, ev, "") })
}

// Submit records a store event an app submitted as userID. The subscription
// binds to userID unless it already belongs to another account, by its row
// or by an account token naming another live account: then it returns
// ErrOwnedElsewhere and changes nothing. An event without an account token
// binds by its row alone.
func (s *Store) Submit(ctx context.Context, userID string, ev Event) (Applied, error) {
	return s.inTx(ctx, func(tx *db.Tx) (Applied, error) { return apply(ctx, tx, ev, userID) })
}

// ApplyDelivery records webhook delivery deliveryID of source and applies
// its events, in one transaction. A delivery already recorded is skipped
// and reported as a duplicate. Events naming no account are acknowledged and
// listed in unknown, so a provider does not retry them forever.
func (s *Store) ApplyDelivery(ctx context.Context, source Source, deliveryID string, events []Event) (duplicate bool, unknown []Event, err error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Discard()
	result, err := tx.ExecContext(ctx, `INSERT INTO billing_deliveries (source, delivery_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, string(source), deliveryID)
	if err != nil {
		return false, nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return err == nil, nil, err
	}
	for _, ev := range events {
		_, err := apply(ctx, tx, ev, "")
		if errors.Is(err, ErrUnknownUser) {
			unknown = append(unknown, ev)
			continue
		}
		if err != nil {
			return false, nil, err
		}
	}
	return false, unknown, tx.Commit()
}

func (s *Store) inTx(ctx context.Context, run func(*db.Tx) (Applied, error)) (Applied, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return Applied{}, err
	}
	defer tx.Discard()
	applied, err := run(tx)
	if err != nil {
		return Applied{}, err
	}
	return applied, tx.Commit()
}

// apply records ev through tx. submitter is the account an app submitted it
// as, or "" for a webhook or a refresh.
func apply(ctx context.Context, tx *db.Tx, ev Event, submitter string) (Applied, error) {
	if ev.ExternalID == "" || ev.EventAt.IsZero() {
		return Applied{}, fmt.Errorf("billing event from %s lacks a subscription id or time", ev.Source)
	}
	// Locks the subscription's row, so two events for it apply one after
	// the other.
	var owner string
	var storedAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT user_id, event_at FROM entitlements
		WHERE source = $1 AND external_id = $2 FOR UPDATE`, string(ev.Source), ev.ExternalID).Scan(&owner, &storedAt)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Applied{}, err
	}
	if ev.Source == SourceWeb {
		owner, err = webOwner(ctx, tx, ev, owner)
	} else {
		owner, err = storeOwner(ctx, tx, ev, owner, submitter)
	}
	if err != nil {
		return Applied{}, err
	}
	if exists && ev.EventAt.Before(storedAt) {
		return Applied{UserID: owner, Stale: true}, nil
	}
	raw := ev.Raw
	if len(raw) == 0 || !json.Valid(raw) {
		raw = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entitlements
			(source, external_id, user_id, product_id, status, will_renew, expires_at, grace_until, environment, event_at, raw, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT (source, external_id) DO UPDATE SET
			user_id = EXCLUDED.user_id, product_id = EXCLUDED.product_id, status = EXCLUDED.status,
			will_renew = EXCLUDED.will_renew, expires_at = EXCLUDED.expires_at, grace_until = EXCLUDED.grace_until,
			environment = EXCLUDED.environment, event_at = EXCLUDED.event_at, raw = EXCLUDED.raw, updated_at = now()`,
		string(ev.Source), ev.ExternalID, owner, ev.ProductID, string(ev.Status), ev.WillRenew,
		ev.ExpiresAt, ev.GraceUntil, string(ev.Environment), ev.EventAt, []byte(raw)); err != nil {
		return Applied{}, err
	}
	// Resubscribing ends the lapse countdown at once, not at the next job
	// run, so the lapse purge never takes an account that is paying again.
	// Once the purge has claimed the account (purge_at set) it is past
	// saving.
	if _, err := tx.ExecContext(ctx, `UPDATE users SET lapsed_at = NULL,
			lapse_notified_at = NULL, purge_warned_at = NULL, purge_notified_at = NULL
		WHERE id = $1 AND lapsed_at IS NOT NULL AND purge_at IS NULL AND `+EntitledSQL("users.id"), owner); err != nil {
		return Applied{}, err
	}
	return Applied{UserID: owner}, nil
}

// webOwner returns the account a web event belongs to: the one whose sub it
// names, which must be the row's owner if there is a row.
func webOwner(ctx context.Context, tx *db.Tx, ev Event, rowOwner string) (string, error) {
	if ev.Subject == "" {
		return "", ErrUnknownUser
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE sub = $1`, ev.Subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownUser
	}
	if err != nil {
		return "", err
	}
	if rowOwner != "" && rowOwner != id {
		return "", fmt.Errorf("web subscription %s belongs to %s, not %s", ev.ExternalID, rowOwner, id)
	}
	return id, nil
}

// Customer returns userID as the web provider knows it: by its OIDC sub.
func (s *Store) Customer(ctx context.Context, userID string) (Customer, error) {
	c := Customer{UserID: userID}
	err := s.database.QueryRowContext(ctx, `SELECT sub, coalesce(email, '') FROM users WHERE id = $1`, userID).Scan(&c.Subject, &c.Email)
	return c, err
}

// storeOwner returns the account a store event belongs to. A subscription
// with a row stays with its owner. Without one, it binds to the submitter,
// unless its account token names another live account; a webhook binds it
// to the account its token names, and one without a token names no account.
func storeOwner(ctx context.Context, tx *db.Tx, ev Event, rowOwner, submitter string) (string, error) {
	tokenOwner := ""
	if ids.IsUUID(ev.AccountToken) {
		err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE store_account_token = $1`, ev.AccountToken).Scan(&tokenOwner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	switch {
	case rowOwner != "" && submitter != "" && rowOwner != submitter:
		return "", ErrOwnedElsewhere
	case rowOwner != "":
		return rowOwner, nil
	case submitter != "" && tokenOwner != "" && tokenOwner != submitter:
		return "", ErrOwnedElsewhere
	case submitter != "":
		return submitter, nil
	case tokenOwner != "":
		return tokenOwner, nil
	}
	return "", ErrUnknownUser
}

// State is an account's billing state, as GET /api/billing reports it.
type State struct {
	Entitled bool
	// Current is the row that best describes the account: the entitling row
	// that runs longest, or else the one with the latest news. Nil when the
	// account never subscribed.
	Current *Entitlement
	// StoreAccountToken is what the apps pass to StoreKit and Play.
	StoreAccountToken string
}

// Entitlement is one stored subscription.
type Entitlement struct {
	Source      Source
	ExternalID  string
	ProductID   string
	Status      Status
	WillRenew   bool
	ExpiresAt   *time.Time
	GraceUntil  *time.Time
	Environment Environment
	EventAt     time.Time
}

// State returns userID's billing state.
func (s *Store) State(ctx context.Context, userID string) (State, error) {
	var state State
	err := s.database.QueryRowContext(ctx, `SELECT store_account_token, `+EntitledSQL("u.id")+`
		FROM users u WHERE u.id = $1`, userID).Scan(&state.StoreAccountToken, &state.Entitled)
	if err != nil {
		return State{}, err
	}
	var e Entitlement
	err = s.database.QueryRowContext(ctx, `SELECT source, external_id, product_id, status, will_renew,
			expires_at, grace_until, environment, event_at
		FROM entitlements e WHERE user_id = $1
		ORDER BY ((e.status = 'active' AND e.expires_at > now()) OR (e.status = 'grace' AND e.grace_until > now())) DESC,
			greatest(e.expires_at, e.grace_until) DESC NULLS LAST, e.event_at DESC
		LIMIT 1`, userID).
		Scan(&e.Source, &e.ExternalID, &e.ProductID, &e.Status, &e.WillRenew, &e.ExpiresAt, &e.GraceUntil, &e.Environment, &e.EventAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return State{}, err
	}
	state.Current = &e
	return state, nil
}

// Ref names one subscription at its provider.
type Ref struct {
	Source     Source
	ExternalID string
}

// Stale returns the subscriptions reconciliation refreshes: those still
// live (active, grace or on hold) that end within soon, or that have had no
// news for longer than quiet. Expired and revoked rows are final, and a
// provider reports a resubscription itself.
func (s *Store) Stale(ctx context.Context, soon, quiet time.Duration) ([]Ref, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT source, external_id FROM entitlements e
		WHERE e.status IN ('active', 'grace', 'on_hold')
			AND (greatest(e.expires_at, e.grace_until) < now() + make_interval(secs => $1)
				OR e.updated_at < now() - make_interval(secs => $2))
		ORDER BY e.updated_at`, soon.Seconds(), quiet.Seconds())
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}

// Touch records that the provider was asked about ref and had nothing newer
// than what is stored, so reconciliation does not ask again until quiet
// passes.
func (s *Store) Touch(ctx context.Context, ref Ref) error {
	_, err := s.database.ExecContext(ctx, `UPDATE entitlements SET updated_at = now()
		WHERE source = $1 AND external_id = $2`, string(ref.Source), ref.ExternalID)
	return err
}

// Cancellable returns userID's subscriptions the server can stop renewing:
// web and Play ones that are not over. The App Store's cannot be cancelled
// from the server.
func (s *Store) Cancellable(ctx context.Context, userID string) ([]Ref, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT source, external_id FROM entitlements
		WHERE user_id = $1 AND source IN ('web', 'play') AND status NOT IN ('expired', 'revoked') AND will_renew
		ORDER BY source, external_id`, userID)
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}

func scanRefs(rows *sql.Rows) ([]Ref, error) {
	defer rows.Close()
	var refs []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.Source, &r.ExternalID); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// PruneDeliveries deletes the delivery log's entries older than age.
func (s *Store) PruneDeliveries(ctx context.Context, age time.Duration) (int64, error) {
	result, err := s.database.ExecContext(ctx, `DELETE FROM billing_deliveries
		WHERE received_at < now() - make_interval(secs => $1)`, age.Seconds())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Lapse retention (launch plan C5): a lapsed account stays read-only, is
// warned PurgeWarningAfter its lapse, and purged PurgeAfter it.
const (
	PurgeAfter        = 90 * 24 * time.Hour
	PurgeWarningAfter = PurgeAfter - 7*24*time.Hour
)

// MarkLapsed sets lapsed_at on every account that has subscribed but is no
// longer entitled and has no lapse recorded: to when its last entitlement
// ended, or now if that is later. It returns how many it marked.
func (s *Store) MarkLapsed(ctx context.Context) (int64, error) {
	result, err := s.database.ExecContext(ctx, `UPDATE users u
		SET lapsed_at = least(now(), (SELECT max(`+entitlementEnd+`) FROM entitlements e WHERE e.user_id = u.id))
		WHERE u.lapsed_at IS NULL
			AND EXISTS (SELECT 1 FROM entitlements e WHERE e.user_id = u.id)
			AND NOT `+EntitledSQL("u.id"))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ClearResubscribed ends the lapse of every account that is entitled again
// and not yet claimed by the purge, clearing its lapse email stamps so a
// later lapse is announced afresh. Apply already does this for the account
// an event concerns; this catches what that missed. It returns how many it
// cleared.
func (s *Store) ClearResubscribed(ctx context.Context) (int64, error) {
	result, err := s.database.ExecContext(ctx, `UPDATE users SET lapsed_at = NULL,
			lapse_notified_at = NULL, purge_warned_at = NULL, purge_notified_at = NULL
		WHERE lapsed_at IS NOT NULL AND purge_at IS NULL AND `+EntitledSQL("users.id"))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Lapse is a lapsed account awaiting its emails or its purge.
type Lapse struct {
	UserID   string
	LapsedAt time.Time
	// Notified and Warned report that the lapse and purge warning emails
	// went out.
	Notified bool
	Warned   bool
	// WarningDue and PurgeDue report that PurgeWarningAfter and PurgeAfter
	// have passed since the lapse.
	WarningDue bool
	PurgeDue   bool
}

// PurgeAt is when the lapsed account is purged.
func (l Lapse) PurgeAt() time.Time { return l.LapsedAt.Add(PurgeAfter) }

// Lapses returns the lapsed accounts that are neither entitled, nor pending
// deletion, nor already claimed by the purge, oldest lapse first. An account
// whose owner asked to delete it is purged sooner by that request and is
// not emailed about its lapse.
func (s *Store) Lapses(ctx context.Context) ([]Lapse, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT id, lapsed_at, lapse_notified_at IS NOT NULL,
			purge_warned_at IS NOT NULL, lapsed_at <= now() - make_interval(secs => $1),
			lapsed_at <= now() - make_interval(secs => $2)
		FROM users
		WHERE lapsed_at IS NOT NULL AND deletion_requested_at IS NULL AND purge_at IS NULL
			AND NOT `+EntitledSQL("users.id")+`
		ORDER BY lapsed_at, id`, PurgeWarningAfter.Seconds(), PurgeAfter.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lapses []Lapse
	for rows.Next() {
		var l Lapse
		if err := rows.Scan(&l.UserID, &l.LapsedAt, &l.Notified, &l.Warned, &l.WarningDue, &l.PurgeDue); err != nil {
			return nil, err
		}
		lapses = append(lapses, l)
	}
	return lapses, rows.Err()
}

// ClaimLapsePurge schedules userID's purge now, if its lapse is still
// PurgeAfter old, it is still not entitled, and nothing else scheduled one.
// The account purge then takes it like any other; from here on resubscribing
// no longer saves it. It reports whether it claimed the account.
func (s *Store) ClaimLapsePurge(ctx context.Context, userID string) (bool, error) {
	result, err := s.database.ExecContext(ctx, `UPDATE users SET purge_at = now()
		WHERE id = $1 AND lapsed_at <= now() - make_interval(secs => $2)
			AND purge_at IS NULL AND deletion_requested_at IS NULL AND NOT `+EntitledSQL("users.id"),
		userID, PurgeAfter.Seconds())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// Entitlements lists userID's subscriptions, newest news first.
func (s *Store) Entitlements(ctx context.Context, userID string) ([]Entitlement, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT source, external_id, product_id, status, will_renew,
			expires_at, grace_until, environment, event_at
		FROM entitlements WHERE user_id = $1 ORDER BY event_at DESC, source, external_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Entitlement
	for rows.Next() {
		var e Entitlement
		if err := rows.Scan(&e.Source, &e.ExternalID, &e.ProductID, &e.Status, &e.WillRenew,
			&e.ExpiresAt, &e.GraceUntil, &e.Environment, &e.EventAt); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}
