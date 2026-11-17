package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/hostedauth"
)

type accountBody struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Region *struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	} `json:"region"`
	Deletion *struct {
		RequestedAt time.Time `json:"requested_at"`
		PurgeAt     time.Time `json:"purge_at"`
	} `json:"deletion"`
	Usage struct {
		Collections int64 `json:"collections"`
		Blobs       int64 `json:"blobs"`
		StoredBytes int64 `json:"stored_bytes"`
	} `json:"usage"`
}

type errorBody struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	RequestID  string `json:"requestId"`
}

func expectHostedError(t *testing.T, res response, status int) {
	t.Helper()
	var body errorBody
	res.json(t, &body)
	if res.status != status || body.StatusCode != status || body.Message == "" || body.RequestID == "" {
		t.Fatalf("response = %d %s, want a %d hosted error", res.status, res.body, status)
	}
}

func TestCapabilityAndRegions(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	res := c.expect(http.MethodGet, "/", nil, http.StatusOK)
	want := `{"name":"futo-notes","version":"test","auth_mode":"oidc","signup":"open","billing":false,"mutation_ids":{"supported":true,"required":false,"retention_days":30,"successful_create_outcomes":"durable"}}` + "\n"
	if res.body != want {
		t.Fatalf("capability = %s, want %s", res.body, want)
	}
	res = c.expect(http.MethodGet, "/api/regions", nil, http.StatusOK)
	want = `[{"id":"us-east","name":"us-east name","description":"us-east description","endpoint":"http://storage-us-east.test"},{"id":"eu-central","name":"eu-central name","description":"eu-central description","endpoint":"http://storage-eu-central.test"}]` + "\n"
	if res.body != want {
		t.Fatalf("regions = %s, want %s", res.body, want)
	}
}

func TestDashboardSignInSetsTheSessionCookie(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)

	var account accountBody
	browser.expect(http.MethodGet, "/api/account", nil, http.StatusOK).json(t, &account)
	if account.User.Email != sub+"@example.test" || account.User.Name != "Test "+sub || account.Region != nil || account.Deletion != nil {
		t.Fatalf("account = %#v", account)
	}
	res := browser.expect(http.MethodGet, "/api/auth", nil, http.StatusOK)
	if !strings.Contains(res.body, `"id":"`+account.User.ID+`"`) {
		t.Fatalf("current user = %s", res.body)
	}

	// Signing in again keeps the same user, keyed by sub.
	again := h.dashboardLogin(sub)
	var second accountBody
	again.expect(http.MethodGet, "/api/account", nil, http.StatusOK).json(t, &second)
	if second.User.ID != account.User.ID {
		t.Fatalf("second sign-in user = %s, want %s", second.User.ID, account.User.ID)
	}

	browser.expect(http.MethodPost, "/api/auth/logout", nil, http.StatusNoContent)
	expectHostedError(t, browser.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized)
	again.expect(http.MethodGet, "/api/account", nil, http.StatusOK)
}

func TestCallbackRejectsBadState(t *testing.T) {
	h := newHarness(t)
	browser := h.client()
	callback := browser.authorize(browser.do(http.MethodGet, "/api/auth/oidc/start", nil), h.newSub())

	// Another browser holds no state cookie for this login.
	expectHostedError(t, h.client().do(http.MethodGet, callback, nil), http.StatusBadRequest)

	tampered, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	q := tampered.Query()
	q.Set("state", "not-the-state")
	tampered.RawQuery = q.Encode()
	expectHostedError(t, browser.do(http.MethodGet, tampered.String(), nil), http.StatusBadRequest)

	// The rejections above cleared this browser's state cookie, so the login
	// can no longer complete here either.
	expectHostedError(t, browser.do(http.MethodGet, callback, nil), http.StatusBadRequest)

	// A successful callback cannot be replayed.
	fresh := h.client()
	callback = fresh.authorize(fresh.do(http.MethodGet, "/api/auth/oidc/start", nil), h.newSub())
	if res := fresh.do(http.MethodGet, callback, nil); res.status != http.StatusSeeOther {
		t.Fatalf("callback = %d %s", res.status, res.body)
	}
	expectHostedError(t, fresh.do(http.MethodGet, callback, nil), http.StatusBadRequest)
}

