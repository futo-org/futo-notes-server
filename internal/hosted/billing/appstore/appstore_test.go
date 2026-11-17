package appstore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/appstore"
)

const origID = "2000000000000001"

func TestSource(t *testing.T) {
	f := newFixture(t, false)
	if f.provider.Source() != billing.SourceAppStore {
		t.Fatalf("source = %q", f.provider.Source())
	}
}

func TestSubmit(t *testing.T) {
	f := newFixture(t, false)
	submitted := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	answered := time.Now().Truncate(time.Millisecond)
	apiTx := tx("Production", answered)
	f.production.set(origID, fakeAnswer{code: http.StatusOK, body: statusBody("Production", 1,
		f.signer.sign(t, apiTx), f.signer.sign(t, renewalInfo(1, answered)))})

	ev, err := f.provider.Submit(context.Background(), billing.Purchase{Token: f.signer.sign(t, tx("Production", submitted))})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Source != billing.SourceAppStore || ev.ExternalID != origID || ev.AccountToken != accountTok ||
		ev.ProductID != productID || ev.Status != billing.StatusActive || !ev.WillRenew ||
		ev.Environment != billing.EnvironmentProduction || !ev.EventAt.Equal(answered) || ev.GraceUntil != nil || ev.Subject != "" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ExpiresAt == nil || ev.ExpiresAt.UnixMilli() != apiTx["expiresDate"] {
		t.Fatalf("expires = %v", ev.ExpiresAt)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Raw, &raw); err != nil || raw["transaction"] == nil || raw["renewalInfo"] == nil || string(raw["status"]) != "1" {
		t.Fatalf("raw = %s", ev.Raw)
	}
	if f.production.callCount() != 1 || f.sandbox.callCount() != 0 {
		t.Fatalf("calls: production %d, sandbox %d", f.production.callCount(), f.sandbox.callCount())
	}
}

func TestSubmitSandbox(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)

	f := newFixture(t, true)
	f.sandbox.set(origID, fakeAnswer{code: http.StatusOK, body: statusBody("Sandbox", 1,
		f.signer.sign(t, tx("Sandbox", now)), f.signer.sign(t, renewalInfo(1, now)))})
	ev, err := f.provider.Submit(context.Background(), billing.Purchase{Token: f.signer.sign(t, tx("Sandbox", now))})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Environment != billing.EnvironmentSandbox || ev.Status != billing.StatusActive {
		t.Fatalf("event = %+v", ev)
	}
	if f.production.callCount() != 0 || f.sandbox.callCount() != 1 {
		t.Fatalf("calls: production %d, sandbox %d", f.production.callCount(), f.sandbox.callCount())
	}

	refusing := newFixture(t, false)
	_, err = refusing.provider.Submit(context.Background(), billing.Purchase{Token: refusing.signer.sign(t, tx("Sandbox", now))})
	if !errors.Is(err, billing.ErrInvalidPurchase) {
		t.Fatalf("sandbox when refused: err = %v", err)
	}
	if refusing.production.callCount()+refusing.sandbox.callCount() != 0 {
		t.Fatal("a refused sandbox transaction reached the API")
	}
}

func TestSubmitRejects(t *testing.T) {
	now := time.Now()
	f := newFixture(t, true)
	other := newSigner(t, chainOpts{})
	expired := newSigner(t, chainOpts{leafNotAfter: now.Add(-time.Hour)})
	unmarkedLeaf := newSigner(t, chainOpts{noLeafMarker: true})
	unmarkedIntermediate := newSigner(t, chainOpts{noIntermediateMarker: true})
	good := tx("Production", now)
	tampered := func() string {
		parts := strings.Split(f.signer.sign(t, good), ".")
		forged := strings.Split(f.signer.sign(t, with(good, "productId", "notes.yearly")), ".")
		return parts[0] + "." + forged[1] + "." + parts[2]
	}
	for name, token := range map[string]string{
		"wrong root":            other.sign(t, good),
		"expired leaf":          expired.sign(t, good),
		"leaf without marker":   unmarkedLeaf.sign(t, good),
		"intermediate unmarked": unmarkedIntermediate.sign(t, good),
		"tampered payload":      tampered(),
		"not a jws":             "abc",
		"no signedDate":         f.signer.sign(t, with(good, "signedDate", nil)),
		"wrong bundle":          f.signer.sign(t, with(good, "bundleId", "com.example.other")),
		"unknown product":       f.signer.sign(t, with(good, "productId", "notes.lifetime")),
		"xcode environment":     f.signer.sign(t, with(good, "environment", "Xcode")),
		"family shared":         f.signer.sign(t, with(good, "inAppOwnershipType", "FAMILY_SHARED")),
		"no appAccountToken":    f.signer.sign(t, with(good, "appAccountToken", nil)),
		"unknown to apple":      f.signer.sign(t, good),
	} {
		_, err := f.provider.Submit(context.Background(), billing.Purchase{Token: token})
		if !errors.Is(err, billing.ErrInvalidPurchase) {
			t.Errorf("%s: err = %v, want ErrInvalidPurchase", name, err)
		}
	}
	if n := f.production.callCount(); n != 1 {
		t.Fatalf("only the genuine transaction should reach the API, got %d calls", n)
	}
}

