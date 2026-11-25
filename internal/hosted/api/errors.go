package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/syncapi"
)

// Error codes of the hosted API, besides httputil's. docs/openapi-hosted.yaml
// lists each in the ErrorCode enum.
const (
	codeInvalidSession         = "invalid_session"
	codeProviderUnavailable    = "provider_unavailable"
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
	codePayloadTooLarge        = "payload_too_large"
	// codePairingAlreadyKeyed answers a second key posted to a pairing.
	codePairingAlreadyKeyed = "pairing_already_keyed"
	// codeSignupNotAllowed refuses a new account whose email
	// ALLOWED_EMAIL_DOMAINS does not admit; the app gets it as its redirect's
	// error.
	codeSignupNotAllowed = "signup_not_allowed"
	// codeReauthenticationRequired asks the client to sign in again with
	// reauthenticate=true before deleting the account.
	codeReauthenticationRequired = "reauthentication_required"
	// codeSubscriptionRequired answers a write from an account without a
	// subscription, in the sync shape.
	codeSubscriptionRequired = "subscription_required"
	// Billing failures.
	codeBillingUnavailable         = "billing_unavailable"
	codeAlreadySubscribed          = "already_subscribed"
	codeManagedByStore             = "managed_by_store"
	codeStoreSubscriptionElsewhere = "store_subscription_owned_elsewhere"
	codeInvalidPurchase            = "invalid_purchase"
	codeInvalidDelivery            = "invalid_delivery"
	// codeCheckoutLinkExpired answers an app's checkout link that is
	// forged, expired or already used.
	codeCheckoutLinkExpired = "checkout_link_expired"
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
func writePendingDeletion(w http.ResponseWriter, purgeAt *time.Time) {
	if purgeAt != nil {
		utc := purgeAt.UTC()
		purgeAt = &utc
	}
	syncapi.WriteJSON(w, http.StatusForbidden, pendingDeletionResponse{
		Code: codeAccountPendingDeletion, Error: account.ErrPendingDeletion.Error(), PurgeAt: purgeAt,
	})
}

// pendingDeletionResponse is the sync-shaped 403 of an account pending
// deletion, with when it will be purged.
type pendingDeletionResponse struct {
	Code    string     `json:"code"`
	Error   string     `json:"error"`
	PurgeAt *time.Time `json:"purge_at,omitempty"`
}
