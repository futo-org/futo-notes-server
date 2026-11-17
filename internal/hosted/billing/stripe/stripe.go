// Package stripe is the Stripe web payment provider, over Stripe's REST API
// (form-encoded, at a pinned API version). Each FUTO account is a Stripe
// Customer carrying metadata.sub, its OIDC sub, and every subscription the
// checkout creates carries the same sub, which is how webhooks and refreshes
// find the account. Nothing is matched by email.
//
// Checkout is our own page: the server opens a Checkout Session in
// ui_mode=elements, and the page mounts Stripe's Payment Element for the
// card and confirms the session with Stripe.js. Webhooks
// (customer.subscription.created, .updated, .deleted) only say which
// subscription changed; its state is always fetched from Stripe.
package stripe

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"futo-notes-server/internal/hosted/billing"
)

const (
	// APIURL is Stripe's API.
	APIURL = "https://api.stripe.com"
	// APIVersion is the Stripe-Version every request pins, so the shapes
	// read here do not move with the account's default version.
	APIVersion = "2026-10-28.endive"
	// JSURL is Stripe.js for APIVersion. Stripe requires loading it from
	// js.stripe.com; it may not be bundled or self-hosted.
	JSURL = "https://js.stripe.com/endive/stripe.js"
	// SignatureHeader carries a webhook delivery's signatures.
	SignatureHeader = "Stripe-Signature"
	// SignatureTolerance is how far a delivery's signed timestamp may be
	// from now. Stripe signs every retry afresh.
	SignatureTolerance = 5 * time.Minute
	// metadataSub is the metadata key holding the account's OIDC sub, on
	// customers, subscriptions and checkout sessions.
	metadataSub = "sub"
)

const (
	requestTimeout   = 30 * time.Second
	maxResponseBytes = 1 << 20
)

// Config is a Stripe account and the plan the deployment sells.
type Config struct {
	// SecretKey calls the API (STRIPE_SECRET_KEY): sk_ or a restricted rk_.
	SecretKey string
	// PublishableKey mounts checkout in the browser (STRIPE_PUBLISHABLE_KEY).
	PublishableKey string
	// WebhookSecret verifies webhook signatures (STRIPE_WEBHOOK_SECRET),
	// whole, whsec_ prefix included.
	WebhookSecret []byte
	// PriceID is the one plan (STRIPE_PRICE_ID).
	PriceID string
	// PublicURL is the API's external base URL; the portal returns to its
	// dashboard.
	PublicURL string
	// APIURL replaces Stripe's (STRIPE_API_URL, for tests); empty means
	// Stripe's.
	APIURL string
}

// Provider is the Stripe web payment provider.
type Provider struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	// plan caches the price once read: a Stripe price's amount, currency
	// and recurrence can never change.
	planMu sync.Mutex
	plan   *billing.Plan
}

var _ billing.CustomCheckout = (*Provider)(nil)

// New returns the Stripe provider for cfg.
func New(cfg Config) *Provider {
	if cfg.APIURL == "" {
		cfg.APIURL = APIURL
	}
	cfg.APIURL = strings.TrimSuffix(cfg.APIURL, "/")
	return &Provider{cfg: cfg, http: &http.Client{Timeout: requestTimeout}, now: time.Now}
}

// Source is billing.SourceWeb.
func (p *Provider) Source() billing.Source { return billing.SourceWeb }

// PublishableKey is the key Stripe.js is initialized with.
func (p *Provider) PublishableKey() string { return p.cfg.PublishableKey }

// errNotFound reports an object Stripe does not have.
var errNotFound = errors.New("stripe: no such object")

// apiError is an error response from Stripe.
type apiError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("stripe api: %d %s %s: %s", e.StatusCode, e.Type, e.Code, e.Message)
}

