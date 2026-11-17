// Package play is the Google Play billing provider: purchase tokens the app
// submits, real-time developer notifications pushed by Pub/Sub, and the Play
// Developer API (androidpublisher v3) for current state. Every state comes
// from subscriptionsv2.get; a notification only says which token to fetch.
package play

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"

	"futo-notes-server/internal/hosted/billing"
)

// The Play Developer API's base URL, the OAuth scope the service account
// needs, and Google's OIDC signing keys and issuer for Pub/Sub push tokens.
const (
	APIURL   = "https://androidpublisher.googleapis.com"
	Scope    = "https://www.googleapis.com/auth/androidpublisher"
	CertsURL = "https://www.googleapis.com/oauth2/v3/certs"
	issuer   = "https://accounts.google.com"
)

const (
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 1 << 20
)

// Config is a Play app, the service account the server calls the Play
// Developer API as, and the Pub/Sub push subscription that delivers its
// real-time developer notifications.
type Config struct {
	PackageName    string
	ServiceAccount *jwt.Config
	// ProductIDs are the subscriptions that entitle.
	ProductIDs []string
	// RTDNAudience is the audience the push subscription's OIDC token
	// carries, and RTDNServiceAccount the service account it is issued
	// for.
	RTDNAudience       string
	RTDNServiceAccount string
	// APIURL and CertsURL replace Google's; empty means Google's.
	APIURL   string
	CertsURL string
}

// ParseServiceAccount reads a service account key file (JSON) into the JWT
// configuration the Play Developer API is called with.
func ParseServiceAccount(data []byte) (*jwt.Config, error) {
	cfg, err := google.JWTConfigFromJSON(data, Scope)
	if err != nil {
		return nil, err
	}
	if cfg.Email == "" || len(cfg.PrivateKey) == 0 {
		return nil, errors.New("service account key has no client_email or private_key")
	}
	return cfg, nil
}

// Provider is the Google Play payment source.
type Provider struct {
	cfg  Config
	http *http.Client
	rtdn *oidc.IDTokenVerifier
}

var _ billing.StoreProvider = (*Provider)(nil)

// New returns the Play provider for cfg.
func New(cfg Config) *Provider {
	if cfg.APIURL == "" {
		cfg.APIURL = APIURL
	}
	if cfg.CertsURL == "" {
		cfg.CertsURL = CertsURL
	}
	cfg.APIURL = strings.TrimSuffix(cfg.APIURL, "/")
	base := &http.Client{Timeout: requestTimeout}
	client := cfg.ServiceAccount.Client(context.WithValue(context.Background(), oauth2.HTTPClient, base))
	client.Timeout = requestTimeout
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), base), cfg.CertsURL)
	return &Provider{
		cfg:  cfg,
		http: client,
		rtdn: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: cfg.RTDNAudience}),
	}
}

// Source is billing.SourcePlay.
func (p *Provider) Source() billing.Source { return billing.SourcePlay }

// subscription is SubscriptionPurchaseV2, as far as billing reads it.
type subscription struct {
	SubscriptionState          string          `json:"subscriptionState"`
	AcknowledgementState       string          `json:"acknowledgementState"`
	TestPurchase               json.RawMessage `json:"testPurchase"`
	ExternalAccountIdentifiers struct {
		ObfuscatedExternalAccountID string `json:"obfuscatedExternalAccountId"`
	} `json:"externalAccountIdentifiers"`
	LineItems []lineItem `json:"lineItems"`
}

type lineItem struct {
	ProductID        string    `json:"productId"`
	ExpiryTime       time.Time `json:"expiryTime"`
	AutoRenewingPlan *struct {
		AutoRenewEnabled bool `json:"autoRenewEnabled"`
	} `json:"autoRenewingPlan"`
}

// errNotFound reports a purchase token Google does not know, or no longer
// keeps (410 Gone, 60 days after expiry).
var errNotFound = errors.New("play: purchase token not found")

// errUnknownProduct reports a subscription to none of the configured
// products.
var errUnknownProduct = errors.New("play: subscription to another product")

// apiError is an error response from the Play Developer API.
type apiError struct {
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("play developer api: %d %s", e.StatusCode, e.Body)
}

// call sends one Play Developer API request and returns its body.
func (p *Provider) call(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.APIURL+"/androidpublisher/v3/applications/"+url.PathEscape(p.cfg.PackageName)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("play developer api: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("play developer api: reading response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		e := &apiError{StatusCode: res.StatusCode, Body: strings.TrimSpace(string(data))}
		if len(e.Body) > 300 {
			e.Body = e.Body[:300]
		}
		if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone {
			return nil, fmt.Errorf("%w: %w", errNotFound, e)
		}
		return nil, e
	}
	return data, nil
}

