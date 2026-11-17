// Package httputil is the HTTP plumbing the hosted binaries share: the hosted
// error body and JSON writer used by the hosted API and the storage service,
// the access log line, and the readiness probe with its two-phase drain.
package httputil

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/hlog"
)

// ErrorResponse is the hosted error body: every storage service error, and
// the errors of the endpoints only the hosted API serves. The hosted API's
// sync endpoints keep the sync contract's {"error": ...} body instead.
type ErrorResponse struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	// RequestID is always present, as both hosted specs require. It is the
	// X-Request-Id the request ID handler assigned, or "" outside that chain.
	RequestID string `json:"requestId"`
}

// WriteError answers with status and an ErrorResponse carrying message and
// the request id.
func WriteError(w http.ResponseWriter, r *http.Request, status int, message string) {
	id := ""
	if requestID, ok := hlog.IDFromRequest(r); ok {
		id = requestID.String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorResponse{StatusCode: status, Message: message, RequestID: id}); err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("writing error response")
	}
}

// WriteJSON answers with status and v encoded as JSON.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("writing JSON response")
	}
}