func TestAppSignIn(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.client()
	back := browser.appCode(sub, "http://127.0.0.1:43123/callback?from=app")
	if back.Host != "127.0.0.1:43123" || back.Path != "/callback" || back.Query().Get("from") != "app" ||
		back.Query().Get("state") != "app-state" || back.Query().Get("code") == "" {
		t.Fatalf("redirect back to the app = %s", back)
	}
	code := back.Query().Get("code")

	app := h.client()
	var tokens appTokens
	res := app.do(http.MethodPost, "/api/auth/oidc/token", map[string]string{"code": code, "code_verifier": verifier},
		"User-Agent", "FUTO Notes test app")
	if res.status != http.StatusOK || res.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token = %d %v %s", res.status, res.header, res.body)
	}
	res.json(t, &tokens)
	if tokens.Token == "" || tokens.StorageRefreshToken == "" || tokens.User.Email != sub+"@example.test" {
		t.Fatalf("tokens = %#v", tokens)
	}
	expectHostedError(t, app.do(http.MethodPost, "/api/auth/oidc/token",
		map[string]string{"code": code, "code_verifier": verifier}), http.StatusBadRequest)

	app.bearer = tokens.Token
	var sessions struct {
		Sessions []struct {
			ID        string `json:"id"`
			UserAgent string `json:"user_agent"`
			Current   bool   `json:"current"`
		} `json:"sessions"`
	}
	app.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK).json(t, &sessions)
	if len(sessions.Sessions) != 1 || !sessions.Sessions[0].Current || sessions.Sessions[0].UserAgent != "FUTO Notes test app" {
		t.Fatalf("sessions = %#v", sessions)
	}

	// A wrong verifier spends the code.
	back = h.client().appCode(sub, "http://[::1]:8080/cb")
	code = back.Query().Get("code")
	expectHostedError(t, app.do(http.MethodPost, "/api/auth/oidc/token",
		map[string]string{"code": code, "code_verifier": verifier + "x"}), http.StatusBadRequest)
	expectHostedError(t, app.do(http.MethodPost, "/api/auth/oidc/token",
		map[string]string{"code": code, "code_verifier": verifier}), http.StatusBadRequest)

	back = h.client().appCode(sub, appScheme+":/oauth")
	if back.Scheme != appScheme || back.Query().Get("code") == "" {
		t.Fatalf("custom scheme redirect = %s", back)
	}
}

func TestAppSignInRejectsDisallowedRedirects(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for _, redirect := range []string{
		"https://evil.example/cb",
		"http://evil.example:8080/cb",
		"http://localhost:8080/cb",
		"http://127.0.0.1/cb",
		"http://127.0.0.2:8080/cb",
		"http://user@127.0.0.1:8080/cb",
		"http://127.0.0.1:8080/cb#fragment",
		"other.scheme:/cb",
		"javascript:alert(1)",
	} {
		res := c.do(http.MethodGet, "/api/auth/oidc/start?"+url.Values{
			"redirect_uri": {redirect}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		}.Encode(), nil)
		expectHostedError(t, res, http.StatusBadRequest)
	}
	for _, params := range []url.Values{
		{"redirect_uri": {"http://127.0.0.1:8080/cb"}},
		{"redirect_uri": {"http://127.0.0.1:8080/cb"}, "code_challenge": {"short"}},
		{"redirect_uri": {"http://127.0.0.1:8080/cb"}, "code_challenge": {challenge}, "code_challenge_method": {"plain"}},
	} {
		expectHostedError(t, c.do(http.MethodGet, "/api/auth/oidc/start?"+params.Encode(), nil), http.StatusBadRequest)
	}
}

