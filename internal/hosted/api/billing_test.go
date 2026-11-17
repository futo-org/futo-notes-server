package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/mockweb"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/ids"
)

// fakeStore is an app store provider whose purchases the test sells: a
// submitted token is valid once sold, and its deliveries are "signed" with
// a fixed header.
type fakeStore struct {
	source billing.Source
	mu     sync.Mutex
	// purchases are the sold subscriptions by purchase token.
	purchases map[string]billing.Event
	cancelled []string
}

func newFakeStore(source billing.Source) *fakeStore {
	return &fakeStore{source: source, purchases: map[string]billing.Event{}}
}

func (f *fakeStore) Source() billing.Source { return f.source }

// sell records a purchase made with accountToken and returns its purchase
// token.
func (f *fakeStore) sell(accountToken string) (string, billing.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	token := "tok_" + uuid.NewV7().String()
	ev := billing.Event{
		Source: f.source, ExternalID: "orig_" + uuid.NewV7().String(), AccountToken: accountToken, ProductID: "notes.monthly",
		Status: billing.StatusActive, WillRenew: true, ExpiresAt: new(now.Add(30 * 24 * time.Hour)),
		Environment: billing.EnvironmentSandbox, EventAt: now, Raw: []byte(`{"fake":true}`),
	}
	f.purchases[token] = ev
	return token, ev
}

// change updates a sold subscription as the store would report it later.
func (f *fakeStore) change(token string, update func(*billing.Event)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := f.purchases[token]
	update(&ev)
	ev.EventAt = time.Now()
	f.purchases[token] = ev
}

func (f *fakeStore) Submit(_ context.Context, p billing.Purchase) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.purchases[p.Token]
	if !ok {
		return billing.Event{}, billing.ErrInvalidPurchase
	}
	return ev, nil
}

const fakeSignatureHeader = "X-Fake-Signature"

func (f *fakeStore) delivery(token string) billing.Delivery {
	body, _ := json.Marshal(map[string]string{"delivery": "dlv_" + uuid.NewV7().String(), "token": token})
	return billing.Delivery{Header: http.Header{fakeSignatureHeader: {"valid"}}, Body: body}
}

