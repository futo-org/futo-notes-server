// Package billing decides whether a hosted account has paid. Payment
// providers report subscriptions as normalized Events; the entitlement store
// keeps one row per subscription, and an account is entitled while any of its
// rows is active or in grace. The web provider (Stripe in production, a mock
// in development) is pluggable; the App Store and Play are providers behind the same interface.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Source names where a subscription was bought.
type Source string

// The subscription sources.
const (
	SourceWeb      Source = "web"
	SourceAppStore Source = "app_store"
	SourcePlay     Source = "play"
)

// Status is a subscription's state as its provider reports it.
type Status string

// The subscription states. Active and grace entitle the account; the rest do
// not.
const (
	StatusActive  Status = "active"
	StatusGrace   Status = "grace"
	StatusOnHold  Status = "on_hold"
	StatusExpired Status = "expired"
	StatusRevoked Status = "revoked"
)

// Environment is the provider environment a subscription lives in. App Review
// and TestFlight purchases are sandbox transactions in a production build.
type Environment string

// The provider environments.
const (
	EnvironmentProduction Environment = "production"
	EnvironmentSandbox    Environment = "sandbox"
	// EnvironmentXcode is Xcode's StoreKit testing, accepted only in
	// development. No provider knows its subscriptions, so reconciliation
	// skips them.
	EnvironmentXcode Environment = "xcode"
)

// Event is one provider's view of one subscription at EventAt. The store keeps
// the newest per (Source, ExternalID) and ignores older ones, so a late or
// retried notification never rolls an account back.
type Event struct {
	Source Source
	// ExternalID identifies the subscription at its provider: the web
	// provider's subscription id, the App Store originalTransactionId, or
	// the Play purchase token. A Play upgrade or resubscription issues a new
	// token (linkedPurchaseToken names the old one); each token is its own
	// row, the old one expires, and any active row entitles the account.
	ExternalID string
	// Subject is the OIDC sub of the account a web subscription belongs
	// to, as the web provider recorded it at checkout. Empty for store
	// sources.
	Subject string
	// AccountToken is the account's store_account_token as the store
	// recorded it: the App Store appAccountToken or the Play
	// obfuscatedExternalAccountId. Empty for web.
	AccountToken string
	ProductID    string
	Status       Status
	WillRenew    bool
	ExpiresAt    *time.Time
	GraceUntil   *time.Time
	Environment  Environment
	EventAt      time.Time
	// Raw is the provider's payload, decoded and verified, kept for support.
	Raw json.RawMessage
}

// Delivery is one webhook request as received, its body unread by anyone
// else so a signature over the raw bytes can be checked.
type Delivery struct {
	Header http.Header
	Body   []byte
}

// Provider is one payment source.
type Provider interface {
	Source() Source
	// Verify authenticates a webhook delivery and decodes it. deliveryID
	// identifies the delivery for deduplication. A delivery that verifies
	// but concerns nothing billing tracks returns no events.
	Verify(ctx context.Context, d Delivery) (deliveryID string, events []Event, err error)
	// Refresh fetches a subscription's current state from the provider, for
	// reconciliation when a notification may have been missed.
	Refresh(ctx context.Context, externalID string) (Event, error)
	// Cancel stops a subscription renewing at the end of its period. Sources
	// the server cannot cancel (the App Store) return ErrNotSupported.
	Cancel(ctx context.Context, externalID string) error
}

// Customer is the account a web checkout or portal is for. The web provider
// knows it by Subject, its OIDC sub; Email is only for receipts and is never
// matched on.
type Customer struct {
	UserID  string
	Subject string
	Email   string
}

// WebProvider is the web payment provider: checkout and self-service portal
// pages the browser is sent to.
type WebProvider interface {
	Provider
	CheckoutURL(ctx context.Context, c Customer) (string, error)
	PortalURL(ctx context.Context, c Customer) (string, error)
}