func TestRegionChoiceAndStorageTokens(t *testing.T) {
	h := newHarness(t)
	app, tokens := h.appLogin(h.newSub())

	res := app.do(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": tokens.StorageRefreshToken})
	expectHostedError(t, res, http.StatusConflict)

	expectHostedError(t, app.do(http.MethodPut, "/api/account/region", map[string]string{"region_id": "mars"}), http.StatusBadRequest)
	var account accountBody
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK).json(t, &account)
	if account.Region == nil || account.Region.ID != "us-east" || account.Region.Endpoint != "http://storage-us-east.test" {
		t.Fatalf("account after choosing = %#v", account)
	}
	for _, region := range []string{"us-east", "eu-central"} {
		expectHostedError(t, app.do(http.MethodPut, "/api/account/region", map[string]string{"region_id": region}), http.StatusConflict)
	}

	var issued struct {
		AccessToken  string    `json:"access_token"`
		ExpiresAt    time.Time `json:"expires_at"`
		RefreshToken string    `json:"refresh_token"`
		Region       struct {
			ID       string `json:"id"`
			Endpoint string `json:"endpoint"`
		} `json:"region"`
	}
	anonymous := h.client()
	res = anonymous.expect(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": tokens.StorageRefreshToken}, http.StatusOK)
	res.json(t, &issued)
	if issued.Region.ID != "us-east" || issued.Region.Endpoint != "http://storage-us-east.test" ||
		issued.RefreshToken == "" || issued.RefreshToken == tokens.StorageRefreshToken {
		t.Fatalf("storage token = %#v", issued)
	}
	verifierFor := func(region string) *hostedauth.Verifier {
		v, err := hostedauth.NewVerifier(h.apiKeys, region)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	access, err := verifierFor("us-east").VerifyAccess(issued.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	var accessClaims jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(issued.AccessToken, &accessClaims); err != nil {
		t.Fatal(err)
	}
	if access.UserID != tokens.User.ID || !accessClaims.ExpiresAt.Equal(issued.ExpiresAt) ||
		time.Until(accessClaims.ExpiresAt.Time) < 23*time.Hour {
		t.Fatalf("access token = %#v, exp %v, response expiry %v", access, accessClaims.ExpiresAt, issued.ExpiresAt)
	}
	if _, err := verifierFor("eu-central").VerifyAccess(issued.AccessToken); err == nil {
		t.Fatal("access token opens another region")
	}

	// The old token right after rotating reads as a concurrent exchange, not
	// reuse, and the new one keeps working.
	expectHostedError(t, anonymous.do(http.MethodPost, "/api/storage/token",
		map[string]string{"refresh_token": tokens.StorageRefreshToken}), http.StatusConflict)
	anonymous.expect(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": issued.RefreshToken}, http.StatusOK)
	expectHostedError(t, anonymous.do(http.MethodPost, "/api/storage/token",
		map[string]string{"refresh_token": "unknown"}), http.StatusUnauthorized)
	expectHostedError(t, anonymous.do(http.MethodPost, "/api/storage/token", map[string]string{}), http.StatusBadRequest)
}

func TestRefreshTokenReuseRevokesTheSession(t *testing.T) {
	h := newHarness(t)
	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "eu-central"}, http.StatusOK)
	app.expect(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": tokens.StorageRefreshToken}, http.StatusOK)
	// Move the rotation out of the concurrent-exchange grace.
	if _, err := h.database.ExecContext(t.Context(), `UPDATE storage_refresh_tokens SET rotated_at = rotated_at - interval '1 minute'
		WHERE rotated_at IS NOT NULL AND session_id IN (SELECT id FROM sessions WHERE user_id = $1)`, tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	expectHostedError(t, app.do(http.MethodPost, "/api/storage/token",
		map[string]string{"refresh_token": tokens.StorageRefreshToken}), http.StatusUnauthorized)
	if res := app.do(http.MethodGet, "/api/collections", nil); res.status != http.StatusUnauthorized ||
		res.body != `{"code":"invalid_session","error":"session expired or invalid"}`+"\n" {
		t.Fatalf("session after reuse = %d %s, want it revoked", res.status, res.body)
	}
}

type objectBody struct {
	Object struct {
		ID        string `json:"id"`
		Version   string `json:"version"`
		BlobKey   string `json:"blob_key"`
		SizeBytes string `json:"size_bytes"`
	} `json:"object"`
	CollectionVersion int64 `json:"collectionVersion"`
	Replayed          *bool `json:"replayed"`
}

func TestReceiptsThroughObjectEndpoints(t *testing.T) {
	h := newHarness(t)
	app, tokens := h.appLogin(h.newSub())
	userID := tokens.User.ID
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)
	objectsPath := "/api/collections/" + claimed.Collection.ID + "/objects"

	receipt, key := h.receipt("us-east", userID, 4096)
	var created objectBody
	res := app.do(http.MethodPost, objectsPath, map[string]string{"receipt": receipt}, "Mutation-Id", "create-1")
	if res.status != http.StatusCreated {
		t.Fatalf("create = %d %s", res.status, res.body)
	}
	res.json(t, &created)
	if created.Object.BlobKey != key || created.Object.SizeBytes != "4096" || created.Object.Version != "1" ||
		created.Replayed == nil || *created.Replayed {
		t.Fatalf("created = %s", res.body)
	}
	var replayed objectBody
	res = app.do(http.MethodPost, objectsPath, map[string]string{"receipt": receipt}, "Mutation-Id", "create-1")
	res.json(t, &replayed)
	if res.status != http.StatusCreated || replayed.Replayed == nil || !*replayed.Replayed || replayed.Object.ID != created.Object.ID {
		t.Fatalf("replay = %d %s", res.status, res.body)
	}

	res = app.do(http.MethodPost, objectsPath, map[string]string{"receipt": receipt}, "Mutation-Id", "create-2")
	if res.status != http.StatusConflict || res.body != `{"error":"receipt already used"}`+"\n" {
		t.Fatalf("reused receipt = %d %s", res.status, res.body)
	}

	invalid := `{"error":"invalid receipt"}` + "\n"
	otherUser, _ := h.receipt("us-east", "0195e2a1-0000-7000-8000-00000000abcd", 1)
	wrongRegion, _ := h.receipt("eu-central", userID, 1)
	expiredSigner, err := hostedauth.NewReceiptSigner(h.keys["us-east"], "us-east",
		hostedauth.WithClock(func() time.Time { return time.Now().Add(-25 * time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := expiredSigner.Sign(userID, userID+"/0195e2a1-0000-7000-8000-000000000003", 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]any{
		"wrong user":   map[string]string{"receipt": otherUser},
		"wrong region": map[string]string{"receipt": wrongRegion},
		"expired":      map[string]string{"receipt": expired},
		"missing":      map[string]string{},
		"garbage":      map[string]string{"receipt": "not.a.jwt"},
		"staged key":   map[string]any{"blob_key": key, "size_bytes": 1},
	} {
		if res := app.do(http.MethodPost, objectsPath, body); res.status != http.StatusBadRequest || res.body != invalid {
			t.Fatalf("%s receipt = %d %s, want 400 invalid receipt", name, res.status, res.body)
		}
	}
	if res := app.do(http.MethodPost, objectsPath, "not an object"); res.status != http.StatusBadRequest || res.body != `{"error":"invalid json"}`+"\n" {
		t.Fatalf("non-object body = %d %s", res.status, res.body)
	}

	objectPath := objectsPath + "/" + created.Object.ID
	update, updateKey := h.receipt("us-east", userID, 77)
	if res := app.do(http.MethodPut, objectPath, map[string]any{"receipt": update}); res.status != http.StatusBadRequest ||
		res.body != `{"error":"invalid version"}`+"\n" {
		t.Fatalf("update without version = %d %s", res.status, res.body)
	}
	if res := app.do(http.MethodPut, objectPath, map[string]any{"version": 2, "receipt": wrongRegion}); res.status != http.StatusBadRequest || res.body != invalid {
		t.Fatalf("update with a wrong-region receipt = %d %s", res.status, res.body)
	}
	var updated objectBody
	app.expect(http.MethodPut, objectPath, map[string]any{"version": 2, "receipt": update}, http.StatusOK).json(t, &updated)
	if updated.Object.BlobKey != updateKey || updated.Object.SizeBytes != "77" || updated.Object.Version != "2" {
		t.Fatalf("updated = %#v", updated)
	}
	stale, _ := h.receipt("us-east", userID, 5)
	if res := app.do(http.MethodPut, objectPath, map[string]any{"version": 2, "receipt": stale}); res.status != http.StatusConflict ||
		!strings.Contains(res.body, `"currentVersion":2`) {
		t.Fatalf("stale update = %d %s", res.status, res.body)
	}

	var account accountBody
	app.expect(http.MethodGet, "/api/account", nil, http.StatusOK).json(t, &account)
	if account.Usage.Collections != 1 || account.Usage.Blobs != 2 || account.Usage.StoredBytes != 4096+77 {
		t.Fatalf("usage = %#v", account.Usage)
	}
}

func TestDeletionGatesSyncAndStorageUntilRestored(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	app, tokens := h.appLogin(sub)
	other, _ := h.appLogin(sub)
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated)

	var account accountBody
	app.expect(http.MethodDelete, "/api/account", nil, http.StatusOK).json(t, &account)
	if account.Deletion == nil || account.Deletion.PurgeAt.Sub(account.Deletion.RequestedAt) != 30*24*time.Hour {
		t.Fatalf("account after delete = %#v", account.Deletion)
	}
	if res := other.do(http.MethodGet, "/api/collections", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("other device after delete = %d %s, want signed out", res.status, res.body)
	}

	pending := `{"error":"account pending deletion"}` + "\n"
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/auth"},
		{http.MethodGet, "/api/collections"},
		{http.MethodPost, "/api/collections"},
		{http.MethodGet, "/api/sync/events"},
	} {
		if res := app.do(req.method, req.path, nil); res.status != http.StatusForbidden || res.body != pending {
			t.Fatalf("%s %s while pending = %d %s", req.method, req.path, res.status, res.body)
		}
	}
	if res := app.do(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": tokens.StorageRefreshToken}); res.status != http.StatusForbidden || res.body != pending {
		t.Fatalf("storage token while pending = %d %s", res.status, res.body)
	}
	app.expect(http.MethodGet, "/api/account", nil, http.StatusOK)
	app.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK)

	app.expect(http.MethodPost, "/api/account/restore", nil, http.StatusOK).json(t, &account)
	if account.Deletion != nil {
		t.Fatalf("account after restore = %#v", account.Deletion)
	}
	app.expect(http.MethodGet, "/api/collections", nil, http.StatusOK)
	app.expect(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": tokens.StorageRefreshToken}, http.StatusOK)

	app.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	app.expect(http.MethodPost, "/api/auth/logout", nil, http.StatusNoContent)
}

func TestSessionsListAndRevoke(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)
	app, _ := h.appLogin(sub)
	second, secondTokens := h.appLogin(sub)
	second.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)

	var listed struct {
		Sessions []struct {
			ID         string    `json:"id"`
			CreatedAt  time.Time `json:"created_at"`
			LastUsedAt time.Time `json:"last_used_at"`
			ExpiresAt  time.Time `json:"expires_at"`
			Current    bool      `json:"current"`
		} `json:"sessions"`
	}
	app.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK).json(t, &listed)
	if len(listed.Sessions) != 3 {
		t.Fatalf("sessions = %#v, want 3", listed.Sessions)
	}
	current := 0
	for _, s := range listed.Sessions {
		if s.Current {
			current++
		}
		if s.ExpiresAt.Sub(s.LastUsedAt) < 89*24*time.Hour || s.CreatedAt.IsZero() {
			t.Fatalf("session = %#v", s)
		}
	}
	if current != 1 {
		t.Fatalf("current sessions = %d, want 1", current)
	}

	var browserSession, secondSession string
	var browserList, secondList struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	browser.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK).json(t, &browserList)
	second.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK).json(t, &secondList)
	for _, s := range browserList.Sessions {
		if s.Current {
			browserSession = s.ID
		}
	}
	for _, s := range secondList.Sessions {
		if s.Current {
			secondSession = s.ID
		}
	}

	app.expect(http.MethodDelete, "/api/sessions/"+browserSession, nil, http.StatusNoContent)
	expectHostedError(t, browser.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized)
	expectHostedError(t, app.do(http.MethodDelete, "/api/sessions/"+browserSession, nil), http.StatusNotFound)
	expectHostedError(t, app.do(http.MethodDelete, "/api/sessions/not-a-uuid", nil), http.StatusNotFound)

	// Revoking a device revokes its storage refresh token too.
	app.expect(http.MethodDelete, "/api/sessions/"+secondSession, nil, http.StatusNoContent)
	expectHostedError(t, app.do(http.MethodPost, "/api/storage/token",
		map[string]string{"refresh_token": secondTokens.StorageRefreshToken}), http.StatusUnauthorized)

	// Another user's session is not found.
	stranger, _ := h.appLogin(h.newSub())
	var mine struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	app.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK).json(t, &mine)
	expectHostedError(t, stranger.do(http.MethodDelete, "/api/sessions/"+mine.Sessions[0].ID, nil), http.StatusNotFound)
}

