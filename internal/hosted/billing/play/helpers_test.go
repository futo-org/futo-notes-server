package play_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"futo-notes-server/internal/hosted/billing/play"
)

const (
	packageName    = "tech.futo.notes"
	productID      = "notes_monthly"
	purchaseToken  = "opaque-token-up-to-150-characters"
	accountToken   = "7e3fb20b-4cdb-47cc-936d-99d65f608138"
	rtdnAudience   = "https://notes.example.com/api/billing/webhooks/play"
	rtdnAccount    = "rtdn-push@futo-notes.iam.gserviceaccount.com"
	apiAccessToken = "ya29.test-access-token"
)

// fakeGoogle is Google as the Play provider sees it: the OAuth token
// endpoint, the Play Developer API and the OIDC signing keys.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server
	// signingKey signs push tokens; its public half is served as kid "k1".
	signingKey *rsa.PrivateKey

	mu            sync.Mutex
	subscriptions map[string]fakeAnswer
	calls         []string
	jwksFetches   int
	tokenFetches  int
	ackStatus     int
}

type fakeAnswer struct {
	code int
	body any
}

// testKey is one RSA key shared by every fake: generating one per test is
// slow, and nothing depends on keys being distinct across tests.
var testKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGoogle{t: t, signingKey: key, subscriptions: map[string]fakeAnswer{}, ackStatus: http.StatusNoContent}
	g.srv = httptest.NewServer(g)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.URL.Path {
	case "/token":
		g.tokenFetches++
		if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.PostForm.Get("assertion") == "" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": apiAccessToken, "token_type": "Bearer", "expires_in": 3600})
		return
	case "/certs":
		g.jwksFetches++
		pub := g.signingKey.PublicKey
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+apiAccessToken {
		g.t.Errorf("%s %s without the service account's token", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/androidpublisher/v3/applications/"+packageName+"/purchases/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	g.calls = append(g.calls, r.Method+" "+rest)
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "subscriptionsv2/tokens/"):
		answer, found := g.subscriptions[strings.TrimPrefix(rest, "subscriptionsv2/tokens/")]
		if !found {
			answer = fakeAnswer{code: http.StatusNotFound, body: map[string]any{"error": map[string]any{"code": 404, "message": "The purchase token was not found."}}}
		}
		writeJSON(w, answer.code, answer.body)
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "subscriptions/"):
		w.WriteHeader(g.ackStatus)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (g *fakeGoogle) set(token string, answer fakeAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.subscriptions[token] = answer
}

func (g *fakeGoogle) apiCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// serviceAccountJSON is a service account key file whose token_uri is the
// fake.
func (g *fakeGoogle) serviceAccountJSON(t *testing.T) []byte {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "futo-notes",
		"private_key_id": "sa-key-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "play-api@futo-notes.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      g.srv.URL + "/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (g *fakeGoogle) provider(t *testing.T) *play.Provider {
	t.Helper()
	sa, err := play.ParseServiceAccount(g.serviceAccountJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	return play.New(play.Config{
		PackageName:        packageName,
		ServiceAccount:     sa,
		ProductIDs:         []string{"notes_yearly", productID},
		RTDNAudience:       rtdnAudience,
		RTDNServiceAccount: rtdnAccount,
		APIURL:             g.srv.URL,
		CertsURL:           g.srv.URL + "/certs",
	})
}

// pushToken is the OIDC token Pub/Sub attaches to a push, signed by key.
func pushToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	now := time.Now()
	base := jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            rtdnAudience,
		"email":          rtdnAccount,
		"email_verified": true,
		"sub":            "112233445566778899",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// push is a Pub/Sub push request carrying notification.
func push(t *testing.T, bearer string, notification map[string]any) (http.Header, []byte) {
	t.Helper()
	data, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":        base64.StdEncoding.EncodeToString(data),
			"messageId":   "2070443601311540",
			"message_id":  "2070443601311540",
			"publishTime": time.Now().Format(time.RFC3339),
		},
		"subscription": "projects/futo-notes/subscriptions/play-rtdn-push",
	})
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Content-Type": {"application/json"}}
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	return header, body
}

func subscriptionNotification(kind int, token string) map[string]any {
	return map[string]any{
		"version":         "1.0",
		"packageName":     packageName,
		"eventTimeMillis": "1700000000000",
		"subscriptionNotification": map[string]any{
			"version":          "1.0",
			"notificationType": kind,
			"purchaseToken":    token,
			"subscriptionId":   productID,
		},
	}
}

// purchase is a SubscriptionPurchaseV2 with sensible defaults.
func purchase(state string, expiry time.Time, kv ...any) map[string]any {
	p := map[string]any{
		"kind":                 "androidpublisher#subscriptionPurchaseV2",
		"regionCode":           "US",
		"startTime":            expiry.Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano),
		"subscriptionState":    state,
		"latestOrderId":        "GPA.3333-4444-5555-66666",
		"acknowledgementState": "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED",
		"externalAccountIdentifiers": map[string]any{
			"obfuscatedExternalAccountId": accountToken,
		},
		"lineItems": []any{map[string]any{
			"productId":        productID,
			"expiryTime":       expiry.Format(time.RFC3339Nano),
			"autoRenewingPlan": map[string]any{"autoRenewEnabled": true},
		}},
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(p, kv[i].(string))
			continue
		}
		p[kv[i].(string)] = kv[i+1]
	}
	return p
}
