package api_test

import (
	"maps"
	"net/http"
	"strings"
	"testing"
)

func TestHostedKeyPutRefusesUnknownFields(t *testing.T) {
	h := newHarness(t)
	app, _ := h.appLogin(h.newSub())
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)
	keyPath := "/api/collections/" + claimed.Collection.ID + "/key"
	key := map[string]any{"key_salt": "salt", "key_kdf": map[string]any{"alg": "argon2id"}, "encrypted_vault_key": "evk"}

	withExtra := map[string]any{"recovery_envelope": "half", "zeta": 1}
	maps.Copy(withExtra, key)
	res := app.do(http.MethodPut, keyPath, withExtra)
	if res.status != http.StatusBadRequest || res.body != `{"code":"invalid_request","error":"unknown field \"recovery_envelope\""}`+"\n" {
		t.Fatalf("key with an unknown field = %d %s", res.status, res.body)
	}
	if res := app.expect(http.MethodGet, keyPath, nil, http.StatusOK); res.body != `{"key":null}`+"\n" {
		t.Fatalf("key after a refused put = %s, want none stored", res.body)
	}
	// Anything that is not a JSON object is answered as on self-hosted.
	if res := app.do(http.MethodPut, keyPath, []string{"key_salt"}); res.status != http.StatusBadRequest || res.body != `{"error":"invalid json"}`+"\n" {
		t.Fatalf("non-object key body = %d %s", res.status, res.body)
	}
	huge := map[string]any{"key_salt": strings.Repeat("s", 64<<10), "key_kdf": map[string]any{}, "encrypted_vault_key": "evk"}
	if res := app.do(http.MethodPut, keyPath, huge); res.status != http.StatusBadRequest || !strings.HasPrefix(res.body, `{"code":"invalid_request",`) {
		t.Fatalf("oversized key body = %d %s", res.status, res.body)
	}
	if res := app.expect(http.MethodPut, keyPath, key, http.StatusOK); !strings.Contains(res.body, `"key_salt":"salt"`) {
		t.Fatalf("key put = %s", res.body)
	}
}
