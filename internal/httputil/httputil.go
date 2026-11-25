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
	StatusCode int `json:"statusCode"`
	// Code is stable and machine-readable; clients branch on it, never on
	// Message, which is for people and may change.
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequestID is always present, as both hosted specs require. It is the
	// X-Request-Id the request ID handler assigned, or "" outside that chain.
	RequestID string `json:"requestId"`
}

// Error codes both hosted services use. Each service adds its own.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeUnauthorized     = "unauthorized"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternalError    = "internal_error"
)

// NewError returns the ErrorResponse for status, code and message, with r's
// request id.
func NewError(r *http.Request, status int, code, message string) ErrorResponse {
	id := ""
	if requestID, ok := hlog.IDFromRequest(r); ok {
		id = requestID.String()
	}
	return ErrorResponse{StatusCode: status, Code: code, Message: message, RequestID: id}
}

// WriteError answers with status and an ErrorResponse carrying code, message
// and the request id.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(NewError(r, status, code, message)); err != nil {
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
