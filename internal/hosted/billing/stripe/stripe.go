// Package stripe is the Stripe web payment provider, over Stripe's REST API
// (form-encoded, at a pinned API version). Each FUTO account is a Stripe
// Customer carrying metadata.sub, its OIDC sub, and every subscription the
// checkout creates carries the same sub, which is how webhooks and refreshes
// find the account. Nothing is matched by email.
//
// Checkout depends on the buyer's billing country. In the deployment's
// self-payment countries it is our own page: Stripe's Payment Element, made
// without an intent, collects the card, and Stripe.js turns it into a
// ConfirmationToken that the page sends us. The server reads the token's
// billing address, refuses one outside the country chosen before anything
// exists, and otherwise creates the subscription and confirms its first
// payment with the token itself (docs.stripe.com/payments/
// finalize-payments-on-the-server?type=subscription). Everywhere else
// Stripe sells as merchant of record (Managed Payments), on its hosted
// Checkout page, and handles the buyer's taxes; Stripe refuses a few
// countries, which cannot pay on the web. Webhooks
// (customer.subscription.created, .updated, .deleted) only say which
// subscription changed; its state is always fetched from Stripe.
//
// The billing country decides the route, so our page's subscriptions record
// the one chosen (metadata.billing_country). As a second line behind the
// check before payment, a subscription whose first payment's charge was
// billed elsewhere is refused wherever it could entitle the account:
// cancelled at once, refunded, and never reported.
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
	// metadataBillingCountry is the metadata key holding the billing
	// country chosen on our page, on the subscriptions it creates.
	metadataBillingCountry = "billing_country"
	// metadataToken names, on a subscription our page created, the
	// ConfirmationToken of the payment that created it.
	metadataToken = "confirmation_token"
)

const (
	requestTimeout   = 30 * time.Second
	maxResponseBytes = 1 << 20
	// idempotentAttempts and idempotentWait bound how long idempotent waits
	// for a concurrent request under the same key.
	idempotentAttempts = 5
	idempotentWait     = 500 * time.Millisecond
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
	// SelfPaymentCountries are the billing countries, ISO 3166-1 alpha-2
	// in upper case, that pay through our own checkout page
	// (STRIPE_SELF_PAYMENT_COUNTRIES), with FUTO as merchant of record.
	SelfPaymentCountries []string
}

// managedPaymentsRefused are the billing countries Stripe Managed Payments
// does not sell to (docs.stripe.com/payments/managed-payments/eligibility,
// "Restricted countries").
var managedPaymentsRefused = []string{"AC", "CN", "CU", "IR", "KP", "RU", "SY", "TA", "XK"}

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
	// Never quote a successful body in an error: a customer session or a
	// payment intent carries its client secret.
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

// entitling are the subscription statuses event maps to active or grace.
var entitling = []string{"active", "trialing", "past_due"}

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

// Refresh fetches a subscription's current state. One refused for its
// billing country is refused again (in case a step failed before) and
// reported as it then is: over.
func (p *Provider) Refresh(ctx context.Context, id string) (billing.Event, error) {
	s, raw, err := p.getSubscription(ctx, id)
	if err != nil {
		return billing.Event{}, err
	}
	err = p.checkCountry(ctx, s)
	if mismatch := (*billing.CountryMismatchError)(nil); errors.As(err, &mismatch) {
		s, raw, err = p.getSubscription(ctx, id)
	}
	if err != nil {
		return billing.Event{}, err
	}
	return event(s, raw, p.now())
}

// paidInvoice is a paid invoice, as far as the billing country check reads
// it. At this API version an invoice no longer names its PaymentIntent;
// its payments list does (docs.stripe.com/changelog/basil/2025-03-31/
// add-support-for-multiple-partial-payments-on-invoices).
type paidInvoice struct {
	BillingReason string `json:"billing_reason"`
	Payments      struct {
		Data []invoicePayment `json:"data"`
	} `json:"payments"`
}

// invoicePayment is one of an invoice's payments.
type invoicePayment struct {
	Status  string `json:"status"`
	Payment struct {
		PaymentIntent string `json:"payment_intent"`
	} `json:"payment"`
}

// paidIntent is the PaymentIntent of the invoice's paid payment, "" when it
// has none.
func (in paidInvoice) paidIntent() string {
	for _, payment := range in.Payments.Data {
		if payment.Status == "paid" && payment.Payment.PaymentIntent != "" {
			return payment.Payment.PaymentIntent
		}
	}
	return ""
}

