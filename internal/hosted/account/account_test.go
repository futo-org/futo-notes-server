package account_test

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"golang.org/x/oauth2"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/hostedtest"
)

// clock is a settable time source.
type clock struct{ now atomic.Pointer[time.Time] }

func newClock() *clock {
	c := &clock{}
	c.set(time.Now().UTC().Truncate(time.Millisecond))
	return c
}

func (c *clock) set(t time.Time)                       { c.now.Store(&t) }
func (c *clock) advance(d time.Duration)               { c.set(c.get().Add(d)) }
func (c *clock) get() time.Time                        { return *c.now.Load() }
func (c *clock) store(s *account.Store) *account.Store { return s.WithClock(c.get) }

func newUser(t *testing.T, database *db.DB, store *account.Store) auth.User {
	t.Helper()
	sub := "account-test-" + uuid.NewV7().String()
	user, err := store.UpsertUser(t.Context(), account.Identity{Subject: sub, Email: sub + "@example.test", Name: "Account Test"})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, user.ID)
	return user
}

// rotationKey derives refresh token successors in these tests.
var rotationKey = bytes.Repeat([]byte{0x17}, account.RotationKeySize)

const verifier = "dBjftJeZ4CVP-mJ92K9YQ4WllYjv4Ac4lDmRVLz4xBxl0vpEqbr7iw"

var challenge = oauth2.S256ChallengeFromVerifier(verifier)

