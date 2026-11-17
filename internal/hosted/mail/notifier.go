package mail

import (
	"context"
	"errors"
	"fmt"
	"time"

	"futo-notes-server/internal/db"
)

// ErrNoAddress reports a user with no email address to send to: their ID
// token carried no email claim.
var ErrNoAddress = errors.New("user has no email address")

// stampColumns is the users column that records each lapse email as sent.
var stampColumns = map[Template]string{
	Lapsed:       "lapse_notified_at",
	PurgeWarning: "purge_warned_at",
	Purged:       "purge_notified_at",
}

// Notifier sends the lapse emails, each at most once per user.
type Notifier struct {
	database     *db.DB
	mailer       Mailer
	dashboardURL string
}

// NewNotifier returns a Notifier that sends through mailer and links to
// publicURL's dashboard. database must have the hosted migrations applied.
func NewNotifier(database *db.DB, mailer Mailer, publicURL string) *Notifier {
	return &Notifier{database: database, mailer: mailer, dashboardURL: publicURL + "/dashboard"}
}

// SendOnce sends user userID the lapse email t, for a purge at purgeAt, to
// the address their last sign-in stored, unless t's stamp is already set on
// the user. It sets the stamp only after the mailer accepted the email, so a
// failed send is retried by the next call and a retried job never sends
// twice. sent reports whether an email went out, even when stamping it then
// failed.
//
// The email carries an idempotency key made of the user, t and the user's
// lapse (lapsed_at), so a send whose stamp failed to save is not delivered
// again by the next call within the provider's window, while a later lapse
// is announced afresh. purgeAt must be the same on every call for one lapse,
// so a repeated send is the same message.
//
// Two calls for one user at once could both send: callers run under the job
// lock (hostedjobs). The purge deletes the users row, so it must send Purged
// after deleting the account's data and before deleting the row.
func (n *Notifier) SendOnce(ctx context.Context, userID string, t Template, purgeAt time.Time) (sent bool, err error) {
	column, ok := stampColumns[t]
	if !ok {
		return false, ErrUnknownTemplate
	}
	var email string
	var stamped bool
	var lapsedAt *time.Time
	err = n.database.QueryRowContext(ctx, `SELECT email, `+column+` IS NOT NULL, lapsed_at FROM users WHERE id = $1`, userID).
		Scan(&email, &stamped, &lapsedAt)
	if err != nil {
		return false, fmt.Errorf("reading user %s: %w", userID, err)
	}
	if stamped {
		return false, nil
	}
	if email == "" {
		return false, ErrNoAddress
	}
	message, err := Render(t, Data{Email: email, PurgeAt: purgeAt, DashboardURL: n.dashboardURL})
	if err != nil {
		return false, err
	}
	cycle := purgeAt
	if lapsedAt != nil {
		cycle = *lapsedAt
	}
	message.IdempotencyKey = idempotencyKey(userID, t, cycle)
	if err := n.mailer.Send(ctx, message); err != nil {
		return false, fmt.Errorf("sending %s email: %w", t, err)
	}
	if _, err := n.database.ExecContext(ctx, `UPDATE users SET `+column+` = now() WHERE id = $1 AND `+column+` IS NULL`, userID); err != nil {
		return true, fmt.Errorf("stamping %s email: %w", t, err)
	}
	return true, nil
}

// idempotencyKey names one lapse email: user, template and the lapse it is
// about.
func idempotencyKey(userID string, t Template, lapse time.Time) string {
	return fmt.Sprintf("lapse-email/%s/%s/%d", userID, t, lapse.UTC().UnixMicro())
}
