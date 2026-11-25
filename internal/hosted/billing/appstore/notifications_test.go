package appstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/billing"
)

const notificationID = "002e14d5-51f5-4503-b5a8-c3a1af68eb20"

func notificationPayload(kind, subtype string, signedAt time.Time, data map[string]any) map[string]any {
	d := map[string]any{"appAppleId": appAppleID, "bundleId": bundleID, "bundleVersion": "1", "environment": "Production"}
	maps.Copy(d, data)
	n := map[string]any{
		"notificationType": kind,
		"notificationUUID": notificationID,
		"version":          "2.0",
		"signedDate":       ms(signedAt),
		"data":             d,
	}
	if subtype != "" {
		n["subtype"] = subtype
	}
	return n
}

func delivery(signedPayload string) billing.Delivery {
	body, _ := json.Marshal(map[string]string{"signedPayload": signedPayload})
	return billing.Delivery{Header: http.Header{"Content-Type": {"application/json"}}, Body: body}
}

func TestVerifyMapsNotifications(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	signedAt := now.Add(-time.Second)
	graceEnd := now.Add(16 * 24 * time.Hour)
	expired := with(tx("Production", signedAt), "expiresDate", ms(signedAt.Add(-time.Hour)))
	refunded := with(tx("Production", signedAt), "revocationDate", ms(signedAt))
	for _, tc := range []struct {
		kind, subtype string
		status        int
		tx            map[string]any
		renewal       map[string]any
		want          billing.Status
		willRenew     bool
		grace         bool
	}{
		{"SUBSCRIBED", "INITIAL_BUY", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
		{"SUBSCRIBED", "RESUBSCRIBE", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
		{"DID_RENEW", "", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
		{"DID_RENEW", "BILLING_RECOVERY", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
		{"DID_FAIL_TO_RENEW", "GRACE_PERIOD", 4, expired, with(renewalInfo(1, signedAt), "gracePeriodExpiresDate", ms(graceEnd)), billing.StatusGrace, true, true},
		{"DID_FAIL_TO_RENEW", "", 3, expired, renewalInfo(1, signedAt), billing.StatusOnHold, true, false},
		{"GRACE_PERIOD_EXPIRED", "", 3, expired, renewalInfo(1, signedAt), billing.StatusOnHold, true, false},
		{"EXPIRED", "VOLUNTARY", 2, expired, renewalInfo(0, signedAt), billing.StatusExpired, false, false},
		{"EXPIRED", "BILLING_RETRY", 2, expired, renewalInfo(1, signedAt), billing.StatusExpired, false, false},
		{"DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", 1, tx("Production", signedAt), renewalInfo(0, signedAt), billing.StatusActive, false, false},
		{"DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_ENABLED", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
		{"DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_ENABLED", 3, expired, renewalInfo(1, signedAt), billing.StatusOnHold, true, false},
		{"DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", 0, tx("Production", signedAt), renewalInfo(0, signedAt), billing.StatusActive, false, false},
		{"DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", 0, expired, renewalInfo(0, signedAt), billing.StatusExpired, false, false},
		{"REFUND", "", 5, refunded, renewalInfo(1, signedAt), billing.StatusRevoked, false, false},
		{"REVOKE", "", 5, refunded, renewalInfo(1, signedAt), billing.StatusRevoked, false, false},
		{"REFUND_REVERSED", "", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
		{"RENEWAL_EXTENDED", "", 1, tx("Production", signedAt), renewalInfo(1, signedAt), billing.StatusActive, true, false},
	} {
		t.Run(tc.kind+"/"+tc.subtype, func(t *testing.T) {
			f := newFixture(t, false)
			data := map[string]any{
				"signedTransactionInfo": f.signer.sign(t, tc.tx),
				"signedRenewalInfo":     f.signer.sign(t, tc.renewal),
			}
			if tc.status != 0 {
				data["status"] = tc.status
			}
			id, events, err := f.provider.Verify(context.Background(), delivery(f.signer.sign(t, notificationPayload(tc.kind, tc.subtype, now, data))))
			if err != nil {
				t.Fatal(err)
			}
			if id != notificationID || len(events) != 1 {
				t.Fatalf("id = %q, events = %+v", id, events)
			}
			ev := events[0]
			if ev.Status != tc.want || ev.WillRenew != tc.willRenew || ev.ExternalID != origID || ev.AccountToken != accountTok ||
				ev.ProductID != productID || ev.Environment != billing.EnvironmentProduction || !ev.EventAt.Equal(now) {
				t.Fatalf("event = %+v", ev)
			}
			if ev.ExpiresAt == nil || ev.ExpiresAt.UnixMilli() != tc.tx["expiresDate"] {
				t.Fatalf("expires = %v", ev.ExpiresAt)
			}
			if tc.grace != (ev.GraceUntil != nil) || (tc.grace && !ev.GraceUntil.Equal(graceEnd)) {
				t.Fatalf("grace until = %v", ev.GraceUntil)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(ev.Raw, &raw); err != nil || raw["notification"] == nil || raw["transaction"] == nil {
				t.Fatalf("raw = %s", ev.Raw)
			}
		})
	}
}

func TestVerifyWithoutAccountToken(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	f := newFixture(t, false)
	payload := notificationPayload("SUBSCRIBED", "INITIAL_BUY", now, map[string]any{
		"signedTransactionInfo": f.signer.sign(t, with(tx("Production", now), "appAccountToken", nil)),
		"signedRenewalInfo":     f.signer.sign(t, renewalInfo(1, now)),
	})
	id, events, err := f.provider.Verify(context.Background(), delivery(f.signer.sign(t, payload)))
	if err != nil || id != notificationID || len(events) != 1 {
		t.Fatalf("id = %q, events = %+v, err = %v", id, events, err)
	}
	if ev := events[0]; ev.AccountToken != "" || ev.ExternalID != origID || ev.Status != billing.StatusActive {
		t.Fatalf("event = %+v", ev)
	}
}

func TestVerifyIgnores(t *testing.T) {
	now := time.Now()
	f := newFixture(t, false)
	signed := func(m map[string]any) string { return f.signer.sign(t, m) }
	for name, payload := range map[string]map[string]any{
		"test":              notificationPayload("TEST", "", now, nil),
		"consumption":       notificationPayload("CONSUMPTION_REQUEST", "", now, map[string]any{"signedTransactionInfo": signed(tx("Production", now))}),
		"price increase":    notificationPayload("PRICE_INCREASE", "PENDING", now, map[string]any{"signedTransactionInfo": signed(tx("Production", now))}),
		"refused sandbox":   notificationPayload("SUBSCRIBED", "INITIAL_BUY", now, map[string]any{"environment": "Sandbox", "signedTransactionInfo": signed(tx("Sandbox", now))}),
		"family shared":     notificationPayload("SUBSCRIBED", "INITIAL_BUY", now, map[string]any{"signedTransactionInfo": signed(with(tx("Production", now), "inAppOwnershipType", "FAMILY_SHARED"))}),
		"another product":   notificationPayload("DID_RENEW", "", now, map[string]any{"signedTransactionInfo": signed(with(tx("Production", now), "productId", "notes.tips"))}),
		"no transaction":    notificationPayload("EXPIRED", "VOLUNTARY", now, nil),
		"external purchase": {"notificationType": "EXTERNAL_PURCHASE_TOKEN", "notificationUUID": notificationID, "signedDate": ms(now)},
	} {
		id, events, err := f.provider.Verify(context.Background(), delivery(signed(payload)))
		if err != nil || id != notificationID || len(events) != 0 {
			t.Errorf("%s: id = %q, events = %+v, err = %v", name, id, events, err)
		}
	}
}

func TestVerifySandbox(t *testing.T) {
	now := time.Now()
	f := newFixture(t, true)
	payload := notificationPayload("SUBSCRIBED", "INITIAL_BUY", now, map[string]any{
		"environment":           "Sandbox",
		"signedTransactionInfo": f.signer.sign(t, tx("Sandbox", now)),
	})
	delete(payload["data"].(map[string]any), "appAppleId")
	_, events, err := f.provider.Verify(context.Background(), delivery(f.signer.sign(t, payload)))
	if err != nil || len(events) != 1 || events[0].Environment != billing.EnvironmentSandbox || events[0].WillRenew {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	now := time.Now()
	f := newFixture(t, false)
	other := newSigner(t, chainOpts{})
	expiredLeaf := newSigner(t, chainOpts{leafNotAfter: now.Add(-time.Hour)})
	goodTx := f.signer.sign(t, tx("Production", now))
	good := notificationPayload("DID_RENEW", "", now, map[string]any{"signedTransactionInfo": goodTx})
	signed := f.signer.sign(t, good)
	forged := f.signer.sign(t, notificationPayload("DID_RENEW", "", now.Add(time.Hour), map[string]any{"signedTransactionInfo": goodTx}))
	a, b := strings.Split(signed, "."), strings.Split(forged, ".")
	tampered := a[0] + "." + b[1] + "." + a[2]
	for name, d := range map[string]billing.Delivery{
		"wrong root":           delivery(other.sign(t, good)),
		"expired leaf":         delivery(expiredLeaf.sign(t, good)),
		"tampered payload":     delivery(tampered),
		"inner wrong root":     delivery(f.signer.sign(t, notificationPayload("DID_RENEW", "", now, map[string]any{"signedTransactionInfo": other.sign(t, tx("Production", now))}))),
		"inner renewal forged": delivery(f.signer.sign(t, notificationPayload("DID_RENEW", "", now, map[string]any{"signedTransactionInfo": goodTx, "signedRenewalInfo": other.sign(t, renewalInfo(1, now))}))),
		"another bundle":       delivery(f.signer.sign(t, notificationPayload("DID_RENEW", "", now, map[string]any{"bundleId": "com.example.other", "signedTransactionInfo": goodTx}))),
		"another app id":       delivery(f.signer.sign(t, notificationPayload("DID_RENEW", "", now, map[string]any{"appAppleId": 42, "signedTransactionInfo": goodTx}))),
		"no uuid":              delivery(f.signer.sign(t, with(good, "notificationUUID", nil))),
		"empty body":           {Body: []byte(`{}`)},
		"not json":             {Body: []byte(`signedPayload=x`)},
	} {
		if _, _, err := f.provider.Verify(context.Background(), d); !errors.Is(err, billing.ErrInvalidDelivery) {
			t.Errorf("%s: err = %v, want ErrInvalidDelivery", name, err)
		}
	}
}