// call sends one API request with form as its query (GET) or body, and
// decodes the response into out. A non-empty idempotencyKey makes a retried
// POST return the first one's result.
func (p *Provider) call(ctx context.Context, method, path string, form url.Values, idempotencyKey string, out any) error {
	target := p.cfg.APIURL + "/v1" + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.SecretKey)
	req.Header.Set("Stripe-Version", APIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	res, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe api: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("stripe api: reading response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var envelope struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		e := &apiError{StatusCode: res.StatusCode, Type: envelope.Error.Type, Code: envelope.Error.Code, Message: envelope.Error.Message}
		if res.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %w", errNotFound, e)
		}
		return e
	}
	// Never quote a successful body in an error: a checkout session's
	// carries its client secret.
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("stripe api: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// customerRef is a Customer as a subscription or session names it: its id,
// or the object when expanded.
type customerRef struct {
	ID       string
	Deleted  bool
	Metadata map[string]string
}

func (c *customerRef) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &c.ID)
	}
	var obj struct {
		ID       string            `json:"id"`
		Deleted  bool              `json:"deleted"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*c = customerRef{ID: obj.ID, Deleted: obj.Deleted, Metadata: obj.Metadata}
	return nil
}

// subscription is a Subscription, as far as billing reads it. At this API
// version the billing period lives on each item, not the subscription.
type subscription struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	CancelAt          *int64            `json:"cancel_at"`
	Livemode          bool              `json:"livemode"`
	Metadata          map[string]string `json:"metadata"`
	Customer          customerRef       `json:"customer"`
	Items             struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
			Price            struct {
				ID      string `json:"id"`
				Product string `json:"product"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// getSubscription fetches a subscription with its customer expanded, whose
// metadata names the account when the subscription's does not.
func (p *Provider) getSubscription(ctx context.Context, id string) (subscription, json.RawMessage, error) {
	var raw json.RawMessage
	if err := p.call(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(id), url.Values{"expand[]": {"customer"}}, "", &raw); err != nil {
		return subscription{}, nil, err
	}
	var s subscription
	if err := json.Unmarshal(raw, &s); err != nil {
		return subscription{}, nil, fmt.Errorf("stripe api: decoding subscription: %w", err)
	}
	return s, raw, nil
}

// event is s as billing records it, reported at eventAt.
func event(s subscription, raw json.RawMessage, eventAt time.Time) (billing.Event, error) {
	ev := billing.Event{
		Source:      billing.SourceWeb,
		ExternalID:  s.ID,
		Subject:     s.Metadata[metadataSub],
		Environment: billing.EnvironmentProduction,
		EventAt:     eventAt,
		Raw:         raw,
	}
	if ev.Subject == "" {
		ev.Subject = s.Customer.Metadata[metadataSub]
	}
	if !s.Livemode {
		ev.Environment = billing.EnvironmentSandbox
	}
	var periodEnd int64
	for _, item := range s.Items.Data {
		periodEnd = max(periodEnd, item.CurrentPeriodEnd)
		if ev.ProductID == "" {
			ev.ProductID = item.Price.Product
		}
	}
	if periodEnd > 0 {
		ev.ExpiresAt = new(time.Unix(periodEnd, 0).UTC())
	}
	ended := false
	switch s.Status {
	case "active", "trialing":
		ev.Status = billing.StatusActive
	case "past_due":
		// The renewal failed and Stripe is retrying it; the account keeps
		// access until the period it failed to pay for ends.
		ev.Status = billing.StatusGrace
		ev.GraceUntil = ev.ExpiresAt
	case "unpaid", "paused", "incomplete":
		ev.Status = billing.StatusOnHold
	case "canceled", "incomplete_expired":
		ev.Status = billing.StatusExpired
		ended = true
	default:
		return billing.Event{}, fmt.Errorf("stripe api: unknown subscription status %q", s.Status)
	}
	// cancel_at is a scheduled end; one at or before the period's end
	// means it does not renew, which is what cancel_at_period_end sets too.
	scheduledEnd := s.CancelAt != nil && *s.CancelAt <= periodEnd
	ev.WillRenew = !ended && !s.CancelAtPeriodEnd && !scheduledEnd
	return ev, nil
}

// Refresh fetches a subscription's current state.
func (p *Provider) Refresh(ctx context.Context, id string) (billing.Event, error) {
	s, raw, err := p.getSubscription(ctx, id)
	if err != nil {
		return billing.Event{}, err
	}
	return event(s, raw, p.now())
}

// Cancel stops a subscription renewing; it stays paid up until its period
// ends. One that has already ended, or already will, is left alone, since
// Stripe refuses to change a cancelled subscription.
func (p *Provider) Cancel(ctx context.Context, id string) error {
	ev, err := p.Refresh(ctx, id)
	if err != nil {
		return err
	}
	if !ev.WillRenew {
		return nil
	}
	var updated subscription
	return p.call(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(id), url.Values{"cancel_at_period_end": {"true"}}, "", &updated)
}

// customerHit is a Customer a search found.
type customerHit struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Created int64  `json:"created"`
}

