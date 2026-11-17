package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/objects"
	"futo-notes-server/internal/syncapi"
)

// storageTokenResponse is the body of POST /api/storage/token.
type storageTokenResponse struct {
	AccessToken  string             `json:"access_token"`
	ExpiresAt    time.Time          `json:"expires_at"`
	RefreshToken string             `json:"refresh_token"`
	Region       storageTokenRegion `json:"region"`
}

type storageTokenRegion struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
}

// handleStorageToken serves POST /api/storage/token: a device spends its
// storage refresh token for a 24h access token to its region's storage
// service and the refresh token that replaces it. Spending it again before
// the replacement is used returns the same replacement.
func (s *Server) handleStorageToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.RefreshToken == "" {
		httputil.WriteError(w, r, http.StatusBadRequest, "refresh_token is required")
		return
	}
	exchange, err := s.accounts.ExchangeRefreshToken(r.Context(), body.RefreshToken)
	switch {
	case errors.Is(err, account.ErrInvalidRefreshToken):
		s.metrics.refresh(r.Context(), "invalid")
		httputil.WriteError(w, r, http.StatusUnauthorized, "Invalid refresh token")
		return
	case errors.Is(err, account.ErrRefreshTokenReused):
		s.metrics.refresh(r.Context(), "reuse_detected")
		hlog.FromRequest(r).Warn().Msg("storage refresh token reused; session revoked")
		httputil.WriteError(w, r, http.StatusUnauthorized, "Refresh token already used; the session has been signed out")
		return
	case errors.Is(err, account.ErrPendingDeletion):
		s.metrics.refresh(r.Context(), "pending_deletion")
		syncapi.WriteError(w, http.StatusForbidden, account.ErrPendingDeletion.Error())
		return
	case errors.Is(err, account.ErrNoRegion):
		s.metrics.refresh(r.Context(), "no_region")
		httputil.WriteError(w, r, http.StatusConflict, "Choose a region before requesting a storage token")
		return
	case err != nil:
		serverError(w, r, "exchanging storage refresh token", err)
		return
	}
	if exchange.Reissued {
		s.metrics.refresh(r.Context(), "reissued")
	} else {
		s.metrics.refresh(r.Context(), "rotated")
		s.metrics.issued(r.Context(), tokenStorageRefresh)
	}

	region, ok := s.regions[exchange.RegionID]
	if !ok {
		serverError(w, r, "minting storage token", fmt.Errorf("user's region %q is not configured", exchange.RegionID))
		return
	}
	accessToken, expiresAt, err := s.minter.MintAccess(region.ID, exchange.UserID)
	if err != nil {
		serverError(w, r, "minting storage token", err)
		return
	}
	s.metrics.issued(r.Context(), tokenStorageAccess)
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, r, http.StatusOK, storageTokenResponse{
		AccessToken:  accessToken,
		ExpiresAt:    expiresAt,
		RefreshToken: exchange.RefreshToken,
		Region:       storageTokenRegion{ID: region.ID, Endpoint: region.Endpoint},
	})
}

// receiptBlobs reads object creates and updates on the hosted API, which name
// their blob with the receipt the region's storage service signed for the
// upload. The receipt must be the caller's, from the caller's region, and
// unexpired; the store then checks the ledger has never seen its key.
type receiptBlobs struct {
	verifier *hostedauth.ReceiptVerifier
	metrics  *Metrics
}

// ReadCreate decodes {"receipt": "<jwt>"}. The receipt is verified only if
// the create runs, not when it replays a stored result.
func (b receiptBlobs) ReadCreate(r *http.Request) (syncapi.BlobClaim, error) {
	var body struct {
		Receipt *string `json:"receipt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return syncapi.BlobClaim{}, errors.New("invalid json")
	}
	return syncapi.DeferredClaim(func() (objects.Blob, error) { return b.verify(r, body.Receipt) }), nil
}

// ReadUpdate decodes {"version": n, "receipt": "<jwt>"}. The receipt is
// verified only if the update runs, not when it replays a stored result.
func (b receiptBlobs) ReadUpdate(r *http.Request) (int64, syncapi.BlobClaim, error) {
	var body struct {
		Version *int64  `json:"version"`
		Receipt *string `json:"receipt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 0, syncapi.BlobClaim{}, errors.New("invalid json")
	}
	if body.Version == nil || *body.Version < 1 {
		return 0, syncapi.BlobClaim{}, errors.New("invalid version")
	}
	return *body.Version, syncapi.DeferredClaim(func() (objects.Blob, error) { return b.verify(r, body.Receipt) }), nil
}

var errInvalidReceipt = errors.New("invalid receipt")

func (b receiptBlobs) verify(r *http.Request, token *string) (objects.Blob, error) {
	p := principalFrom(r)
	reason := ""
	var receipt hostedauth.Receipt
	switch {
	case token == nil || *token == "":
		reason = "missing"
	case p.RegionID == "":
		reason = "no_region"
	default:
		var err error
		receipt, err = b.verifier.Verify(*token, p.RegionID)
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			reason = "expired"
		case err != nil:
			reason = "invalid"
			hlog.FromRequest(r).Info().Err(err).Msg("rejecting receipt")
		case receipt.UserID != p.Session.User.ID:
			reason = "wrong_user"
		}
	}
	if reason != "" {
		b.metrics.receiptFailure(r.Context(), reason)
		return objects.Blob{}, errInvalidReceipt
	}
	return objects.ReceiptBlob(receipt.Key, receipt.Size), nil
}
