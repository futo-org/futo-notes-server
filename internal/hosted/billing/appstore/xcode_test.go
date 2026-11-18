package appstore_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/appstore"
)

// xcodeSign returns payload as Xcode's StoreKit testing signs it: ES256 by
// a local self-signed certificate, the only one in x5c.
func xcodeSign(t *testing.T, payload any) string {
	t.Helper()
	key := genKey(t)
	cert := issue(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "StoreKit Testing in Xcode"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}, nil, key, key)
	return signWith(t, key, map[string]any{"alg": "ES256", "kid": "Apple_Xcode_Key", "x5c": []string{base64.StdEncoding.EncodeToString(cert.Raw)}}, payload)
}

func signWith(t *testing.T, key *ecdsa.PrivateKey, header, payload any) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// xcodeOnly is the provider APPLE_STOREKIT_XCODE configures without an App
// Store app: no In-App Purchase key, Apple's root.
func xcodeOnly() *appstore.Provider {
	return appstore.New(appstore.Config{BundleID: bundleID, ProductIDs: []string{productID}, AcceptXcode: true})
}

func TestSubmitXcode(t *testing.T) {
	signed := time.Now().Truncate(time.Millisecond)
	payload := tx("Xcode", signed)
	ev, err := xcodeOnly().Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, payload)})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Source != billing.SourceAppStore || ev.ExternalID != origID || ev.AccountToken != accountTok ||
		ev.ProductID != productID || ev.Status != billing.StatusActive || ev.WillRenew ||
		ev.Environment != billing.EnvironmentXcode || !ev.EventAt.Equal(signed) || ev.GraceUntil != nil {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ExpiresAt == nil || ev.ExpiresAt.UnixMilli() != payload["expiresDate"] {
		t.Fatalf("expires = %v", ev.ExpiresAt)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Raw, &raw); err != nil || raw["transaction"] == nil {
		t.Fatalf("raw = %s", ev.Raw)
	}

	expired := with(payload, "expiresDate", ms(signed.Add(-time.Minute)))
	if ev, err = xcodeOnly().Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, expired)}); err != nil || ev.Status != billing.StatusExpired {
		t.Fatalf("expired: %+v, %v", ev, err)
	}
	refunded := with(payload, "revocationDate", ms(signed))
	if ev, err = xcodeOnly().Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, refunded)}); err != nil || ev.Status != billing.StatusRevoked {
		t.Fatalf("refunded: %+v, %v", ev, err)
	}
	if ev, err = xcodeOnly().Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, with(payload, "appAccountToken", nil))}); err != nil || ev.AccountToken != "" {
		t.Fatalf("no account token: %+v, %v", ev, err)
	}
}

func TestSubmitXcodeChecksTheTransaction(t *testing.T) {
	now := time.Now()
	for name, payload := range map[string]map[string]any{
		"other bundle":     with(tx("Xcode", now), "bundleId", "com.example.other"),
		"other product":    with(tx("Xcode", now), "productId", "notes.yearly"),
		"family shared":    with(tx("Xcode", now), "inAppOwnershipType", "FAMILY_SHARED"),
		"no original id":   with(tx("Xcode", now), "originalTransactionId", nil),
		"other env":        with(tx("Xcode", now), "environment", "xcode"),
		"production":       tx("Production", now),
		"sandbox":          tx("Sandbox", now),
		"malformed fields": with(tx("Xcode", now), "expiresDate", "tomorrow"),
	} {
		if _, err := xcodeOnly().Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, payload)}); !errors.Is(err, billing.ErrInvalidPurchase) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Without AcceptXcode, an Xcode transaction fails verification like any
// other the App Store did not sign, and Apple is not asked.
func TestSubmitXcodeRefusedByDefault(t *testing.T) {
	f := newFixture(t, true)
	_, err := f.provider.Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, tx("Xcode", time.Now()))})
	if !errors.Is(err, billing.ErrInvalidPurchase) {
		t.Fatalf("err = %v", err)
	}
	if f.production.callCount()+f.sandbox.callCount() != 0 {
		t.Fatal("called the App Store Server API")
	}
}

// AcceptXcode leaves every other environment verified: a Production or
// Sandbox transaction signed outside the trusted chain is refused, and one
// signed inside it still goes to Apple.
func TestXcodeKeepsVerifyingOtherEnvironments(t *testing.T) {
	trusted := newSigner(t, chainOpts{})
	untrusted := newSigner(t, chainOpts{})
	apiKey := genKey(t)
	prod, prodSrv := newFakeAPI(t, &apiKey.PublicKey)
	sand, sandSrv := newFakeAPI(t, &apiKey.PublicKey)
	p := appstore.New(appstore.Config{
		BundleID: bundleID, AppAppleID: appAppleID, IssuerID: issuerID, KeyID: keyID, PrivateKey: apiKey,
		ProductIDs: []string{productID}, AcceptSandbox: true, AcceptXcode: true,
		ProductionURL: prodSrv.URL, SandboxURL: sandSrv.URL, Roots: trusted.roots(),
	})
	now := time.Now().Truncate(time.Millisecond)
	for _, env := range []string{"Production", "Sandbox"} {
		for name, token := range map[string]string{
			"untrusted chain": untrusted.sign(t, tx(env, now)),
			"self-signed":     xcodeSign(t, tx(env, now)),
		} {
			if _, err := p.Submit(context.Background(), billing.Purchase{Token: token}); !errors.Is(err, billing.ErrInvalidPurchase) {
				t.Errorf("%s %s: err = %v", env, name, err)
			}
		}
	}
	if prod.callCount()+sand.callCount() != 0 {
		t.Fatal("called the App Store Server API for an unverified transaction")
	}

	prod.set(origID, fakeAnswer{code: 200, body: statusBody("Production", 1, trusted.sign(t, tx("Production", now)), trusted.sign(t, renewalInfo(1, now)))})
	ev, err := p.Submit(context.Background(), billing.Purchase{Token: trusted.sign(t, tx("Production", now))})
	if err != nil || ev.Environment != billing.EnvironmentProduction || prod.callCount() != 1 {
		t.Fatalf("trusted production: %+v, %v, %d calls", ev, err, prod.callCount())
	}
	ev, err = p.Submit(context.Background(), billing.Purchase{Token: xcodeSign(t, tx("Xcode", now))})
	if err != nil || ev.Environment != billing.EnvironmentXcode || prod.callCount()+sand.callCount() != 1 {
		t.Fatalf("xcode beside an app: %+v, %v", ev, err)
	}
}

// Without an In-App Purchase key, only Xcode transactions are accepted and
// nothing calls Apple.
func TestXcodeOnlyCallsNoAPI(t *testing.T) {
	s := newSigner(t, chainOpts{})
	p := appstore.New(appstore.Config{BundleID: bundleID, ProductIDs: []string{productID}, AcceptSandbox: true, AcceptXcode: true, Roots: s.roots()})
	for _, env := range []string{"Production", "Sandbox"} {
		if _, err := p.Submit(context.Background(), billing.Purchase{Token: s.sign(t, tx(env, time.Now()))}); !errors.Is(err, billing.ErrInvalidPurchase) {
			t.Errorf("%s: err = %v", env, err)
		}
	}
	if _, err := p.Refresh(context.Background(), origID); err == nil {
		t.Fatal("refresh without a key succeeded")
	}
}