// appSession signs a user in the way an app does and returns its session.
func appSession(t *testing.T, store *account.Store, user auth.User) account.AppSession {
	t.Helper()
	code, err := store.CreateAppCode(t.Context(), user.ID, challenge, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RedeemAppCode(t.Context(), code, verifier, account.Device{UserAgent: "test-device/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestUpdateUserNeverCreates(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	sub := "update-" + uuid.NewV7().String()
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE sub = $1`, sub)
	if _, err := store.UpdateUser(t.Context(), account.Identity{Subject: sub, Email: sub + "@example.test", Name: "None"}); !errors.Is(err, account.ErrNoUser) {
		t.Fatalf("UpdateUser of an unknown subject = %v, want ErrNoUser", err)
	}
	var users int
	if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM users WHERE sub = $1`, sub).Scan(&users); err != nil || users != 0 {
		t.Fatalf("users with the subject = %d, %v", users, err)
	}

	first, err := store.UpsertUser(t.Context(), account.Identity{Subject: sub, Email: "a-" + sub + "@example.test", Name: "First"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.UpdateUser(t.Context(), account.Identity{Subject: sub, Email: "b-" + sub + "@example.test", Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Name != "Second" || second.Email != "b-"+sub+"@example.test" {
		t.Fatalf("update = %#v, first = %#v", second, first)
	}
}

func TestUpsertUserBySubRefreshesProfile(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	sub := "upsert-" + uuid.NewV7().String()
	first, err := store.UpsertUser(t.Context(), account.Identity{Subject: sub, Email: "a-" + sub + "@example.test", Name: "First"})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, first.ID)
	second, err := store.UpsertUser(t.Context(), account.Identity{Subject: sub, Email: "b-" + sub + "@example.test", Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Name != "Second" || second.Email != "b-"+sub+"@example.test" {
		t.Fatalf("second sign-in = %#v, first = %#v", second, first)
	}

	// The provider owns email uniqueness: another subject may arrive with an
	// address a stale row still holds.
	other, err := store.UpsertUser(t.Context(), account.Identity{Subject: sub + "-other", Email: second.Email, Name: "Other"})
	if err != nil {
		t.Fatalf("second subject with the same email: %v", err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, other.ID)
	if other.ID == first.ID {
		t.Fatal("a different subject reused the user")
	}
}

func TestSessionsSlideAndExpire(t *testing.T) {
	database := hostedtest.Postgres(t)
	c := newClock()
	store := c.store(account.New(database, rotationKey))
	ctx := t.Context()
	user := newUser(t, database, store)

	token, err := store.CreateSession(ctx, user.ID, "browser/1.0", c.get())
	if err != nil {
		t.Fatal(err)
	}
	created := c.get()
	expiry := func() time.Time {
		t.Helper()
		var expiresAt time.Time
		if err := database.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE access_token_hash = $1`,
			auth.HashToken(token)).Scan(&expiresAt); err != nil {
			t.Fatal(err)
		}
		return expiresAt.UTC()
	}
	if got := expiry(); !got.Equal(created.Add(account.SessionTTL)) {
		t.Fatalf("expiry = %v, want %v", got, created.Add(account.SessionTTL))
	}

	c.advance(5 * time.Minute)
	if p, err := store.Authenticate(ctx, token); err != nil || p.Slid {
		t.Fatalf("Authenticate within the touch interval = %#v, %v; want no slide", p, err)
	}
	if got := expiry(); !got.Equal(created.Add(account.SessionTTL)) {
		t.Fatalf("expiry after a use within the touch interval = %v, want it unchanged", got)
	}

	c.advance(10 * time.Minute)
	used := c.get()
	p, err := store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Session.User.ID != user.ID || p.RegionID != "" || p.PendingDeletion() || !p.Slid {
		t.Fatalf("principal = %#v", p)
	}
	if got := expiry(); !got.Equal(used.Add(account.SessionTTL)) {
		t.Fatalf("expiry after a later use = %v, want %v", got, used.Add(account.SessionTTL))
	}

	c.advance(account.SessionTTL)
	if _, err := store.Authenticate(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("Authenticate after expiry = %v, want ErrNoSession", err)
	}
	var remaining int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, user.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired session kept, %d rows", remaining)
	}
}

func TestRefreshTokenRotationAndReuseDetection(t *testing.T) {
	database := hostedtest.Postgres(t)
	c := newClock()
	store := c.store(account.New(database, rotationKey))
	ctx := t.Context()
	user := newUser(t, database, store)
	session := appSession(t, store, user)

	if _, err := store.ExchangeRefreshToken(ctx, session.RefreshToken); !errors.Is(err, account.ErrNoRegion) {
		t.Fatalf("exchange without a region = %v, want ErrNoRegion", err)
	}
	if err := store.ChooseRegion(ctx, user.ID, "us-east"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExchangeRefreshToken(ctx, "not-a-token"); !errors.Is(err, account.ErrInvalidRefreshToken) {
		t.Fatalf("unknown token = %v, want ErrInvalidRefreshToken", err)
	}

	first, err := store.ExchangeRefreshToken(ctx, session.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if first.UserID != user.ID || first.RegionID != "us-east" || first.RefreshToken == "" || first.RefreshToken == session.RefreshToken {
		t.Fatalf("exchange = %#v", first)
	}

	if first.Reissued {
		t.Fatalf("first exchange reported as reissued: %#v", first)
	}

	// The response was lost: the device presents the same token again and
	// gets the same successor, signed in still.
	c.advance(time.Hour)
	again, err := store.ExchangeRefreshToken(ctx, session.RefreshToken)
	if err != nil {
		t.Fatalf("re-presenting a token whose successor is unused = %v", err)
	}
	if again.RefreshToken != first.RefreshToken || !again.Reissued || again.RegionID != "us-east" {
		t.Fatalf("re-presented exchange = %#v, want the same successor %q", again, first.RefreshToken)
	}
	second, err := store.ExchangeRefreshToken(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("the successor after a lost response: %v", err)
	}
	if second.RefreshToken == first.RefreshToken || second.Reissued {
		t.Fatalf("second exchange = %#v", second)
	}

	// The chain has moved on past the token's successor: reuse.
	if _, err := store.ExchangeRefreshToken(ctx, session.RefreshToken); !errors.Is(err, account.ErrRefreshTokenReused) {
		t.Fatalf("token whose successor was exchanged = %v, want ErrRefreshTokenReused", err)
	}
	if _, err := store.ExchangeRefreshToken(ctx, second.RefreshToken); !errors.Is(err, account.ErrInvalidRefreshToken) {
		t.Fatalf("current token after reuse = %v, want the session revoked", err)
	}
	if _, err := store.Authenticate(ctx, session.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("session after reuse = %v, want revoked", err)
	}
}

func TestRefreshTokensStoreHashesOnly(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	ctx := t.Context()
	user := newUser(t, database, store)
	if err := store.ChooseRegion(ctx, user.ID, "us-east"); err != nil {
		t.Fatal(err)
	}
	session := appSession(t, store, user)
	exchange, err := store.ExchangeRefreshToken(ctx, session.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{session.RefreshToken, exchange.RefreshToken} {
		var n int
		if err := database.QueryRowContext(ctx, `SELECT count(*) FROM storage_refresh_tokens WHERE token_hash = $1`,
			auth.HashToken(token)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("rows under the hash of %q = %d, want 1", token, n)
		}
	}
}

func TestRefreshTokenAfterRotationKeyChange(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	ctx := t.Context()
	user := newUser(t, database, store)
	if err := store.ChooseRegion(ctx, user.ID, "us-east"); err != nil {
		t.Fatal(err)
	}
	session := appSession(t, store, user)
	exchange, err := store.ExchangeRefreshToken(ctx, session.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	rotated := account.New(database, bytes.Repeat([]byte{0x5a}, account.RotationKeySize))
	if _, err := rotated.ExchangeRefreshToken(ctx, session.RefreshToken); !errors.Is(err, account.ErrInvalidRefreshToken) {
		t.Fatalf("spent token after the rotation key changed = %v, want ErrInvalidRefreshToken", err)
	}
	if _, err := rotated.Authenticate(ctx, session.Token); err != nil {
		t.Fatalf("a token derived under the old key must not revoke the session: %v", err)
	}
	// The successor itself was stored, so it still exchanges under the new
	// key.
	if _, err := rotated.ExchangeRefreshToken(ctx, exchange.RefreshToken); err != nil {
		t.Fatalf("the unexchanged successor after the key changed = %v", err)
	}
}

func TestConcurrentRefreshExchangesShareOneSuccessor(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	ctx := t.Context()
	user := newUser(t, database, store)
	if err := store.ChooseRegion(ctx, user.ID, "eu-central"); err != nil {
		t.Fatal(err)
	}
	session := appSession(t, store, user)

	const racers = 8
	results := make([]account.Exchange, racers)
	errs := make([]error, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range racers {
		done.Go(func() {
			start.Wait()
			results[i], errs[i] = store.ExchangeRefreshToken(ctx, session.RefreshToken)
		})
	}
	start.Done()
	done.Wait()

	rotatedBy := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d error = %v, want every racer to succeed", i, err)
		}
		if results[i].RefreshToken != results[0].RefreshToken {
			t.Fatalf("racers got different successors: %q and %q", results[i].RefreshToken, results[0].RefreshToken)
		}
		if !results[i].Reissued {
			rotatedBy++
		}
	}
	if rotatedBy != 1 {
		t.Fatalf("racers that rotated the token = %d, want exactly 1", rotatedBy)
	}
	if _, err := store.Authenticate(ctx, session.Token); err != nil {
		t.Fatalf("concurrent exchanges must not revoke the session: %v", err)
	}
	if _, err := store.ExchangeRefreshToken(ctx, results[0].RefreshToken); err != nil {
		t.Fatalf("the shared successor = %v", err)
	}
}

func TestRefreshExchangeRefusedWhilePendingDeletion(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	ctx := t.Context()
	user := newUser(t, database, store)
	if err := store.ChooseRegion(ctx, user.ID, "us-east"); err != nil {
		t.Fatal(err)
	}
	session := appSession(t, store, user)
	p, err := store.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequestDeletion(ctx, user.ID, p.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExchangeRefreshToken(ctx, session.RefreshToken); !errors.Is(err, account.ErrPendingDeletion) {
		t.Fatalf("exchange while pending deletion = %v, want ErrPendingDeletion", err)
	}
	if err := store.Restore(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExchangeRefreshToken(ctx, session.RefreshToken); err != nil {
		t.Fatalf("a refused exchange must leave the token unspent: %v", err)
	}
	// Re-presenting the spent token is refused the same way.
	if err := store.RequestDeletion(ctx, user.ID, p.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExchangeRefreshToken(ctx, session.RefreshToken); !errors.Is(err, account.ErrPendingDeletion) {
		t.Fatalf("re-presented token while pending deletion = %v, want ErrPendingDeletion", err)
	}
}

func TestAppCodesAreSingleUseAndBoundToTheChallenge(t *testing.T) {
	database := hostedtest.Postgres(t)
	c := newClock()
	store := c.store(account.New(database, rotationKey))
	ctx := t.Context()
	user := newUser(t, database, store)

	code, err := store.CreateAppCode(ctx, user.ID, challenge, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemAppCode(ctx, code, verifier+"x", account.Device{UserAgent: "ua"}); !errors.Is(err, account.ErrCodeVerifier) {
		t.Fatalf("wrong verifier = %v, want ErrCodeVerifier", err)
	}
	if _, err := store.RedeemAppCode(ctx, code, verifier, account.Device{UserAgent: "ua"}); !errors.Is(err, account.ErrInvalidCode) {
		t.Fatalf("code after a wrong verifier = %v, want it spent", err)
	}

	code, err = store.CreateAppCode(ctx, user.ID, challenge, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RedeemAppCode(ctx, code, verifier, account.Device{UserAgent: "ua"})
	if err != nil {
		t.Fatal(err)
	}
	if session.User != user || session.Token == "" || session.RefreshToken == "" {
		t.Fatalf("session = %#v", session)
	}
	if _, err := store.RedeemAppCode(ctx, code, verifier, account.Device{UserAgent: "ua"}); !errors.Is(err, account.ErrInvalidCode) {
		t.Fatalf("replayed code = %v, want ErrInvalidCode", err)
	}

	code, err = store.CreateAppCode(ctx, user.ID, challenge, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if _, err := store.RedeemAppCode(ctx, code, verifier, account.Device{UserAgent: "ua"}); !errors.Is(err, account.ErrInvalidCode) {
		t.Fatalf("expired code = %v, want ErrInvalidCode", err)
	}
}

func TestLoginsAreTakenOnceAndExpire(t *testing.T) {
	database := hostedtest.Postgres(t)
	c := newClock()
	store := c.store(account.New(database, rotationKey))
	ctx := t.Context()

	login := account.Login{State: auth.GenerateToken(), Nonce: "n", CodeVerifier: "v",
		AppRedirectURI: "http://127.0.0.1:1234/cb", AppState: "s", AppCodeChallenge: challenge, Reauthenticate: true}
	if err := store.SaveLogin(ctx, login); err != nil {
		t.Fatal(err)
	}
	got, err := store.TakeLogin(ctx, login.State)
	if err != nil || got != login {
		t.Fatalf("TakeLogin = %#v, %v; want %#v", got, err, login)
	}
	if _, err := store.TakeLogin(ctx, login.State); !errors.Is(err, account.ErrUnknownLogin) {
		t.Fatalf("second TakeLogin = %v, want ErrUnknownLogin", err)
	}

	stale := account.Login{State: auth.GenerateToken(), Nonce: "n", CodeVerifier: "v"}
	if err := store.SaveLogin(ctx, stale); err != nil {
		t.Fatal(err)
	}
	c.advance(account.LoginTTL + time.Second)
	if _, err := store.DeleteExpiredLogins(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakeLogin(ctx, stale.State); !errors.Is(err, account.ErrUnknownLogin) {
		t.Fatalf("expired login = %v, want ErrUnknownLogin", err)
	}
}

func TestRegionIsChosenOnceAndDeletionCanBeRestored(t *testing.T) {
	database := hostedtest.Postgres(t)
	c := newClock()
	store := c.store(account.New(database, rotationKey))
	ctx := t.Context()
	user := newUser(t, database, store)

	if err := store.ChooseRegion(ctx, user.ID, "us-east"); err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"us-east", "eu-central"} {
		if err := store.ChooseRegion(ctx, user.ID, region); !errors.Is(err, account.ErrRegionChosen) {
			t.Fatalf("second choice %s = %v, want ErrRegionChosen", region, err)
		}
	}

	keep, err := store.CreateSession(ctx, user.ID, "keep", c.get())
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateSession(ctx, user.ID, "other", c.get())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Authenticate(ctx, keep)
	if err != nil {
		t.Fatal(err)
	}
	requested := c.get()
	if err := store.RequestDeletion(ctx, user.ID, p.Session.ID); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	if err := store.RequestDeletion(ctx, user.ID, p.Session.ID); err != nil {
		t.Fatal(err)
	}
	a, err := store.Account(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.RegionID != "us-east" || a.DeletionRequestedAt == nil || !a.DeletionRequestedAt.Equal(requested) ||
		!a.PurgeAt.Equal(requested.Add(account.DeletionGrace)) {
		t.Fatalf("account = %#v, want deletion scheduled from the first request", a)
	}
	if _, err := store.Authenticate(ctx, other); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("other session after deletion request = %v, want signed out", err)
	}
	if p, err := store.Authenticate(ctx, keep); err != nil || !p.PendingDeletion() {
		t.Fatalf("requesting session = %#v, %v; want kept and pending deletion", p, err)
	}

	if err := store.Restore(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if a, err := store.Account(ctx, user.ID); err != nil || a.DeletionRequestedAt != nil || a.PurgeAt != nil {
		t.Fatalf("restored account = %#v, %v", a, err)
	}
}

func TestRestoreRefusesOnceThePurgeIsDue(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	ctx := t.Context()
	user := newUser(t, database, store)

	if err := store.Restore(ctx, user.ID); err != nil {
		t.Fatalf("restoring an account with no deletion pending = %v", err)
	}
	if err := store.RequestDeletion(ctx, user.ID, uuid.NewV7().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE users SET purge_at = now() - interval '1 second' WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, user.ID); !errors.Is(err, account.ErrPurgeStarted) {
		t.Fatalf("restore after the grace period = %v, want ErrPurgeStarted", err)
	}
	if a, err := store.Account(ctx, user.ID); err != nil || a.DeletionRequestedAt == nil || a.PurgeAt == nil {
		t.Fatalf("account after a refused restore = %#v, %v; want still pending", a, err)
	}
}

func TestScheduleDeletionNeedsARecentSignIn(t *testing.T) {
	database := hostedtest.Postgres(t)
	c := newClock()
	store := c.store(account.New(database, rotationKey))
	ctx := t.Context()
	user := newUser(t, database, store)

	signedIn := c.get().Add(-account.ReauthWindow)
	token, err := store.CreateSession(ctx, user.ID, "browser", signedIn)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if !p.AuthenticatedAt.Equal(signedIn) || !store.RecentlyAuthenticated(p) {
		t.Fatalf("principal signed in at %v, recent %v; want %v and recent", p.AuthenticatedAt, store.RecentlyAuthenticated(p), signedIn)
	}

	c.advance(time.Second)
	if err := store.ScheduleDeletion(ctx, p); !errors.Is(err, account.ErrReauthenticationRequired) {
		t.Fatalf("deletion with a sign-in past the window = %v, want ErrReauthenticationRequired", err)
	}
	if a, err := store.Account(ctx, user.ID); err != nil || a.DeletionRequestedAt != nil {
		t.Fatalf("account after a refused deletion = %#v, %v; want nothing scheduled", a, err)
	}

	// Signing in again on this session makes it recent.
	if ok, err := store.Reauthenticate(ctx, token, uuid.NewV7().String(), c.get()); err != nil || ok {
		t.Fatalf("reauthenticating another user's session = %v, %v; want refused", ok, err)
	}
	if ok, err := store.Reauthenticate(ctx, token, user.ID, c.get()); err != nil || !ok {
		t.Fatalf("Reauthenticate = %v, %v", ok, err)
	}
	if p, err = store.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := store.ScheduleDeletion(ctx, p); err != nil {
		t.Fatalf("deletion after signing in again = %v", err)
	}
	if a, err := store.Account(ctx, user.ID); err != nil || a.DeletionRequestedAt == nil {
		t.Fatalf("account after deletion = %#v, %v", a, err)
	}
}

func TestAppSessionsKeepTheSignInTime(t *testing.T) {
	database := hostedtest.Postgres(t)
	store := account.New(database, rotationKey)
	ctx := t.Context()
	user := newUser(t, database, store)
	signedIn := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	code, err := store.CreateAppCode(ctx, user.ID, challenge, signedIn)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RedeemAppCode(ctx, code, verifier, account.Device{Name: "Pixel", UserAgent: "ua"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !p.AuthenticatedAt.Equal(signedIn) || store.RecentlyAuthenticated(p) {
		t.Fatalf("app session signed in at %v, want %v and not recent", p.AuthenticatedAt, signedIn)
	}
}
