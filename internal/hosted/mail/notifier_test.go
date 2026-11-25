package mail_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/hosted/mail"
)

// recordingMailer records what it sends, and fails while fail is set.
type recordingMailer struct {
	sent []mail.Message
	fail error
}

func (m *recordingMailer) Send(_ context.Context, message mail.Message) error {
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, message)
	return nil
}

// newUser inserts a user with email and removes it when the test ends.
func newUser(t *testing.T, database *db.DB, email string) string {
	t.Helper()
	id := uuid.NewV7().String()
	if _, err := database.ExecContext(t.Context(), `INSERT INTO users (id, sub, name, email) VALUES ($1, $2, 'mail test', $3)`,
		id, "mail-test-"+id, email); err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, id)
	return id
}

// stamps reads the user's three stamps.
func stamps(t *testing.T, database *db.DB, userID string) map[mail.Template]*time.Time {
	t.Helper()
	var lapse, warned, purged *time.Time
	if err := database.QueryRowContext(t.Context(), `SELECT lapse_notified_at, purge_warned_at, purge_notified_at FROM users WHERE id = $1`,
		userID).Scan(&lapse, &warned, &purged); err != nil {
		t.Fatal(err)
	}
	return map[mail.Template]*time.Time{mail.Lapsed: lapse, mail.PurgeWarning: warned, mail.Purged: purged}
}

func TestSendOnceSendsThenStamps(t *testing.T) {
	database := hostedtest.Postgres(t)
	purgeAt := time.Date(2027, time.March, 3, 12, 0, 0, 0, time.UTC)
	for _, tmpl := range mail.Templates {
		t.Run(string(tmpl), func(t *testing.T) {
			mailer := &recordingMailer{}
			notifier := mail.NewNotifier(database, mailer, "https://notes.example.test")
			userID := newUser(t, database, "send-once@example.test")

			before := time.Now()
			sent, err := notifier.SendOnce(t.Context(), userID, tmpl, purgeAt)
			if err != nil || !sent {
				t.Fatalf("SendOnce = %v, %v; want sent", sent, err)
			}
			want, err := mail.Render(tmpl, mail.Data{Email: "send-once@example.test", PurgeAt: purgeAt, DashboardURL: "https://notes.example.test/dashboard"})
			if err != nil {
				t.Fatal(err)
			}
			if len(mailer.sent) != 1 || mailer.sent[0] != want {
				t.Fatalf("sent %#v\nwant %#v", mailer.sent, want)
			}
			for other, stamp := range stamps(t, database, userID) {
				switch {
				case other == tmpl && (stamp == nil || stamp.Before(before.Add(-time.Minute))):
					t.Errorf("%s stamp = %v, want set now", other, stamp)
				case other != tmpl && stamp != nil:
					t.Errorf("%s stamp = %v, want unset: only %s was sent", other, stamp, tmpl)
				}
			}
		})
	}
}

func TestSendOnceSkipsAStampedUser(t *testing.T) {
	database := hostedtest.Postgres(t)
	mailer := &recordingMailer{}
	notifier := mail.NewNotifier(database, mailer, "https://notes.example.test")
	userID := newUser(t, database, "stamped@example.test")

	if sent, err := notifier.SendOnce(t.Context(), userID, mail.Lapsed, time.Now()); err != nil || !sent {
		t.Fatalf("first SendOnce = %v, %v", sent, err)
	}
	first := stamps(t, database, userID)[mail.Lapsed]
	for range 2 {
		if sent, err := notifier.SendOnce(t.Context(), userID, mail.Lapsed, time.Now()); err != nil || sent {
			t.Fatalf("SendOnce on a stamped user = %v, %v; want skipped", sent, err)
		}
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(mailer.sent))
	}
	if again := stamps(t, database, userID)[mail.Lapsed]; again == nil || !again.Equal(*first) {
		t.Fatalf("stamp moved from %v to %v", first, again)
	}

	// Another email has its own stamp.
	if sent, err := notifier.SendOnce(t.Context(), userID, mail.PurgeWarning, time.Now()); err != nil || !sent {
		t.Fatalf("SendOnce of another email = %v, %v; want sent", sent, err)
	}
}

func TestSendOnceFailedSendLeavesTheStampUnset(t *testing.T) {
	database := hostedtest.Postgres(t)
	refused := &mail.ResendError{StatusCode: 503, Name: "application_error", Message: "down"}
	mailer := &recordingMailer{fail: refused}
	notifier := mail.NewNotifier(database, mailer, "https://notes.example.test")
	userID := newUser(t, database, "failing@example.test")

	sent, err := notifier.SendOnce(t.Context(), userID, mail.PurgeWarning, time.Now())
	if sent || !errors.Is(err, refused) {
		t.Fatalf("SendOnce with a failing mailer = %v, %v; want the mailer's error", sent, err)
	}
	if stamp := stamps(t, database, userID)[mail.PurgeWarning]; stamp != nil {
		t.Fatalf("stamp = %v after a failed send, want unset", stamp)
	}

	// The retry sends.
	mailer.fail = nil
	if sent, err := notifier.SendOnce(t.Context(), userID, mail.PurgeWarning, time.Now()); err != nil || !sent {
		t.Fatalf("retry = %v, %v; want sent", sent, err)
	}
	if len(mailer.sent) != 1 || stamps(t, database, userID)[mail.PurgeWarning] == nil {
		t.Fatalf("after the retry: %d sent, stamp %v", len(mailer.sent), stamps(t, database, userID)[mail.PurgeWarning])
	}
}

func TestSendOnceRefuses(t *testing.T) {
	database := hostedtest.Postgres(t)
	mailer := &recordingMailer{}
	notifier := mail.NewNotifier(database, mailer, "https://notes.example.test")

	noAddress := newUser(t, database, "")
	if sent, err := notifier.SendOnce(t.Context(), noAddress, mail.Lapsed, time.Now()); sent || !errors.Is(err, mail.ErrNoAddress) {
		t.Errorf("SendOnce to a user without an address = %v, %v; want ErrNoAddress", sent, err)
	}
	if stamp := stamps(t, database, noAddress)[mail.Lapsed]; stamp != nil {
		t.Errorf("stamp = %v for a user without an address, want unset", stamp)
	}
	if sent, err := notifier.SendOnce(t.Context(), uuid.NewV7().String(), mail.Lapsed, time.Now()); sent || !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SendOnce to an unknown user = %v, %v; want sql.ErrNoRows", sent, err)
	}
	userID := newUser(t, database, "unknown-template@example.test")
	if sent, err := notifier.SendOnce(t.Context(), userID, "welcome", time.Now()); sent || !errors.Is(err, mail.ErrUnknownTemplate) {
		t.Errorf("SendOnce of an unknown template = %v, %v; want ErrUnknownTemplate", sent, err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("sent %d emails, want none", len(mailer.sent))
	}
}
