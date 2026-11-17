package api_test

import (
	"maps"
	"net/http"
	"strings"
	"testing"
)

type hostedKey struct {
	KeySalt                   string         `json:"key_salt"`
	KeyKDF                    map[string]any `json:"key_kdf"`
	EncryptedVaultKey         string         `json:"encrypted_vault_key"`
	KeyUpdatedAt              string         `json:"key_updated_at"`
	RecoveryKeySalt           *string        `json:"recovery_key_salt"`
	RecoveryKeyKDF            map[string]any `json:"recovery_key_kdf"`
	RecoveryEncryptedVaultKey *string        `json:"recovery_encrypted_vault_key"`
}

func (k hostedKey) hasRecovery() bool {
	return k.RecoveryKeySalt != nil || k.RecoveryKeyKDF != nil || k.RecoveryEncryptedVaultKey != nil
}

// keyClient is an app holding a claimed collection's key path.
type keyClient struct {
	*client
	collectionID, path string
}

func (h *harness) keyClient() keyClient {
	h.t.Helper()
	app, _ := h.appLogin(h.newSub())
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(h.t, &claimed)
	return keyClient{client: app, collectionID: claimed.Collection.ID, path: "/api/collections/" + claimed.Collection.ID + "/key"}
}

// put sends body and decodes the answered key, failing unless it is 200.
func (k keyClient) put(body map[string]any) hostedKey {
	k.h.t.Helper()
	var out struct{ Key hostedKey }
	k.expect(http.MethodPut, k.path, body, http.StatusOK).json(k.h.t, &out)
	return out.Key
}

func (k keyClient) get() *hostedKey {
	k.h.t.Helper()
	var out struct{ Key *hostedKey }
	k.expect(http.MethodGet, k.path, nil, http.StatusOK).json(k.h.t, &out)
	return out.Key
}

func passwordEnvelope(suffix string) map[string]any {
	return map[string]any{"key_salt": "salt-" + suffix, "key_kdf": map[string]any{"kdf": "argon2id"}, "encrypted_vault_key": "evk-" + suffix}
}

func recoveryEnvelope(suffix string) map[string]any {
	return map[string]any{"recovery_key_salt": "rsalt-" + suffix, "recovery_key_kdf": map[string]any{"kdf": "hkdf-sha256"},
		"recovery_encrypted_vault_key": "revk-" + suffix}
}

func with(parts ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range parts {
		maps.Copy(out, m)
	}
	return out
}

func wantRecovery(t *testing.T, what string, key hostedKey, suffix string) {
	t.Helper()
	if key.RecoveryKeySalt == nil || *key.RecoveryKeySalt != "rsalt-"+suffix || key.RecoveryKeyKDF["kdf"] != "hkdf-sha256" ||
		key.RecoveryEncryptedVaultKey == nil || *key.RecoveryEncryptedVaultKey != "revk-"+suffix {
		t.Fatalf("%s = %+v, want recovery envelope %q", what, key, suffix)
	}
}

func TestRecoveryEnvelopeTravelsWithTheKey(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()

	claimed := k.put(with(passwordEnvelope("one"), recoveryEnvelope("one")))
	wantRecovery(t, "claim", claimed, "one")
	got := k.get()
	if got == nil || got.KeyUpdatedAt != claimed.KeyUpdatedAt || got.KeySalt != "salt-one" {
		t.Fatalf("get = %+v, want the claimed key", got)
	}
	wantRecovery(t, "get", *got, "one")

	// A new recovery key is a rotation: both envelopes move to one new revision.
	rotated := k.put(with(passwordEnvelope("one"), recoveryEnvelope("two"),
		map[string]any{"previous_key_updated_at": claimed.KeyUpdatedAt}))
	wantRecovery(t, "rotation", rotated, "two")
	if rotated.KeyUpdatedAt == claimed.KeyUpdatedAt {
		t.Fatalf("rotation kept key_updated_at %s", rotated.KeyUpdatedAt)
	}
	if got := k.get(); got.KeyUpdatedAt != rotated.KeyUpdatedAt {
		t.Fatalf("get after rotation = %+v, want revision %s", got, rotated.KeyUpdatedAt)
	}
}

func TestRecoveryEnvelopeIdempotentClaim(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()
	body := with(passwordEnvelope("one"), recoveryEnvelope("one"))
	first := k.put(body)
	again := k.put(body)
	if again.KeyUpdatedAt != first.KeyUpdatedAt {
		t.Fatalf("identical claim = %s, want the original key_updated_at %s", again.KeyUpdatedAt, first.KeyUpdatedAt)
	}
	wantRecovery(t, "identical claim", again, "one")
}

