package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/syncapi"
)

// keyFields are the fields the hosted PUT /api/collections/{id}/key accepts.
var keyFields = []string{"key_salt", "key_kdf", "encrypted_vault_key", "previous_key_updated_at"}

// maxKeyBody bounds the key body, which strictKeyBody reads whole.
const maxKeyBody = 64 << 10

// strictKeyBody refuses a key body naming a field outside keyFields with a
// coded 400, then hands the body to next unchanged. A client that sends a
// field this server does not store would otherwise believe all of it was
// saved. Bodies that are not a JSON object go to next, which answers them as
// self-hosted does.
func strictKeyBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKeyBody))
		if err != nil {
			syncapi.WriteCodedError(w, http.StatusBadRequest, httputil.CodeInvalidRequest, "could not read body")
			return
		}
		var fields map[string]json.RawMessage
		if json.NewDecoder(bytes.NewReader(body)).Decode(&fields) == nil {
			var unknown []string
			for name := range fields {
				if !slices.Contains(keyFields, name) {
					unknown = append(unknown, name)
				}
			}
			if len(unknown) > 0 {
				slices.Sort(unknown)
				syncapi.WriteCodedError(w, http.StatusBadRequest, httputil.CodeInvalidRequest,
					fmt.Sprintf("unknown field %q", unknown[0]))
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}
