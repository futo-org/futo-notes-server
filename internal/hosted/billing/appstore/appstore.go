// Package appstore is the App Store billing provider: StoreKit 2 signed
// transactions the app submits, App Store Server Notifications v2, and the
// App Store Server API for current status.
//
// Family Sharing is off for the subscription, so a transaction whose
// inAppOwnershipType is not PURCHASED never entitles: Submit refuses it and
// Verify and Refresh ignore it.
//
// In development, AcceptXcode admits transactions from Xcode's StoreKit
// testing, which Apple neither signs nor knows about (see Config).
package appstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"futo-notes-server/internal/hosted/billing"
)

// Config is an App Store app and the In-App Purchase key the server calls
// the App Store Server API with.
type Config struct {
	BundleID string
	// AppAppleID is the app's numeric Apple ID, which production
	// notifications carry.
	AppAppleID int64
	IssuerID   string
	KeyID      string
	PrivateKey *ecdsa.PrivateKey
	// ProductIDs are the auto-renewable subscriptions that entitle.
	ProductIDs []string
	// AcceptSandbox admits sandbox transactions: App Review and TestFlight
	// purchases are sandbox transactions in the production build.
	AcceptSandbox bool
	// ProductionURL and SandboxURL replace the App Store Server API's base
	// URLs; empty means Apple's.
	ProductionURL string
	SandboxURL    string
	// Roots replaces the trust anchor; nil means Apple Root CA - G3.
	Roots *x509.CertPool
	// AcceptXcode admits transactions whose environment is Xcode: StoreKit
	// testing in Xcode signs them with a local certificate, so, as in
	// Apple's App Store Server Library, their signature is not checked,
	// and Apple has no record of them, so Submit takes the transaction
	// itself as the subscription's state. Development only: anyone can
	// forge one. Transactions of other environments are verified as
	// always.
	AcceptXcode bool
}

// Provider is the App Store payment source.
type Provider struct {
	cfg      Config
	verifier verifier
	// api is nil without an In-App Purchase key: then only Xcode
	// transactions can be submitted, and nothing refreshed.
	api *apiClient
}

var _ billing.StoreProvider = (*Provider)(nil)

// New returns the App Store provider for cfg.
func New(cfg Config) *Provider {
	if cfg.ProductionURL == "" {
		cfg.ProductionURL = ProductionURL
	}
	if cfg.SandboxURL == "" {
		cfg.SandboxURL = SandboxURL
	}
	if cfg.Roots == nil {
		cfg.Roots = AppleRoots()
	}
	p := &Provider{cfg: cfg, verifier: verifier{roots: cfg.Roots}}
	if cfg.PrivateKey != nil {
		p.api = &apiClient{
			http:     &http.Client{Timeout: requestTimeout},
			issuerID: cfg.IssuerID,
			keyID:    cfg.KeyID,
			bundleID: cfg.BundleID,
			key:      cfg.PrivateKey,
		}
	}
	return p
}

// Source is billing.SourceAppStore.
func (p *Provider) Source() billing.Source { return billing.SourceAppStore }

// The App Store's environment names.
const (
	appleProduction = "Production"
	appleSandbox    = "Sandbox"
	appleXcode      = "Xcode"
)

// transaction is JWSTransactionDecodedPayload, as far as billing reads it.
type transaction struct {
	OriginalTransactionID string `json:"originalTransactionId"`
	BundleID              string `json:"bundleId"`
	ProductID             string `json:"productId"`
	AppAccountToken       string `json:"appAccountToken"`
	Environment           string `json:"environment"`
	InAppOwnershipType    string `json:"inAppOwnershipType"`
	ExpiresDate           int64  `json:"expiresDate"`
	RevocationDate        int64  `json:"revocationDate"`
	SignedDate            int64  `json:"signedDate"`
}

// renewal is JWSRenewalInfoDecodedPayload, as far as billing reads it.
type renewal struct {
	AutoRenewStatus        int   `json:"autoRenewStatus"`
	GracePeriodExpiresDate int64 `json:"gracePeriodExpiresDate"`
	SignedDate             int64 `json:"signedDate"`
}

// signed is a transaction and its renewal info, verified, with the payloads
// as Apple signed them.
type signed struct {
	tx         transaction
	txRaw      json.RawMessage
	renewal    renewal
	renewalRaw json.RawMessage
}

