// Package hostedauth mints and verifies the Ed25519 JWTs that the hosted API
// and the regional storage services exchange.
//
// There are three token kinds:
//
//	kind     signed by   iss         aud         claims                 lifetime
//	access   API         notes-api   region id   sub, scope=blobs       24h
//	admin    API         notes-api   region id   scope=admin (no sub)   5 min
//	receipt  region      region id   notes-api   sub, key, size         24h
//
// Every token carries a "kid" header that must resolve in the verifier's
// KeySet, and only EdDSA is accepted. Scope keeps access and admin tokens
// apart; the swapped iss/aud keeps receipts from passing as either.
//
// # Key format
//
// Keys are PEM with a "kid" PEM header, so one environment variable can hold
// a whole rotation set by concatenating blocks:
//
//	-----BEGIN PUBLIC KEY-----
//	kid: api-2026-11
//
//	MCowBQYDK2VwAyEA...
//	-----END PUBLIC KEY-----
//
// A signing key is a single PKCS#8 "PRIVATE KEY" block (ParseSigningKey); a
// verification set is one or more PKIX "PUBLIC KEY" blocks (ParseKeySet).
// cmd/hosted-keygen prints a fresh pair in this format. Rotation: add the new
// public key to every verifier, then switch the signer, then drop the old
// public key once its tokens have expired.
package hostedauth

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"

	"futo-notes-server/internal/ids"
)

const (
	// APIIssuer is the iss of access and admin tokens and the aud of receipts.
	APIIssuer = "notes-api"
	// ScopeBlobs is the scope of access tokens.
	ScopeBlobs = "blobs"
	// ScopeAdmin is the scope of admin tokens.
	ScopeAdmin = "admin"
	// AccessTokenTTL is how long an access token stays valid.
	AccessTokenTTL = 24 * time.Hour
	// AdminTokenTTL is how long an admin token stays valid.
	AdminTokenTTL = 5 * time.Minute
	// ReceiptTTL is how long a receipt stays valid.
	ReceiptTTL = 24 * time.Hour
)

// clockSkew tolerates drift between the API and storage pods' clocks.
const clockSkew = 30 * time.Second

// ErrInvalidToken wraps every verification failure. The wrapped cause is for
// logs; callers should answer every ErrInvalidToken the same way.
var ErrInvalidToken = errors.New("invalid token")

var regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Option configures a minter, signer or verifier.
type Option func(*options)

type options struct {
	now func() time.Time
}

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

