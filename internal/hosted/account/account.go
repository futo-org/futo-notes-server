// Package account is the hosted API's account state in Postgres: users
// signed in through OIDC, their device sessions and storage refresh tokens,
// logins in flight, and the account itself (region, pending deletion). The
// HTTP API and the dashboard both act through it, so they cannot drift.
package account

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"uuid"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/db"
)

// DeletionGrace is how long a user can cancel a requested account deletion
// before the account is purged.
const DeletionGrace = 30 * 24 * time.Hour

// Domain failures.
var (
	// ErrNotFound reports that the user has no such session.
	ErrNotFound = errors.New("not found")
	// ErrRegionChosen reports that the user already chose a region; it is
	// fixed once chosen.
	ErrRegionChosen = errors.New("region already chosen")
	// ErrPendingDeletion reports that the user's account is scheduled for
	// deletion.
	ErrPendingDeletion = errors.New("account pending deletion")
	// ErrNoRegion reports that the user has not chosen a region yet.
	ErrNoRegion = errors.New("no region chosen")
	// ErrPurgeStarted reports that the account's deletion grace period is
	// over, so the deletion can no longer be cancelled.
	ErrPurgeStarted = errors.New("account purge already started")
	// ErrReauthenticationRequired reports that the session's sign-in is
	// older than ReauthWindow, too old to delete the account.
	ErrReauthenticationRequired = errors.New("reauthentication required")
)

// ReauthWindow is how recent a session's sign-in must be to delete the
// account.
const ReauthWindow = 10 * time.Minute

// Store reads and changes hosted account state.
type Store struct {
	database    *db.DB
	rotationKey []byte
	now         func() time.Time
}

// New returns a Store over database, which must have both migration tracks
// applied. rotationKey (RotationKeySize bytes) derives each storage refresh
// token's successor; replacing it invalidates every successor not yet
// exchanged.
func New(database *db.DB, rotationKey []byte) *Store {
	return &Store{database: database, rotationKey: rotationKey, now: time.Now}
}

// WithClock returns a copy of the store that reads the time from now, for
// tests.
func (s *Store) WithClock(now func() time.Time) *Store {
	return &Store{database: s.database, rotationKey: s.rotationKey, now: now}
}

// Identity is what the identity provider asserts about a signed-in user.
type Identity struct {
	Subject string
	Email   string
	Name    string
	// AuthTime is when the user last authenticated to the provider, from the
	// ID token's auth_time; zero when the token has none.
	AuthTime time.Time
}

// UpsertUser returns the user for the identity's subject, creating it on
// first sign-in and refreshing its email and name on later ones.
func (s *Store) UpsertUser(ctx context.Context, id Identity) (auth.User, error) {
	var u auth.User
	err := s.database.QueryRowContext(ctx, `INSERT INTO users (id, sub, email, name) VALUES ($1, $2, $3, $4)
		ON CONFLICT (sub) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name
		RETURNING id, email, name`, uuid.NewV7().String(), id.Subject, id.Email, id.Name).Scan(&u.ID, &u.Email, &u.Name)
	return u, err
}

// Account is a user's profile, region, deletion status and usage.
type Account struct {
	User auth.User
	// RegionID is "" until the user chooses a region.
	RegionID string
	// DeletionRequestedAt and PurgeAt are set while a deletion is pending.
	DeletionRequestedAt *time.Time
	PurgeAt             *time.Time
	Collections         int64
	// Blobs and StoredBytes count every blob the user's storage still holds,
	// including superseded ones awaiting garbage collection.
	Blobs       int64
	StoredBytes int64
}

// Account returns the user's account.
func (s *Store) Account(ctx context.Context, userID string) (Account, error) {
	var a Account
	var region sql.NullString
	err := s.database.QueryRowContext(ctx, `SELECT u.id, u.email, u.name, u.region_id,
			u.deletion_requested_at, u.purge_at,
			(SELECT count(*) FROM collections c WHERE c.user_id = u.id),
			(SELECT count(*) FROM blob_ledger b WHERE b.user_id = u.id),
			(SELECT coalesce(sum(b.size_bytes), 0) FROM blob_ledger b WHERE b.user_id = u.id)
		FROM users u WHERE u.id = $1`, userID).
		Scan(&a.User.ID, &a.User.Email, &a.User.Name, &region, &a.DeletionRequestedAt, &a.PurgeAt,
			&a.Collections, &a.Blobs, &a.StoredBytes)
	if err != nil {
		return Account{}, err
	}
	a.RegionID = region.String
	return a, nil
}

// ChooseRegion sets the user's region. It returns ErrRegionChosen when the
// user already has one, even the same one. The caller checks that regionID
// names a configured region.
func (s *Store) ChooseRegion(ctx context.Context, userID, regionID string) error {
	result, err := s.database.ExecContext(ctx, `UPDATE users SET region_id = $2 WHERE id = $1 AND region_id IS NULL`, userID, regionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRegionChosen
	}
	return nil
}

// RequestDeletion schedules the user's account for deletion DeletionGrace
// from now and signs out every session but keepSessionID. Requesting it again
// keeps the original schedule.
func (s *Store) RequestDeletion(ctx context.Context, userID, keepSessionID string) error {
	now := s.now()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Discard()
	if _, err := tx.ExecContext(ctx, `UPDATE users
		SET deletion_requested_at = coalesce(deletion_requested_at, $2), purge_at = coalesce(purge_at, $3)
		WHERE id = $1`, userID, now, now.Add(DeletionGrace)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = $1 AND id <> $2`, userID, keepSessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// RecentlyAuthenticated reports whether p's session signed in within
// ReauthWindow.
func (s *Store) RecentlyAuthenticated(p Principal) bool {
	return s.now().Sub(p.AuthenticatedAt) <= ReauthWindow
}

// ScheduleDeletion is RequestDeletion on behalf of p's session, which must
// have signed in within ReauthWindow: otherwise it returns
// ErrReauthenticationRequired and changes nothing.
func (s *Store) ScheduleDeletion(ctx context.Context, p Principal) error {
	if !s.RecentlyAuthenticated(p) {
		return ErrReauthenticationRequired
	}
	return s.RequestDeletion(ctx, p.Session.User.ID, p.Session.ID)
}

// Restore cancels a pending deletion. Restoring an account that is not
// pending deletion changes nothing. Once the grace period is over it returns
// ErrPurgeStarted: the purge may already have deleted the account's blobs.
func (s *Store) Restore(ctx context.Context, userID string) error {
	// The database clock decides, not s.now, and the purge job claims a user
	// by locking its row under the same clock (hostedjobs): one clock for
	// every replica, and whichever of the two takes the row first wins.
	result, err := s.database.ExecContext(ctx, `UPDATE users SET deletion_requested_at = NULL, purge_at = NULL
		WHERE id = $1 AND (purge_at IS NULL OR purge_at > clock_timestamp())`, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrPurgeStarted
	}
	return nil
}
