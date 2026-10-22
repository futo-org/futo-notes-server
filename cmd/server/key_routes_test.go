package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"uuid"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// keyEnvelopes is the wire shape of GET/PUT /api/collections/{id}/key, with the
// recovery fields as pointers so the test can tell "absent" from "empty".
type keyEnvelopes struct {
	KeySalt           string          `json:"key_salt"`
	KeyKDF            json.RawMessage `json:"key_kdf"`
	EncryptedVaultKey string          `json:"encrypted_vault_key"`
	KeyUpdatedAt      string          `json:"key_updated_at"`

	RecoveryKeySalt           *string         `json:"recovery_key_salt"`
	RecoveryKeyKDF            json.RawMessage `json:"recovery_key_kdf"`
	RecoveryEncryptedVaultKey *string         `json:"recovery_encrypted_vault_key"`
}

type keyRouteHarness struct {
	url    string
	client *http.Client
	token  string
	keyURL string
}

func newKeyRouteHarness(t *testing.T, database *databasepkg.DB, email string) keyRouteHarness {
	t.Helper()
	cfg := config.Config{AuthMode: "dev", CookieSecure: false, BlobDir: t.TempDir()}
	hub := events.NewHub()
	server := httptest.NewServer(routes(cfg, database, &blobs.FileStore{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), nil))
	t.Cleanup(server.Close)
	client := serverClient(server)

	login := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
		`{"email":"`+email+`","name":"Key Routes"}`, "", nil)
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %q", login.StatusCode, login.Body)
	}
	var loggedIn struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(login.Body), &loggedIn); err != nil {
		t.Fatal(err)
	}

	claimed := doRequest(t, client, http.MethodPost, server.URL+"/api/collections", "", loggedIn.Token, nil)
	if claimed.StatusCode != http.StatusCreated {
		t.Fatalf("claim collection = %d %q", claimed.StatusCode, claimed.Body)
	}
	var claim struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	if err := json.Unmarshal([]byte(claimed.Body), &claim); err != nil {
		t.Fatal(err)
	}

	return keyRouteHarness{
		url:    server.URL,
		client: client,
		token:  loggedIn.Token,
		keyURL: server.URL + "/api/collections/" + claim.Collection.ID + "/key",
	}
}

func (h keyRouteHarness) put(t *testing.T, body string) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, http.MethodPut, h.keyURL, body, h.token, nil)
}

func (h keyRouteHarness) get(t *testing.T) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, http.MethodGet, h.keyURL, "", h.token, nil)
}

// decodeKey pulls the material out of a {"key": ...} or {"currentKey": ...}
// envelope and fails when the field is null.
func decodeKey(t *testing.T, field string, response routeTestResponse) keyEnvelopes {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("decoding %q: %v", response.Body, err)
	}
	raw, ok := body[field]
	if !ok || string(raw) == "null" {
		t.Fatalf("%s is missing or null in %q", field, response.Body)
	}
	var key keyEnvelopes
	if err := json.Unmarshal(raw, &key); err != nil {
		t.Fatal(err)
	}
	return key
}

const passwordOnlyBody = `{"key_salt":"salt-v1","key_kdf":{"kdf":"pbkdf2-sha256"},` +
	`"encrypted_vault_key":"wrapped-v1"}`

func bothEnvelopesBody(suffix, previous string) string {
	body := `{"key_salt":"salt-` + suffix + `","key_kdf":{"kdf":"argon2id"},` +
		`"encrypted_vault_key":"wrapped-` + suffix + `",` +
		`"recovery_key_salt":"recovery-salt-` + suffix + `",` +
		`"recovery_key_kdf":{"kdf":"hkdf-sha256"},` +
		`"recovery_encrypted_vault_key":"recovery-wrapped-` + suffix + `"`
	if previous != "" {
		body += `,"previous_key_updated_at":"` + previous + `"`
	}
	return body + "}"
}

