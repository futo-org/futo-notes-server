package appstore

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"strings"
	"testing"
)

// The embedded anchor must be exactly Apple Root CA - G3, as Apple publishes
// it at https://www.apple.com/certificateauthority/.
func TestEmbeddedRootIsAppleRootCAG3(t *testing.T) {
	sum := sha256.Sum256(appleRootG3)
	if got := strings.ToUpper(hex.EncodeToString(sum[:])); got != "63343ABFB89A6A03EBB57E9B3F5FA7BE7C4F5C756F3017B3A8C488C3653E9179" {
		t.Fatalf("embedded root fingerprint = %s", got)
	}
	root, err := x509.ParseCertificate(appleRootG3)
	if err != nil {
		t.Fatal(err)
	}
	if root.Subject.CommonName != "Apple Root CA - G3" || !root.IsCA {
		t.Fatalf("root = %s", root.Subject)
	}
	if AppleRoots() == nil {
		t.Fatal("no roots")
	}
}
