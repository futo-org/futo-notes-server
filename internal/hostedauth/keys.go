package hostedauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
)

const (
	pemPrivateType = "PRIVATE KEY"
	pemPublicType  = "PUBLIC KEY"
	pemKidHeader   = "kid"
)

var kidPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// SigningKey is an Ed25519 private key and the kid stamped into the header of
// every token it signs.
type SigningKey struct {
	kid string
	key ed25519.PrivateKey
}

// KeySet holds the Ed25519 public keys a verifier accepts, by kid. Listing an
// old and a new kid side by side is how keys rotate without downtime.
type KeySet struct {
	keys map[string]ed25519.PublicKey
}

// GenerateSigningKey returns a fresh Ed25519 signing key with the given kid.
func GenerateSigningKey(kid string) (SigningKey, error) {
	if !kidPattern.MatchString(kid) {
		return SigningKey{}, fmt.Errorf("invalid kid %q: want 1-64 of [A-Za-z0-9._-]", kid)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKey{}, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return SigningKey{kid: kid, key: key}, nil
}

// ParseSigningKey parses exactly one PKCS#8 "PRIVATE KEY" PEM block holding an
// Ed25519 key and a "kid" PEM header. Text outside the block is ignored.
func ParseSigningKey(data []byte) (SigningKey, error) {
	blocks, err := decodePEM(data)
	if err != nil {
		return SigningKey{}, err
	}
	if len(blocks) != 1 {
		return SigningKey{}, fmt.Errorf("want exactly one PEM block, got %d", len(blocks))
	}
	block := blocks[0]
	if block.Type != pemPrivateType {
		return SigningKey{}, fmt.Errorf("want a %q PEM block, got %q", pemPrivateType, block.Type)
	}
	kid, err := blockKid(block)
	if err != nil {
		return SigningKey{}, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return SigningKey{}, fmt.Errorf("kid %q: parse private key: %w", kid, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return SigningKey{}, fmt.Errorf("kid %q: want an Ed25519 key, got %T", kid, parsed)
	}
	return SigningKey{kid: kid, key: key}, nil
}

// ParseKeySet parses one or more PKIX "PUBLIC KEY" PEM blocks, each holding an
// Ed25519 key and a unique "kid" PEM header. Text between blocks is ignored.
func ParseKeySet(data []byte) (KeySet, error) {
	blocks, err := decodePEM(data)
	if err != nil {
		return KeySet{}, err
	}
	if len(blocks) == 0 {
		return KeySet{}, errors.New("no PEM blocks found")
	}
	keys := make(map[string]ed25519.PublicKey, len(blocks))
	for _, block := range blocks {
		if block.Type != pemPublicType {
			return KeySet{}, fmt.Errorf("want %q PEM blocks, got %q", pemPublicType, block.Type)
		}
		kid, err := blockKid(block)
		if err != nil {
			return KeySet{}, err
		}
		if _, dup := keys[kid]; dup {
			return KeySet{}, fmt.Errorf("duplicate kid %q", kid)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return KeySet{}, fmt.Errorf("kid %q: parse public key: %w", kid, err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return KeySet{}, fmt.Errorf("kid %q: want an Ed25519 key, got %T", kid, parsed)
		}
		keys[kid] = key
	}
	return KeySet{keys: keys}, nil
}

// KeyID returns the kid this key signs with.
func (k SigningKey) KeyID() string { return k.kid }

// MarshalPEM encodes the key in the format ParseSigningKey reads.
func (k SigningKey) MarshalPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemPrivateType, Headers: map[string]string{pemKidHeader: k.kid}, Bytes: der}), nil
}

// MarshalPublicPEM encodes the public half in the format ParseKeySet reads.
func (k SigningKey) MarshalPublicPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(k.key.Public())
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemPublicType, Headers: map[string]string{pemKidHeader: k.kid}, Bytes: der}), nil
}

// KeyIDs returns the kids in the set, in no particular order.
func (s KeySet) KeyIDs() []string {
	kids := make([]string, 0, len(s.keys))
	for kid := range s.keys {
		kids = append(kids, kid)
	}
	return kids
}

func decodePEM(data []byte) ([]*pem.Block, error) {
	var blocks []*pem.Block
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
	}
	// pem.Decode silently stops at a malformed block, which would drop every
	// key after it from a rotation set.
	if bytes.Contains(rest, []byte("-----BEGIN")) {
		return nil, errors.New("malformed PEM block")
	}
	return blocks, nil
}

func blockKid(block *pem.Block) (string, error) {
	kid, ok := block.Headers[pemKidHeader]
	if !ok {
		return "", fmt.Errorf("%s block has no %q PEM header", block.Type, pemKidHeader)
	}
	if !kidPattern.MatchString(kid) {
		return "", fmt.Errorf("invalid kid %q: want 1-64 of [A-Za-z0-9._-]", kid)
	}
	if len(block.Headers) != 1 {
		return "", fmt.Errorf("kid %q: unexpected PEM headers besides %q", kid, pemKidHeader)
	}
	return kid, nil
}
