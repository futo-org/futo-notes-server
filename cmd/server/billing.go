package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/polar"
	"futo-notes-server/internal/subscriptions"
	"futo-notes-server/internal/usage"
)

// requireEntitlement refuses a write from an account that may not currently
// write. It is middleware rather than a check inside the sync handlers, so
// the sync layer stays unaware that billing exists: adding a write path adds
// a line here, not a branch there.
//
// Off the hosted service it is the identity function. A self-hosted server
// sells nothing, so it gates nothing and takes no extra read per request.
func requireEntitlement(cfg config.Config, database *db.DB) func(http.HandlerFunc) http.HandlerFunc {
	if cfg.AuthMode != "oidc" {
		return func(next http.HandlerFunc) http.HandlerFunc { return next }
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// One primary-key read against the local record. Polar is never
			// on this path, so a Polar outage cannot stop people syncing.
			record, err := subscriptions.Of(r.Context(), database, sessionFrom(r).User.ID)
			if err != nil {
				serverError(w, r, "reading the account's Entitlement", err)
				return
			}
			if !record.Entitled(time.Now()) {
				writeJSON(w, http.StatusPaymentRequired, map[string]string{
					"error": "an active subscription is required to write",
					"code":  "subscription_required",
				})
				return
			}
			next(w, r)
		}
	}
}

// handleBillingStatus answers what an app needs to show an account's
// standing: whether it may write, the subscription state behind that, the
// Plan it is on, and how much of its Storage Quota it has used.
func handleBillingStatus(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeBillingStatus(w, r, database)
	}
}

// writeBillingStatus answers with the billing status body. The checkout
// route reuses it for an account that is already entitled, so an app that
// asks to pay twice gets the standing it should have shown instead, in the
// shape it already reads.
func writeBillingStatus(w http.ResponseWriter, r *http.Request, database *db.DB) {
	userID := sessionFrom(r).User.ID
	account, err := usage.Of(r.Context(), database, userID)
	if err != nil {
		serverError(w, r, "reading billing status", err)
		return
	}
	record, err := subscriptions.Of(r.Context(), database, userID)
	if err != nil {
		serverError(w, r, "reading the subscription record", err)
		return
	}
	var graceUntil *string
	if record.GraceUntil != nil {
		formatted := db.Timestamp(*record.GraceUntil)
		graceUntil = &formatted
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entitled":    record.Entitled(time.Now()),
		"state":       record.State,
		"grace_until": graceUntil,
		"plan":        account.Plan,
		"usage":       map[string]int64{"bytes_used": account.BytesUsed},
	})
}

// storageLimits is what a route accepting ciphertext needs before it reads a
// body: how large one blob may be, and whether the account has room for it.
//
// The per-blob cap comes from the Plan in every deployment, and the one plan
// carries the value the server used as a constant, so nothing changes for a
// self-hoster. The Storage Quota is a hosted limit, so quotaEnforced is off
// outside hosted mode and a self-hosted server keeps storing whatever fits on
// its disk.
type storageLimits struct {
	account       usage.Account
	quotaEnforced bool
}

// readBlobBodyWithinLimits is the whole admission check a route accepting one
// blob of ciphertext performs: the body is read under the Plan's per-blob cap
// and refused if storing it would cross the account's Storage Quota. It writes
// the 413 or 507 itself, so a false second return means the response is done.
func readBlobBodyWithinLimits(w http.ResponseWriter, r *http.Request, database *db.DB, quotaEnforced bool) ([]byte, bool) {
	limits, ok := loadStorageLimits(w, r, database, quotaEnforced)
	if !ok {
		return nil, false
	}
	body, ok := readBlobBody(w, r, limits.account.Plan.BlobMaxBytes)
	if !ok {
		return nil, false
	}
	if limits.exceeded(int64(len(body))) {
		writeAccountQuotaExceeded(w, limits.account)
		return nil, false
	}
	return body, true
}

func loadStorageLimits(w http.ResponseWriter, r *http.Request, database *db.DB, quotaEnforced bool) (storageLimits, bool) {
	account, err := usage.Of(r.Context(), database, sessionFrom(r).User.ID)
	if err != nil {
		serverError(w, r, "reading plan limits", err)
		return storageLimits{}, false
	}
	return storageLimits{account: account, quotaEnforced: quotaEnforced}, true
}

// exceeded reports whether storing incoming more bytes would put the account
// past its Storage Quota.
func (l storageLimits) exceeded(incoming int64) bool {
	return l.quotaEnforced && l.account.Exceeds(incoming)
}

// recordClaim adds bytes this request has just had claimed, so a batch weighs
// each entry against what the entries before it already took.
func (l *storageLimits) recordClaim(bytes int64) {
	l.account.BytesUsed += bytes
}

// writeAccountQuotaExceeded names the quota and the usage behind the refusal,
// so a client can tell the person how much room is left rather than only that
// the write failed. Both refusal points answer with this same body: the
// upload that would not fit, and the claim that finds the account full.
func writeAccountQuotaExceeded(w http.ResponseWriter, account usage.Account) {
	writeJSON(w, http.StatusInsufficientStorage, map[string]any{
		"error":       "storage quota exceeded",
		"code":        "quota_exceeded",
		"quota_bytes": account.Plan.StorageQuotaBytes,
		"bytes_used":  account.BytesUsed,
	})
}

// accountQuotaExceededEntry is the same refusal as one entry of a batch
// response, which reports per entry rather than failing the whole request.
func accountQuotaExceededEntry(account usage.Account) map[string]any {
	return map[string]any{
		"status":      "quota_exceeded",
		"quota_bytes": account.Plan.StorageQuotaBytes,
		"bytes_used":  account.BytesUsed,
	}
}