func TestSubmitAPIFailure(t *testing.T) {
	f := newFixture(t, false)
	f.production.set(origID, fakeAnswer{code: http.StatusInternalServerError, body: map[string]any{"errorCode": 5000000, "errorMessage": "An unknown error occurred."}})
	_, err := f.provider.Submit(context.Background(), billing.Purchase{Token: f.signer.sign(t, tx("Production", time.Now()))})
	if err == nil || errors.Is(err, billing.ErrInvalidPurchase) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

func TestSigningTimeDecidesCertificateValidity(t *testing.T) {
	// The leaf expired an hour ago but was valid when it signed.
	now := time.Now()
	s := newSigner(t, chainOpts{leafNotAfter: now.Add(-time.Hour)})
	p := appstore.New(appstore.Config{BundleID: bundleID, AppAppleID: appAppleID, ProductIDs: []string{productID}, Roots: s.roots()})
	payload := notificationPayload("EXPIRED", "VOLUNTARY", now.Add(-2*time.Hour), map[string]any{
		"signedTransactionInfo": s.sign(t, tx("Production", now.Add(-2*time.Hour))),
	})
	if _, events, err := p.Verify(context.Background(), delivery(s.sign(t, payload))); err != nil || len(events) != 1 {
		t.Fatalf("events = %v, err = %v", events, err)
	}
}

func TestRefreshStatuses(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	grace := now.Add(6 * 24 * time.Hour)
	for _, tc := range []struct {
		name      string
		status    int
		tx        map[string]any
		renewal   map[string]any
		want      billing.Status
		willRenew bool
		graceEnd  *time.Time
	}{
		{"active", 1, tx("Production", now), renewalInfo(1, now), billing.StatusActive, true, nil},
		{"active, renewal off", 1, tx("Production", now), renewalInfo(0, now), billing.StatusActive, false, nil},
		{"expired", 2, tx("Production", now), renewalInfo(0, now), billing.StatusExpired, false, nil},
		{"billing retry", 3, tx("Production", now), renewalInfo(1, now), billing.StatusOnHold, true, nil},
		{"grace", 4, tx("Production", now), with(renewalInfo(1, now), "gracePeriodExpiresDate", ms(grace)), billing.StatusGrace, true, &grace},
		{"revoked", 5, tx("Production", now), renewalInfo(1, now), billing.StatusRevoked, false, nil},
		{"refunded", 1, with(tx("Production", now), "revocationDate", ms(now)), renewalInfo(1, now), billing.StatusRevoked, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, false)
			f.production.set(origID, fakeAnswer{code: http.StatusOK, body: statusBody("Production", tc.status, f.signer.sign(t, tc.tx), f.signer.sign(t, tc.renewal))})
			ev, err := f.provider.Refresh(context.Background(), origID)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Status != tc.want || ev.WillRenew != tc.willRenew || ev.ExternalID != origID || ev.AccountToken != accountTok {
				t.Fatalf("event = %+v", ev)
			}
			if (tc.graceEnd == nil) != (ev.GraceUntil == nil) || (tc.graceEnd != nil && !ev.GraceUntil.Equal(*tc.graceEnd)) {
				t.Fatalf("grace until = %v, want %v", ev.GraceUntil, tc.graceEnd)
			}
		})
	}
}

func TestRefreshFallsBackToSandbox(t *testing.T) {
	now := time.Now()
	f := newFixture(t, true)
	f.sandbox.set(origID, fakeAnswer{code: http.StatusOK, body: statusBody("Sandbox", 1, f.signer.sign(t, tx("Sandbox", now)), f.signer.sign(t, renewalInfo(1, now)))})
	ev, err := f.provider.Refresh(context.Background(), origID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Environment != billing.EnvironmentSandbox || f.production.callCount() != 1 || f.sandbox.callCount() != 1 {
		t.Fatalf("event = %+v, calls: production %d, sandbox %d", ev, f.production.callCount(), f.sandbox.callCount())
	}

	refusing := newFixture(t, false)
	refusing.sandbox.set(origID, fakeAnswer{code: http.StatusOK, body: statusBody("Sandbox", 1, refusing.signer.sign(t, tx("Sandbox", now)), "")})
	if _, err := refusing.provider.Refresh(context.Background(), origID); err == nil {
		t.Fatal("Refresh found a sandbox transaction with sandbox refused")
	}
	if refusing.sandbox.callCount() != 0 {
		t.Fatal("Refresh asked sandbox with sandbox refused")
	}

	failing := newFixture(t, true)
	failing.production.set(origID, fakeAnswer{code: http.StatusTooManyRequests, body: map[string]any{"errorCode": 4290000, "errorMessage": "Rate limit exceeded."}})
	if _, err := failing.provider.Refresh(context.Background(), origID); err == nil || failing.sandbox.callCount() != 0 {
		t.Fatalf("a production failure must not fall back: err = %v, sandbox calls %d", err, failing.sandbox.callCount())
	}
}

func TestRefreshRejectsForeignAnswers(t *testing.T) {
	now := time.Now()
	f := newFixture(t, false)
	body := statusBody("Production", 1, f.signer.sign(t, tx("Production", now)), "")
	body["bundleId"] = "com.example.other"
	f.production.set(origID, fakeAnswer{code: http.StatusOK, body: body})
	if _, err := f.provider.Refresh(context.Background(), origID); err == nil {
		t.Fatal("Refresh accepted an answer for another bundle")
	}
	other := newSigner(t, chainOpts{})
	f.production.set(origID, fakeAnswer{code: http.StatusOK, body: statusBody("Production", 1, other.sign(t, tx("Production", now)), "")})
	if _, err := f.provider.Refresh(context.Background(), origID); err == nil {
		t.Fatal("Refresh accepted a transaction signed under another root")
	}
}

func TestCancel(t *testing.T) {
	f := newFixture(t, false)
	if err := f.provider.Cancel(context.Background(), origID); !errors.Is(err, billing.ErrNotSupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestParsePrivateKey(t *testing.T) {
	ec := genKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	key, err := appstore.ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil || !key.Equal(ec) {
		t.Fatalf("key = %v, err = %v", key, err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"rsa":     pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER}),
		"garbage": []byte("not a key"),
	} {
		if _, err := appstore.ParsePrivateKey(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
