package hostedauth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testUser   = "a0000000-0000-4000-8000-000000000001"
	otherUser  = "b0000000-0000-4000-8000-000000000002"
	testRegion = "us-east"
	testBlob   = "0190f5c8-0000-7000-8000-000000000001"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func fixedClock(t time.Time) Option { return WithClock(func() time.Time { return t }) }

func mustKey(t *testing.T, kid string) SigningKey {
	t.Helper()
	k, err := GenerateSigningKey(kid)
	if err != nil {
		t.Fatalf("generate %s: %v", kid, err)
	}
	return k
}

func mustKeySet(t *testing.T, keys ...SigningKey) KeySet {
	t.Helper()
	var bundle []byte
	for _, k := range keys {
		pub, err := k.MarshalPublicPEM()
		if err != nil {
			t.Fatalf("marshal public: %v", err)
		}
		bundle = append(bundle, pub...)
	}
	set, err := ParseKeySet(bundle)
	if err != nil {
		t.Fatalf("parse key set: %v", err)
	}
	return set
}

func mustVerifier(t *testing.T, keys KeySet, region string, now time.Time) *Verifier {
	t.Helper()
	v, err := NewVerifier(keys, region, fixedClock(now))
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

// unverifiedClaims reads the registered claims of a token the test already
// verified.
func unverifiedClaims(t *testing.T, token string) jwt.RegisteredClaims {
	t.Helper()
	var claims jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	return claims
}

func forge(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("forge: %v", err)
	}
	return signed
}

func accessClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   APIIssuer,
		"aud":   []string{testRegion},
		"sub":   testUser,
		"scope": ScopeBlobs,
		"iat":   testNow.Unix(),
		"exp":   testNow.Add(AccessTokenTTL).Unix(),
	}
}

