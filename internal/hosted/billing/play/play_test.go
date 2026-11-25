package play_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"futo-notes-server/internal/hosted/billing"
	"futo-notes-server/internal/hosted/billing/play"
)

const (
	ackPath    = "POST subscriptions/" + productID + "/tokens/" + purchaseToken + ":acknowledge"
	cancelPath = "POST subscriptions/" + productID + "/tokens/" + purchaseToken + ":cancel"
	getPath    = "GET subscriptionsv2/tokens/" + purchaseToken
)

func TestSource(t *testing.T) {
	if p := newFakeGoogle(t).provider(t); p.Source() != billing.SourcePlay {
		t.Fatalf("source = %q", p.Source())
	}
}

func TestSubmitAcknowledges(t *testing.T) {
	expiry := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Millisecond)
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", expiry, "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING")})
	p := g.provider(t)
	before := time.Now()
	ev, err := p.Submit(context.Background(), billing.Purchase{ProductID: productID, Token: purchaseToken})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Source != billing.SourcePlay || ev.ExternalID != purchaseToken || ev.AccountToken != accountToken || ev.ProductID != productID ||
		ev.Status != billing.StatusActive || !ev.WillRenew || ev.ExpiresAt == nil || !ev.ExpiresAt.Equal(expiry) ||
		ev.Environment != billing.EnvironmentProduction || ev.EventAt.Before(before) || ev.GraceUntil != nil || ev.Subject != "" {
		t.Fatalf("event = %+v", ev)
	}
	var raw map[string]any
	if err := json.Unmarshal(ev.Raw, &raw); err != nil || raw["subscriptionState"] != "SUBSCRIPTION_STATE_ACTIVE" {
		t.Fatalf("raw = %s", ev.Raw)
	}
	if calls := g.apiCalls(); !slices.Equal(calls, []string{getPath, ackPath}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestSubmitSkipsAcknowledged(t *testing.T) {
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour))})
	if _, err := g.provider(t).Submit(context.Background(), billing.Purchase{Token: purchaseToken}); err != nil {
		t.Fatal(err)
	}
	if calls := g.apiCalls(); !slices.Equal(calls, []string{getPath}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestSubmitNeverAcknowledgesUnpaid(t *testing.T) {
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_PENDING", time.Now().Add(time.Hour), "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING")})
	ev, err := g.provider(t).Submit(context.Background(), billing.Purchase{Token: purchaseToken})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != billing.StatusOnHold || ev.WillRenew {
		t.Fatalf("event = %+v", ev)
	}
	if calls := g.apiCalls(); !slices.Equal(calls, []string{getPath}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestSubmitAcknowledgeFailure(t *testing.T) {
	g := newFakeGoogle(t)
	g.ackStatus = http.StatusServiceUnavailable
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour), "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING")})
	_, err := g.provider(t).Submit(context.Background(), billing.Purchase{Token: purchaseToken})
	if err == nil || errors.Is(err, billing.ErrInvalidPurchase) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

func TestSubmitSandbox(t *testing.T) {
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour), "testPurchase", map[string]any{})})
	ev, err := g.provider(t).Submit(context.Background(), billing.Purchase{Token: purchaseToken})
	if err != nil || ev.Environment != billing.EnvironmentSandbox {
		t.Fatalf("event = %+v, err = %v", ev, err)
	}
}

