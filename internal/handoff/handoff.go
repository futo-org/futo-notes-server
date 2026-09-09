// Package handoff owns Login Hand-off tickets: the short-lived, single-use
// claim an app opens a browser login with, and later redeems for the session
// the server's identity-provider exchange produced. The app never touches
// provider tokens.
//
// A ticket records which account authenticated, never a session token. The
// session is opened when the ticket is redeemed, so this table never holds a
// usable bearer credential.
package handoff

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/db"
)

const (
	// TicketTTL is how long a freshly minted ticket stays claimable. It has
	// to cover the person's whole trip through the provider's login pages.
	TicketTTL = 10 * time.Minute

	// RedemptionTTL is how long a fulfilled ticket stays redeemable, giving
	// the app's poll a window that starts when the browser finishes rather
	// than when the ticket was minted.
	RedemptionTTL = 5 * time.Minute
)

// ticketBytes is how much entropy a ticket carries.
const ticketBytes = 32

// ValidTicket reports whether value is shaped like a minted ticket — 32
// random bytes, base64url, unpadded. The OIDC start route takes a ticket
// from an untrusted query parameter and carries it in a cookie, so anything
// else is discarded rather than stored.
func ValidTicket(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == ticketBytes
}

func newTicket() string {
	var b [ticketBytes]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Mint records a pending ticket and returns it.
func Mint(ctx context.Context, database *db.DB) (string, error) {
	ticket := newTicket()
	_, err := database.ExecContext(ctx,
		`INSERT INTO login_handoffs (ticket, expires_at) VALUES ($1, $2)`,
		ticket, db.Timestamp(time.Now().Add(TicketTTL)))
	if err != nil {
		return "", err
	}
	return ticket, nil
}

// Fulfil binds a pending ticket to the account that just authenticated and
// restarts the clock on the redemption window. A ticket that is unknown,
// already fulfilled, or expired matches nothing and is not an error: the
// browser still completed its login, and the app's poll answers 404 so it
// mints a fresh ticket.
func Fulfil(ctx context.Context, database *db.DB, ticket, userID string) error {
	now := time.Now()
	_, err := database.ExecContext(ctx,
		`UPDATE login_handoffs SET user_id = $1, expires_at = $2
		 WHERE ticket = $3 AND user_id IS NULL AND expires_at > $4`,
		userID, db.Timestamp(now.Add(RedemptionTTL)), ticket, db.Timestamp(now))
	return err
}

// Discard consumes a ticket the server refused to fulfil. Without it an app
// would poll a login that is never coming until the ticket aged out; with it
// the next poll is a 404 and the app can start over.
func Discard(ctx context.Context, database *db.DB, ticket string) error {
	_, err := database.ExecContext(ctx, `DELETE FROM login_handoffs WHERE ticket = $1`, ticket)
	return err
}

// RedeemCode says what a poll found.
type RedeemCode int

const (
	// RedeemGone is a ticket that is unknown, already redeemed, refused, or
	// past its window. They are one answer on purpose, so polling never
	// reveals whether a ticket exists.
	RedeemGone RedeemCode = iota
	// RedeemPending is a ticket still waiting on the browser.
	RedeemPending
	// RedeemOK carries the session the app asked for.
	RedeemOK
)

// Redeemed is the outcome of one poll, and for RedeemOK the login itself.
type Redeemed struct {
	Code  RedeemCode
	User  auth.User
	Token string
}

// Redeem consumes a fulfilled ticket and opens the session it stands for.
// Consuming the ticket and reading its account happen in one transaction, so
// exactly one caller of a given ticket ever reaches RedeemOK.
func Redeem(ctx context.Context, database *db.DB, ticket string) (Redeemed, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Redeemed{}, err
	}
	defer tx.Rollback()

	var userID sql.NullString
	var expiresAt db.Time
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM login_handoffs WHERE ticket = $1`+
			database.Dialect().ForUpdate(), ticket).
		Scan(&userID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Redeemed{Code: RedeemGone}, nil
	}
	if err != nil {
		return Redeemed{}, err
	}
	// Rows past their window read as gone here; the recurring sweep deletes them.
	if !expiresAt.Time.After(time.Now()) {
		return Redeemed{Code: RedeemGone}, nil
	}
	if !userID.Valid {
		return Redeemed{Code: RedeemPending}, nil
	}

	var user auth.User
	if err := tx.QueryRowContext(ctx, `SELECT id, email, name FROM users WHERE id = $1`, userID.String).
		Scan(&user.ID, &user.Email, &user.Name); err != nil {
		return Redeemed{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_handoffs WHERE ticket = $1`, ticket); err != nil {
		return Redeemed{}, err
	}
	if err := tx.Commit(); err != nil {
		return Redeemed{}, err
	}

	// The ticket is spent, so this session is opened for one caller only.
	token, err := auth.CreateSession(ctx, database, user.ID)
	if err != nil {
		return Redeemed{}, err
	}
	return Redeemed{Code: RedeemOK, User: user, Token: token}, nil
}