func TestMintAccessVerifies(t *testing.T) {
	key := mustKey(t, "api-1")
	token, exp, err := NewMinter(key, fixedClock(testNow)).MintAccess(testRegion, testUser)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !exp.Equal(testNow.Add(AccessTokenTTL)) {
		t.Errorf("expiry %v, want %v", exp, testNow.Add(AccessTokenTTL))
	}
	got, err := mustVerifier(t, mustKeySet(t, key), testRegion, testNow.Add(time.Hour)).VerifyAccess(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != (AccessToken{UserID: testUser}) {
		t.Errorf("got %+v, want user %s", got, testUser)
	}
	if claims := unverifiedClaims(t, token); !claims.ExpiresAt.Equal(testNow.Add(AccessTokenTTL)) {
		t.Errorf("exp %v, want %v", claims.ExpiresAt, testNow.Add(AccessTokenTTL))
	}
}

func TestMintAdminVerifies(t *testing.T) {
	key := mustKey(t, "api-1")
	token, err := NewMinter(key, fixedClock(testNow)).MintAdmin(testRegion)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	v := mustVerifier(t, mustKeySet(t, key), testRegion, testNow.Add(4*time.Minute))
	if err := v.VerifyAdmin(token); err != nil {
		t.Fatalf("verify: %v", err)
	}
	expired := mustVerifier(t, mustKeySet(t, key), testRegion, testNow.Add(AdminTokenTTL+clockSkew+time.Second))
	if err := expired.VerifyAdmin(token); !errors.Is(err, jwt.ErrTokenExpired) {
		t.Errorf("admin token past 5 min: got %v, want expired", err)
	}
}

func TestMintRejectsBadInput(t *testing.T) {
	m := NewMinter(mustKey(t, "api-1"))
	cases := map[string]func() error{
		"user not a uuid":      func() error { _, _, err := m.MintAccess(testRegion, "alice"); return err },
		"user uppercase uuid":  func() error { _, _, err := m.MintAccess(testRegion, strings.ToUpper(testUser)); return err },
		"empty region":         func() error { _, _, err := m.MintAccess("", testUser); return err },
		"region with slash":    func() error { _, _, err := m.MintAccess("us/east", testUser); return err },
		"region is api issuer": func() error { _, err := m.MintAdmin(APIIssuer); return err },
	}
	for name, mint := range cases {
		t.Run(name, func(t *testing.T) {
			if mint() == nil {
				t.Fatal("minted, want error")
			}
		})
	}
}

func TestVerifyAccessRejects(t *testing.T) {
	key := mustKey(t, "api-1")
	other := mustKey(t, "api-1")
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubPEM, _ := key.MarshalPublicPEM()
	v := mustVerifier(t, mustKeySet(t, key), testRegion, testNow)
	signed := func(mutate func(jwt.MapClaims)) string {
		c := accessClaims()
		mutate(c)
		return forge(t, jwt.SigningMethodEdDSA, key.key, key.kid, c)
	}
	valid := signed(func(jwt.MapClaims) {})
	if _, err := v.VerifyAccess(valid); err != nil {
		t.Fatalf("baseline token rejected: %v", err)
	}

	cases := map[string]string{
		"alg none":                            forge(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, key.kid, accessClaims()),
		"HS256 keyed with the raw public key": forge(t, jwt.SigningMethodHS256, []byte(key.key.Public().(ed25519.PublicKey)), key.kid, accessClaims()),
		"HS256 keyed with the public PEM":     forge(t, jwt.SigningMethodHS256, pubPEM, key.kid, accessClaims()),
		"ES256":                               forge(t, jwt.SigningMethodES256, ecKey, key.kid, accessClaims()),
		"signed by another key with same kid": forge(t, jwt.SigningMethodEdDSA, other.key, key.kid, accessClaims()),
		"unknown kid":                         forge(t, jwt.SigningMethodEdDSA, key.key, "api-0", accessClaims()),
		"missing kid":                         forge(t, jwt.SigningMethodEdDSA, key.key, "", accessClaims()),
		"tampered payload":                    tamper(t, valid, func(c map[string]any) { c["sub"] = otherUser }),
		"stripped signature":                  strings.Join(strings.Split(valid, ".")[:2], ".") + ".",
		"expired":                             signed(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-time.Hour).Unix() }),
		"missing exp":                         signed(func(c jwt.MapClaims) { delete(c, "exp") }),
		"issued in the future":                signed(func(c jwt.MapClaims) { c["iat"] = testNow.Add(time.Hour).Unix() }),
		"wrong audience":                      signed(func(c jwt.MapClaims) { c["aud"] = []string{"eu-central"} }),
		"missing audience":                    signed(func(c jwt.MapClaims) { delete(c, "aud") }),
		"several audiences":                   signed(func(c jwt.MapClaims) { c["aud"] = []string{testRegion, "eu-central"} }),
		"wrong issuer":                        signed(func(c jwt.MapClaims) { c["iss"] = "eu-central" }),
		"missing issuer":                      signed(func(c jwt.MapClaims) { delete(c, "iss") }),
		"admin scope":                         signed(func(c jwt.MapClaims) { c["scope"] = ScopeAdmin }),
		"missing scope":                       signed(func(c jwt.MapClaims) { delete(c, "scope") }),
		"scope list":                          signed(func(c jwt.MapClaims) { c["scope"] = "blobs admin" }),
		"sub not a uuid":                      signed(func(c jwt.MapClaims) { c["sub"] = "alice" }),
		"sub non-canonical uuid":              signed(func(c jwt.MapClaims) { c["sub"] = "{" + testUser + "}" }),
		"missing sub":                         signed(func(c jwt.MapClaims) { delete(c, "sub") }),
		"garbage":                             "not.a.jwt",
		"empty":                               "",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.VerifyAccess(token); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("got %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerifyAdminRejects(t *testing.T) {
	key := mustKey(t, "api-1")
	m := NewMinter(key, fixedClock(testNow))
	v := mustVerifier(t, mustKeySet(t, key), testRegion, testNow)
	access, _, err := m.MintAccess(testRegion, testUser)
	if err != nil {
		t.Fatalf("mint access: %v", err)
	}
	adminClaims := func(mutate func(jwt.MapClaims)) string {
		c := jwt.MapClaims{"iss": APIIssuer, "aud": []string{testRegion}, "scope": ScopeAdmin, "iat": testNow.Unix(), "exp": testNow.Add(AdminTokenTTL).Unix()}
		mutate(c)
		return forge(t, jwt.SigningMethodEdDSA, key.key, key.kid, c)
	}
	otherRegion, err := m.MintAdmin("eu-central")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	cases := map[string]string{
		"access token":       access,
		"other region":       otherRegion,
		"admin with a sub":   adminClaims(func(c jwt.MapClaims) { c["sub"] = testUser }),
		"blobs scope no sub": adminClaims(func(c jwt.MapClaims) { c["scope"] = ScopeBlobs }),
		"expired":            adminClaims(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-time.Hour).Unix() }),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if err := v.VerifyAdmin(token); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("got %v, want ErrInvalidToken", err)
			}
		})
	}
	admin, err := m.MintAdmin(testRegion)
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	if _, err := v.VerifyAccess(admin); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("admin token accepted as access token: %v", err)
	}
}

