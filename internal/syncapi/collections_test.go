package syncapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/dbtest"
)

// Self-hosted key routes keep no recovery envelope: they take the recovery
// fields the hosted API stores, whole, partial or mistyped, as any other
// unknown field, and never answer one.
func TestSelfHostedKeyRoutesIgnoreRecoveryFields(t *testing.T) {
	ctx := t.Context()
	database := dbtest.SQLite(t)
	user, err := auth.UpsertUserByEmail(ctx, database, "keys@example.invalid", "Keys")
	if err != nil {
		t.Fatal(err)
	}
	collection, _, err := collections.Claim(ctx, database, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	keys := collections.Keys{DB: database}
	serve := func(handler http.HandlerFunc, method, body string) (int, string) {
		r := httptest.NewRequestWithContext(ctx, method, "/api/collections/"+collection.ID+"/key", strings.NewReader(body))
		r.SetPathValue("id", collection.ID)
		r = r.WithContext(WithSession(r.Context(), &auth.Session{User: user}))
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code, w.Body.String()
	}
	timestamp := regexp.MustCompile(`"key_updated_at":"[^"]+"`)
	want := `{"key":{"key_salt":"salt","key_kdf":{"name":"scrypt"},"encrypted_vault_key":"evk","key_updated_at":"T"}}` + "\n"

	material := `"key_salt":"salt","key_kdf":{"name":"scrypt"},"encrypted_vault_key":"evk"`
	for name, body := range map[string]string{
		"whole envelope":   `{` + material + `,"recovery_key_salt":"rs","recovery_key_kdf":{"kdf":"hkdf-sha256"},"recovery_encrypted_vault_key":"rk"}`,
		"partial envelope": `{` + material + `,"recovery_key_salt":"rs"}`,
		"mistyped fields":  `{` + material + `,"recovery_key_salt":7,"recovery_key_kdf":"x"}`,
	} {
		status, got := serve(HandlePutCollectionKey(keys), http.MethodPut, body)
		if status != http.StatusOK || timestamp.ReplaceAllString(got, `"key_updated_at":"T"`) != want {
			t.Errorf("PUT with %s = %d %s, want 200 %s", name, status, got, want)
		}
	}
	status, got := serve(HandleGetCollectionKey(keys), http.MethodGet, "")
	if status != http.StatusOK || timestamp.ReplaceAllString(got, `"key_updated_at":"T"`) != want {
		t.Errorf("GET = %d %s, want 200 %s", status, got, want)
	}
}