// maxWebhookBody caps one Polar delivery. Subscription payloads are a few
// kilobytes; the cap is what keeps an unauthenticated route from buffering
// whatever an unsigned caller sends.
const maxWebhookBody = 1 << 20

// handlePolarWebhook is how the subscription record learns anything. It is
// unauthenticated by necessity — Polar has no session — and authenticated in
// substance by the signature over the exact bytes delivered. A delivery that
// does not verify changes nothing.
func handlePolarWebhook(cfg config.Config, database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not read body")
			return
		}
		if err := polar.Verify(cfg.PolarWebhookSecret, r.Header, body, time.Now()); err != nil {
			slog.Warn("rejected a Polar webhook delivery", "err", err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "invalid webhook signature",
				"code":  "invalid_signature",
			})
			return
		}

		event, err := polar.ParseEvent(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		// The endpoint may be subscribed to more event types than this
		// server acts on. Acknowledging the rest is what stops Polar
		// retrying deliveries nothing will ever read.
		if !event.IsSubscription() {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}

		matched, err := subscriptions.Apply(r.Context(), database, event, body, time.Now())
		if err != nil {
			serverError(w, r, "applying a Polar subscription event", err)
			return
		}
		if !matched {
			// A retry would not find the account either, so this is an
			// acknowledgement with a log line rather than a failure.
			slog.Warn("Polar subscription event names an unknown customer",
				"type", event.Type, "external_customer_id", event.Subscription.ExternalCustomerID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "unknown_customer"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "applied"})
	}
}

// billingDonePath is where the page Polar returns the browser to is served,
// and therefore what every checkout's success URL points at.
const billingDonePath = "/billing/done"

// checkoutSuccessURL is where Polar sends the browser once the payment goes
// through. The server has no configured public origin of its own, so it takes
// the one OIDC_REDIRECT_URI already had to name, the same way the Login
// Hand-off builds its login URL. Config guarantees that URI is absolute and
// ends in the OIDC callback, so trimming the callback leaves the origin along
// with any prefix the server is mounted under.
func checkoutSuccessURL(redirectURI string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, config.OIDCCallbackPath) + billingDonePath
	return u.String(), nil
}

// handleBillingDone serves the page Polar returns the browser to. It says
// nothing about the account on purpose: Entitlement moves when Polar's
// webhook lands, not when the browser arrives here, so the page cannot
// honestly report success. The app finds out by re-reading GET /api/billing.
func handleBillingDone(w http.ResponseWriter, _ *http.Request) {
	writeBrowserDonePage(w, billingDonePage)
}

// handleBillingCheckout starts a subscription: it answers with a Polar
// checkout URL for the app to open in a browser. An account that may already
// write gets its billing status instead, because sending it to pay a second
// time would open a second subscription.
func handleBillingCheckout(cfg config.Config, database *db.DB, client *polar.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record, err := subscriptions.Of(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			serverError(w, r, "reading the subscription record", err)
			return
		}
		if record.Entitled(time.Now()) {
			writeBillingStatus(w, r, database)
			return
		}

		successURL, err := checkoutSuccessURL(cfg.OIDCRedirectURI)
		if err != nil {
			serverError(w, r, "building the checkout's success URL", err)
			return
		}
		customerID, ok := polarCustomerID(w, r, database, client)
		if !ok {
			return
		}
		checkoutURL, err := client.CreateCheckout(r.Context(), polar.CheckoutRequest{
			ProductID:  cfg.PolarProductID,
			DiscountID: cfg.PolarDiscountID,
			CustomerID: customerID,
			SuccessURL: successURL,
		})
		if err != nil {
			serverError(w, r, "opening a Polar checkout", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": checkoutURL})
	}
}

// handleBillingPortal answers with a pre-authenticated Polar customer portal
// URL, which is where a person cancels or changes a card. The server
// implements none of that itself.
//
// It is a GET that can write: reaching the portal without ever having checked
// out still needs a Polar customer, so the first call registers one. That is
// the plan's route shape, and the write is idempotent — the account ends up
// with the same customer however many times it is asked for.
func handleBillingPortal(database *db.DB, client *polar.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		customerID, ok := polarCustomerID(w, r, database, client)
		if !ok {
			return
		}
		portalURL, err := client.CreateCustomerSession(r.Context(), customerID)
		if err != nil {
			serverError(w, r, "opening a Polar customer portal session", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": portalURL})
	}
}

// polarCustomerID resolves the account's Polar customer, registering it the
// first time. Registration is lazy because most accounts never reach either
// of these routes, and Polar's copy of the account is only needed once one
// does.
//
// The account's OIDC sub goes to Polar as the external customer id, which is
// what the webhook matches a subscription back to, so both ends of the round
// trip agree without a lookup table. A false second return means the response
// is done.
func polarCustomerID(w http.ResponseWriter, r *http.Request, database *db.DB, client *polar.Client) (string, bool) {
	userID := sessionFrom(r).User.ID
	customer, err := subscriptions.CustomerOf(r.Context(), database, userID)
	if err != nil {
		serverError(w, r, "reading the account's Polar customer", err)
		return "", false
	}
	if customer.ID != "" {
		return customer.ID, true
	}

	// Polar refuses a second customer carrying an external id it has
	// already seen, so this call has to happen at most once per account.
	customerID, err := client.CreateCustomer(r.Context(), customer.ExternalID, customer.Email)
	if err != nil {
		serverError(w, r, "creating the account's Polar customer", err)
		return "", false
	}
	if err := subscriptions.SaveCustomerID(r.Context(), database, userID, customerID, time.Now()); err != nil {
		serverError(w, r, "storing the account's Polar customer", err)
		return "", false
	}
	return customerID, true
}