// runKeyRecoveryScenario is the whole recovery-envelope contract, driven over
// HTTP so it can run identically against Postgres and SQLite.
func runKeyRecoveryScenario(t *testing.T, h keyRouteHarness) {
	t.Helper()

	// Nothing claimed yet.
	if got := h.get(t); got.StatusCode != http.StatusOK || got.Body != "{\"key\":null}\n" {
		t.Fatalf("GET before any claim = %d %q", got.StatusCode, got.Body)
	}

	// A v1 client sends only the password envelope. It keeps working, and the
	// recovery fields come back explicitly absent rather than missing.
	claimed := h.put(t, passwordOnlyBody)
	if claimed.StatusCode != http.StatusOK {
		t.Fatalf("v1 claim = %d %q", claimed.StatusCode, claimed.Body)
	}
	v1 := decodeKey(t, "key", claimed)
	if v1.EncryptedVaultKey != "wrapped-v1" {
		t.Fatalf("v1 claim encrypted_vault_key = %q", v1.EncryptedVaultKey)
	}
	if v1.RecoveryKeySalt != nil || v1.RecoveryEncryptedVaultKey != nil || string(v1.RecoveryKeyKDF) != "null" {
		t.Fatalf("v1 claim carries a recovery envelope: %q", claimed.Body)
	}

	// Re-wrap from the current revision: both envelopes replaced, no conflict.
	rewrapped := h.put(t, bothEnvelopesBody("v2", v1.KeyUpdatedAt))
	if rewrapped.StatusCode != http.StatusOK {
		t.Fatalf("re-wrap = %d %q", rewrapped.StatusCode, rewrapped.Body)
	}
	v2 := decodeKey(t, "key", rewrapped)
	assertRecoveryEnvelope(t, "re-wrap", v2, "v2")
	if v2.KeyUpdatedAt == v1.KeyUpdatedAt {
		t.Fatalf("re-wrap did not advance key_updated_at past %q", v1.KeyUpdatedAt)
	}

	// A second device reads both envelopes back.
	read := decodeKey(t, "key", h.get(t))
	assertRecoveryEnvelope(t, "GET after re-wrap", read, "v2")

	// A stale revision token loses, and the 409 carries the authoritative
	// material including the recovery envelope the loser must adopt.
	stale := h.put(t, bothEnvelopesBody("v3", v1.KeyUpdatedAt))
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale re-wrap = %d %q, want 409", stale.StatusCode, stale.Body)
	}
	assertRecoveryEnvelope(t, "409 currentKey", decodeKey(t, "currentKey", stale), "v2")

	// Re-wrapping from the current revision again succeeds — a second write is
	// not a conflict, only a stale one is.
	second := h.put(t, bothEnvelopesBody("v4", read.KeyUpdatedAt))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second re-wrap = %d %q", second.StatusCode, second.Body)
	}
	v4 := decodeKey(t, "key", second)
	assertRecoveryEnvelope(t, "second re-wrap", v4, "v4")

	// A v1 client re-wrapping mints a new vault key, so the recovery envelope
	// it does not send is cleared rather than left wrapping a dead key.
	cleared := h.put(t, `{"key_salt":"salt-v5","key_kdf":{"kdf":"pbkdf2-sha256"},`+
		`"encrypted_vault_key":"wrapped-v5","previous_key_updated_at":"`+v4.KeyUpdatedAt+`"}`)
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("v1 re-wrap = %d %q", cleared.StatusCode, cleared.Body)
	}
	v5 := decodeKey(t, "key", cleared)
	if v5.RecoveryKeySalt != nil || v5.RecoveryEncryptedVaultKey != nil || string(v5.RecoveryKeyKDF) != "null" {
		t.Fatalf("v1 re-wrap left a recovery envelope behind: %q", cleared.Body)
	}

	// A partial recovery envelope is never stored: it would leave a vault whose
	// recovery door exists but cannot be opened.
	for name, body := range map[string]string{
		"salt only": `{"key_salt":"s","key_kdf":{"n":1},"encrypted_vault_key":"k",` +
			`"recovery_key_salt":"rs","previous_key_updated_at":"` + v5.KeyUpdatedAt + `"}`,
		"no kdf": `{"key_salt":"s","key_kdf":{"n":1},"encrypted_vault_key":"k",` +
			`"recovery_key_salt":"rs","recovery_encrypted_vault_key":"rk",` +
			`"previous_key_updated_at":"` + v5.KeyUpdatedAt + `"}`,
		"kdf not an object": `{"key_salt":"s","key_kdf":{"n":1},"encrypted_vault_key":"k",` +
			`"recovery_key_salt":"rs","recovery_key_kdf":"hkdf","recovery_encrypted_vault_key":"rk",` +
			`"previous_key_updated_at":"` + v5.KeyUpdatedAt + `"}`,
		"empty recovery key": `{"key_salt":"s","key_kdf":{"n":1},"encrypted_vault_key":"k",` +
			`"recovery_key_salt":"rs","recovery_key_kdf":{"n":1},"recovery_encrypted_vault_key":"",` +
			`"previous_key_updated_at":"` + v5.KeyUpdatedAt + `"}`,
	} {
		if got := h.put(t, body); got.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s = %d %q, want 400", name, got.StatusCode, got.Body)
		}
	}

	// The rejected writes changed nothing.
	unchanged := decodeKey(t, "key", h.get(t))
	if unchanged.KeyUpdatedAt != v5.KeyUpdatedAt || unchanged.EncryptedVaultKey != "wrapped-v5" {
		t.Fatalf("a rejected write changed the stored material: %#v", unchanged)
	}
}