// get calls purchases.subscriptionsv2.get.
func (p *Provider) get(ctx context.Context, token string) (subscription, json.RawMessage, error) {
	data, err := p.call(ctx, http.MethodGet, "/purchases/subscriptionsv2/tokens/"+url.PathEscape(token), nil)
	if err != nil {
		return subscription{}, nil, err
	}
	var s subscription
	if err := json.Unmarshal(data, &s); err != nil {
		return subscription{}, nil, fmt.Errorf("play developer api: %w", err)
	}
	return s, data, nil
}

// productCall posts to a purchases.subscriptions (v1) method, which names
// the subscription's product as well as its token.
func (p *Provider) productCall(ctx context.Context, productID, token, method string) error {
	_, err := p.call(ctx, http.MethodPost,
		"/purchases/subscriptions/"+url.PathEscape(productID)+"/tokens/"+url.PathEscape(token)+":"+method, strings.NewReader("{}"))
	return err
}

// event is the subscription s for token, as fetched at now.
func (p *Provider) event(token string, s subscription, raw json.RawMessage, now time.Time) (billing.Event, error) {
	i := slices.IndexFunc(s.LineItems, func(li lineItem) bool { return slices.Contains(p.cfg.ProductIDs, li.ProductID) })
	if i < 0 {
		return billing.Event{}, errUnknownProduct
	}
	item := s.LineItems[i]
	ev := billing.Event{
		Source:       billing.SourcePlay,
		ExternalID:   token,
		AccountToken: s.ExternalAccountIdentifiers.ObfuscatedExternalAccountID,
		ProductID:    item.ProductID,
		WillRenew:    item.AutoRenewingPlan != nil && item.AutoRenewingPlan.AutoRenewEnabled,
		Environment:  billing.EnvironmentProduction,
		EventAt:      now,
		Raw:          raw,
	}
	if !item.ExpiryTime.IsZero() {
		expires := item.ExpiryTime
		ev.ExpiresAt = &expires
	}
	if len(s.TestPurchase) > 0 && string(s.TestPurchase) != "null" {
		ev.Environment = billing.EnvironmentSandbox
	}
	switch s.SubscriptionState {
	case "SUBSCRIPTION_STATE_ACTIVE":
		ev.Status = billing.StatusActive
	case "SUBSCRIPTION_STATE_IN_GRACE_PERIOD":
		// Play keeps the line item's expiryTime at the end of the grace
		// period while it lasts.
		ev.Status = billing.StatusGrace
		ev.GraceUntil = ev.ExpiresAt
	case "SUBSCRIPTION_STATE_ON_HOLD", "SUBSCRIPTION_STATE_PAUSED":
		ev.Status = billing.StatusOnHold
	case "SUBSCRIPTION_STATE_CANCELED":
		// Cancelled by the user or the developer: paid up until expiry.
		ev.Status = billing.StatusActive
		if ev.ExpiresAt == nil || !ev.ExpiresAt.After(now) {
			ev.Status = billing.StatusExpired
		}
		ev.WillRenew = false
	case "SUBSCRIPTION_STATE_PENDING":
		// The first payment has not cleared.
		ev.Status = billing.StatusOnHold
		ev.WillRenew = false
	case "SUBSCRIPTION_STATE_EXPIRED", "SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED":
		ev.Status = billing.StatusExpired
		ev.WillRenew = false
	default:
		return billing.Event{}, fmt.Errorf("play developer api: unknown subscriptionState %q", s.SubscriptionState)
	}
	return ev, nil
}

// Submit verifies a purchase token with Google and acknowledges it if it is
// not yet, which Play requires within three days or it refunds the
// purchase. The product comes from Google; p.ProductID is not read.
func (p *Provider) Submit(ctx context.Context, purchase billing.Purchase) (billing.Event, error) {
	if purchase.Token == "" {
		return billing.Event{}, fmt.Errorf("%w: no purchase token", billing.ErrInvalidPurchase)
	}
	s, raw, err := p.get(ctx, purchase.Token)
	var apiErr *apiError
	if errors.Is(err, errNotFound) || (errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest) {
		return billing.Event{}, fmt.Errorf("%w: %w", billing.ErrInvalidPurchase, err)
	}
	if err != nil {
		return billing.Event{}, err
	}
	ev, err := p.event(purchase.Token, s, raw, time.Now())
	if errors.Is(err, errUnknownProduct) {
		return billing.Event{}, fmt.Errorf("%w: %w", billing.ErrInvalidPurchase, err)
	}
	if err != nil {
		return billing.Event{}, err
	}
	if ev.AccountToken == "" {
		return billing.Event{}, fmt.Errorf("%w: purchase has no obfuscatedExternalAccountId", billing.ErrInvalidPurchase)
	}
	if err := p.acknowledge(ctx, purchase.Token, s, ev); err != nil {
		return billing.Event{}, err
	}
	return ev, nil
}