func TestAuthenticationFailuresKeepEachSurfaceShape(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	if res := c.do(http.MethodGet, "/api/collections", nil); res.status != http.StatusUnauthorized || res.body != `{"error":"unauthorized"}`+"\n" {
		t.Fatalf("sync without a token = %d %s", res.status, res.body)
	}
	c.bearer = "garbage"
	res := c.do(http.MethodGet, "/api/collections", nil)
	if res.status != http.StatusUnauthorized || res.body != `{"code":"invalid_session","error":"session expired or invalid"}`+"\n" ||
		res.header.Get("WWW-Authenticate") != `Bearer realm="futo-notes", error="invalid_token"` {
		t.Fatalf("sync with a dead token = %d %v %s", res.status, res.header, res.body)
	}
	expectHostedError(t, c.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized)
	c.bearer = ""
	expectHostedError(t, c.do(http.MethodGet, "/api/sessions", nil), http.StatusUnauthorized)
}

func TestRemovedBlobRoutesAreAbsent(t *testing.T) {
	h := newHarness(t)
	app, tokens := h.appLogin(h.newSub())
	collection := "0195e2a1-0000-7000-8000-000000000002"
	object := "0195e2a1-0000-7000-8000-000000000004"
	blob := tokens.User.ID + "/0195e2a1-0000-7000-8000-000000000005"
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/api/blobs"},
		{http.MethodGet, "/api/blobs/" + blob},
		{http.MethodHead, "/api/blobs/" + blob},
		{http.MethodDelete, "/api/blobs/" + blob},
		{http.MethodPost, "/api/blobs/batch"},
		{http.MethodPost, "/api/collections/" + collection + "/blob-objects"},
		{http.MethodPut, "/api/collections/" + collection + "/blob-objects/" + object},
		{http.MethodPost, "/api/collections/" + collection + "/blob-objects/batch"},
	} {
		res := app.do(req.method, req.path, nil)
		if res.status != http.StatusNotFound {
			t.Fatalf("%s %s = %d %s, want 404", req.method, req.path, res.status, res.body)
		}
		if req.method != http.MethodHead {
			expectHostedError(t, res, http.StatusNotFound)
		}
	}
	if res := app.do(http.MethodHead, "/api/collections", nil); res.status != http.StatusOK {
		t.Fatalf("HEAD on a sync GET route = %d", res.status)
	}
}