// First claim is untouched by the recovery columns: the second claim on a row
// that already has material still gets 200 with the winner's material, not a
// conflict, and the winner's recovery envelope comes with it.
func runKeyFirstClaimScenario(t *testing.T, h keyRouteHarness) {
	t.Helper()

	// A revision token against an empty key row is still a 400, not a claim.
	early := h.put(t, bothEnvelopesBody("early", "2026-01-01T00:00:00Z"))
	assertResponse(t, early, http.StatusBadRequest, "{\"error\":\"no existing key to rotate\"}\n")

	winner := h.put(t, bothEnvelopesBody("winner", ""))
	if winner.StatusCode != http.StatusOK {
		t.Fatalf("first claim = %d %q", winner.StatusCode, winner.Body)
	}
	won := decodeKey(t, "key", winner)

	loser := h.put(t, bothEnvelopesBody("loser", ""))
	if loser.StatusCode != http.StatusOK {
		t.Fatalf("losing claim = %d %q, want 200", loser.StatusCode, loser.Body)
	}
	adopted := decodeKey(t, "key", loser)
	assertRecoveryEnvelope(t, "losing claim", adopted, "winner")
	if adopted.KeyUpdatedAt != won.KeyUpdatedAt {
		t.Fatalf("losing claim key_updated_at = %q, want the winner's %q",
			adopted.KeyUpdatedAt, won.KeyUpdatedAt)
	}
}

func assertRecoveryEnvelope(t *testing.T, what string, got keyEnvelopes, suffix string) {
	t.Helper()
	if got.RecoveryKeySalt == nil || got.RecoveryEncryptedVaultKey == nil {
		t.Fatalf("%s: recovery envelope absent, want the %q one", what, suffix)
	}
	if *got.RecoveryKeySalt != "recovery-salt-"+suffix {
		t.Fatalf("%s: recovery_key_salt = %q, want %q", what, *got.RecoveryKeySalt, "recovery-salt-"+suffix)
	}
	if *got.RecoveryEncryptedVaultKey != "recovery-wrapped-"+suffix {
		t.Fatalf("%s: recovery_encrypted_vault_key = %q, want %q",
			what, *got.RecoveryEncryptedVaultKey, "recovery-wrapped-"+suffix)
	}
	var kdf struct {
		KDF string `json:"kdf"`
	}
	if err := json.Unmarshal(got.RecoveryKeyKDF, &kdf); err != nil || kdf.KDF != "hkdf-sha256" {
		t.Fatalf("%s: recovery_key_kdf = %s, %v", what, got.RecoveryKeyKDF, err)
	}
}

func sqliteKeyDatabase(t *testing.T) *databasepkg.DB {
	t.Helper()
	database, err := databasepkg.Open(config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(t.TempDir(), "notes.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := databasepkg.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

func postgresKeyDatabase(t *testing.T) *databasepkg.DB {
	t.Helper()
	databaseURL := os.Getenv("SERVER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SERVER_TEST_DATABASE_URL is not set")
	}
	rawDatabase, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := databasepkg.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := databasepkg.Wrap(rawDatabase, dialect)
	t.Cleanup(func() { database.Close() })
	if _, err := databasepkg.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestCollectionKeyRecoveryEnvelopeOnSQLite(t *testing.T) {
	runKeyRecoveryScenario(t, newKeyRouteHarness(t, sqliteKeyDatabase(t), "key-sqlite@example.invalid"))
}

func TestCollectionKeyRecoveryEnvelopeOnPostgres(t *testing.T) {
	runKeyRecoveryScenario(t, newKeyRouteHarness(t, postgresKeyDatabase(t),
		"key-pg-"+uuid.NewV7().String()+"@example.invalid"))
}

func TestCollectionKeyFirstClaimOnSQLite(t *testing.T) {
	runKeyFirstClaimScenario(t, newKeyRouteHarness(t, sqliteKeyDatabase(t), "claim-sqlite@example.invalid"))
}

func TestCollectionKeyFirstClaimOnPostgres(t *testing.T) {
	runKeyFirstClaimScenario(t, newKeyRouteHarness(t, postgresKeyDatabase(t),
		"claim-pg-"+uuid.NewV7().String()+"@example.invalid"))
}