// acknowledge acknowledges an entitling purchase Google still holds as
// pending, which Play requires within three days or it refunds it. Both
// Submit and notifications do it, so a purchase whose app never submitted it
// (killed, offline) is still kept. A purchase without the account token our
// app sets was not bought through it and is left alone.
func (p *Provider) acknowledge(ctx context.Context, token string, s subscription, ev billing.Event) error {
	if s.AcknowledgementState != "ACKNOWLEDGEMENT_STATE_PENDING" || ev.AccountToken == "" ||
		(ev.Status != billing.StatusActive && ev.Status != billing.StatusGrace) {
		return nil
	}
	if err := p.productCall(ctx, ev.ProductID, token, "acknowledge"); err != nil {
		return fmt.Errorf("acknowledging: %w", err)
	}
	return nil
}

// Refresh fetches the subscription with this purchase token.
func (p *Provider) Refresh(ctx context.Context, token string) (billing.Event, error) {
	s, raw, err := p.get(ctx, token)
	if err != nil {
		return billing.Event{}, err
	}
	return p.event(token, s, raw, time.Now())
}

// Cancel stops the subscription with this purchase token renewing; it stays
// paid up until its expiry.
func (p *Provider) Cancel(ctx context.Context, token string) error {
	ev, err := p.Refresh(ctx, token)
	if err != nil {
		return err
	}
	return p.productCall(ctx, ev.ProductID, token, "cancel")
}

// pushBody is a Pub/Sub push request.
type pushBody struct {
	Message struct {
		Data      []byte `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
}

// notification is a DeveloperNotification, as far as billing reads it.
type notification struct {
	PackageName              string `json:"packageName"`
	SubscriptionNotification *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
	} `json:"subscriptionNotification"`
	VoidedPurchaseNotification *struct {
		PurchaseToken string `json:"purchaseToken"`
		ProductType   int    `json:"productType"`
	} `json:"voidedPurchaseNotification"`
}

const (
	// subscriptionRevoked is SUBSCRIPTION_REVOKED: refunded and ended
	// before its expiry.
	subscriptionRevoked = 12
	// productTypeSubscription is a voided purchase's PRODUCT_TYPE_SUBSCRIPTION.
	productTypeSubscription = 1
)

// Verify authenticates a Pub/Sub push of a real-time developer notification
// and fetches the subscription it names. The delivery id is the Pub/Sub
// messageId.
func (p *Provider) Verify(ctx context.Context, d billing.Delivery) (string, []billing.Event, error) {
	raw, ok := strings.CutPrefix(d.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", nil, fmt.Errorf("%w: no bearer token", billing.ErrInvalidDelivery)
	}
	token, err := p.rtdn.Verify(ctx, raw)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", billing.ErrInvalidDelivery, err)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := token.Claims(&claims); err != nil || claims.Email != p.cfg.RTDNServiceAccount || !claims.EmailVerified {
		return "", nil, fmt.Errorf("%w: push token is not from %s", billing.ErrInvalidDelivery, p.cfg.RTDNServiceAccount)
	}
	var push pushBody
	if err := json.Unmarshal(d.Body, &push); err != nil || push.Message.MessageID == "" {
		return "", nil, fmt.Errorf("%w: not a Pub/Sub push", billing.ErrInvalidDelivery)
	}
	var n notification
	if err := json.Unmarshal(push.Message.Data, &n); err != nil {
		return "", nil, fmt.Errorf("%w: message data: %w", billing.ErrInvalidDelivery, err)
	}
	id := push.Message.MessageID
	var purchaseToken string
	revoked := false
	switch {
	case n.PackageName != p.cfg.PackageName:
		return id, nil, nil
	case n.SubscriptionNotification != nil:
		purchaseToken = n.SubscriptionNotification.PurchaseToken
		revoked = n.SubscriptionNotification.NotificationType == subscriptionRevoked
	case n.VoidedPurchaseNotification != nil && n.VoidedPurchaseNotification.ProductType == productTypeSubscription:
		purchaseToken = n.VoidedPurchaseNotification.PurchaseToken
		revoked = true
	default:
		// Test and one-time product notifications.
		return id, nil, nil
	}
	if purchaseToken == "" {
		return "", nil, fmt.Errorf("%w: notification without a purchase token", billing.ErrInvalidDelivery)
	}
	s, payload, err := p.get(ctx, purchaseToken)
	if errors.Is(err, errNotFound) {
		return id, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	ev, err := p.event(purchaseToken, s, payload, time.Now())
	if errors.Is(err, errUnknownProduct) {
		return id, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if err := p.acknowledge(ctx, purchaseToken, s, ev); err != nil {
		return "", nil, err
	}
	// Google reports a revoked subscription as expired; the notification
	// says why.
	if revoked && ev.Status == billing.StatusExpired {
		ev.Status = billing.StatusRevoked
	}
	return id, []billing.Event{ev}, nil
}