func TestSubmitRejects(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	for name, answer := range map[string]*fakeAnswer{
		"unknown token":   nil,
		"gone":            {code: http.StatusGone, body: map[string]any{"error": map[string]any{"code": 410}}},
		"malformed token": {code: http.StatusBadRequest, body: map[string]any{"error": map[string]any{"code": 400, "message": "Invalid Value"}}},
		"another product": {code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", expiry, "lineItems", []any{map[string]any{"productId": "notes_tip", "expiryTime": expiry.Format(time.RFC3339)}})},
		"no account id":   {code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", expiry, "externalAccountIdentifiers", nil, "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING")},
	} {
		g := newFakeGoogle(t)
		if answer != nil {
			g.set(purchaseToken, *answer)
		}
		_, err := g.provider(t).Submit(context.Background(), billing.Purchase{Token: purchaseToken})
		if !errors.Is(err, billing.ErrInvalidPurchase) {
			t.Errorf("%s: err = %v, want ErrInvalidPurchase", name, err)
		}
		if slices.Contains(g.apiCalls(), ackPath) {
			t.Errorf("%s: acknowledged a refused purchase", name)
		}
	}
	g := newFakeGoogle(t)
	if _, err := g.provider(t).Submit(context.Background(), billing.Purchase{}); !errors.Is(err, billing.ErrInvalidPurchase) || len(g.apiCalls()) != 0 {
		t.Fatalf("empty token: err = %v, calls = %v", err, g.apiCalls())
	}
}

func TestSubmitAPIFailure(t *testing.T) {
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusInternalServerError, body: map[string]any{"error": map[string]any{"code": 500}}})
	_, err := g.provider(t).Submit(context.Background(), billing.Purchase{Token: purchaseToken})
	if err == nil || errors.Is(err, billing.ErrInvalidPurchase) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

func TestRefreshMapsStates(t *testing.T) {
	future := time.Now().Add(3 * 24 * time.Hour).Truncate(time.Millisecond)
	past := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for _, tc := range []struct {
		state     string
		expiry    time.Time
		autoRenew bool
		want      billing.Status
		willRenew bool
		grace     bool
	}{
		{"SUBSCRIPTION_STATE_ACTIVE", future, true, billing.StatusActive, true, false},
		{"SUBSCRIPTION_STATE_ACTIVE", future, false, billing.StatusActive, false, false},
		{"SUBSCRIPTION_STATE_IN_GRACE_PERIOD", future, true, billing.StatusGrace, true, true},
		{"SUBSCRIPTION_STATE_ON_HOLD", past, true, billing.StatusOnHold, true, false},
		{"SUBSCRIPTION_STATE_PAUSED", past, true, billing.StatusOnHold, true, false},
		{"SUBSCRIPTION_STATE_CANCELED", future, false, billing.StatusActive, false, false},
		{"SUBSCRIPTION_STATE_CANCELED", past, false, billing.StatusExpired, false, false},
		{"SUBSCRIPTION_STATE_EXPIRED", past, false, billing.StatusExpired, false, false},
		{"SUBSCRIPTION_STATE_PENDING", future, true, billing.StatusOnHold, false, false},
		{"SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED", past, false, billing.StatusExpired, false, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			g := newFakeGoogle(t)
			g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase(tc.state, tc.expiry, "lineItems", []any{map[string]any{
				"productId":        productID,
				"expiryTime":       tc.expiry.Format(time.RFC3339Nano),
				"autoRenewingPlan": map[string]any{"autoRenewEnabled": tc.autoRenew},
			}})})
			ev, err := g.provider(t).Refresh(context.Background(), purchaseToken)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Status != tc.want || ev.WillRenew != tc.willRenew || ev.ExternalID != purchaseToken || !ev.ExpiresAt.Equal(tc.expiry) {
				t.Fatalf("event = %+v", ev)
			}
			if tc.grace != (ev.GraceUntil != nil) || (tc.grace && !ev.GraceUntil.Equal(tc.expiry)) {
				t.Fatalf("grace until = %v", ev.GraceUntil)
			}
		})
	}
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_UNSPECIFIED", future)})
	if _, err := g.provider(t).Refresh(context.Background(), purchaseToken); err == nil {
		t.Fatal("Refresh accepted an unspecified state")
	}
}

func TestCancel(t *testing.T) {
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour))})
	if err := g.provider(t).Cancel(context.Background(), purchaseToken); err != nil {
		t.Fatal(err)
	}
	if calls := g.apiCalls(); !slices.Equal(calls, []string{getPath, cancelPath}) {
		t.Fatalf("calls = %v", calls)
	}

	failing := newFakeGoogle(t)
	failing.ackStatus = http.StatusForbidden
	failing.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour))})
	if err := failing.provider(t).Cancel(context.Background(), purchaseToken); err == nil {
		t.Fatal("Cancel ignored a refusal")
	}
	unknown := newFakeGoogle(t)
	if err := unknown.provider(t).Cancel(context.Background(), purchaseToken); err == nil {
		t.Fatal("Cancel succeeded for an unknown token")
	}
}