func buildOptions(opts []Option) options {
	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// AccessToken is a verified access token.
type AccessToken struct {
	UserID    string
	ExpiresAt time.Time
}

// Receipt is a verified upload receipt.
type Receipt struct {
	Region    string
	UserID    string
	Key       string
	Size      int64
	ExpiresAt time.Time
}

type scopedClaims struct {
	jwt.RegisteredClaims
	Scope string `json:"scope"`
}

type receiptClaims struct {
	jwt.RegisteredClaims
	Key  string `json:"key"`
	Size *int64 `json:"size"`
}

// Minter signs access and admin tokens. The API holds one.
type Minter struct {
	key SigningKey
	now func() time.Time
}

// NewMinter returns a Minter that signs with key.
func NewMinter(key SigningKey, opts ...Option) *Minter {
	return &Minter{key: key, now: buildOptions(opts).now}
}

// MintAccess signs a 24h access token for userID at region. It returns the
// token and its expiry.
func (m *Minter) MintAccess(region, userID string) (string, time.Time, error) {
	if err := checkRegion(region); err != nil {
		return "", time.Time{}, err
	}
	if !ids.IsUserID(userID) {
		return "", time.Time{}, fmt.Errorf("user id %q is not a UUID", userID)
	}
	return m.mint(region, userID, ScopeBlobs, AccessTokenTTL)
}

// MintAdmin signs a 5-minute admin token for region.
func (m *Minter) MintAdmin(region string) (string, error) {
	if err := checkRegion(region); err != nil {
		return "", err
	}
	token, _, err := m.mint(region, "", ScopeAdmin, AdminTokenTTL)
	return token, err
}

func (m *Minter) mint(region, subject, scope string, ttl time.Duration) (string, time.Time, error) {
	now := m.now()
	exp := now.Add(ttl)
	token, err := sign(m.key, scopedClaims{
		Issuer:    APIIssuer,
		Subject:   subject,
		Audience:  jwt.ClaimStrings{region},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		Scope:     scope,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	// NumericDate truncates to seconds; report what the token actually says.
	return token, exp.Truncate(time.Second).UTC(), nil
}

// Verifier checks access and admin tokens. Each storage region holds one,
// pinned to its own region id.
type Verifier struct {
	keys   KeySet
	region string
	now    func() time.Time
}

// NewVerifier returns a Verifier that accepts tokens signed by any key in keys
// and addressed to region.
func NewVerifier(keys KeySet, region string, opts ...Option) (*Verifier, error) {
	if err := checkRegion(region); err != nil {
		return nil, err
	}
	if len(keys.keys) == 0 {
		return nil, errors.New("empty key set")
	}
	return &Verifier{keys: keys, region: region, now: buildOptions(opts).now}, nil
}

// VerifyAccess checks an access token and returns its user.
func (v *Verifier) VerifyAccess(token string) (AccessToken, error) {
	c, err := v.verify(token, ScopeBlobs)
	if err != nil {
		return AccessToken{}, err
	}
	if !ids.IsUserID(c.Subject) {
		return AccessToken{}, invalid(errors.New("sub is not a UUID"))
	}
	return AccessToken{UserID: c.Subject, ExpiresAt: c.ExpiresAt.UTC()}, nil
}

// VerifyAdmin checks an admin token.
func (v *Verifier) VerifyAdmin(token string) error {
	c, err := v.verify(token, ScopeAdmin)
	if err != nil {
		return err
	}
	if c.Subject != "" {
		return invalid(errors.New("admin token has a sub"))
	}
	return nil
}

func (v *Verifier) verify(token, scope string) (scopedClaims, error) {
	var c scopedClaims
	if err := parse(token, &c, v.keys, APIIssuer, v.region, v.now); err != nil {
		return scopedClaims{}, err
	}
	if c.Scope != scope {
		return scopedClaims{}, invalid(fmt.Errorf("scope %q, want %q", c.Scope, scope))
	}
	return c, nil
}

// ReceiptSigner signs upload receipts. Each storage region holds one.
type ReceiptSigner struct {
	key    SigningKey
	region string
	now    func() time.Time
}

// NewReceiptSigner returns a ReceiptSigner for region that signs with key.
func NewReceiptSigner(key SigningKey, region string, opts ...Option) (*ReceiptSigner, error) {
	if err := checkRegion(region); err != nil {
		return nil, err
	}
	return &ReceiptSigner{key: key, region: region, now: buildOptions(opts).now}, nil
}

// Sign returns a 24h receipt saying userID stored size bytes at key. The key
// must be "<userID>/<uuidv7>".
func (s *ReceiptSigner) Sign(userID, key string, size int64) (string, error) {
	if !ids.IsUserID(userID) {
		return "", fmt.Errorf("user id %q is not a UUID", userID)
	}
	if err := checkBlobKey(userID, key); err != nil {
		return "", err
	}
	if size < 0 {
		return "", fmt.Errorf("negative size %d", size)
	}
	now := s.now()
	return sign(s.key, receiptClaims{
		Issuer:    s.region,
		Subject:   userID,
		Audience:  jwt.ClaimStrings{APIIssuer},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ReceiptTTL)),
		Key:       key,
		Size:      &size,
	})
}

// ReceiptVerifier checks receipts on the API. Each region has its own key set,
// so one region's key cannot vouch for an upload to another.
type ReceiptVerifier struct {
	regions map[string]KeySet
	now     func() time.Time
}

// NewReceiptVerifier returns a ReceiptVerifier trusting each region's keys.
func NewReceiptVerifier(regions map[string]KeySet, opts ...Option) (*ReceiptVerifier, error) {
	if len(regions) == 0 {
		return nil, errors.New("no regions")
	}
	copied := make(map[string]KeySet, len(regions))
	for region, keys := range regions {
		if err := checkRegion(region); err != nil {
			return nil, err
		}
		if len(keys.keys) == 0 {
			return nil, fmt.Errorf("region %q: empty key set", region)
		}
		copied[region] = keys
	}
	return &ReceiptVerifier{regions: copied, now: buildOptions(opts).now}, nil
}

// Verify checks a receipt issued by region, which is the region the caller
// expects (the user's). It does not check that the receipt's sub is the
// caller; compare Receipt.UserID yourself.
func (v *ReceiptVerifier) Verify(token, region string) (Receipt, error) {
	keys, ok := v.regions[region]
	if !ok {
		return Receipt{}, invalid(fmt.Errorf("unknown region %q", region))
	}
	var c receiptClaims
	if err := parse(token, &c, keys, region, APIIssuer, v.now); err != nil {
		return Receipt{}, err
	}
	if !ids.IsUserID(c.Subject) {
		return Receipt{}, invalid(errors.New("sub is not a UUID"))
	}
	if err := checkBlobKey(c.Subject, c.Key); err != nil {
		return Receipt{}, invalid(err)
	}
	if c.Size == nil || *c.Size < 0 {
		return Receipt{}, invalid(errors.New("missing or negative size"))
	}
	return Receipt{Region: region, UserID: c.Subject, Key: c.Key, Size: *c.Size, ExpiresAt: c.ExpiresAt.UTC()}, nil
}

func sign(key SigningKey, claims jwt.Claims) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	t.Header["kid"] = key.kid
	signed, err := t.SignedString(key.key)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

func parse(token string, claims jwt.Claims, keys KeySet, issuer, audience string, now func() time.Time) error {
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("missing kid header")
		}
		key, ok := keys.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(now),
		jwt.WithStrictDecoding(),
	)
	if err != nil {
		return invalid(err)
	}
	aud, err := claims.GetAudience()
	if err != nil {
		return invalid(err)
	}
	// WithAudience accepts any token whose aud list contains ours; a token
	// minted for several regions must not open all of them.
	if len(aud) != 1 {
		return invalid(fmt.Errorf("want exactly one aud, got %d", len(aud)))
	}
	return nil
}

func invalid(cause error) error {
	return fmt.Errorf("%w: %w", ErrInvalidToken, cause)
}

func checkRegion(region string) error {
	if len(region) > 63 || !regionPattern.MatchString(region) {
		return fmt.Errorf("invalid region id %q", region)
	}
	if region == APIIssuer {
		return fmt.Errorf("region id %q collides with the API issuer", region)
	}
	return nil
}

func checkBlobKey(userID, key string) error {
	blob, ok := strings.CutPrefix(key, userID+"/")
	if !ok {
		return fmt.Errorf("key %q is not under %q", key, userID+"/")
	}
	id, err := uuid.Parse(blob)
	if err != nil || id[6]>>4 != 7 || id.String() != blob {
		return fmt.Errorf("key %q does not end in a UUIDv7", key)
	}
	return nil
}
