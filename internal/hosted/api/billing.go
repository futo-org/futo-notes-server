package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/httputil"
)

// maxWebhookBody caps a provider's webhook delivery. App Store notifications,
// the largest, are a few KiB of JWS.
const maxWebhookBody = 1 << 20

// billingRoutes mounts the caller's billing endpoints. None is gated: an
// account without a subscription must be able to get one.
func (s *Server) billingRoutes(r chi.Router) {
	r.Get("/billing", s.handleBilling)
	r.Post("/billing/checkout", s.handleCheckout)
	r.Get("/billing/portal", s.handlePortal)
	r.Post("/billing/app-store/transactions", s.handleAppStoreTransaction)
	r.Post("/billing/play/purchases", s.handlePlayPurchase)
}

// billingResponse is the body of GET /api/billing and of the purchase
// submissions. The subscription fields describe the account's current
// subscription and are null when it never had one.
type billingResponse struct {
	Entitled          bool         `json:"entitled"`
	Status            *string      `json:"status"`
	Source            *string      `json:"source"`
	ProductID         *string      `json:"product_id"`
	ExpiresAt         *time.Time   `json:"expires_at"`
	GraceUntil        *time.Time   `json:"grace_until"`
	WillRenew         bool         `json:"will_renew"`
	StoreAccountToken string       `json:"store_account_token"`
	Rails             billingRails `json:"rails"`
}

// billingRails says which ways to pay the deployment offers.
type billingRails struct {
	Web      bool `json:"web"`
	AppStore bool `json:"app_store"`
	Play     bool `json:"play"`
}

// billingURL is the body of the checkout and portal endpoints.
type billingURL struct {
	URL string `json:"url"`
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return new(t.UTC())
}

func (s *Server) billingBody(state billing.State) billingResponse {
	response := billingResponse{
		Entitled:          state.Entitled,
		StoreAccountToken: state.StoreAccountToken,
		Rails: billingRails{
			Web:      s.providers.Web != nil,
			AppStore: s.providers.Store(billing.SourceAppStore) != nil,
			Play:     s.providers.Store(billing.SourcePlay) != nil,
		},
	}
	if e := state.Current; e != nil {
		response.Status = new(string(e.Status))
		response.Source = new(string(e.Source))
		response.ProductID = new(e.ProductID)
		response.ExpiresAt = utc(e.ExpiresAt)
		response.GraceUntil = utc(e.GraceUntil)
		response.WillRenew = e.WillRenew
	}
	return response
}

// writeBilling answers with the caller's billing state.
func (s *Server) writeBilling(w http.ResponseWriter, r *http.Request) {
	state, err := s.billing.State(r.Context(), principalFrom(r).Session.User.ID)
	if err != nil {
		serverError(w, r, "reading billing state", err)
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, s.billingBody(state))
}

// handleBilling serves GET /api/billing.
func (s *Server) handleBilling(w http.ResponseWriter, r *http.Request) {
	s.writeBilling(w, r)
}

// Billing failures shared by the JSON routes and the dashboard.
var (
	errBillingUnavailable = errors.New("no web payment provider is configured")
	errAlreadySubscribed  = errors.New("already subscribed")
	errManagedByStore     = errors.New("the subscription is managed by an app store")
)

// checkoutURL is the web checkout for p's account. It refuses an account
// that is already entitled from any source, so nobody pays twice.
func (s *Server) checkoutURL(ctx context.Context, userID, email string) (string, error) {
	if s.providers.Web == nil {
		return "", errBillingUnavailable
	}
	state, err := s.billing.State(ctx, userID)
	if err != nil {
		return "", err
	}
	if state.Entitled {
		return "", errAlreadySubscribed
	}
	return s.providers.Web.CheckoutURL(ctx, billing.Customer{UserID: userID, Email: email})
}

// portalURL is the web provider's self-service page for the account,
// refused when the account's subscription is an app store's, which only the
// store can manage.
func (s *Server) portalURL(ctx context.Context, userID, email string) (string, error) {
	if s.providers.Web == nil {
		return "", errBillingUnavailable
	}
	state, err := s.billing.State(ctx, userID)
	if err != nil {
		return "", err
	}
	if state.Current != nil && state.Current.Source != billing.SourceWeb {
		return "", errManagedByStore
	}
	return s.providers.Web.PortalURL(ctx, billing.Customer{UserID: userID, Email: email})
}

// writeBillingURLError answers a checkout or portal failure.
func writeBillingURLError(w http.ResponseWriter, r *http.Request, action string, err error) {
	switch {
	case errors.Is(err, errBillingUnavailable):
		httputil.WriteError(w, r, http.StatusServiceUnavailable, codeBillingUnavailable, "Web payments are not available")
	case errors.Is(err, errAlreadySubscribed):
		httputil.WriteError(w, r, http.StatusConflict, codeAlreadySubscribed, "The account already has an active subscription")
	case errors.Is(err, errManagedByStore):
		httputil.WriteError(w, r, http.StatusConflict, codeManagedByStore, "The subscription is managed in the app store it was bought from")
	default:
		serverError(w, r, action, err)
	}
}

// handleCheckout serves POST /api/billing/checkout.
func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	user := principalFrom(r).Session.User
	target, err := s.checkoutURL(r.Context(), user.ID, user.Email)
	if err != nil {
		writeBillingURLError(w, r, "creating checkout", err)
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, billingURL{URL: target})
}

