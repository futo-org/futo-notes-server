package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/httputil"
)

// serverError logs an internal failure and answers with a hosted 500.
func serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	hlog.FromRequest(r).Error().Err(err).Str("action", action).Msg("internal server error")
	httputil.WriteError(w, r, http.StatusInternalServerError, "Internal server error")
}

// decodeJSON reads a JSON request body into v, answering a 400 when it is
// not one.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		httputil.WriteError(w, r, http.StatusBadRequest, "Invalid JSON body")
		return false
	}
	return true
}
