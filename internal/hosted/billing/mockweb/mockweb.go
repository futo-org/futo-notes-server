// Package mockweb is a web payment provider for development and for the
// apps' test stack, in the style of cmd/mock-oidc. Like the real one, it
// knows each customer by their OIDC sub. Its checkout and portal
// are pages the API itself serves under /dev/billing when DEV_UI is on.
// Completing a checkout, or cancelling in the portal, produces a webhook
// delivery signed with the shared secret, which the API applies through the
// same webhook route and Verify as a real provider's. Subscriptions live in
// memory, so a restart forgets them; the configuration refuses it without
// DEV_UI, so it never runs in production.
package mockweb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
	"uuid"

	"futo-notes-server/internal/hosted/billing"
)

const (
	// ProductID is the one plan the mock sells.
	ProductID = "notes-monthly"
	// Period is how long one payment entitles the account.
	Period = 30 * 24 * time.Hour
	// SignatureHeader carries the hex HMAC-SHA256 of a delivery's body under
	// the webhook secret.
	SignatureHeader = "X-Mock-Billing-Signature"
	// CheckoutPath is the API's dev page the mock's checkout URLs point at.
	CheckoutPath = "/dev/billing/checkout"
	// PortalPath is the API's dev page the mock's portal URLs point at.
	PortalPath = "/dev/billing/portal"
)

// ErrUnknownSubscription reports a subscription id the mock never sold, or
// sold before the process restarted.
var ErrUnknownSubscription = errors.New("unknown mock subscription")

// Provider is the mock web provider.
type Provider struct {
	publicURL string
	secret    []byte
	now       func() time.Time

	mu   sync.Mutex
	subs map[string]Subscription
}

// Subscription is one mock subscription.
type Subscription struct {
	ID string `json:"id"`
	// Subject is the customer's OIDC sub.
	Subject   string    `json:"subject"`
	ProductID string    `json:"product_id"`
	Status    string    `json:"status"`
	WillRenew bool      `json:"will_renew"`
	ExpiresAt time.Time `json:"expires_at"`
	// UpdatedAt is when the subscription last changed, the event time of
	// its deliveries.
	UpdatedAt time.Time `json:"updated_at"`
}

// payload is a delivery's body.
type payload struct {
	DeliveryID   string       `json:"delivery_id"`
	Subscription Subscription `json:"subscription"`
}

// New returns a mock whose pages are under publicURL and whose deliveries
// are signed with secret.
func New(publicURL string, secret []byte) *Provider {
	return &Provider{publicURL: publicURL, secret: secret, now: time.Now, subs: map[string]Subscription{}}
}

// Source is billing.SourceWeb.
func (p *Provider) Source() billing.Source { return billing.SourceWeb }

// CheckoutURL is the mock checkout page for c.
func (p *Provider) CheckoutURL(_ context.Context, c billing.Customer) (string, error) {
	return p.customerURL(CheckoutPath, c.Subject), nil
}

// PortalURL is the mock portal page for c.
func (p *Provider) PortalURL(_ context.Context, c billing.Customer) (string, error) {
	return p.customerURL(PortalPath, c.Subject), nil
}

// customerURL signs subject into a page link, as a real provider's session
// URL names its customer unforgeably.
func (p *Provider) customerURL(path, subject string) string {
	return p.publicURL + path + "?" + url.Values{"customer": {subject}, "sig": {p.sign([]byte("customer:" + subject))}}.Encode()
}

// Customer returns the sub of the customer a page link names, or false when
// its signature does not hold.
func (p *Provider) Customer(query url.Values) (string, bool) {
	subject := query.Get("customer")
	return subject, subject != "" && hmac.Equal([]byte(query.Get("sig")), []byte(p.sign([]byte("customer:"+subject))))
}