// errIgnored marks a verified transaction billing does not track: another
// product, a family-shared copy, or a refused environment.
var errIgnored = errors.New("app store: transaction not tracked")

// decode verifies a transaction and its optional renewal info, and checks
// the transaction belongs to this app. It returns errIgnored, wrapped, for
// one billing does not track.
func (p *Provider) decode(signedTx, signedRenewal string) (signed, error) {
	var s signed
	var err error
	if s.txRaw, err = p.verifyTransaction(signedTx, &s.tx); err != nil {
		return signed{}, err
	}
	if signedRenewal != "" {
		if s.renewalRaw, err = p.verifier.verify(signedRenewal, &s.renewal); err != nil {
			return signed{}, err
		}
	}
	if s.tx.BundleID != p.cfg.BundleID {
		return signed{}, fmt.Errorf("transaction for bundle %q", s.tx.BundleID)
	}
	if s.tx.OriginalTransactionID == "" {
		return signed{}, errors.New("transaction without originalTransactionId")
	}
	switch {
	case !slices.Contains(p.cfg.ProductIDs, s.tx.ProductID):
		return signed{}, fmt.Errorf("%w: product %q", errIgnored, s.tx.ProductID)
	case s.tx.InAppOwnershipType != "PURCHASED":
		return signed{}, fmt.Errorf("%w: ownership %q", errIgnored, s.tx.InAppOwnershipType)
	case !p.acceptsEnvironment(s.tx.Environment):
		return signed{}, fmt.Errorf("%w: environment %q", errIgnored, s.tx.Environment)
	}
	return s, nil
}

// acceptsEnvironment reports whether transactions of the App Store
// environment env are tracked.
func (p *Provider) acceptsEnvironment(env string) bool {
	switch env {
	case appleProduction:
		return true
	case appleSandbox:
		return p.cfg.AcceptSandbox
	case appleXcode:
		return p.cfg.AcceptXcode
	}
	return false
}

// verifyTransaction verifies a signed transaction and decodes it into v.
// With AcceptXcode, one whose payload says it is from Xcode is decoded
// without checking its signature, which no Apple certificate made.
func (p *Provider) verifyTransaction(signedTx string, v *transaction) (json.RawMessage, error) {
	if p.cfg.AcceptXcode {
		parts := strings.Split(signedTx, ".")
		if len(parts) == 3 {
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			var env struct {
				Environment string `json:"environment"`
			}
			if err == nil && json.Unmarshal(payload, &env) == nil && env.Environment == appleXcode {
				if err := json.Unmarshal(payload, v); err != nil {
					return nil, fmt.Errorf("%w: payload: %w", errSignature, err)
				}
				return payload, nil
			}
		}
	}
	return p.verifier.verify(signedTx, v)
}

// event is the subscription s describes, in status as of eventAt.
func (p *Provider) event(s signed, status billing.Status, eventAt int64, raw json.RawMessage) billing.Event {
	if s.tx.RevocationDate > 0 {
		status = billing.StatusRevoked
	}
	ev := billing.Event{
		Source:       billing.SourceAppStore,
		ExternalID:   s.tx.OriginalTransactionID,
		AccountToken: s.tx.AppAccountToken,
		ProductID:    s.tx.ProductID,
		Status:       status,
		WillRenew:    s.renewal.AutoRenewStatus == 1 && status != billing.StatusExpired && status != billing.StatusRevoked,
		ExpiresAt:    millis(s.tx.ExpiresDate),
		Environment:  billing.EnvironmentProduction,
		EventAt:      time.UnixMilli(eventAt),
		Raw:          raw,
	}
	switch s.tx.Environment {
	case appleSandbox:
		ev.Environment = billing.EnvironmentSandbox
	case appleXcode:
		ev.Environment = billing.EnvironmentXcode
	}
	if status == billing.StatusGrace {
		ev.GraceUntil = millis(s.renewal.GracePeriodExpiresDate)
	}
	return ev
}

func millis(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms)
	return &t
}

// statusCode maps the App Store Server API's subscription status.
func statusCode(code int) (billing.Status, bool) {
	switch code {
	case 1:
		return billing.StatusActive, true
	case 2:
		return billing.StatusExpired, true
	case 3: // billing retry, past any grace period
		return billing.StatusOnHold, true
	case 4:
		return billing.StatusGrace, true
	case 5:
		return billing.StatusRevoked, true
	}
	return "", false
}

