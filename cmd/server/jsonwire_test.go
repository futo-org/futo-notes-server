package main

// Golden bytes for every JSON shape that crosses the wire. Go 1.27 swaps
// encoding/json's implementation for v2, which is claimed to preserve v1
// behavior; these freeze the v1 output so the swap can be diffed rather than
// trusted. GOEXPERIMENT=nojsonv2 restores v1 if one of these breaks.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/collections"
)

var (
	wholeSecond = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	withNanos   = time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.UTC)

	testUser = auth.User{
		ID:    "0195e2a1-0000-7000-8000-000000000001",
		Email: "local@futo-notes.local",
		Name:  "FUTO Notes",
	}
	testCollection = collections.Collection{
		ID:             "0195e2a1-0000-7000-8000-000000000002",
		UserID:         testUser.ID,
		CurrentVersion: "42",
		CreatedAt:      wholeSecond,
	}
	testKey = &collections.KeyMaterial{
		KeySalt:           "c2FsdA==",
		KeyKDF:            json.RawMessage(`{ "name": "scrypt", "N": 16384 }`),
		EncryptedVaultKey: "ZW5jcnlwdGVk",
		KeyUpdatedAt:      withNanos,
	}
)

func TestResponseWireFormat(t *testing.T) {
	cases := []struct {
		name  string
		write func(w http.ResponseWriter)
		want  string
	}{{
		name:  "capability",
		write: func(w http.ResponseWriter) { handleCapability("password")(w, httptest.NewRequest("GET", "/", nil)) },
		want:  `{"name":"futo-notes","version":"0.1.0","auth_mode":"password","signup":"closed","billing":false,"mutation_ids":{"supported":true,"required":false,"retention_days":30,"successful_create_outcomes":"durable"}}`,
	}, {
		name:  "health ok",
		write: func(w http.ResponseWriter) { writeJSON(w, 200, healthStatus{Status: "ok", DB: "connected"}) },
		want:  `{"status":"ok","db":"connected"}`,
	}, {
		name:  "health degraded",
		write: func(w http.ResponseWriter) { writeJSON(w, 503, healthStatus{Status: "degraded", DB: "unreachable"}) },
		want:  `{"status":"degraded","db":"unreachable"}`,
	}, {
		name:  "error",
		write: func(w http.ResponseWriter) { writeError(w, 404, "not found") },
		want:  `{"error":"not found"}`,
	}, {
		// The 401 body clients key off to distinguish a dead session from
		// missing credentials.
		name: "invalid session",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 401, map[string]string{"error": "session expired or invalid", "code": "invalid_session"})
		},
		want: `{"code":"invalid_session","error":"session expired or invalid"}`,
	}, {
		name:  "current user",
		write: func(w http.ResponseWriter) { writeJSON(w, 200, map[string]any{"user": testUser}) },
		want:  `{"user":{"id":"0195e2a1-0000-7000-8000-000000000001","email":"local@futo-notes.local","name":"FUTO Notes"}}`,
	}, {
		name: "login",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 200, map[string]any{"user": testUser, "token": strings.Repeat("ab", 32)})
		},
		want: `{"token":"abababababababababababababababababababababababababababababababab","user":{"id":"0195e2a1-0000-7000-8000-000000000001","email":"local@futo-notes.local","name":"FUTO Notes"}}`,
	}, {
		name:  "collection",
		write: func(w http.ResponseWriter) { writeJSON(w, 201, map[string]any{"collection": testCollection}) },
		want:  `{"collection":{"id":"0195e2a1-0000-7000-8000-000000000002","user_id":"0195e2a1-0000-7000-8000-000000000001","current_version":"42","created_at":"2026-02-03T04:05:06Z"}}`,
	}, {
		name: "collections empty",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 200, map[string]any{"collections": []collections.Collection{}})
		},
		want: `{"collections":[]}`,
	}, {
		name: "collections nil slice",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 200, map[string]any{"collections": []collections.Collection(nil)})
		},
		want: `{"collections":null}`,
	}, {
		name: "collections list",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 200, map[string]any{"collections": []collections.Collection{testCollection}})
		},
		want: `{"collections":[{"id":"0195e2a1-0000-7000-8000-000000000002","user_id":"0195e2a1-0000-7000-8000-000000000001","current_version":"42","created_at":"2026-02-03T04:05:06Z"}]}`,
	}, {
		// key_kdf goes out compacted, not byte-for-byte as stored, and
		// key_updated_at must render with nanoseconds because it is the
		// rotation token the client echoes back.
		name:  "key",
		write: func(w http.ResponseWriter) { writeJSON(w, 200, map[string]any{"key": testKey}) },
		want:  `{"key":{"key_salt":"c2FsdA==","key_kdf":{"name":"scrypt","N":16384},"encrypted_vault_key":"ZW5jcnlwdGVk","key_updated_at":"2026-02-03T04:05:06.123456789Z"}}`,
	}, {
		name:  "key unclaimed",
		write: func(w http.ResponseWriter) { writeJSON(w, 200, map[string]any{"key": (*collections.KeyMaterial)(nil)}) },
		want:  `{"key":null}`,
	}, {
		name: "key conflict",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 409, map[string]any{"error": "key conflict", "currentKey": testKey})
		},
		want: `{"currentKey":{"key_salt":"c2FsdA==","key_kdf":{"name":"scrypt","N":16384},"encrypted_vault_key":"ZW5jcnlwdGVk","key_updated_at":"2026-02-03T04:05:06.123456789Z"},"error":"key conflict"}`,
	}, {
		// v1 escapes <, > and & in strings and inside RawMessage.
		name: "html escaping",
		write: func(w http.ResponseWriter) {
			writeJSON(w, 200, map[string]any{"key": &collections.KeyMaterial{
				KeySalt:           "a<b>c&d",
				KeyKDF:            json.RawMessage(`{"note":"<x&y>"}`),
				EncryptedVaultKey: "e",
				KeyUpdatedAt:      wholeSecond,
			}})
		},
		want: `{"key":{"key_salt":"a\u003cb\u003ec\u0026d","key_kdf":{"note":"\u003cx\u0026y\u003e"},"encrypted_vault_key":"e","key_updated_at":"2026-02-03T04:05:06Z"}}`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.write(w)
			if got := w.Body.String(); got != tc.want+"\n" {
				t.Errorf("body mismatch\n got: %s\nwant: %s\n", got, tc.want)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// putKeyBody mirrors the request body decoded by handlePutCollectionKey. The
// pointer fields carry meaning: previous_key_updated_at absent or null is a
// claim, present is a rotation.
type putKeyBody struct {
	KeySalt              *string         `json:"key_salt"`
	KeyKDF               json.RawMessage `json:"key_kdf"`
	EncryptedVaultKey    *string         `json:"encrypted_vault_key"`
	PreviousKeyUpdatedAt *string         `json:"previous_key_updated_at"`
}

func TestPutKeyBodyDecoding(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		check   func(t *testing.T, b putKeyBody)
	}{{
		name: "rotation token absent",
		body: `{"key_salt":"s","key_kdf":{},"encrypted_vault_key":"e"}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.PreviousKeyUpdatedAt != nil {
				t.Errorf("PreviousKeyUpdatedAt = %q, want nil", *b.PreviousKeyUpdatedAt)
			}
		},
	}, {
		name: "rotation token null",
		body: `{"key_salt":"s","key_kdf":{},"encrypted_vault_key":"e","previous_key_updated_at":null}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.PreviousKeyUpdatedAt != nil {
				t.Errorf("PreviousKeyUpdatedAt = %q, want nil", *b.PreviousKeyUpdatedAt)
			}
		},
	}, {
		name: "rotation token empty string",
		body: `{"key_salt":"s","key_kdf":{},"encrypted_vault_key":"e","previous_key_updated_at":""}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.PreviousKeyUpdatedAt == nil || *b.PreviousKeyUpdatedAt != "" {
				t.Errorf("PreviousKeyUpdatedAt = %v, want non-nil empty string", b.PreviousKeyUpdatedAt)
			}
		},
	}, {
		name: "key_salt null is not absent",
		body: `{"key_salt":null,"key_kdf":{},"encrypted_vault_key":"e"}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.KeySalt != nil {
				t.Errorf("KeySalt = %q, want nil", *b.KeySalt)
			}
		},
	}, {
		name: "key_kdf keeps its bytes verbatim",
		body: `{"key_salt":"s","key_kdf":{ "N": 16384, "r": 8 },"encrypted_vault_key":"e"}`,
		check: func(t *testing.T, b putKeyBody) {
			if got := string(b.KeyKDF); got != `{ "N": 16384, "r": 8 }` {
				t.Errorf("KeyKDF = %s, want the original bytes including whitespace", got)
			}
		},
	}, {
		// isJSONObject rejects this; it must not arrive as a nil RawMessage,
		// which the handler reads the same way but for a different reason.
		name: "key_kdf null decodes to the null literal",
		body: `{"key_salt":"s","key_kdf":null,"encrypted_vault_key":"e"}`,
		check: func(t *testing.T, b putKeyBody) {
			if got := string(b.KeyKDF); got != "null" {
				t.Errorf("KeyKDF = %q, want %q", got, "null")
			}
		},
	}, {
		name: "unknown fields ignored",
		body: `{"key_salt":"s","key_kdf":{},"encrypted_vault_key":"e","nope":1}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.KeySalt == nil || *b.KeySalt != "s" {
				t.Errorf("KeySalt = %v, want s", b.KeySalt)
			}
		},
	}, {
		name: "field names match case-insensitively",
		body: `{"KEY_SALT":"s","key_kdf":{},"encrypted_vault_key":"e"}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.KeySalt == nil || *b.KeySalt != "s" {
				t.Errorf("KeySalt = %v, want s", b.KeySalt)
			}
		},
	}, {
		name: "duplicate keys take the last value",
		body: `{"key_salt":"first","key_salt":"second","key_kdf":{},"encrypted_vault_key":"e"}`,
		check: func(t *testing.T, b putKeyBody) {
			if b.KeySalt == nil || *b.KeySalt != "second" {
				t.Errorf("KeySalt = %v, want second", b.KeySalt)
			}
		},
	}, {
		// The handler uses a Decoder, which reads one value and stops.
		name: "trailing data after the object is not read",
		body: `{"key_salt":"s","key_kdf":{},"encrypted_vault_key":"e"} trailing garbage`,
		check: func(t *testing.T, b putKeyBody) {
			if b.KeySalt == nil || *b.KeySalt != "s" {
				t.Errorf("KeySalt = %v, want s", b.KeySalt)
			}
		},
	}, {
		name:    "wrong type",
		body:    `{"key_salt":1}`,
		wantErr: true,
	}, {
		name:    "empty body",
		body:    ``,
		wantErr: true,
	}, {
		name:    "not an object",
		body:    `[]`,
		wantErr: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b putKeyBody
			err := json.NewDecoder(strings.NewReader(tc.body)).Decode(&b)
			if tc.wantErr {
				if err == nil {
					t.Fatal("decoded without error, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			tc.check(t, b)
		})
	}
}

func TestLoginBodyDecoding(t *testing.T) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(strings.NewReader(`{"password":"hunter2"}`)).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Password != "hunter2" {
		t.Fatalf("Password = %q, want hunter2", body.Password)
	}

	// An absent password is a 400 from the empty-string check, not a decode
	// error.
	body.Password = ""
	if err := json.NewDecoder(strings.NewReader(`{}`)).Decode(&body); err != nil {
		t.Fatalf("empty object: %v", err)
	}
	if body.Password != "" {
		t.Fatalf("Password = %q, want empty", body.Password)
	}
}
