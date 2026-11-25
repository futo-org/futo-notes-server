package appstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// appleRootG3 is Apple Root CA - G3 (DER), from https://www.apple.com/certificateauthority/.
// SHA-256 fingerprint 63343ABFB89A6A03EBB57E9B3F5FA7BE7C4F5C756F3017B3A8C488C3653E9179.
//
//go:embed AppleRootCA-G3.cer
var appleRootG3 []byte

// AppleRoots returns the trust anchor App Store signatures chain to: Apple
// Root CA - G3.
func AppleRoots() *x509.CertPool {
	root, err := x509.ParseCertificate(appleRootG3)
	if err != nil {
		panic("appstore: embedded Apple root: " + err.Error())
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return pool
}

// The marker extensions Apple's App Store Server Library requires: the leaf
// must be an App Store receipt-signing certificate and the intermediate the
// Apple Worldwide Developer Relations CA - G6, so no other certificate Apple
// issued under the same root can sign a transaction.
var (
	oidReceiptSigning   = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidWWDRIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}
)

// errSignature is any failure to verify a signed App Store payload.
var errSignature = errors.New("app store signature")

// verifier checks the JWS payloads the App Store signs: the x5c chain must
// lead to one of roots, and the leaf must have signed the payload with ES256.
type verifier struct {
	roots *x509.CertPool
}

// verify checks the compact JWS s and decodes its payload into v, returning
// the payload as signed. Certificates are checked as of the payload's
// signedDate, as Apple's library does without online checks, so a payload
// stays verifiable after the leaf that signed it expires.
func (vr verifier) verify(s string, v any) (json.RawMessage, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a compact JWS", errSignature)
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header: %w", errSignature, err)
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("%w: header: %w", errSignature, err)
	}
	if header.Alg != "ES256" {
		return nil, fmt.Errorf("%w: alg %q, want ES256", errSignature, header.Alg)
	}
	if len(header.X5C) != 3 {
		return nil, fmt.Errorf("%w: x5c has %d certificates, want 3", errSignature, len(header.X5C))
	}
	certs := make([]*x509.Certificate, len(header.X5C))
	for i, b64 := range header.X5C {
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("%w: x5c[%d]: %w", errSignature, i, err)
		}
		if certs[i], err = x509.ParseCertificate(der); err != nil {
			return nil, fmt.Errorf("%w: x5c[%d]: %w", errSignature, i, err)
		}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload: %w", errSignature, err)
	}
	var dated struct {
		SignedDate int64 `json:"signedDate"`
	}
	if err := json.Unmarshal(payload, &dated); err != nil || dated.SignedDate <= 0 {
		return nil, fmt.Errorf("%w: payload has no signedDate", errSignature)
	}
	leaf, intermediate := certs[0], certs[1]
	intermediates := x509.NewCertPool()
	intermediates.AddCert(intermediate)
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         vr.roots,
		Intermediates: intermediates,
		CurrentTime:   time.UnixMilli(dated.SignedDate),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: chain: %w", errSignature, err)
	}
	if !slices.ContainsFunc(chains, func(c []*x509.Certificate) bool { return len(c) == 3 && c[1].Equal(intermediate) }) {
		return nil, fmt.Errorf("%w: chain does not run leaf, intermediate, root", errSignature)
	}
	if !hasExtension(leaf, oidReceiptSigning) || !hasExtension(intermediate, oidWWDRIntermediate) {
		return nil, fmt.Errorf("%w: certificates lack Apple's App Store markers", errSignature)
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: leaf key is not P-256", errSignature)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, fmt.Errorf("%w: malformed signature", errSignature)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, fmt.Errorf("%w: bad signature", errSignature)
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return nil, fmt.Errorf("%w: payload: %w", errSignature, err)
	}
	return payload, nil
}

func hasExtension(c *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	return slices.ContainsFunc(c.Extensions, func(e pkix.Extension) bool { return e.Id.Equal(oid) })
}