func TestRecoveryEnvelopeAllOrNone(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()
	whole := recoveryEnvelope("x")
	refusals := map[string]map[string]any{}
	for _, drop := range []string{"recovery_key_salt", "recovery_key_kdf", "recovery_encrypted_vault_key"} {
		partial := maps.Clone(whole)
		delete(partial, drop)
		refusals["without "+drop] = partial
		nulled := maps.Clone(whole)
		nulled[drop] = nil
		refusals["with a null "+drop] = nulled
	}
	refusals["only a salt"] = map[string]any{"recovery_key_salt": "s"}
	const together = `{"code":"invalid_request","error":"recovery_key_salt, recovery_key_kdf and recovery_encrypted_vault_key go together: send all three or none"}` + "\n"
	for name, recovery := range refusals {
		res := k.do(http.MethodPut, k.path, with(passwordEnvelope("one"), recovery))
		if res.status != http.StatusBadRequest || res.body != together {
			t.Errorf("claim %s = %d %s, want 400 %s", name, res.status, res.body, together)
		}
	}
	for name, recovery := range map[string]map[string]any{
		`{"code":"invalid_request","error":"recovery_key_salt must be non-empty"}`:            with(whole, map[string]any{"recovery_key_salt": ""}),
		`{"code":"invalid_request","error":"recovery_key_kdf must be an object"}`:             with(whole, map[string]any{"recovery_key_kdf": "hkdf"}),
		`{"code":"invalid_request","error":"recovery_encrypted_vault_key must be non-empty"}`: with(whole, map[string]any{"recovery_encrypted_vault_key": ""}),
	} {
		if res := k.do(http.MethodPut, k.path, with(passwordEnvelope("one"), recovery)); res.status != http.StatusBadRequest || res.body != name+"\n" {
			t.Errorf("claim with %v = %d %s, want 400 %s", recovery, res.status, res.body, name)
		}
	}
	if got := k.get(); got != nil {
		t.Fatalf("key after refused claims = %+v, want none", got)
	}

	// Three nulls are no envelope.
	nulls := map[string]any{"recovery_key_salt": nil, "recovery_key_kdf": nil, "recovery_encrypted_vault_key": nil}
	if claimed := k.put(with(passwordEnvelope("one"), nulls)); claimed.hasRecovery() {
		t.Fatalf("claim with null recovery fields = %+v, want no envelope", claimed)
	}
	if res := k.expect(http.MethodGet, k.path, nil, http.StatusOK); res.body == "" || containsRecovery(res.body) {
		t.Fatalf("get after a claim without an envelope = %s, want no recovery fields", res.body)
	}
}

func containsRecovery(body string) bool {
	return strings.Contains(body, `"recovery_`)
}

// A PUT replaces both envelopes: leaving the recovery fields out deletes the
// stored one.
func TestRecoveryEnvelopeOmittedIsDeleted(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()
	claimed := k.put(with(passwordEnvelope("one"), recoveryEnvelope("one")))
	rotated := k.put(with(passwordEnvelope("two"), map[string]any{"previous_key_updated_at": claimed.KeyUpdatedAt}))
	if rotated.hasRecovery() {
		t.Fatalf("rotation without an envelope answered %+v, want none", rotated)
	}
	if got := k.get(); got.hasRecovery() || got.KeySalt != "salt-two" {
		t.Fatalf("get after rotation without an envelope = %+v, want salt-two and no envelope", got)
	}
	var rows int
	if err := h.database.QueryRowContext(t.Context(),
		`SELECT count(*) FROM collection_recovery_envelopes WHERE collection_id = $1`, k.collectionID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("envelope rows after deletion = %d, %v, want 0", rows, err)
	}
}

func TestRecoveryEnvelopeInStaleConflict(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()
	claimed := k.put(with(passwordEnvelope("one"), recoveryEnvelope("one")))
	stale := map[string]any{"previous_key_updated_at": claimed.KeyUpdatedAt}
	winner := k.put(with(passwordEnvelope("two"), recoveryEnvelope("two"), stale))
	var conflict struct {
		CurrentKey hostedKey `json:"currentKey"`
		Error      string    `json:"error"`
	}
	k.expect(http.MethodPut, k.path, with(passwordEnvelope("three"), stale), http.StatusConflict).json(t, &conflict)
	if conflict.Error != "key conflict" || conflict.CurrentKey.KeyUpdatedAt != winner.KeyUpdatedAt {
		t.Fatalf("stale rotation = %+v, want the winner's key", conflict)
	}
	wantRecovery(t, "conflict currentKey", conflict.CurrentKey, "two")
	wantRecovery(t, "get after the conflict", *k.get(), "two")
}

func TestRecoveryEnvelopePartialRowReadsAsNone(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()
	k.put(with(passwordEnvelope("one"), recoveryEnvelope("one")))
	for _, column := range []string{"salt", "kdf", "encrypted_vault_key"} {
		for _, query := range []string{
			`UPDATE collection_recovery_envelopes SET salt = 'rsalt-one', kdf = '{"kdf":"hkdf-sha256"}', encrypted_vault_key = 'revk-one' WHERE collection_id = $1`,
			`UPDATE collection_recovery_envelopes SET ` + column + ` = NULL WHERE collection_id = $1`,
		} {
			if _, err := h.database.ExecContext(t.Context(), query, k.collectionID); err != nil {
				t.Fatal(err)
			}
		}
		if got := k.get(); got.hasRecovery() {
			t.Fatalf("get with a null %s = %+v, want no envelope", column, got)
		}
	}
}

func TestRecoveryEnvelopeGoesWithTheCollection(t *testing.T) {
	h := newHarness(t)
	k := h.keyClient()
	k.put(with(passwordEnvelope("one"), recoveryEnvelope("one")))
	k.expect(http.MethodDelete, "/api/collections/"+k.collectionID, nil, http.StatusNoContent)
	var rows int
	if err := h.database.QueryRowContext(t.Context(),
		`SELECT count(*) FROM collection_recovery_envelopes WHERE collection_id = $1`, k.collectionID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("envelope rows after deleting the collection = %d, %v, want 0", rows, err)
	}
}