func TestSyncEventsReachStreamsThroughPostgres(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	listening := make(chan struct{})
	go func() {
		defer close(listening)
		events.Listen(ctx, hostedtest.URL(t), h.hub)
	}()
	t.Cleanup(func() {
		cancel()
		<-listening
	})

	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)

	want := `data: {"collectionId":"` + claimed.Collection.ID + `","currentVersion":1}`
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The listener may still be connecting, and connecting closes the
		// streams opened before it; reconnect as a client would.
		if found := readChange(t, h, tokens.Token, func() {
			receipt, _ := h.receipt("us-east", tokens.User.ID, 9)
			app.expect(http.MethodPost, "/api/collections/"+claimed.Collection.ID+"/objects",
				map[string]string{"receipt": receipt}, http.StatusCreated)
		}, want); found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no change event arrived")
		}
		// Later attempts find the collection at a higher version.
		want = `data: {"collectionId":"` + claimed.Collection.ID + `","currentVersion":`
	}
}

// readChange opens a sync event stream, runs mutate once it is ready, and
// reports whether a line starting with want arrives before the stream ends or
// two seconds pass.
func readChange(t *testing.T, h *harness, token string, mutate func(), want string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.api.URL+"/api/sync/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := h.client().http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sync events = %d %v", res.StatusCode, res.Header)
	}
	reader := bufio.NewReader(res.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: ready\n" {
		return false
	}
	mutate()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return false
		}
		if strings.HasPrefix(line, want) {
			var change map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &change); err != nil {
				t.Fatalf("change payload %q: %v", line, err)
			}
			return true
		}
	}
}