func TestRotation(t *testing.T) {
	oldKey := mustKey(t, "api-2026-10")
	newKey := mustKey(t, "api-2026-11")
	oldToken, _, err := NewMinter(oldKey, fixedClock(testNow)).MintAccess(testRegion, testUser)
	if err != nil {
		t.Fatalf("mint old: %v", err)
	}
	newToken, _, err := NewMinter(newKey, fixedClock(testNow)).MintAccess(testRegion, testUser)
	if err != nil {
		t.Fatalf("mint new: %v", err)
	}

	both := mustVerifier(t, mustKeySet(t, oldKey, newKey), testRegion, testNow)
	for name, token := range map[string]string{"old": oldToken, "new": newToken} {
		if _, err := both.VerifyAccess(token); err != nil {
			t.Errorf("%s kid rejected while both are listed: %v", name, err)
		}
	}

	newOnly := mustVerifier(t, mustKeySet(t, newKey), testRegion, testNow)
	if _, err := newOnly.VerifyAccess(newToken); err != nil {
		t.Errorf("new kid rejected: %v", err)
	}
	if _, err := newOnly.VerifyAccess(oldToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("old kid accepted after removal: %v", err)
	}
}

func TestReceiptRoundTrip(t *testing.T) {
	key := mustKey(t, "us-east-1")
	signer, err := NewReceiptSigner(key, testRegion, fixedClock(testNow))
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	blobKey := testUser + "/" + testBlob
	token, err := signer.Sign(testUser, blobKey, 1234)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	rv, err := NewReceiptVerifier(map[string]KeySet{testRegion: mustKeySet(t, key)}, fixedClock(testNow.Add(23*time.Hour)))
	if err != nil {
		t.Fatalf("new receipt verifier: %v", err)
	}
	got, err := rv.Verify(token, testRegion)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := Receipt{UserID: testUser, Key: blobKey, Size: 1234}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if claims := unverifiedClaims(t, token); claims.Issuer != testRegion || !claims.ExpiresAt.Equal(testNow.Add(ReceiptTTL)) {
		t.Errorf("iss %q exp %v, want %q %v", claims.Issuer, claims.ExpiresAt, testRegion, testNow.Add(ReceiptTTL))
	}
	zero, err := signer.Sign(testUser, blobKey, 0)
	if err != nil {
		t.Fatalf("sign empty blob: %v", err)
	}
	if r, err := rv.Verify(zero, testRegion); err != nil || r.Size != 0 {
		t.Errorf("empty blob receipt: %+v, %v", r, err)
	}
}

func TestReceiptSignRejectsBadInput(t *testing.T) {
	signer, err := NewReceiptSigner(mustKey(t, "us-east-1"), testRegion)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	cases := map[string]struct {
		user, key string
		size      int64
	}{
		"user not a uuid":      {"alice", "alice/" + testBlob, 1},
		"key of another user":  {testUser, otherUser + "/" + testBlob, 1},
		"key not uuidv7":       {testUser, testUser + "/" + otherUser, 1},
		"key with extra path":  {testUser, testUser + "/" + testBlob + "/x", 1},
		"negative size":        {testUser, testUser + "/" + testBlob, -1},
		"key without a prefix": {testUser, testBlob, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := signer.Sign(tc.user, tc.key, tc.size); err == nil {
				t.Fatal("signed, want error")
			}
		})
	}
}