// Submit verifies a StoreKit 2 transaction (its jwsRepresentation in
// p.Token) and confirms its subscription's current state with Apple. The
// signed productId is authoritative; p.ProductID is not read. The event's
// AccountToken is the submitted transaction's appAccountToken, empty when it
// has none: offer-code redemptions can carry none, and refusing them would
// leave the transaction unfinished on the device forever. An Xcode
// transaction is its own subscription state, as of its signedDate, and
// Apple is not asked: it has no record of it. Without the renewal info,
// which the app does not submit, it is reported as not renewing.
func (p *Provider) Submit(ctx context.Context, purchase billing.Purchase) (billing.Event, error) {
	s, err := p.decode(purchase.Token, "")
	if err != nil {
		return billing.Event{}, fmt.Errorf("%w: %w", billing.ErrInvalidPurchase, err)
	}
	if s.tx.Environment == appleXcode {
		raw, err := json.Marshal(map[string]any{"transaction": s.txRaw})
		if err != nil {
			return billing.Event{}, err
		}
		status := fallbackStatus(0, s.tx.ExpiresDate, s.tx.SignedDate)
		return p.event(s, status, s.tx.SignedDate, raw), nil
	}
	if p.api == nil {
		return billing.Event{}, fmt.Errorf("%w: %s transaction without an In-App Purchase key to confirm it", billing.ErrInvalidPurchase, s.tx.Environment)
	}
	base := p.cfg.ProductionURL
	if s.tx.Environment == appleSandbox {
		base = p.cfg.SandboxURL
	}
	ev, err := p.lookup(ctx, base, s.tx.OriginalTransactionID)
	if errors.Is(err, errNotFound) || errors.Is(err, errIgnored) {
		return billing.Event{}, fmt.Errorf("%w: %w", billing.ErrInvalidPurchase, err)
	}
	if err != nil {
		return billing.Event{}, err
	}
	ev.AccountToken = s.tx.AppAccountToken
	return ev, nil
}

// Refresh fetches the subscription with this originalTransactionId from the
// App Store Server API: production first, then sandbox when it is accepted
// and production does not know the transaction. Xcode transactions have
// nothing to refresh from; reconciliation leaves them alone.
func (p *Provider) Refresh(ctx context.Context, originalTransactionID string) (billing.Event, error) {
	if p.api == nil {
		return billing.Event{}, errors.New("app store: no In-App Purchase key to call the App Store Server API with")
	}
	ev, err := p.lookup(ctx, p.cfg.ProductionURL, originalTransactionID)
	if errors.Is(err, errNotFound) && p.cfg.AcceptSandbox {
		ev, err = p.lookup(ctx, p.cfg.SandboxURL, originalTransactionID)
	}
	return ev, err
}

// lookup asks the API at base for the subscription's status.
func (p *Provider) lookup(ctx context.Context, base, originalTransactionID string) (billing.Event, error) {
	res, err := p.api.subscriptionStatuses(ctx, base, originalTransactionID)
	if err != nil {
		return billing.Event{}, err
	}
	if res.BundleID != p.cfg.BundleID {
		return billing.Event{}, fmt.Errorf("app store server api: answered for bundle %q", res.BundleID)
	}
	for _, group := range res.Data {
		for _, last := range group.LastTransactions {
			if last.OriginalTransactionID != originalTransactionID {
				continue
			}
			status, ok := statusCode(last.Status)
			if !ok {
				return billing.Event{}, fmt.Errorf("app store server api: unknown status %d", last.Status)
			}
			s, err := p.decode(last.SignedTransactionInfo, last.SignedRenewalInfo)
			if err != nil {
				return billing.Event{}, fmt.Errorf("app store server api: %w", err)
			}
			raw, err := json.Marshal(map[string]any{"status": last.Status, "transaction": s.txRaw, "renewalInfo": s.renewalRaw})
			if err != nil {
				return billing.Event{}, err
			}
			// Apple signs both payloads as it answers, so their date is now
			// on Apple's clock, comparable with notifications' signedDate.
			return p.event(s, status, max(s.tx.SignedDate, s.renewal.SignedDate), raw), nil
		}
	}
	return billing.Event{}, fmt.Errorf("%w: %s not in the statuses", errNotFound, originalTransactionID)
}