func (f *fakeStore) Verify(_ context.Context, d billing.Delivery) (string, []billing.Event, error) {
	var body struct {
		Delivery string `json:"delivery"`
		Token    string `json:"token"`
	}
	if d.Header.Get(fakeSignatureHeader) != "valid" || json.Unmarshal(d.Body, &body) != nil {
		return "", nil, billing.ErrInvalidDelivery
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.purchases[body.Token]
	if !ok {
		return body.Delivery, nil, nil
	}
	return body.Delivery, []billing.Event{ev}, nil
}

func (f *fakeStore) Refresh(_ context.Context, externalID string) (billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.purchases {
		if ev.ExternalID == externalID {
			return ev, nil
		}
	}
	return billing.Event{}, errors.New("unknown subscription")
}

func (f *fakeStore) Cancel(_ context.Context, externalID string) error {
	if f.source == billing.SourceAppStore {
		return billing.ErrNotSupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, externalID)
	return nil
}

func (f *fakeStore) cancelledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

// billingHarness is a harness with billing on: the mock web provider and
// both stores faked.
type billingHarness struct {
	*harness
	mock     *mockweb.Provider
	appStore *fakeStore
	play     *fakeStore
}

const mockSecret = "test-mock-webhook-secret"

func newBillingHarness(t *testing.T) *billingHarness {
	t.Helper()
	b := &billingHarness{appStore: newFakeStore(billing.SourceAppStore), play: newFakeStore(billing.SourcePlay)}
	b.harness = newHarnessWithBilling(t, func(cfg config.Config) billing.Providers {
		b.mock = mockweb.New(cfg.PublicURL, []byte(mockSecret))
		return billing.Providers{Web: b.mock, Stores: map[billing.Source]billing.StoreProvider{
			billing.SourceAppStore: b.appStore, billing.SourcePlay: b.play,
		}}
	}, func(c *config.Config) { c.DevUI = true })
	return b
}

// newStoresOnlyHarness is a billing harness without a web provider.
func newStoresOnlyHarness(t *testing.T) *billingHarness {
	t.Helper()
	b := &billingHarness{appStore: newFakeStore(billing.SourceAppStore), play: newFakeStore(billing.SourcePlay)}
	b.harness = newHarnessWithBilling(t, func(config.Config) billing.Providers {
		return billing.Providers{Stores: map[billing.Source]billing.StoreProvider{
			billing.SourceAppStore: b.appStore, billing.SourcePlay: b.play,
		}}
	}, func(*config.Config) {})
	return b
}

type billingBody struct {
	Entitled          bool       `json:"entitled"`
	Status            *string    `json:"status"`
	Source            *string    `json:"source"`
	ProductID         *string    `json:"product_id"`
	ExpiresAt         *time.Time `json:"expires_at"`
	GraceUntil        *time.Time `json:"grace_until"`
	WillRenew         bool       `json:"will_renew"`
	StoreAccountToken string     `json:"store_account_token"`
	Rails             struct {
		Web      bool `json:"web"`
		AppStore bool `json:"app_store"`
		Play     bool `json:"play"`
	} `json:"rails"`
}

func (c *client) billing() billingBody {
	c.h.t.Helper()
	var b billingBody
	c.expect(http.MethodGet, "/api/billing", nil, http.StatusOK).json(c.h.t, &b)
	return b
}

func str(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// subscribe buys a web subscription through the mock checkout, the way the
// apps' test harness does: ask for the checkout, then complete it.
func (c *client) subscribe() {
	c.h.t.Helper()
	var checkout struct {
		URL string `json:"url"`
	}
	c.expect(http.MethodPost, "/api/billing/checkout", nil, http.StatusOK).json(c.h.t, &checkout)
	page, err := url.Parse(checkout.URL)
	if err != nil || !strings.HasPrefix(checkout.URL, c.h.api.URL+mockweb.CheckoutPath+"?") {
		c.h.t.Fatalf("checkout URL %q (%v)", checkout.URL, err)
	}
	expectContains(c.h.t, c.expect(http.MethodGet, checkout.URL, nil, http.StatusOK), "Complete checkout")
	done := c.postForm(page.Path, page.Query())
	if done.status != http.StatusOK || !strings.Contains(done.body, "Subscribed") {
		c.h.t.Fatalf("completing the mock checkout = %d %s", done.status, done.body)
	}
}

// webhook posts a delivery to the source's webhook route as the provider
// would.
func (h *harness) webhook(source string, d billing.Delivery) response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.api.URL+"/api/billing/webhooks/"+source, bytes.NewReader(d.Body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header = d.Header.Clone()
	return h.client().send(req)
}

func expectWebhookAck(t *testing.T, res response, duplicate bool) {
	t.Helper()
	if want := fmt.Sprintf(`{"duplicate":%t}`+"\n", duplicate); res.status != http.StatusOK || res.body != want {
		t.Fatalf("webhook = %d %s, want 200 %s", res.status, res.body, want)
	}
}

// subscriptionRequired is the 402 body of a gated write.
const subscriptionRequired = `{"code":"subscription_required","error":"subscription required"}` + "\n"

func TestBillingOff(t *testing.T) {
	h := newHarness(t)
	app, _ := h.appLogin(h.newSub())
	b := app.billing()
	if b.Entitled || b.Status != nil || b.Source != nil || b.Rails.Web || b.Rails.AppStore || b.Rails.Play || !isUUID(b.StoreAccountToken) {
		t.Fatalf("billing with no provider = %+v", b)
	}
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/checkout", nil), http.StatusServiceUnavailable, "billing_unavailable")
	expectHostedError(t, app.do(http.MethodGet, "/api/billing/portal", nil), http.StatusServiceUnavailable, "billing_unavailable")
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": "jws"}),
		http.StatusServiceUnavailable, "billing_unavailable")
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/play/purchases", map[string]string{"product_id": "p", "purchase_token": "t"}),
		http.StatusServiceUnavailable, "billing_unavailable")
	expectHostedError(t, h.webhook("web", billing.Delivery{Body: []byte(`{}`)}), http.StatusNotFound, "not_found")

	// Nothing is gated without a way to pay.
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)
	key := map[string]any{"key_salt": "salt", "key_kdf": map[string]any{"name": "argon2id"}, "encrypted_vault_key": "vault"}
	app.expect(http.MethodPut, "/api/collections/"+claimed.Collection.ID+"/key", key, http.StatusOK)
}

func isUUID(s string) bool { return ids.IsUUID(s) }