// paidInvoices lists subscription id's paid invoices, with their payments.
func (p *Provider) paidInvoices(ctx context.Context, id string) ([]paidInvoice, error) {
	var invoices struct {
		Data []paidInvoice `json:"data"`
	}
	query := url.Values{"subscription": {id}, "status": {"paid"}, "limit": {"100"}, "expand[]": {"data.payments"}}
	if err := p.call(ctx, http.MethodGet, "/invoices", query, "", &invoices); err != nil {
		return nil, err
	}
	return invoices.Data, nil
}

// billedCountry is the country of the billing address the charge that paid
// intent recorded: billing_details is the payment method's "at the time of
// the transaction" (docs.stripe.com/api/charges/object), so it never moves
// once charged. ok is false while the intent has no charge.
func (p *Provider) billedCountry(ctx context.Context, intent string) (country string, ok bool, err error) {
	var pi struct {
		LatestCharge *struct {
			BillingDetails struct {
				Address *struct {
					Country string `json:"country"`
				} `json:"address"`
			} `json:"billing_details"`
		} `json:"latest_charge"`
	}
	if err := p.call(ctx, http.MethodGet, "/payment_intents/"+url.PathEscape(intent), url.Values{"expand[]": {"latest_charge"}}, "", &pi); err != nil {
		return "", false, err
	}
	if pi.LatestCharge == nil {
		return "", false, nil
	}
	if a := pi.LatestCharge.BillingDetails.Address; a != nil {
		return a.Country, true, nil
	}
	return "", true, nil
}

// checkCountry refuses s if our page sold it for one billing country and
// the charge of its first payment (the subscription_create invoice's) was
// billed in another, or nowhere: it cancels s at once, refunds every
// payment on it, and returns the mismatch as a *billing.CountryMismatchError.
// Until that charge exists, a subscription that would entitle is an error,
// so a webhook arriving first is retried, never applied. Subscriptions
// without a chosen country (Stripe's own page) pass.
func (p *Provider) checkCountry(ctx context.Context, s subscription) error {
	chosen := s.Metadata[metadataBillingCountry]
	if chosen == "" {
		return nil
	}
	invoices, err := p.paidInvoices(ctx, s.ID)
	if err != nil {
		return err
	}
	intent := ""
	for _, invoice := range invoices {
		if invoice.BillingReason == "subscription_create" {
			intent = invoice.paidIntent()
		}
	}
	billed, charged := "", false
	if intent != "" {
		if billed, charged, err = p.billedCountry(ctx, intent); err != nil {
			return err
		}
	}
	if !charged {
		if slices.Contains(entitling, s.Status) {
			return fmt.Errorf("stripe: subscription %s is %s before its first payment was charged", s.ID, s.Status)
		}
		return nil
	}
	if billed == chosen {
		return nil
	}
	if err := p.refuse(ctx, s, invoices); err != nil {
		return err
	}
	return &billing.CountryMismatchError{Chosen: chosen, Billed: billed, Refunded: true}
}

// over reports whether a subscription status is final.
func over(status string) bool {
	return status == "canceled" || status == "incomplete_expired"
}

// cancelNow cancels s at once, unless it is over. Stripe ignores
// idempotency keys on DELETE and refuses to cancel a cancelled
// subscription, so a refusal from a cancellation that raced this one is
// read back as done.
func (p *Provider) cancelNow(ctx context.Context, s subscription) error {
	if over(s.Status) {
		return nil
	}
	var canceled struct{}
	err := p.call(ctx, http.MethodDelete, "/subscriptions/"+url.PathEscape(s.ID), nil, "", &canceled)
	if e := (*apiError)(nil); errors.As(err, &e) && e.StatusCode == http.StatusBadRequest {
		if current, _, getErr := p.getSubscription(ctx, s.ID); getErr == nil && over(current.Status) {
			return nil
		}
	}
	return err
}