// Cancel returns billing.ErrNotSupported: only the subscriber can cancel an
// App Store subscription.
func (p *Provider) Cancel(context.Context, string) error {
	return billing.ErrNotSupported
}

// notification is ResponseBodyV2DecodedPayload, as far as billing reads it.
type notification struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	NotificationUUID string `json:"notificationUUID"`
	SignedDate       int64  `json:"signedDate"`
	Data             *struct {
		AppAppleID            int64  `json:"appAppleId"`
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		SignedTransactionInfo string `json:"signedTransactionInfo"`
		SignedRenewalInfo     string `json:"signedRenewalInfo"`
		Status                int    `json:"status"`
	} `json:"data"`
}

// Verify authenticates an App Store Server Notification v2 and decodes the
// subscription it reports. The delivery id is its notificationUUID.
func (p *Provider) Verify(_ context.Context, d billing.Delivery) (string, []billing.Event, error) {
	var body struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := json.Unmarshal(d.Body, &body); err != nil || body.SignedPayload == "" {
		return "", nil, fmt.Errorf("%w: no signedPayload", billing.ErrInvalidDelivery)
	}
	var n notification
	payload, err := p.verifier.verify(body.SignedPayload, &n)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", billing.ErrInvalidDelivery, err)
	}
	if n.NotificationUUID == "" {
		return "", nil, fmt.Errorf("%w: no notificationUUID", billing.ErrInvalidDelivery)
	}
	status, tracked := notificationStatus(n)
	if !tracked || n.Data == nil {
		return n.NotificationUUID, nil, nil
	}
	if n.Data.BundleID != p.cfg.BundleID || (n.Data.Environment == appleProduction && n.Data.AppAppleID != p.cfg.AppAppleID) {
		return "", nil, fmt.Errorf("%w: notification for app %q (%d)", billing.ErrInvalidDelivery, n.Data.BundleID, n.Data.AppAppleID)
	}
	if n.Data.SignedTransactionInfo == "" {
		return n.NotificationUUID, nil, nil
	}
	s, err := p.decode(n.Data.SignedTransactionInfo, n.Data.SignedRenewalInfo)
	if errors.Is(err, errIgnored) {
		return n.NotificationUUID, nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", billing.ErrInvalidDelivery, err)
	}
	if status == "" {
		status = fallbackStatus(n.Data.Status, s.tx.ExpiresDate, n.SignedDate)
	}
	raw, err := json.Marshal(map[string]any{"notification": payload, "transaction": s.txRaw, "renewalInfo": s.renewalRaw})
	if err != nil {
		return "", nil, err
	}
	return n.NotificationUUID, []billing.Event{p.event(s, status, n.SignedDate, raw)}, nil
}

// notificationStatus maps a notification type to the status it puts the
// subscription in. An empty status with tracked true means the type does not
// settle it, so it comes from the notification's status field. Types billing
// does not track (TEST among them) return false.
func notificationStatus(n notification) (status billing.Status, tracked bool) {
	switch n.NotificationType {
	case "SUBSCRIBED", "DID_RENEW":
		return billing.StatusActive, true
	case "DID_FAIL_TO_RENEW":
		if n.Subtype == "GRACE_PERIOD" {
			return billing.StatusGrace, true
		}
		return billing.StatusOnHold, true
	case "GRACE_PERIOD_EXPIRED":
		// Apple keeps retrying billing after grace ends.
		return billing.StatusOnHold, true
	case "EXPIRED":
		return billing.StatusExpired, true
	case "REFUND", "REVOKE":
		return billing.StatusRevoked, true
	case "DID_CHANGE_RENEWAL_STATUS", "REFUND_REVERSED", "RENEWAL_EXTENDED":
		return "", true
	}
	return "", false
}

// fallbackStatus is the status a notification's data.status reports or,
// when it is absent, whether the transaction had expired when it was signed.
func fallbackStatus(code int, expiresDate, signedDate int64) billing.Status {
	if status, ok := statusCode(code); ok {
		return status
	}
	if expiresDate > signedDate {
		return billing.StatusActive
	}
	return billing.StatusExpired
}
