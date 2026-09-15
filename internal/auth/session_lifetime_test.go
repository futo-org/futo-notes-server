package auth_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
)

// newDatabase is a migrated SQLite database in a temp directory, so these
// tests run without Postgres.
func newDatabase(t *testing.T) *db.DB {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"),
		BlobDir:     filepath.Join(root, "blobs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

func newUser(t *testing.T, database *db.DB) string {
	t.Helper()
	user, err := auth.UpsertUserByEmail(context.Background(), database, "person@example.test", "Person")
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

// theSession reads the one session row these tests open. Each test gets its
// own database, so there is never a second one to confuse it with.
func theSession(t *testing.T, database *db.DB) (id string, expiresAt time.Time) {
	t.Helper()
	var expiry db.Time
	if err := database.QueryRowContext(context.Background(),
		`SELECT id, expires_at FROM sessions`).Scan(&id, &expiry); err != nil {
		t.Fatal(err)
	}
	return id, expiry.Time
}

// lastUsed rewinds the clock on a session without sleeping: it rewrites the
// expiry as the one a session whose last authenticated request was ago in the
// past would be carrying. A row records only its expiry, which is its last
// use plus the lifetime it was opened with, so this is the same arithmetic
// the server does.
func lastUsed(t *testing.T, database *db.DB, ttl, ago time.Duration) {
	t.Helper()
	if _, err := database.ExecContext(context.Background(),
		`UPDATE sessions SET expires_at = $1`,
		db.Timestamp(time.Now().Add(-ago).Add(ttl))); err != nil {
		t.Fatal(err)
	}
}

func TestSlidingSessionOpensNinetyDaysOut(t *testing.T) {
	database := newDatabase(t)
	userID := newUser(t, database)

	if _, err := auth.CreateSession(context.Background(), database, userID, auth.SlidingSession); err != nil {
		t.Fatal(err)
	}

	_, expiresAt := theSession(t, database)
	if want := time.Now().Add(auth.SlidingSessionTTL); expiresAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("expires_at = %v, want about %v", expiresAt, want)
	}
}

func TestSlidingSessionExpiryFollowsTheLastRequest(t *testing.T) {
	database := newDatabase(t)
	userID := newUser(t, database)

	token, err := auth.CreateSession(context.Background(), database, userID, auth.SlidingSession)
	if err != nil {
		t.Fatal(err)
	}
	lastUsed(t, database, auth.SlidingSessionTTL, 2*time.Hour)
	_, before := theSession(t, database)

	session, err := auth.ValidateSession(context.Background(), database, token)
	if err != nil || session == nil {
		t.Fatalf("ValidateSession() = %#v, %v", session, err)
	}

	_, after := theSession(t, database)
	if !after.After(before) {
		t.Fatalf("expires_at stayed at %v after a request two hours later; the session did not slide", before)
	}
	if want := time.Now().Add(auth.SlidingSessionTTL); after.Sub(want).Abs() > time.Minute {
		t.Fatalf("expires_at = %v, want about %v — the slide should restore the full window", after, want)
	}
}

func TestSlidingSessionSkipsTheWriteInsideTheRenewInterval(t *testing.T) {
	database := newDatabase(t)
	userID := newUser(t, database)

	token, err := auth.CreateSession(context.Background(), database, userID, auth.SlidingSession)
	if err != nil {
		t.Fatal(err)
	}
	// Half an hour after the last request, well inside the hour the server
	// lets an expiry go stale. Any write would move expires_at forward, so an
	// unchanged value is proof no write happened.
	lastUsed(t, database, auth.SlidingSessionTTL, auth.SlidingRenewInterval/2)
	_, before := theSession(t, database)

	for range 3 {
		session, err := auth.ValidateSession(context.Background(), database, token)
		if err != nil || session == nil {
			t.Fatalf("ValidateSession() = %#v, %v", session, err)
		}
	}

	_, after := theSession(t, database)
	if !after.Equal(before) {
		t.Fatalf("expires_at moved from %v to %v; the row was written inside the renew interval", before, after)
	}
}

func TestSlidingSessionDiesAfterNinetyDaysOfSilence(t *testing.T) {
	database := newDatabase(t)
	userID := newUser(t, database)

	token, err := auth.CreateSession(context.Background(), database, userID, auth.SlidingSession)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := theSession(t, database)

	// An hour short of the cliff the session is still good, and that request
	// resets the window.
	lastUsed(t, database, auth.SlidingSessionTTL, auth.SlidingSessionTTL-time.Hour)
	if session, err := auth.ValidateSession(context.Background(), database, token); err != nil || session == nil {
		t.Fatalf("ValidateSession() = %#v, %v, want a live session an hour short of 90 days", session, err)
	}

	lastUsed(t, database, auth.SlidingSessionTTL, auth.SlidingSessionTTL+time.Minute)

	session, err := auth.ValidateSession(context.Background(), database, token)
	if err != nil {
		t.Fatal(err)
	}
	if session != nil {
		t.Fatalf("ValidateSession() = %#v, want nil for a session untouched for 90 days", session)
	}

	var remaining int
	if err := database.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sessions WHERE id = $1`, sessionID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("the dead session row survived being presented")
	}
}

func TestFixedSessionDoesNotSlide(t *testing.T) {
	database := newDatabase(t)
	userID := newUser(t, database)

	token, err := auth.CreateSession(context.Background(), database, userID, auth.FixedSession)
	if err != nil {
		t.Fatal(err)
	}
	_, opened := theSession(t, database)
	if want := time.Now().Add(auth.SessionTTL); opened.Sub(want).Abs() > time.Minute {
		t.Fatalf("expires_at = %v, want about %v", opened, want)
	}

	// Two hours of use is long past the renew interval; a sliding session
	// would have been written forward here.
	lastUsed(t, database, auth.SessionTTL, 2*time.Hour)
	_, before := theSession(t, database)
	if session, err := auth.ValidateSession(context.Background(), database, token); err != nil || session == nil {
		t.Fatalf("ValidateSession() = %#v, %v", session, err)
	}

	_, after := theSession(t, database)
	if !after.Equal(before) {
		t.Fatalf("expires_at moved from %v to %v; a fixed session must keep its issue-time expiry", before, after)
	}
}