// searchQuote quotes v as a string in Stripe's search query language.
func searchQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// customer returns the id of c's Customer, by metadata.sub, creating it if
// there is none. Search lags writes by up to a minute, so creation carries
// an idempotency key derived from the sub: a second checkout inside that
// window gets the same Customer back instead of a duplicate. The email is
// set separately, so a changed address never conflicts with that key.
func (p *Provider) customer(ctx context.Context, c billing.Customer) (string, error) {
	if c.Subject == "" {
		return "", errors.New("stripe: customer without a subject")
	}
	var found struct {
		Data []customerHit `json:"data"`
	}
	query := url.Values{"query": {"metadata['" + metadataSub + "']:" + searchQuote(c.Subject)}, "limit": {"10"}}
	if err := p.call(ctx, http.MethodGet, "/customers/search", query, "", &found); err != nil {
		return "", err
	}
	var id, email string
	if len(found.Data) > 0 {
		oldest := slices.MinFunc(found.Data, func(a, b customerHit) int { return cmp.Compare(a.Created, b.Created) })
		id, email = oldest.ID, oldest.Email
	} else {
		key := sha256.Sum256([]byte("futo-notes customer\x00" + c.Subject))
		var created struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		}
		if err := p.call(ctx, http.MethodPost, "/customers", url.Values{"metadata[" + metadataSub + "]": {c.Subject}},
			"customer-"+hex.EncodeToString(key[:16]), &created); err != nil {
			return "", err
		}
		id, email = created.ID, created.Email
	}
	if c.Email != "" && c.Email != email {
		var updated struct{}
		if err := p.call(ctx, http.MethodPost, "/customers/"+url.PathEscape(id), url.Values{"email": {c.Email}}, "", &updated); err != nil {
			return "", err
		}
	}
	return id, nil
}

// CheckoutURL is not supported: Stripe checkout is a page of ours
// (StartCheckout), never a page the browser is sent to.
func (p *Provider) CheckoutURL(context.Context, billing.Customer) (string, error) {
	return "", billing.ErrNotSupported
}

// Plan reads the plan's price.
func (p *Provider) Plan(ctx context.Context) (billing.Plan, error) {
	p.planMu.Lock()
	defer p.planMu.Unlock()
	if p.plan != nil {
		return *p.plan, nil
	}
	var price struct {
		UnitAmount *int64 `json:"unit_amount"`
		Currency   string `json:"currency"`
		Recurring  *struct {
			Interval      string `json:"interval"`
			IntervalCount int    `json:"interval_count"`
		} `json:"recurring"`
	}
	if err := p.call(ctx, http.MethodGet, "/prices/"+url.PathEscape(p.cfg.PriceID), nil, "", &price); err != nil {
		return billing.Plan{}, err
	}
	if price.UnitAmount == nil || price.Currency == "" || price.Recurring == nil || price.Recurring.IntervalCount < 1 {
		return billing.Plan{}, fmt.Errorf("stripe api: price %s is not a fixed recurring price", p.cfg.PriceID)
	}
	p.plan = &billing.Plan{
		Amount: *price.UnitAmount, Currency: price.Currency,
		Interval: price.Recurring.Interval, IntervalCount: price.Recurring.IntervalCount,
	}
	return *p.plan, nil
}

