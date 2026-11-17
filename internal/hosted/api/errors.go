package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/syncapi"
)

// Error codes of the hosted API, besides httputil's. docs/openapi-hosted.yaml
// lists each in the ErrorCode enum.
const (
	codeInvalidSession         = "invalid_session"
	codeInvalidLoginState      = "invalid_login_state"
	codeSignInNotCompleted     = "sign_in_not_completed"
	codeProviderUnavailable    = "provider_unavailable"
	codeProviderError          = "provider_error"
	codeInvalidCode            = "invalid_code"
	codeCodeVerifierMismatch   = "code_verifier_mismatch"
	codeRefreshTokenInvalid    = "refresh_token_invalid"
	codeRefreshTokenReused     = "refresh_token_reused"
	codeRegionRequired         = "region_required"
	codeRegionAlreadyChosen    = "region_already_chosen"
	codePurgeStarted           = "purge_started"
	codeAccountPendingDeletion = "account_pending_deletion"
	codeReceiptInvalid         = "receipt_invalid"
	codeReceiptExpired         = "receipt_expired"
	codeReceiptWrongRegion     = "receipt_wrong_region"
	codeReceiptWrongUser       = "receipt_wrong_user"
	// codeReauthenticationRequired asks the client to sign in again with
	// reauthenticate=true before deleting the account.
	codeReauthenticationRequired = "reauthentication_required"
)

// serverError logs an internal failure and answers with a hosted 500.
func serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	hlog.FromRequest(r).Error().Err(err).Str("action", action).Msg("internal server error")
	httputil.WriteError(w, r, http.StatusInternalServerError, httputil.CodeInternalError, "Internal server error")
}

// badRequest answers a hosted 400 for a request the client got wrong.
func badRequest(w http.ResponseWriter, r *http.Request, message string) {
	httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, message)
}

// decodeJSON reads a JSON request body into v, answering a 400 when it is
// not one.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		badRequest(w, r, "Invalid JSON body")
		return false
	}
	return true
}

// writePendingDeletion answers the 403 for an account scheduled for
// deletion, in the sync contract's shape with its code, wherever it occurs.
func writePendingDeletion(w http.ResponseWriter) {
	syncapi.WriteCodedError(w, http.StatusForbidden, codeAccountPendingDeletion, account.ErrPendingDeletion.Error())
}