func TestReceiptVerifyRejects(t *testing.T) {
	usKey := mustKey(t, "us-east-1")
	euKey := mustKey(t, "eu-central-1")
	apiKey := mustKey(t, "api-1")
	rv, err := NewReceiptVerifier(map[string]KeySet{
		testRegion:   mustKeySet(t, usKey),
		"eu-central": mustKeySet(t, euKey),
	}, fixedClock(testNow))
	if err != nil {
		t.Fatalf("new receipt verifier: %v", err)
	}
	blobKey := testUser + "/" + testBlob
	claims := func(mutate func(jwt.MapClaims)) jwt.MapClaims {
		c := jwt.MapClaims{
			"iss": testRegion, "aud": []string{APIIssuer}, "sub": testUser,
			"key": blobKey, "size": 10,
			"iat": testNow.Unix(), "exp": testNow.Add(ReceiptTTL).Unix(),
		}
		mutate(c)
		return c
	}
	signed := func(mutate func(jwt.MapClaims)) string {
		return forge(t, jwt.SigningMethodEdDSA, usKey.key, usKey.kid, claims(mutate))
	}
	euSigner, err := NewReceiptSigner(euKey, "eu-central", fixedClock(testNow))
	if err != nil {
		t.Fatalf("new eu signer: %v", err)
	}
	euReceipt, err := euSigner.Sign(testUser, blobKey, 10)
	if err != nil {
		t.Fatalf("sign eu: %v", err)
	}
	access, _, err := NewMinter(usKey, fixedClock(testNow)).MintAccess(testRegion, testUser)
	if err != nil {
		t.Fatalf("mint access: %v", err)
	}

	cases := map[string]string{
		"receipt from another region":         euReceipt,
		"other region's key claiming us-east": forge(t, jwt.SigningMethodEdDSA, euKey.key, euKey.kid, claims(func(jwt.MapClaims) {})),
		"other region's key under our kid":    forge(t, jwt.SigningMethodEdDSA, euKey.key, usKey.kid, claims(func(jwt.MapClaims) {})),
		"API key":                             forge(t, jwt.SigningMethodEdDSA, apiKey.key, apiKey.kid, claims(func(jwt.MapClaims) {})),
		"access token from region key":        access,
		"alg none":                            forge(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, usKey.kid, claims(func(jwt.MapClaims) {})),
		"tampered size":                       tamper(t, signed(func(jwt.MapClaims) {}), func(c map[string]any) { c["size"] = 1 }),
		"expired":                             signed(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-time.Minute - clockSkew).Unix() }),
		"wrong audience":                      signed(func(c jwt.MapClaims) { c["aud"] = []string{testRegion} }),
		"wrong issuer":                        signed(func(c jwt.MapClaims) { c["iss"] = "eu-central" }),
		"key of another user":                 signed(func(c jwt.MapClaims) { c["key"] = otherUser + "/" + testBlob }),
		"missing key":                         signed(func(c jwt.MapClaims) { delete(c, "key") }),
		"missing size":                        signed(func(c jwt.MapClaims) { delete(c, "size") }),
		"negative size":                       signed(func(c jwt.MapClaims) { c["size"] = -1 }),
		"fractional size":                     signed(func(c jwt.MapClaims) { c["size"] = 1.5 }),
		"string size":                         signed(func(c jwt.MapClaims) { c["size"] = "10" }),
		"sub not a uuid":                      signed(func(c jwt.MapClaims) { c["sub"] = "alice" }),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := rv.Verify(token, testRegion); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("got %v, want ErrInvalidToken", err)
			}
		})
	}
	if _, err := rv.Verify(euReceipt, "ap-southeast"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("unknown region: got %v, want ErrInvalidToken", err)
	}
}

func TestReceiptIsNotAnAccessToken(t *testing.T) {
	key := mustKey(t, "shared-1")
	signer, err := NewReceiptSigner(key, testRegion, fixedClock(testNow))
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	receipt, err := signer.Sign(testUser, testUser+"/"+testBlob, 1)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	v := mustVerifier(t, mustKeySet(t, key), testRegion, testNow)
	if _, err := v.VerifyAccess(receipt); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("receipt accepted as access token: %v", err)
	}
	if err := v.VerifyAdmin(receipt); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("receipt accepted as admin token: %v", err)
	}
}

func TestConstructorsRejectBadConfig(t *testing.T) {
	key := mustKey(t, "k1")
	if _, err := NewVerifier(KeySet{}, testRegion); err == nil {
		t.Error("NewVerifier accepted an empty key set")
	}
	if _, err := NewVerifier(mustKeySet(t, key), "US EAST"); err == nil {
		t.Error("NewVerifier accepted an invalid region")
	}
	if _, err := NewReceiptSigner(key, APIIssuer); err == nil {
		t.Error("NewReceiptSigner accepted the API issuer as a region")
	}
	if _, err := NewReceiptVerifier(nil); err == nil {
		t.Error("NewReceiptVerifier accepted no regions")
	}
	if _, err := NewReceiptVerifier(map[string]KeySet{testRegion: {}}); err == nil {
		t.Error("NewReceiptVerifier accepted an empty key set")
	}
}

func tamper(t *testing.T, token string, mutate func(map[string]any)) string {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	mutate(c)
	raw, err = json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(raw)
	return strings.Join(parts, ".")
}