// CheckoutRoute is how a buyer pays on the web, decided by their billing
// country.
type CheckoutRoute int

// The checkout routes.
const (
	// RouteUnavailable: the web takes no payment from this country.
	RouteUnavailable CheckoutRoute = iota
	// RouteSelf: our own checkout page (StartCheckout), where FUTO is the
	// merchant of record.
	RouteSelf
	// RouteMerchantOfRecord: the provider's own checkout page
	// (StartMerchantCheckout), where the provider is the merchant of
	// record and handles the buyer's taxes.
	RouteMerchantOfRecord
)

// CustomCheckout is a web provider whose checkout is a page of ours: the
// provider's own fields collect the card inside it, and our page confirms
// the payment. For buyers in countries where it sells as merchant of
// record, the browser is sent to its own checkout page instead.
// CheckoutURL is unused.
type CustomCheckout interface {
	WebProvider
	// Route is how a buyer whose billing country is country, an ISO
	// 3166-1 alpha-2 code, pays.
	Route(country string) CheckoutRoute
	// StartMerchantCheckout opens a checkout on the provider's own page,
	// where it is the merchant of record, for c, and returns the URL to
	// send the browser to. Once paid, the browser returns to successURL,
	// with the checkout's id in place of {CHECKOUT_SESSION_ID}; abandoned,
	// to cancelURL. CompleteCheckout confirms it like any other.
	StartMerchantCheckout(ctx context.Context, c Customer, successURL, cancelURL string) (string, error)
	// PublishableKey is the browser-side key the page mounts checkout with.
	PublishableKey() string
	// Plan is the subscription checkout sells, as the provider prices it.
	Plan(ctx context.Context) (Plan, error)
	// StartCheckout opens a checkout for c that, once paid, returns the
	// browser to returnURL. The client secret mounts it in the browser and
	// must not be logged.
	StartCheckout(ctx context.Context, c Customer, returnURL string) (Checkout, error)
	// CompleteCheckout returns the subscription a completed checkout
	// created, so entitlement need not wait for the webhook. It returns
	// ErrCheckoutForeign for a checkout opened for another customer, and
	// ErrCheckoutIncomplete for one not yet paid.
	CompleteCheckout(ctx context.Context, c Customer, checkoutID string) (Event, error)
}

// Checkout is an open custom checkout.
type Checkout struct {
	ID           string
	ClientSecret string
}

// Plan is a recurring price: Amount in Currency's minor units, charged
// every IntervalCount Intervals.
type Plan struct {
	Amount int64
	// Currency is an ISO 4217 code, lowercase.
	Currency string
	// Interval is day, week, month or year.
	Interval      string
	IntervalCount int
}

// Purchase is what an app submits after a store purchase: the App Store
// transaction's JWS representation, or the Play purchase token with its
// product.
type Purchase struct {
	ProductID string
	Token     string
}

// StoreProvider is an app store: the app submits each purchase so
// entitlement does not wait for the store's notification.
type StoreProvider interface {
	Provider
	Submit(ctx context.Context, p Purchase) (Event, error)
}

// Provider errors.
var (
	// ErrNotSupported reports an operation the source cannot perform.
	ErrNotSupported = errors.New("not supported by this payment source")
	// ErrInvalidDelivery reports a webhook that fails authentication or
	// cannot be decoded. It answers 400 and is never retried by us.
	ErrInvalidDelivery = errors.New("invalid billing delivery")
	// ErrInvalidPurchase reports a submitted purchase the store does not
	// vouch for.
	ErrInvalidPurchase = errors.New("invalid purchase")
	// ErrCheckoutForeign reports a checkout opened for another customer, or
	// unknown to the provider.
	ErrCheckoutForeign = errors.New("checkout belongs to another customer")
	// ErrCheckoutIncomplete reports a checkout not yet paid.
	ErrCheckoutIncomplete = errors.New("checkout is not complete")
)