// StartCheckout opens a Checkout Session for our page's Payment Element,
// selling the plan to c. Everything the page must not offer is switched off
// on the session itself, never left to the account's dashboard settings: it
// charges the price's own currency (no Adaptive Pricing) and takes cards
// only (no Link, and wallets are hidden by the page). Stripe sends the
// browser to returnURL only after a payment that had to leave the page.
func (p *Provider) StartCheckout(ctx context.Context, c billing.Customer, returnURL string) (billing.Checkout, error) {
	customer, err := p.customer(ctx, c)
	if err != nil {
		return billing.Checkout{}, err
	}
	form := url.Values{
		"mode":                          {"subscription"},
		"ui_mode":                       {"elements"},
		"line_items[0][price]":          {p.cfg.PriceID},
		"line_items[0][quantity]":       {"1"},
		"customer":                      {customer},
		"client_reference_id":           {c.Subject},
		"metadata[" + metadataSub + "]": {c.Subject},
		"subscription_data[metadata][" + metadataSub + "]": {c.Subject},
		"return_url":                      {returnURL},
		"adaptive_pricing[enabled]":       {"false"},
		"allowed_payment_method_types[0]": {"card"},
		"wallet_options[link][display]":   {"never"},
	}
	var session struct {
		ID           string `json:"id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := p.call(ctx, http.MethodPost, "/checkout/sessions", form, "", &session); err != nil {
		return billing.Checkout{}, err
	}
	if session.ID == "" || session.ClientSecret == "" {
		return billing.Checkout{}, errors.New("stripe api: checkout session without an id or client secret")
	}
	return billing.Checkout{ID: session.ID, ClientSecret: session.ClientSecret}, nil
}

// CompleteCheckout returns the subscription a completed Checkout Session
// created, once the session is shown to be c's: its customer's metadata.sub
// and its client_reference_id, where set, must both be c's sub.
func (p *Provider) CompleteCheckout(ctx context.Context, c billing.Customer, id string) (billing.Event, error) {
	if c.Subject == "" || !strings.HasPrefix(id, "cs_") {
		return billing.Event{}, billing.ErrCheckoutForeign
	}
	var session struct {
		Status            string      `json:"status"`
		Mode              string      `json:"mode"`
		ClientReferenceID string      `json:"client_reference_id"`
		Customer          customerRef `json:"customer"`
		Subscription      *string     `json:"subscription"`
	}
	err := p.call(ctx, http.MethodGet, "/checkout/sessions/"+url.PathEscape(id), url.Values{"expand[]": {"customer"}}, "", &session)
	if errors.Is(err, errNotFound) {
		return billing.Event{}, billing.ErrCheckoutForeign
	}
	if err != nil {
		return billing.Event{}, err
	}
	owner := session.Customer.Metadata[metadataSub]
	if owner == "" {
		owner = session.ClientReferenceID
	}
	if owner != c.Subject || (session.ClientReferenceID != "" && session.ClientReferenceID != c.Subject) || session.Mode != "subscription" {
		return billing.Event{}, billing.ErrCheckoutForeign
	}
	if session.Status != "complete" || session.Subscription == nil || *session.Subscription == "" {
		return billing.Event{}, billing.ErrCheckoutIncomplete
	}
	ev, err := p.Refresh(ctx, *session.Subscription)
	if err != nil {
		return billing.Event{}, err
	}
	if ev.Subject != c.Subject {
		return billing.Event{}, billing.ErrCheckoutForeign
	}
	return ev, nil
}

// PortalURL opens a Billing Portal session for c's Customer, returning to
// the dashboard.
func (p *Provider) PortalURL(ctx context.Context, c billing.Customer) (string, error) {
	customer, err := p.customer(ctx, c)
	if err != nil {
		return "", err
	}
	var session struct {
		URL string `json:"url"`
	}
	form := url.Values{"customer": {customer}, "return_url": {p.cfg.PublicURL + "/dashboard"}}
	if err := p.call(ctx, http.MethodPost, "/billing_portal/sessions", form, "", &session); err != nil {
		return "", err
	}
	return session.URL, nil
}

// The webhook event types that concern a subscription.
var subscriptionEvents = []string{
	"customer.subscription.created",
	"customer.subscription.updated",
	"customer.subscription.deleted",
}

// Verify checks a webhook delivery's signature and fetches the
// subscription it concerns. The delivery id is the event id. Other event
// types, and subscriptions Stripe no longer has, produce no events.
func (p *Provider) Verify(ctx context.Context, d billing.Delivery) (string, []billing.Event, error) {
	if err := checkSignature(p.cfg.WebhookSecret, d.Header.Get(SignatureHeader), d.Body, p.now()); err != nil {
		return "", nil, fmt.Errorf("%w: %w", billing.ErrInvalidDelivery, err)
	}
	var notice struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(d.Body, &notice); err != nil || notice.ID == "" || notice.Created == 0 {
		return "", nil, fmt.Errorf("%w: not a Stripe event", billing.ErrInvalidDelivery)
	}
	if !slices.Contains(subscriptionEvents, notice.Type) {
		return notice.ID, nil, nil
	}
	if notice.Data.Object.Object != "subscription" || notice.Data.Object.ID == "" {
		return "", nil, fmt.Errorf("%w: %s without a subscription", billing.ErrInvalidDelivery, notice.Type)
	}
	s, raw, err := p.getSubscription(ctx, notice.Data.Object.ID)
	if errors.Is(err, errNotFound) {
		return notice.ID, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	ev, err := event(s, raw, time.Unix(notice.Created, 0).UTC())
	if err != nil {
		return "", nil, err
	}
	return notice.ID, []billing.Event{ev}, nil
}

// checkSignature verifies a Stripe-Signature header, "t=<unix>,v1=<hex>,...":
// some v1 must be the HMAC-SHA256 of "<t>.<body>" under secret, and t must
// be within SignatureTolerance of now. Several v1 values appear while a
// secret is rolled.
func checkSignature(secret []byte, header string, body []byte, now time.Time) error {
	var timestamp string
	var signatures [][]byte
	for part := range strings.SplitSeq(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			if sig, err := hex.DecodeString(value); err == nil {
				signatures = append(signatures, sig)
			}
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return errors.New("no timestamp or v1 signature")
	}
	at, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("malformed timestamp")
	}
	if skew := now.Sub(time.Unix(at, 0)); skew > SignatureTolerance || skew < -SignatureTolerance {
		return fmt.Errorf("timestamp %s outside the tolerance", timestamp)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range signatures {
		if hmac.Equal(sig, want) {
			return nil
		}
	}
	return errors.New("no signature matches")
}
