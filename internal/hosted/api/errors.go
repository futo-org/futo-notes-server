package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/hlog"
)

// ErrorResponse is the error body of the endpoints only the hosted API
// serves. The sync endpoints keep the sync contract's {"error": ...} body.
type ErrorResponse struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	RequestID  string `json:"requestId,omitempty"`
}

// writeError answers with status and the hosted error body carrying message
// and the request's id.
func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	requestID, _ := hlog.IDFromRequest(r)
	id := ""
	if !requestID.IsNil() {
		id = requestID.String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorResponse{StatusCode: status, Message: message, RequestID: id}); err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("writing error response")
	}
}

// serverError logs an internal failure and answers with a hosted 500.
func serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	hlog.FromRequest(r).Error().Err(err).Str("action", action).Msg("internal server error")
	writeError(w, r, http.StatusInternalServerError, "Internal server error")
}

// writeJSON answers with status and v encoded as JSON.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("writing JSON response")
	}
}

// decodeJSON reads a JSON request body into v, answering a 400 when it is
// not one.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeError(w, r, http.StatusBadRequest, "Invalid JSON body")
		return false
	}
	return true
}
