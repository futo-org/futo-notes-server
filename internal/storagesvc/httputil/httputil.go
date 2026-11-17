// Package httputil writes the storage service's JSON responses.
package httputil

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/hlog"
)

// ErrorResponse is the body of every storage service error.
type ErrorResponse struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	RequestID  string `json:"requestId,omitempty"`
}

// WriteError answers with code and an ErrorResponse carrying message and the
// request id.
func WriteError(w http.ResponseWriter, r *http.Request, code int, message string) {
	reqID, _ := hlog.IDFromRequest(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(ErrorResponse{
		StatusCode: code,
		Message:    message,
		RequestID:  reqID.String(),
	}); err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("failed to write error response")
	}
}

// WriteJSON answers with code and v encoded as JSON.
func WriteJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("failed to write response")
	}
}