func TestBillingCapabilityAndState(t *testing.T) {
	h := newBillingHarness(t)
	res := h.client().expect(http.MethodGet, "/", nil, http.StatusOK)
	expectContains(t, res, `"billing":true`)

	app, _ := h.appLogin(h.newSub())
	b := app.billing()
	if b.Entitled || b.Status != nil || b.Source != nil || b.ProductID != nil || b.ExpiresAt != nil || b.GraceUntil != nil || b.WillRenew ||
		!b.Rails.Web || !b.Rails.AppStore || !b.Rails.Play || !isUUID(b.StoreAccountToken) {
		t.Fatalf("billing of a new account = %+v", b)
	}
	if again := app.billing(); again.StoreAccountToken != b.StoreAccountToken {
		t.Fatalf("store account token changed from %s to %s", b.StoreAccountToken, again.StoreAccountToken)
	}
	other, _ := h.appLogin(h.newSub())
	if other.billing().StoreAccountToken == b.StoreAccountToken {
		t.Fatal("two accounts share a store account token")
	}
}

func TestWebCheckoutAndPortal(t *testing.T) {
	h := newBillingHarness(t)
	subject := h.newSub()
	app, _ := h.appLogin(subject)
	app.subscribe()
	b := app.billing()
	if !b.Entitled || str(b.Status) != "active" || str(b.Source) != "web" || str(b.ProductID) != mockweb.ProductID || !b.WillRenew ||
		b.ExpiresAt == nil || time.Until(*b.ExpiresAt) < 29*24*time.Hour {
		t.Fatalf("billing after checkout = %+v", b)
	}

	// Paying twice is refused, from any source.
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/checkout", nil), http.StatusConflict, "already_subscribed")

	var portal struct {
		URL string `json:"url"`
	}
	app.expect(http.MethodGet, "/api/billing/portal", nil, http.StatusOK).json(t, &portal)
	page, err := url.Parse(portal.URL)
	if err != nil || page.Path != mockweb.PortalPath {
		t.Fatalf("portal URL %q (%v)", portal.URL, err)
	}
	subs := h.mock.Subscriptions(subject)
	if len(subs) != 1 {
		t.Fatalf("mock subscriptions = %+v", subs)
	}
	expectContains(t, app.expect(http.MethodGet, portal.URL, nil, http.StatusOK), subs[0].ID, "Cancel at period end")

	// Cancelling in the portal reaches the API through the webhook.
	form := page.Query()
	form.Set("subscription", subs[0].ID)
	form.Set("action", "cancel")
	if res := app.postForm(page.Path, form); res.status != http.StatusSeeOther {
		t.Fatalf("portal cancel = %d %s", res.status, res.body)
	}
	if b := app.billing(); !b.Entitled || b.WillRenew {
		t.Fatalf("billing after cancelling = %+v, want entitled until the period ends, not renewing", b)
	}
	form.Set("action", "resume")
	app.postForm(page.Path, form)
	if b := app.billing(); !b.WillRenew {
		t.Fatalf("billing after resuming = %+v", b)
	}

	// A mock link only works for the customer it was signed for.
	forged := page.Query()
	forged.Set("customer", uuid.NewV7().String())
	if res := app.do(http.MethodGet, h.api.URL+mockweb.CheckoutPath+"?"+forged.Encode(), nil); res.status != http.StatusForbidden {
		t.Fatalf("forged checkout link = %d %s", res.status, res.body)
	}
	form.Set("subscription", "mock_sub_unknown")
	if res := app.postForm(page.Path, form); res.status != http.StatusNotFound {
		t.Fatalf("portal action on another subscription = %d %s", res.status, res.body)
	}
}

func TestPortalManagedByStoreAndUnavailable(t *testing.T) {
	h := newBillingHarness(t)
	app, _ := h.appLogin(h.newSub())
	token, _ := h.appStore.sell(app.billing().StoreAccountToken)
	app.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": token}, http.StatusOK)
	expectHostedError(t, app.do(http.MethodGet, "/api/billing/portal", nil), http.StatusConflict, "managed_by_store")
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/checkout", nil), http.StatusConflict, "already_subscribed")

	stores := newStoresOnlyHarness(t)
	app, _ = stores.appLogin(stores.newSub())
	expectHostedError(t, app.do(http.MethodPost, "/api/billing/checkout", nil), http.StatusServiceUnavailable, "billing_unavailable")
	expectHostedError(t, app.do(http.MethodGet, "/api/billing/portal", nil), http.StatusServiceUnavailable, "billing_unavailable")
	if b := app.billing(); b.Rails.Web || !b.Rails.AppStore || !b.Rails.Play {
		t.Fatalf("rails = %+v", b.Rails)
	}
}