func TestParseServiceAccount(t *testing.T) {
	g := newFakeGoogle(t)
	sa, err := play.ParseServiceAccount(g.serviceAccountJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if sa.Email != "play-api@futo-notes.iam.gserviceaccount.com" || !slices.Equal(sa.Scopes, []string{play.Scope}) {
		t.Fatalf("service account = %+v", sa)
	}
	for name, data := range map[string]string{
		"not json":      "nope",
		"user creds":    `{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`,
		"no key":        `{"type":"service_account","client_email":"a@b.iam.gserviceaccount.com"}`,
		"no email":      `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMA==\n-----END PRIVATE KEY-----\n"}`,
		"empty":         "",
		"external acct": `{"type":"external_account","audience":"x","subject_token_type":"y","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"file":"/x"}}`,
	} {
		if _, err := play.ParseServiceAccount([]byte(data)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestVerifyRefetches(t *testing.T) {
	g := newFakeGoogle(t)
	p := g.provider(t)
	// The notification says renewed; Google's current answer wins.
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_EXPIRED", time.Now().Add(-time.Hour))})
	header, body := push(t, pushToken(t, g.signingKey, nil), subscriptionNotification(2, purchaseToken))
	id, events, err := p.Verify(context.Background(), billing.Delivery{Header: header, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if id != "2070443601311540" || len(events) != 1 || events[0].Status != billing.StatusExpired || events[0].ExternalID != purchaseToken || events[0].AccountToken != accountToken {
		t.Fatalf("id = %q, events = %+v", id, events)
	}
	if calls := g.apiCalls(); !slices.Equal(calls, []string{getPath}) {
		t.Fatalf("calls = %v", calls)
	}
}

// A purchase the app never submitted (killed, offline) is still kept: its
// notification acknowledges it, but only an entitling one bought through the
// app, which set the account id.
func TestVerifyAcknowledgesPendingPurchases(t *testing.T) {
	for name, tc := range map[string]struct {
		body    map[string]any
		wantAck bool
	}{
		"active, pending": {purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour), "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING"), true},
		"no account id":   {purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour), "externalAccountIdentifiers", nil, "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING"), false},
		"not entitling":   {purchase("SUBSCRIPTION_STATE_PENDING", time.Now().Add(time.Hour), "acknowledgementState", "ACKNOWLEDGEMENT_STATE_PENDING"), false},
		"acknowledged":    {purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour)), false},
	} {
		g := newFakeGoogle(t)
		g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: tc.body})
		header, body := push(t, pushToken(t, g.signingKey, nil), subscriptionNotification(4, purchaseToken))
		if _, _, err := g.provider(t).Verify(context.Background(), billing.Delivery{Header: header, Body: body}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := []string{getPath}
		if tc.wantAck {
			want = append(want, ackPath)
		}
		if calls := g.apiCalls(); !slices.Equal(calls, want) {
			t.Errorf("%s: calls = %v, want %v", name, calls, want)
		}
	}
}

func TestVerifyRevocations(t *testing.T) {
	expired := purchase("SUBSCRIPTION_STATE_EXPIRED", time.Now().Add(-time.Hour))
	for name, tc := range map[string]struct {
		notification map[string]any
		answer       map[string]any
		want         billing.Status
	}{
		"revoked": {subscriptionNotification(12, purchaseToken), expired, billing.StatusRevoked},
		"voided subscription": {map[string]any{
			"version": "1.0", "packageName": packageName, "eventTimeMillis": "1700000000000",
			"voidedPurchaseNotification": map[string]any{"purchaseToken": purchaseToken, "orderId": "GPA.1", "productType": 1, "refundType": 1},
		}, expired, billing.StatusRevoked},
		"refunded, still active": {map[string]any{
			"version": "1.0", "packageName": packageName,
			"voidedPurchaseNotification": map[string]any{"purchaseToken": purchaseToken, "orderId": "GPA.1", "productType": 1, "refundType": 1},
		}, purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour)), billing.StatusActive},
		"expired": {subscriptionNotification(13, purchaseToken), expired, billing.StatusExpired},
	} {
		g := newFakeGoogle(t)
		g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: tc.answer})
		header, body := push(t, pushToken(t, g.signingKey, nil), tc.notification)
		_, events, err := g.provider(t).Verify(context.Background(), billing.Delivery{Header: header, Body: body})
		if err != nil || len(events) != 1 || events[0].Status != tc.want {
			t.Errorf("%s: events = %+v, err = %v", name, events, err)
		}
	}
}

func TestVerifyIgnores(t *testing.T) {
	for name, n := range map[string]map[string]any{
		"test":     {"version": "1.0", "packageName": packageName, "testNotification": map[string]any{"version": "1.0"}},
		"one-time": {"version": "1.0", "packageName": packageName, "oneTimeProductNotification": map[string]any{"notificationType": 1, "purchaseToken": "t", "sku": "tip"}},
		"voided one-time": {"version": "1.0", "packageName": packageName,
			"voidedPurchaseNotification": map[string]any{"purchaseToken": "t", "orderId": "GPA.1", "productType": 2}},
		"another package": with(subscriptionNotification(4, purchaseToken), "packageName", "com.example.other"),
	} {
		g := newFakeGoogle(t)
		header, body := push(t, pushToken(t, g.signingKey, nil), n)
		id, events, err := g.provider(t).Verify(context.Background(), billing.Delivery{Header: header, Body: body})
		if err != nil || id == "" || len(events) != 0 || len(g.apiCalls()) != 0 {
			t.Errorf("%s: id = %q, events = %+v, err = %v, calls = %v", name, id, events, err, g.apiCalls())
		}
	}
	for name, answer := range map[string]*fakeAnswer{
		"forgotten token": nil,
		"another product": {code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now(), "lineItems", []any{map[string]any{"productId": "notes_tip"}})},
	} {
		g := newFakeGoogle(t)
		if answer != nil {
			g.set(purchaseToken, *answer)
		}
		header, body := push(t, pushToken(t, g.signingKey, nil), subscriptionNotification(4, purchaseToken))
		id, events, err := g.provider(t).Verify(context.Background(), billing.Delivery{Header: header, Body: body})
		if err != nil || id == "" || len(events) != 0 {
			t.Errorf("%s: id = %q, events = %+v, err = %v", name, id, events, err)
		}
	}
}

