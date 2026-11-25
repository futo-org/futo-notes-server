package api_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/appstore"
	"futo-notes-server/internal/hosted/config"
)

// xcodeTransaction is a StoreKit testing transaction as the app submits
// it. Its signature is junk: Xcode's local certificate signs these, and in
// Xcode mode the server does not check it.
func xcodeTransaction(t *testing.T, env, accountToken string) string {
	t.Helper()
	now := time.Now()
	part := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return part(map[string]any{"alg": "ES256", "kid": "Apple_Xcode_Key", "x5c": []string{"MIIB"}}) + "." + part(map[string]any{
		"transactionId":         "0",
		"originalTransactionId": "0",
		"bundleId":              "com.futo.notes.dev",
		"productId":             "com.futo.notes.sync.monthly",
		"appAccountToken":       accountToken,
		"environment":           env,
		"inAppOwnershipType":    "PURCHASED",
		"type":                  "Auto-Renewable Subscription",
		"purchaseDate":          now.UnixMilli(),
		"expiresDate":           now.Add(30 * 24 * time.Hour).UnixMilli(),
		"signedDate":            now.UnixMilli(),
	}) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
}

// TestXcodeStoreKitPurchase runs the App Store provider as
// APPLE_STOREKIT_XCODE configures it, with no App Store app.
func TestXcodeStoreKitPurchase(t *testing.T) {
	h := newHarnessWithBilling(t, func(c config.Config) billing.Providers {
		return billing.Providers{Stores: map[billing.Source]billing.StoreProvider{billing.SourceAppStore: appstore.New(*c.AppStore)}}
	}, func(c *config.Config) {
		c.DevUI = true
		c.AppStore = &appstore.Config{BundleID: "com.futo.notes.dev", ProductIDs: []string{"com.futo.notes.sync.monthly"}, AcceptXcode: true}
	})
	owner, _ := h.appLogin(h.newSub())

	var status struct {
		Xcode    bool   `json:"xcode"`
		BundleID string `json:"bundle_id"`
	}
	owner.expect(http.MethodGet, "/dev/storekit", nil, http.StatusOK).json(t, &status)
	if !status.Xcode || status.BundleID != "com.futo.notes.dev" {
		t.Fatalf("/dev/storekit = %+v", status)
	}
	before := owner.billing()
	if !before.Rails.AppStore || before.Entitled {
		t.Fatalf("before = %+v", before)
	}

	// Only Xcode transactions are taken without Apple.
	expectHostedError(t, owner.do(http.MethodPost, "/api/billing/app-store/transactions",
		map[string]string{"signed_transaction": xcodeTransaction(t, "Sandbox", before.StoreAccountToken)}), http.StatusBadRequest, "invalid_purchase")

	token := xcodeTransaction(t, "Xcode", before.StoreAccountToken)
	var b billingBody
	owner.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": token}, http.StatusOK).json(t, &b)
	if !b.Entitled || str(b.Source) != "app_store" || str(b.ProductID) != "com.futo.notes.sync.monthly" || b.ExpiresAt == nil {
		t.Fatalf("after an Xcode purchase = %+v", b)
	}

	other, _ := h.appLogin(h.newSub())
	expectHostedError(t, other.do(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": token}),
		http.StatusConflict, "store_subscription_owned_elsewhere")
	if other.billing().Entitled || !owner.billing().Entitled {
		t.Fatal("a refused submit moved the subscription")
	}
}

func TestDevStoreKitOff(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.DevUI = true })
	var status map[string]any
	h.client().expect(http.MethodGet, "/dev/storekit", nil, http.StatusOK).json(t, &status)
	if status["xcode"] != false || len(status) != 1 {
		t.Fatalf("/dev/storekit = %v", status)
	}
}