// handlePortal serves GET /api/billing/portal.
func (s *Server) handlePortal(w http.ResponseWriter, r *http.Request) {
	user := principalFrom(r).Session.User
	target, err := s.portalURL(r.Context(), user.ID, user.Email)
	if err != nil {
		writeBillingURLError(w, r, "creating portal session", err)
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, billingURL{URL: target})
}

// handleAppStoreTransaction serves POST /api/billing/app-store/transactions:
// the app posts the StoreKit 2 transaction it just bought or restored.
func (s *Server) handleAppStoreTransaction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SignedTransaction string `json:"signed_transaction"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.SignedTransaction == "" {
		badRequest(w, r, "signed_transaction is required")
		return
	}
	s.submitPurchase(w, r, billing.SourceAppStore, billing.Purchase{Token: body.SignedTransaction})
}

// handlePlayPurchase serves POST /api/billing/play/purchases: the app posts
// the purchase token Play gave it.
func (s *Server) handlePlayPurchase(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProductID     string `json:"product_id"`
		PurchaseToken string `json:"purchase_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ProductID == "" || body.PurchaseToken == "" {
		badRequest(w, r, "product_id and purchase_token are required")
		return
	}
	s.submitPurchase(w, r, billing.SourcePlay, billing.Purchase{ProductID: body.ProductID, Token: body.PurchaseToken})
}

// submitPurchase verifies a purchase with its store, binds it to the caller
// unless another account owns it, and answers with the caller's billing
// state.
func (s *Server) submitPurchase(w http.ResponseWriter, r *http.Request, source billing.Source, purchase billing.Purchase) {
	store := s.providers.Store(source)
	if store == nil {
		httputil.WriteError(w, r, http.StatusServiceUnavailable, codeBillingUnavailable, fmt.Sprintf("%s purchases are not available", source))
		return
	}
	ev, err := store.Submit(r.Context(), purchase)
	if errors.Is(err, billing.ErrInvalidPurchase) {
		hlog.FromRequest(r).Info().Err(err).Str("source", string(source)).Msg("rejecting purchase")
		httputil.WriteError(w, r, http.StatusBadRequest, codeInvalidPurchase, "The store does not vouch for this purchase")
		return
	}
	if err == nil && ev.Source != source {
		err = fmt.Errorf("the %s provider returned a %s event", source, ev.Source)
	}
	if err != nil {
		serverError(w, r, "verifying purchase", err)
		return
	}
	_, err = s.billing.Submit(r.Context(), principalFrom(r).Session.User.ID, ev)
	if errors.Is(err, billing.ErrOwnedElsewhere) {
		httputil.WriteError(w, r, http.StatusConflict, codeStoreSubscriptionElsewhere,
			"This store subscription belongs to another FUTO account")
		return
	}
	if err != nil {
		serverError(w, r, "recording purchase", err)
		return
	}
	s.writeBilling(w, r)
}

// webhookAck is the body of an accepted webhook delivery.
type webhookAck struct {
	// Duplicate reports a delivery already applied.
	Duplicate bool `json:"duplicate"`
}

// handleBillingWebhook serves POST /api/billing/webhooks/{provider}: a
// provider's signed notification. It is authenticated by the provider's
// signature, not a session. A delivery seen before, or about an account
// that no longer exists, is acknowledged so the provider stops retrying.
func (s *Server) handleBillingWebhook(w http.ResponseWriter, r *http.Request) {
	source := billing.Source(chi.URLParam(r, "provider"))
	provider := s.providers.Provider(source)
	if provider == nil {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if maxErr := new(http.MaxBytesError); errors.As(err, &maxErr) {
		httputil.WriteError(w, r, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "Delivery too large")
		return
	}
	if err != nil {
		badRequest(w, r, "Unreadable body")
		return
	}
	deliveryID, events, err := provider.Verify(r.Context(), billing.Delivery{Header: r.Header, Body: body})
	if errors.Is(err, billing.ErrInvalidDelivery) {
		hlog.FromRequest(r).Warn().Err(err).Str("source", string(source)).Msg("rejecting billing delivery")
		httputil.WriteError(w, r, http.StatusBadRequest, codeInvalidDelivery, "Invalid delivery")
		return
	}
	if err != nil {
		serverError(w, r, "verifying billing delivery", err)
		return
	}
	for _, ev := range events {
		if ev.Source != source {
			serverError(w, r, "verifying billing delivery", fmt.Errorf("the %s provider returned a %s event", source, ev.Source))
			return
		}
	}
	duplicate, unknown, err := s.billing.ApplyDelivery(r.Context(), source, deliveryID, events)
	if err != nil {
		serverError(w, r, "applying billing delivery", err)
		return
	}
	for _, ev := range unknown {
		hlog.FromRequest(r).Warn().Str("source", string(source)).Str("delivery", deliveryID).
			Str("subscription", ev.ExternalID).Msg("billing event names no account; acknowledged")
	}
	httputil.WriteJSON(w, r, http.StatusOK, webhookAck{Duplicate: duplicate})
}

// cancelSubscriptions stops userID's web and Play subscriptions renewing,
// on account deletion. A failure is logged and does not stop the deletion:
// the purge tries again before it deletes the account.
func (s *Server) cancelSubscriptions(ctx context.Context, userID string) {
	if err := s.providers.CancelRenewals(ctx, s.billing, userID); err != nil {
		log.Ctx(ctx).Error().Err(err).Msg("cancelling subscriptions of an account being deleted")
	}
}