func (p *Provider) sign(data []byte) string {
	mac := hmac.New(sha256.New, p.secret)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a delivery's signature and decodes its one event.
func (p *Provider) Verify(_ context.Context, d billing.Delivery) (string, []billing.Event, error) {
	if !hmac.Equal([]byte(d.Header.Get(SignatureHeader)), []byte(p.sign(d.Body))) {
		return "", nil, fmt.Errorf("%w: bad signature", billing.ErrInvalidDelivery)
	}
	var body payload
	if err := json.Unmarshal(d.Body, &body); err != nil || body.DeliveryID == "" || body.Subscription.ID == "" {
		return "", nil, fmt.Errorf("%w: malformed body", billing.ErrInvalidDelivery)
	}
	return body.DeliveryID, []billing.Event{event(body.Subscription, d.Body)}, nil
}

func event(s Subscription, raw []byte) billing.Event {
	expires := s.ExpiresAt
	return billing.Event{
		Source:      billing.SourceWeb,
		ExternalID:  s.ID,
		Subject:     s.Subject,
		ProductID:   s.ProductID,
		Status:      billing.Status(s.Status),
		WillRenew:   s.WillRenew,
		ExpiresAt:   &expires,
		Environment: billing.EnvironmentSandbox,
		EventAt:     s.UpdatedAt,
		Raw:         raw,
	}
}

// Refresh reports a subscription's current state.
func (p *Provider) Refresh(_ context.Context, externalID string) (billing.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.subs[externalID]
	if !ok {
		return billing.Event{}, fmt.Errorf("%w: %s", ErrUnknownSubscription, externalID)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return billing.Event{}, err
	}
	return event(s, raw), nil
}

// Cancel stops a subscription renewing. Like a real provider, it says so in
// a later delivery, not here: Delivery produces it.
func (p *Provider) Cancel(_ context.Context, externalID string) error {
	_, err := p.update(externalID, func(s *Subscription) { s.WillRenew = false })
	return err
}

// Resume makes a cancelled subscription renew again.
func (p *Provider) Resume(externalID string) (Subscription, error) {
	return p.update(externalID, func(s *Subscription) { s.WillRenew = true })
}

func (p *Provider) update(externalID string, change func(*Subscription)) (Subscription, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.subs[externalID]
	if !ok {
		return Subscription{}, fmt.Errorf("%w: %s", ErrUnknownSubscription, externalID)
	}
	change(&s)
	s.UpdatedAt = p.now()
	p.subs[externalID] = s
	return s, nil
}

// Checkout completes a checkout for the customer whose sub is subject: a
// new subscription, paid for one Period.
func (p *Provider) Checkout(subject string) Subscription {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	s := Subscription{
		ID: "mock_sub_" + uuid.NewV7().String(), Subject: subject, ProductID: ProductID,
		Status: string(billing.StatusActive), WillRenew: true, ExpiresAt: now.Add(Period), UpdatedAt: now,
	}
	p.subs[s.ID] = s
	return s
}

// Record makes the mock agree with an event applied outside it, such as the
// /dev panel's synthetic ones, so a later Refresh does not undo it.
func (p *Provider) Record(ev billing.Event) {
	if ev.Source != billing.SourceWeb {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Subscription{
		ID: ev.ExternalID, Subject: ev.Subject, ProductID: ev.ProductID, Status: string(ev.Status),
		WillRenew: ev.WillRenew, UpdatedAt: ev.EventAt,
	}
	if ev.ExpiresAt != nil {
		s.ExpiresAt = *ev.ExpiresAt
	}
	p.subs[s.ID] = s
}

// Subscription returns the mock subscription id.
func (p *Provider) Subscription(id string) (Subscription, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.subs[id]
	return s, ok
}

// Subscriptions lists the mock subscriptions of the customer whose sub is
// subject, newest first.
func (p *Provider) Subscriptions(subject string) []Subscription {
	p.mu.Lock()
	defer p.mu.Unlock()
	var subs []Subscription
	for _, s := range p.subs {
		if s.Subject == subject {
			subs = append(subs, s)
		}
	}
	slices.SortFunc(subs, func(a, b Subscription) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return subs
}

// Delivery is the signed webhook delivery announcing s as it is now.
func (p *Provider) Delivery(s Subscription) (billing.Delivery, error) {
	body, err := json.Marshal(payload{DeliveryID: "mock_dlv_" + uuid.NewV7().String(), Subscription: s})
	if err != nil {
		return billing.Delivery{}, err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set(SignatureHeader, p.sign(body))
	return billing.Delivery{Header: header, Body: body}, nil
}