func TestVerifyAPIFailureIsRetryable(t *testing.T) {
	g := newFakeGoogle(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusServiceUnavailable, body: map[string]any{}})
	header, body := push(t, pushToken(t, g.signingKey, nil), subscriptionNotification(4, purchaseToken))
	_, _, err := g.provider(t).Verify(context.Background(), billing.Delivery{Header: header, Body: body})
	if err == nil || errors.Is(err, billing.ErrInvalidDelivery) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

func TestVerifyRejectsPushTokens(t *testing.T) {
	g := newFakeGoogle(t)
	p := g.provider(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour))})
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := subscriptionNotification(4, purchaseToken)
	for name, bearer := range map[string]string{
		"no token":           "",
		"wrong audience":     pushToken(t, g.signingKey, jwt.MapClaims{"aud": "https://elsewhere.example.com"}),
		"wrong issuer":       pushToken(t, g.signingKey, jwt.MapClaims{"iss": "https://accounts.example.com"}),
		"wrong email":        pushToken(t, g.signingKey, jwt.MapClaims{"email": "someone@futo-notes.iam.gserviceaccount.com"}),
		"no email":           pushToken(t, g.signingKey, jwt.MapClaims{"email": nil}),
		"email not verified": pushToken(t, g.signingKey, jwt.MapClaims{"email_verified": false}),
		"bad signature":      pushToken(t, stranger, nil),
		"expired":            pushToken(t, g.signingKey, jwt.MapClaims{"iat": time.Now().Add(-2 * time.Hour).Unix(), "exp": time.Now().Add(-time.Hour).Unix()}),
		"not a jwt":          "garbage",
	} {
		header, body := push(t, bearer, n)
		if _, _, err := p.Verify(context.Background(), billing.Delivery{Header: header, Body: body}); !errors.Is(err, billing.ErrInvalidDelivery) {
			t.Errorf("%s: err = %v, want ErrInvalidDelivery", name, err)
		}
	}
	if calls := g.apiCalls(); len(calls) != 0 {
		t.Fatalf("an unauthenticated push reached the API: %v", calls)
	}

	header, _ := push(t, pushToken(t, g.signingKey, nil), n)
	for name, body := range map[string]string{
		"not json":     "nope",
		"no messageId": `{"message":{"data":"e30="}}`,
		"data not b64": `{"message":{"data":"%%%","messageId":"1"}}`,
		"data not obj": `{"message":{"data":"bm9wZQ==","messageId":"1"}}`,
		"no token":     `{"message":{"data":"eyJwYWNrYWdlTmFtZSI6InRlY2guZnV0by5ub3RlcyIsInN1YnNjcmlwdGlvbk5vdGlmaWNhdGlvbiI6e319","messageId":"1"}}`,
	} {
		if _, _, err := p.Verify(context.Background(), billing.Delivery{Header: header, Body: []byte(body)}); !errors.Is(err, billing.ErrInvalidDelivery) {
			t.Errorf("%s: err = %v, want ErrInvalidDelivery", name, err)
		}
	}
}

func TestVerifyAcceptsSchemelessIssuerAndCachesKeys(t *testing.T) {
	g := newFakeGoogle(t)
	p := g.provider(t)
	g.set(purchaseToken, fakeAnswer{code: http.StatusOK, body: purchase("SUBSCRIPTION_STATE_ACTIVE", time.Now().Add(time.Hour))})
	for _, iss := range []string{"https://accounts.google.com", "accounts.google.com"} {
		header, body := push(t, pushToken(t, g.signingKey, jwt.MapClaims{"iss": iss}), subscriptionNotification(4, purchaseToken))
		if _, events, err := p.Verify(context.Background(), billing.Delivery{Header: header, Body: body}); err != nil || len(events) != 1 {
			t.Fatalf("iss %q: events = %+v, err = %v", iss, events, err)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.jwksFetches != 1 || g.tokenFetches != 1 {
		t.Fatalf("jwks fetched %d times, access token %d times; want each once", g.jwksFetches, g.tokenFetches)
	}
}

func with(m map[string]any, kv ...any) map[string]any {
	out := maps.Clone(m)
	for i := 0; i < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}
