package appstore_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"futo-notes-server/internal/hosted/billing/appstore"
)

const (
	bundleID   = "tech.futo.notes"
	appAppleID = int64(1234567890)
	productID  = "notes.monthly"
	issuerID   = "57246542-96fe-1a63-e053-0824d011072a"
	keyID      = "2X9R4HXF34"
	accountTok = "7e3fb20b-4cdb-47cc-936d-99d65f608138"
)

// chainOpts shapes a generated signing chain.
type chainOpts struct {
	leafNotAfter         time.Time
	noLeafMarker         bool
	noIntermediateMarker bool
}

// signer is a generated stand-in for Apple's PKI: a root, an intermediate
// and a leaf carrying Apple's marker extensions.
type signer struct {
	root    *x509.Certificate
	leafKey *ecdsa.PrivateKey
	x5c     []string
}

func newSigner(t *testing.T, opts chainOpts) signer {
	t.Helper()
	now := time.Now()
	if opts.leafNotAfter.IsZero() {
		opts.leafNotAfter = now.Add(365 * 24 * time.Hour)
	}
	rootKey := genKey(t)
	root := issue(t, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Root CA - G3"},
		NotBefore:             now.Add(-10 * 365 * 24 * time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, nil, rootKey, rootKey)
	interKey := genKey(t)
	interTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "Test WWDR CA - G6"},
		NotBefore:             now.Add(-5 * 365 * 24 * time.Hour),
		NotAfter:              now.Add(5 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	if !opts.noIntermediateMarker {
		interTmpl.ExtraExtensions = []pkix.Extension{marker(1, 2, 840, 113635, 100, 6, 2, 1)}
	}
	inter := issue(t, interTmpl, root, interKey, rootKey)
	leafKey := genKey(t)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "Test StoreKit Signing"},
		NotBefore:    now.Add(-2 * 365 * 24 * time.Hour),
		NotAfter:     opts.leafNotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if !opts.noLeafMarker {
		leafTmpl.ExtraExtensions = []pkix.Extension{marker(1, 2, 840, 113635, 100, 6, 11, 1)}
	}
	leaf := issue(t, leafTmpl, inter, leafKey, interKey)
	return signer{
		root:    root,
		leafKey: leafKey,
		x5c: []string{
			base64.StdEncoding.EncodeToString(leaf.Raw),
			base64.StdEncoding.EncodeToString(inter.Raw),
			base64.StdEncoding.EncodeToString(root.Raw),
		},
	}
}

func (s signer) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.root)
	return pool
}

// sign returns payload as an App Store JWS.
func (s signer) sign(t *testing.T, payload any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "ES256", "x5c": s.x5c})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	r, sv, err := ecdsa.Sign(rand.Reader, s.leafKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func issue(t *testing.T, tmpl, parent *x509.Certificate, key, parentKey *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func marker(oid ...int) pkix.Extension {
	return pkix.Extension{Id: asn1.ObjectIdentifier(oid), Value: []byte{0x05, 0x00}}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// tx is a transaction payload with sensible defaults.
func tx(env string, signedAt time.Time) map[string]any {
	return map[string]any{
		"transactionId":         "2000000000000002",
		"originalTransactionId": "2000000000000001",
		"bundleId":              bundleID,
		"productId":             productID,
		"appAccountToken":       accountTok,
		"environment":           env,
		"inAppOwnershipType":    "PURCHASED",
		"type":                  "Auto-Renewable Subscription",
		"purchaseDate":          ms(signedAt.Add(-24 * time.Hour)),
		"expiresDate":           ms(signedAt.Add(29 * 24 * time.Hour)),
		"signedDate":            ms(signedAt),
	}
}

func renewalInfo(autoRenew int, signedAt time.Time) map[string]any {
	return map[string]any{
		"originalTransactionId": "2000000000000001",
		"productId":             productID,
		"autoRenewStatus":       autoRenew,
		"signedDate":            ms(signedAt),
	}
}

func with(m map[string]any, kv ...any) map[string]any {
	out := maps.Clone(m)
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(out, kv[i].(string))
			continue
		}
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// fakeAPI is an App Store Server API environment: Get All Subscription
// Statuses answers from statuses, keyed by transaction id.
type fakeAPI struct {
	t        *testing.T
	pub      *ecdsa.PublicKey
	mu       sync.Mutex
	statuses map[string]fakeAnswer
	calls    []string
}

type fakeAnswer struct {
	code int
	body any
}

func newFakeAPI(t *testing.T, pub *ecdsa.PublicKey) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, pub: pub, statuses: map[string]fakeAnswer{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/inApps/v1/subscriptions/")
	if r.Method != http.MethodGet || !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, id)
	answer, found := f.statuses[id]
	f.mu.Unlock()
	if err := f.checkAuth(r); err != nil {
		f.t.Errorf("auth: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !found {
		answer = fakeAnswer{code: http.StatusNotFound, body: map[string]any{"errorCode": 4040010, "errorMessage": "Transaction id not found."}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.code)
	_ = json.NewEncoder(w).Encode(answer.body)
}

func (f *fakeAPI) checkAuth(r *http.Request) error {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token, err := jwt.Parse(raw, func(tok *jwt.Token) (any, error) {
		if tok.Header["kid"] != keyID {
			return nil, jwt.ErrTokenUnverifiable
		}
		return f.pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("appstoreconnect-v1"), jwt.WithIssuer(issuerID), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return err
	}
	claims := token.Claims.(jwt.MapClaims)
	if claims["bid"] != bundleID {
		return jwt.ErrTokenInvalidClaims
	}
	exp, _ := claims.GetExpirationTime()
	if time.Until(exp.Time) > time.Hour {
		return jwt.ErrTokenInvalidClaims
	}
	return nil
}

func (f *fakeAPI) set(id string, answer fakeAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = answer
}

func (f *fakeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// statusBody is a StatusResponse with one subscription.
func statusBody(env string, status int, signedTx, signedRenewal string) map[string]any {
	return map[string]any{
		"environment": env,
		"bundleId":    bundleID,
		"appAppleId":  appAppleID,
		"data": []any{map[string]any{
			"subscriptionGroupIdentifier": "21000000",
			"lastTransactions": []any{map[string]any{
				"originalTransactionId": "2000000000000001",
				"status":                status,
				"signedTransactionInfo": signedTx,
				"signedRenewalInfo":     signedRenewal,
			}},
		}},
	}
}

// fixture is a provider wired to fake production and sandbox APIs, trusting
// a generated root.
type fixture struct {
	signer     signer
	provider   *appstore.Provider
	production *fakeAPI
	sandbox    *fakeAPI
}

func newFixture(t *testing.T, acceptSandbox bool) fixture {
	t.Helper()
	s := newSigner(t, chainOpts{})
	apiKey := genKey(t)
	prod, prodSrv := newFakeAPI(t, &apiKey.PublicKey)
	sand, sandSrv := newFakeAPI(t, &apiKey.PublicKey)
	p := appstore.New(appstore.Config{
		BundleID:      bundleID,
		AppAppleID:    appAppleID,
		IssuerID:      issuerID,
		KeyID:         keyID,
		PrivateKey:    apiKey,
		ProductIDs:    []string{"notes.yearly", productID},
		AcceptSandbox: acceptSandbox,
		ProductionURL: prodSrv.URL,
		SandboxURL:    sandSrv.URL,
		Roots:         s.roots(),
	})
	return fixture{signer: s, provider: p, production: prod, sandbox: sand}
}