// refuse cancels s at once and refunds every payment made on its paid
// invoices. Each step is safe to repeat: a webhook retry and the page may
// refuse the same subscription at the same moment.
func (p *Provider) refuse(ctx context.Context, s subscription, invoices []paidInvoice) error {
	if err := p.cancelNow(ctx, s); err != nil {
		return err
	}
	for _, invoice := range invoices {
		for _, payment := range invoice.Payments.Data {
			intent := payment.Payment.PaymentIntent
			if payment.Status != "paid" || intent == "" {
				continue
			}
			var refund struct{}
			form := url.Values{"payment_intent": {intent}, "metadata[reason]": {"billing_country_mismatch"}}
			err := p.idempotent(ctx, http.MethodPost, "/refunds", form, "refund-"+intent, &refund)
			if e := (*apiError)(nil); errors.As(err, &e) && e.Code == "charge_already_refunded" {
				continue
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// idempotent sends a request under idempotencyKey, waiting out a
// concurrent one under the same key, which Stripe answers 409 until it
// finishes; the retry then gets its result.
func (p *Provider) idempotent(ctx context.Context, method, path string, form url.Values, idempotencyKey string, out any) error {
	for attempt := 1; ; attempt++ {
		err := p.call(ctx, method, path, form, idempotencyKey, out)
		if e := (*apiError)(nil); !errors.As(err, &e) || e.StatusCode != http.StatusConflict || attempt == idempotentAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(idempotentWait):
		}
	}
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

// CheckoutURL is not supported: Stripe checkout is a page of ours (Pay) or
// a Checkout Session the country step opens (StartMerchantCheckout).
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

// Route sends the self-payment countries to our page and every other
// country Stripe Managed Payments sells to to Stripe's.
func (p *Provider) Route(country string) billing.CheckoutRoute {
	switch {
	case slices.Contains(p.cfg.SelfPaymentCountries, country):
		return billing.RouteSelf
	case slices.Contains(managedPaymentsRefused, country):
		return billing.RouteUnavailable
	}
	return billing.RouteMerchantOfRecord
}

// StartPayment opens a CustomerSession for our page's Payment Element and
// c's Customer, and returns its client secret. It offers to save the card
// (payment_method_save; a card saved that way has allow_redisplay=always)
// and shows the cards saved that way (payment_method_redisplay, whose
// filter defaults to always), but never removes one, which would take it
// off a subscription (docs.stripe.com/payments/save-customer-payment-methods).
func (p *Provider) StartPayment(ctx context.Context, c billing.Customer) (string, error) {
	customer, err := p.customer(ctx, c)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"customer":                             {customer},
		"components[payment_element][enabled]": {"true"},
		"components[payment_element][features][payment_method_save]":       {"enabled"},
		"components[payment_element][features][payment_method_save_usage]": {"off_session"},
		"components[payment_element][features][payment_method_redisplay]":  {"enabled"},
	}
	var session struct {
		ClientSecret string `json:"client_secret"`
	}
	if err := p.call(ctx, http.MethodPost, "/customer_sessions", form, "", &session); err != nil {
		return "", err
	}
	if session.ClientSecret == "" {
		return "", errors.New("stripe api: customer session without a client secret")
	}
	return session.ClientSecret, nil
}

// paymentIntent is what settling a payment reads of a PaymentIntent.
type paymentIntent struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	ClientSecret     string `json:"client_secret"`
	LastPaymentError *struct {
		Message string `json:"message"`
	} `json:"last_payment_error"`
}

// subscriptionPayment is a subscription with its latest invoice's payments,
// whose default payment's PaymentIntent pays its first invoice.
type subscriptionPayment struct {
	subscription
	LatestInvoice *paidInvoice `json:"latest_invoice"`
}

// intent is the PaymentIntent paying the latest invoice, "" for none.
func (s subscriptionPayment) intent() string {
	if s.LatestInvoice == nil || len(s.LatestInvoice.Payments.Data) == 0 {
		return ""
	}
	return s.LatestInvoice.Payments.Data[0].Payment.PaymentIntent
}

// pendingFor is how long a payment on our page waiting on its buyer
// (3-D Secure) holds off another for the same customer. One older than that
// was abandoned, and is cancelled so a new one can go ahead.
const pendingFor = 15 * time.Minute

// noOtherPayment refuses a payment with token for customer while another
// could still give it a second subscription: one that entitles already,
// which the account may not show yet, or one of our page's waiting on its
// buyer in another window. Waiting ones older than pendingFor are
// cancelled. The subscription token itself made is no other payment: Pay
// replays it.
func (p *Provider) noOtherPayment(ctx context.Context, customer, token string) error {
	var list struct {
		Data []struct {
			subscription
			Created int64 `json:"created"`
		} `json:"data"`
	}
	query := url.Values{"customer": {customer}, "status": {"all"}, "limit": {"100"}}
	if err := p.call(ctx, http.MethodGet, "/subscriptions", query, "", &list); err != nil {
		return err
	}
	for _, s := range list.Data {
		switch {
		case s.Metadata[metadataToken] == token:
		case slices.Contains(entitling, s.Status):
			return billing.ErrSubscribed
		case s.Status == "incomplete" && s.Metadata[metadataBillingCountry] != "":
			if time.Since(time.Unix(s.Created, 0)) < pendingFor {
				return billing.ErrPaymentPending
			}
			if err := p.cancelNow(ctx, s.subscription); err != nil {
				return err
			}
		}
	}
	return nil
}

// declined is the message for a payment that did not go through.
const declined = "The payment did not go through. Try another card."

// Pay pays for the plan for c with the ConfirmationToken the page made.
// The token's billing address (payment_method_preview.billing_details,
// what the form collected, or a saved card's) must be in the country c
// chose; otherwise nothing is created. Then the subscription is created
// incomplete (payment_behavior=default_incomplete), recording the sub and
// the country, and the PaymentIntent of its first invoice's payment is
// confirmed with the token. Both requests are idempotent under the token,
// which belongs to one attempt.
func (p *Provider) Pay(ctx context.Context, c billing.Customer, pay billing.Payment) (billing.PaymentResult, error) {
	if pay.BillingCountry == "" || !strings.HasPrefix(pay.ConfirmationToken, "ctoken_") {
		return billing.PaymentResult{}, billing.ErrCheckoutForeign
	}
	customer, err := p.customer(ctx, c)
	if err != nil {
		return billing.PaymentResult{}, err
	}
	if err := p.noOtherPayment(ctx, customer, pay.ConfirmationToken); err != nil {
		return billing.PaymentResult{}, err
	}
	var token struct {
		PaymentMethodPreview *struct {
			BillingDetails struct {
				Address *struct {
					Country string `json:"country"`
				} `json:"address"`
			} `json:"billing_details"`
		} `json:"payment_method_preview"`
	}
	err = p.call(ctx, http.MethodGet, "/confirmation_tokens/"+url.PathEscape(pay.ConfirmationToken), nil, "", &token)
	if errors.Is(err, errNotFound) {
		return billing.PaymentResult{}, billing.ErrCheckoutForeign
	}
	if err != nil {
		return billing.PaymentResult{}, err
	}
	billed := ""
	if preview := token.PaymentMethodPreview; preview != nil && preview.BillingDetails.Address != nil {
		billed = preview.BillingDetails.Address.Country
	}
	if billed != pay.BillingCountry {
		return billing.PaymentResult{}, &billing.CountryMismatchError{Chosen: pay.BillingCountry, Billed: billed}
	}

	form := url.Values{
		"customer":           {customer},
		"items[0][price]":    {p.cfg.PriceID},
		"items[0][quantity]": {"1"},
		"payment_behavior":   {"default_incomplete"},
		"payment_settings[save_default_payment_method]": {"on_subscription"},
		"payment_settings[payment_method_types][0]":     {"card"},
		"metadata[" + metadataSub + "]":                 {c.Subject},
		"metadata[" + metadataBillingCountry + "]":      {pay.BillingCountry},
		"metadata[" + metadataToken + "]":               {pay.ConfirmationToken},
		"expand[]":                                      {"latest_invoice.payments"},
	}
	var s subscriptionPayment
	if err := p.idempotent(ctx, http.MethodPost, "/subscriptions", form, "pay-"+pay.ConfirmationToken, &s); err != nil {
		return billing.PaymentResult{}, err
	}
	intent := s.intent()
	if intent == "" {
		return billing.PaymentResult{}, fmt.Errorf("stripe api: subscription %s without a payment to confirm", s.ID)
	}
	var pi paymentIntent
	err = p.idempotent(ctx, http.MethodPost, "/payment_intents/"+url.PathEscape(intent)+"/confirm",
		url.Values{"confirmation_token": {pay.ConfirmationToken}}, "confirm-"+pay.ConfirmationToken, &pi)
	if e := (*apiError)(nil); errors.As(err, &e) && e.StatusCode >= 400 && e.StatusCode < 500 {
		// Refused before any charge: nothing to keep.
		if cancelErr := p.cancelNow(ctx, s.subscription); cancelErr != nil {
			return billing.PaymentResult{}, cancelErr
		}
		if e.Type == "card_error" && e.Message != "" {
			return billing.PaymentResult{}, &billing.PaymentDeclinedError{Message: e.Message}
		}
		return billing.PaymentResult{}, err
	}
	if err != nil {
		return billing.PaymentResult{}, err
	}
	return p.settle(ctx, c, s.subscription, pi)
}

// FinishPayment settles the payment of subscription id, c's, once our page
// has handled its next action.
func (p *Provider) FinishPayment(ctx context.Context, c billing.Customer, id string) (billing.PaymentResult, error) {
	if c.Subject == "" || !strings.HasPrefix(id, "sub_") {
		return billing.PaymentResult{}, billing.ErrCheckoutForeign
	}
	var s subscriptionPayment
	err := p.call(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(id),
		url.Values{"expand[]": {"customer", "latest_invoice.payments"}}, "", &s)
	if errors.Is(err, errNotFound) {
		return billing.PaymentResult{}, billing.ErrCheckoutForeign
	}
	if err != nil {
		return billing.PaymentResult{}, err
	}
	if owner := cmp.Or(s.Metadata[metadataSub], s.Customer.Metadata[metadataSub]); owner != c.Subject || s.Metadata[metadataBillingCountry] == "" {
		return billing.PaymentResult{}, billing.ErrCheckoutForeign
	}
	intent := s.intent()
	if intent == "" {
		return billing.PaymentResult{}, fmt.Errorf("stripe api: subscription %s without a payment", s.ID)
	}
	var pi paymentIntent
	if err := p.call(ctx, http.MethodGet, "/payment_intents/"+url.PathEscape(intent), nil, "", &pi); err != nil {
		return billing.PaymentResult{}, err
	}
	return p.settle(ctx, c, s.subscription, pi)
}

// settle reports s's payment pi: its next action, a decline (cancelling s),
// or, once it went through, s as it now is, checked for its billing country
// against the charge.
func (p *Provider) settle(ctx context.Context, c billing.Customer, s subscription, pi paymentIntent) (billing.PaymentResult, error) {
	result := billing.PaymentResult{SubscriptionID: s.ID}
	switch pi.Status {
	case "requires_action":
		if pi.ClientSecret == "" {
			return billing.PaymentResult{}, fmt.Errorf("stripe api: payment intent %s needs an action but has no client secret", pi.ID)
		}
		result.ActionSecret = pi.ClientSecret
		return result, nil
	case "succeeded", "processing":
	default:
		if err := p.cancelNow(ctx, s); err != nil {
			return billing.PaymentResult{}, err
		}
		message := declined
		if pi.LastPaymentError != nil && pi.LastPaymentError.Message != "" {
			message = pi.LastPaymentError.Message
		}
		return billing.PaymentResult{}, &billing.PaymentDeclinedError{Message: message}
	}
	current, raw, err := p.getSubscription(ctx, s.ID)
	if err != nil {
		return billing.PaymentResult{}, err
	}
	if err := p.checkCountry(ctx, current); err != nil {
		return billing.PaymentResult{}, err
	}
	if result.Event, err = event(current, raw, p.now()); err != nil {
		return billing.PaymentResult{}, err
	}
	if result.Event.Subject != c.Subject {
		return billing.PaymentResult{}, billing.ErrCheckoutForeign
	}
	return result, nil
}

// StartMerchantCheckout opens a Managed Payments Checkout Session on
// Stripe's hosted page, selling the plan to c with Stripe as merchant of
// record, and returns its URL. Stripe owns the rest of the session and
// refuses the parameters our own checkout sets: Adaptive Pricing is always
// on, the payment methods are Stripe's choice, and Stripe adds the tax due
// on top of the price. The customer and the sub are ours, as on our page.
func (p *Provider) StartMerchantCheckout(ctx context.Context, c billing.Customer, successURL, cancelURL string) (string, error) {
	customer, err := p.customer(ctx, c)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"mode":                          {"subscription"},
		"ui_mode":                       {"hosted_page"},
		"managed_payments[enabled]":     {"true"},
		"line_items[0][price]":          {p.cfg.PriceID},
		"line_items[0][quantity]":       {"1"},
		"customer":                      {customer},
		"client_reference_id":           {c.Subject},
		"metadata[" + metadataSub + "]": {c.Subject},
		"subscription_data[metadata][" + metadataSub + "]": {c.Subject},
		"success_url": {successURL},
		"cancel_url":  {cancelURL},
	}
	var session struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := p.call(ctx, http.MethodPost, "/checkout/sessions", form, "", &session); err != nil {
		return "", err
	}
	if session.ID == "" || !strings.HasPrefix(session.URL, "https://") {
		return "", errors.New("stripe api: hosted checkout session without an id or https url")
	}
	return session.URL, nil
}

// CompleteCheckout returns the subscription a completed Checkout Session
// on Stripe's page created, once the session is shown to be c's: its
// customer's metadata.sub and its client_reference_id, where set, must both
// be c's sub.
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
// types, subscriptions Stripe no longer has, and subscriptions refused for
// their billing country (checkCountry) produce no events.
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
	err = p.checkCountry(ctx, s)
	if mismatch := (*billing.CountryMismatchError)(nil); errors.As(err, &mismatch) {
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
