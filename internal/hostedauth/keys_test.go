package hostedauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSigningKeyPEMRoundTrip(t *testing.T) {
	key := mustKey(t, "api-2026-11")
	data, err := key.MarshalPEM()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseSigningKey(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.KeyID() != "api-2026-11" || !parsed.key.Equal(key.key) {
		t.Errorf("round trip changed the key: kid %q", parsed.KeyID())
	}
}

func TestParseKeySetBundle(t *testing.T) {
	a, b := mustKey(t, "a"), mustKey(t, "b")
	pa, _ := a.MarshalPublicPEM()
	pb, _ := b.MarshalPublicPEM()
	bundle := slices.Concat([]byte("# current\n"), pa, []byte("\n# next\n"), pb)
	set, err := ParseKeySet(bundle)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	kids := set.KeyIDs()
	slices.Sort(kids)
	if !slices.Equal(kids, []string{"a", "b"}) {
		t.Errorf("kids %v, want [a b]", kids)
	}
}

func TestParseKeySetRejects(t *testing.T) {
	a := mustKey(t, "a")
	pub, _ := a.MarshalPublicPEM()
	priv, _ := a.MarshalPEM()
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	block := func(headers map[string]string, der []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: pemPublicType, Headers: headers, Bytes: der})
	}
	der := mustDecode(t, pub).Bytes

	cases := map[string][]byte{
		"empty":           nil,
		"no kid":          block(nil, der),
		"empty kid":       block(map[string]string{"kid": ""}, der),
		"kid with space":  block(map[string]string{"kid": "a b"}, der),
		"extra header":    block(map[string]string{"kid": "a", "Proc-Type": "4,ENCRYPTED"}, der),
		"duplicate kid":   slices.Concat(pub, pub),
		"private key":     priv,
		"ecdsa key":       block(map[string]string{"kid": "ec"}, ecDER),
		"malformed block": slices.Concat(pub, []byte("-----BEGIN PUBLIC KEY-----\nkid: b\n\n!!!\n-----END PUBLIC KEY-----\n")),
		"garbage der":     block(map[string]string{"kid": "a"}, []byte("nope")),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKeySet(data); err == nil {
				t.Fatal("parsed, want error")
			}
		})
	}
}

func TestParseSigningKeyRejects(t *testing.T) {
	a, b := mustKey(t, "a"), mustKey(t, "b")
	pa, _ := a.MarshalPEM()
	pb, _ := b.MarshalPEM()
	pub, _ := a.MarshalPublicPEM()
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ecKey)

	cases := map[string][]byte{
		"empty":       nil,
		"two keys":    slices.Concat(pa, pb),
		"public key":  pub,
		"no kid":      bytes.Replace(pa, []byte("kid: a\n\n"), nil, 1),
		"ecdsa key":   pem.EncodeToMemory(&pem.Block{Type: pemPrivateType, Headers: map[string]string{"kid": "ec"}, Bytes: ecDER}),
		"invalid kid": bytes.Replace(pa, []byte("kid: a"), []byte("kid: ../a"), 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSigningKey(data); err == nil {
				t.Fatal("parsed, want error")
			}
		})
	}
}

func TestDevFixturesPair(t *testing.T) {
	dir := filepath.Join("..", "..", "dev", "hosted-keys")
	for _, name := range []string{"api", "us-east", "eu-central", "ap-southeast"} {
		privPEM, err := os.ReadFile(filepath.Join(dir, name+".key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		pubPEM, err := os.ReadFile(filepath.Join(dir, name+".pub.pem"))
		if err != nil {
			t.Fatal(err)
		}
		priv, err := ParseSigningKey(privPEM)
		if err != nil {
			t.Fatalf("%s private: %v", name, err)
		}
		set, err := ParseKeySet(pubPEM)
		if err != nil {
			t.Fatalf("%s public: %v", name, err)
		}
		pub, ok := set.keys[priv.KeyID()]
		if len(set.keys) != 1 || !ok || !pub.Equal(priv.key.Public()) {
			t.Errorf("%s: public fixture does not match the private one", name)
		}
	}
}

func mustDecode(t *testing.T, data []byte) *pem.Block {
	t.Helper()
	b, _ := pem.Decode(data)
	if b == nil {
		t.Fatal("no PEM block")
	}
	return b
}