func TestStorePurchases(t *testing.T) {
	h := newBillingHarness(t)
	owner, _ := h.appLogin(h.newSub())
	ownerToken := owner.billing().StoreAccountToken

	// Malformed and unvouched submissions.
	expectHostedError(t, owner.do(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{}), http.StatusBadRequest, "invalid_request")
	expectHostedError(t, owner.do(http.MethodPost, "/api/billing/play/purchases", map[string]string{"purchase_token": "t"}), http.StatusBadRequest, "invalid_request")
	expectHostedError(t, owner.do(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": "forged"}),
		http.StatusBadRequest, "invalid_purchase")
	expectHostedError(t, owner.do(http.MethodPost, "/api/billing/play/purchases", map[string]string{"product_id": "p", "purchase_token": "forged"}),
		http.StatusBadRequest, "invalid_purchase")

	// The buyer submits and is entitled at once.
	appleToken, _ := h.appStore.sell(ownerToken)
	var b billingBody
	owner.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": appleToken}, http.StatusOK).json(t, &b)
	if !b.Entitled || str(b.Source) != "app_store" || b.StoreAccountToken != ownerToken {
		t.Fatalf("after an App Store purchase = %+v", b)
	}
	playToken, _ := h.play.sell(ownerToken)
	owner.expect(http.MethodPost, "/api/billing/play/purchases", map[string]string{"product_id": "notes.monthly", "purchase_token": playToken}, http.StatusOK)

	// Another FUTO account on the same store account cannot take them.
	sub := h.newSub()
	other, _ := h.appLogin(sub)
	expectHostedError(t, other.do(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": appleToken}),
		http.StatusConflict, "store_subscription_owned_elsewhere")
	expectHostedError(t, other.do(http.MethodPost, "/api/billing/play/purchases", map[string]string{"product_id": "notes.monthly", "purchase_token": playToken}),
		http.StatusConflict, "store_subscription_owned_elsewhere")
	if other.billing().Entitled || !owner.billing().Entitled {
		t.Fatal("a refused restore moved the subscription")
	}

	// Once the owner is purged, the next account to submit it gets it.
	h.exec(`DELETE FROM users WHERE store_account_token = $1`, ownerToken)
	other.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": appleToken}, http.StatusOK).json(t, &b)
	if !b.Entitled || str(b.Source) != "app_store" {
		t.Fatalf("after restoring a released subscription = %+v", b)
	}
}

func TestBillingWebhooks(t *testing.T) {
	h := newBillingHarness(t)
	subject := h.newSub()
	app, _ := h.appLogin(subject)

	sub := h.mock.Checkout(subject)
	delivery, err := h.mock.Delivery(sub)
	if err != nil {
		t.Fatal(err)
	}
	expectWebhookAck(t, h.webhook("web", delivery), false)
	if !app.billing().Entitled {
		t.Fatal("the checkout delivery did not entitle the account")
	}
	// A retried delivery is acknowledged and not applied again.
	expectWebhookAck(t, h.webhook("web", delivery), true)

	// An older notification, delivered late, does not roll the account back.
	if err := h.mock.Cancel(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := h.mock.Subscription(sub.ID)
	newer, _ := h.mock.Delivery(cancelled)
	expectWebhookAck(t, h.webhook("web", newer), false)
	older, _ := h.mock.Delivery(sub)
	expectWebhookAck(t, h.webhook("web", older), false)
	if b := app.billing(); b.WillRenew {
		t.Fatalf("an older delivery rolled the subscription back: %+v", b)
	}

	// Bad signatures, unknown accounts, unconfigured sources, huge bodies.
	forged := delivery
	forged.Header = delivery.Header.Clone()
	forged.Header.Set(mockweb.SignatureHeader, "00")
	expectHostedError(t, h.webhook("web", forged), http.StatusBadRequest, "invalid_delivery")
	nobody, _ := h.mock.Delivery(h.mock.Checkout(uuid.NewV7().String()))
	expectWebhookAck(t, h.webhook("web", nobody), false)
	expectHostedError(t, h.webhook("stripe", delivery), http.StatusNotFound, "not_found")
	huge := billing.Delivery{Header: http.Header{}, Body: bytes.Repeat([]byte("x"), 1<<20+1)}
	expectHostedError(t, h.webhook("web", huge), http.StatusRequestEntityTooLarge, "payload_too_large")

	// A store's notification moves its subscription.
	storeToken, _ := h.play.sell(app.billing().StoreAccountToken)
	expectWebhookAck(t, h.webhook("play", h.play.delivery(storeToken)), false)
	h.play.change(storeToken, func(ev *billing.Event) { ev.Status = billing.StatusOnHold })
	expectWebhookAck(t, h.webhook("play", h.play.delivery(storeToken)), false)
	bad := h.play.delivery(storeToken)
	bad.Header = http.Header{}
	expectHostedError(t, h.webhook("play", bad), http.StatusBadRequest, "invalid_delivery")
	expectHostedError(t, h.webhook("app_store", billing.Delivery{Header: http.Header{}, Body: []byte(`{`)}), http.StatusBadRequest, "invalid_delivery")
}

// TestSpecSubscriptionRequiredOnEveryGatedOperation sends every operation
// that documents the 402 as an account without a subscription: each must
// answer it. Reads and the other operations keep working.
func TestSpecSubscriptionRequiredOnEveryGatedOperation(t *testing.T) {
	h := newBillingHarness(t)
	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)

	gated := 0
	for _, o := range specOperations(t) {
		required := o.op.Responses.Status(http.StatusPaymentRequired)
		if required == nil {
			continue
		}
		if required.Ref != "#/components/responses/SubscriptionRequired" {
			t.Errorf("%s %s documents a 402 that is not SubscriptionRequired", o.method, o.path)
		}
		gated++
		receipt, _ := h.receipt("us-east", tokens.User.ID, 1)
		if res := app.do(o.method, o.path, map[string]any{"receipt": receipt, "version": 2}); res.status != http.StatusPaymentRequired || res.body != subscriptionRequired {
			t.Errorf("%s %s without a subscription = %d %s, want the 402", o.method, o.path, res.status, res.body)
		}
	}
	if gated != 6 {
		t.Fatalf("%d operations document the 402, want the 6 gated writes", gated)
	}

	// Reads are never gated.
	collection := "/api/collections/" + claimed.Collection.ID
	for _, path := range []string{"/api/collections", collection, collection + "/key", collection + "/objects", "/api/auth", "/api/account", "/api/sessions", "/api/billing"} {
		app.expect(http.MethodGet, path, nil, http.StatusOK)
	}

	// Subscribed, the same writes go through.
	app.subscribe()
	receipt, _ := h.receipt("us-east", tokens.User.ID, 3)
	res := app.do(http.MethodPost, collection+"/objects", map[string]string{"receipt": receipt}, "Mutation-Id", "billing-create")
	if res.status != http.StatusCreated {
		t.Fatalf("create once subscribed = %d %s", res.status, res.body)
	}
	key := map[string]any{"key_salt": "salt", "key_kdf": map[string]any{"name": "argon2id"}, "encrypted_vault_key": "vault"}
	app.expect(http.MethodPut, collection+"/key", key, http.StatusOK)
}

// TestGatingPrecedence checks the order of the refusals: no session is 401,
// then a pending deletion is 403, and only then is a missing subscription
// 402.
func TestGatingPrecedence(t *testing.T) {
	h := newBillingHarness(t)
	path := "/api/collections/" + absentID + "/key"
	key := map[string]any{"key_salt": "salt", "key_kdf": map[string]any{"name": "argon2id"}, "encrypted_vault_key": "vault"}
	if res := h.client().do(http.MethodPut, path, key); res.status != http.StatusUnauthorized {
		t.Fatalf("no session = %d %s, want 401", res.status, res.body)
	}

	app, _ := h.appLogin(h.newSub())
	if res := app.do(http.MethodPut, path, key); res.status != http.StatusPaymentRequired {
		t.Fatalf("unsubscribed = %d %s, want 402", res.status, res.body)
	}
	app.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	if res := app.do(http.MethodPut, path, key); res.status != http.StatusForbidden || !strings.Contains(res.body, `"account_pending_deletion"`) {
		t.Fatalf("unsubscribed and pending deletion = %d %s, want the 403", res.status, res.body)
	}

	// Subscribed, the gate lets the write through to the handler.
	subscribed, _ := h.appLogin(h.newSub())
	subscribed.subscribe()
	if res := subscribed.do(http.MethodPut, path, key); res.status != http.StatusNotFound {
		t.Fatalf("subscribed write to an absent collection = %d %s, want 404", res.status, res.body)
	}
}

func TestStorageTokenScopeFollowsTheSubscription(t *testing.T) {
	h := newBillingHarness(t)
	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	verifier, err := hostedauth.NewVerifier(h.apiKeys, "us-east")
	if err != nil {
		t.Fatal(err)
	}
	refresh := tokens.StorageRefreshToken
	exchange := func() hostedauth.AccessToken {
		t.Helper()
		var issued struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		h.client().expect(http.MethodPost, "/api/storage/token", map[string]string{"refresh_token": refresh}, http.StatusOK).json(t, &issued)
		refresh = issued.RefreshToken
		access, err := verifier.VerifyAccess(issued.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		return access
	}

	if access := exchange(); access.Write || access.UserID != tokens.User.ID {
		t.Fatalf("access token without a subscription = %+v, want read only", access)
	}
	app.subscribe()
	if access := exchange(); !access.Write {
		t.Fatalf("access token with a subscription = %+v, want write", access)
	}
}

func TestDeleteAccountCancelsRenewals(t *testing.T) {
	h := newBillingHarness(t)
	subject := h.newSub()
	app, _ := h.appLogin(subject)
	app.subscribe()
	accountToken := app.billing().StoreAccountToken
	playToken, playSub := h.play.sell(accountToken)
	app.expect(http.MethodPost, "/api/billing/play/purchases", map[string]string{"product_id": "notes.monthly", "purchase_token": playToken}, http.StatusOK)
	appleToken, _ := h.appStore.sell(accountToken)
	app.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": appleToken}, http.StatusOK)

	app.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	web := h.mock.Subscriptions(subject)
	if len(web) != 1 || web[0].WillRenew {
		t.Fatalf("web subscriptions after deletion = %+v, want cancelled", web)
	}
	if cancelled := h.play.cancelledIDs(); len(cancelled) != 1 || cancelled[0] != playSub.ExternalID {
		t.Fatalf("play cancellations = %v, want %s", cancelled, playSub.ExternalID)
	}
}

func TestDashboardBilling(t *testing.T) {
	off := newHarness(t).regionBrowser()
	expectLacks(t, off.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `id="billing-heading"`)

	h := newBillingHarness(t)
	browser := h.regionBrowser()
	expectContains(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `id="billing-heading"`, "No subscription yet", `action="/dashboard/billing/checkout"`)

	// Subscribe sends the browser to the checkout, with or without htmx.
	res := browser.submit("/dashboard/billing/checkout", nil)
	if res.status != http.StatusSeeOther || !strings.HasPrefix(res.header.Get("Location"), h.api.URL+mockweb.CheckoutPath+"?") {
		t.Fatalf("dashboard checkout = %d %v", res.status, res.header)
	}
	hx := browser.hxSubmit("/dashboard/billing/checkout", nil)
	if hx.status != http.StatusNoContent || hx.header.Get("HX-Redirect") != res.header.Get("Location") {
		t.Fatalf("htmx dashboard checkout = %d %v", hx.status, hx.header)
	}
	browser.subscribe()
	expectContains(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `data-billing-status="active"`, `data-billing-source="web"`,
		`action="/dashboard/billing/portal"`)
	expectRenderedError(t, browser.submit("/dashboard/billing/checkout", nil), http.StatusConflict)
	if res := browser.submit("/dashboard/billing/portal", nil); res.status != http.StatusSeeOther ||
		!strings.HasPrefix(res.header.Get("Location"), h.api.URL+mockweb.PortalPath+"?") {
		t.Fatalf("dashboard portal = %d %v", res.status, res.header)
	}

	stores := newStoresOnlyHarness(t)
	browser = stores.regionBrowser()
	expectLacks(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `action="/dashboard/billing/checkout"`)
	expectRenderedError(t, browser.submit("/dashboard/billing/checkout", nil), http.StatusServiceUnavailable)
	token, _ := stores.appStore.sell(browser.billing().StoreAccountToken)
	browser.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": token}, http.StatusOK)
	expectContains(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `data-billing-source="app_store"`, "subscription settings of the store")
	expectRenderedError(t, browser.submit("/dashboard/billing/portal", nil), http.StatusServiceUnavailable)
}
