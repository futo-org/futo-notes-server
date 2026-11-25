package hosted

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"uuid"
)

// TestHostedVaultUnlock drives the two ways a device without the vault
// password gets the vault key: a laptop creates the vault with a password
// and a recovery envelope, a phone unlocks through a QR pairing the laptop
// answers, and the recovery key opens the same vault key. The server only
// relays opaque bytes; the crypto here stands in for the apps'.
func TestHostedVaultUnlock(t *testing.T) {
	s := startStack(t)
	sub := "e2e-vault-" + uuid.NewV7().String()

	// The laptop creates the vault: one vault key, wrapped under the vault
	// password and under a recovery key, stored in one PUT.
	laptop := s.login(t, sub, "e2e laptop")
	s.cleanupUser(t, laptop.userID)
	collection := laptop.claimCollection()
	keyPath := "/api/collections/" + collection + "/key"
	vaultKey := randomBytes(t, 32)
	recoveryKey := randomBytes(t, 16)
	passwordSalt, recoverySalt := randomBytes(t, 16), randomBytes(t, 16)
	passwordKey, err := pbkdf2.Key(sha256.New, "correct horse battery staple", passwordSalt, 1000, 32)
	if err != nil {
		t.Fatal(err)
	}
	material := map[string]any{
		"key_salt":                     hex.EncodeToString(passwordSalt),
		"key_kdf":                      map[string]any{"kdf": "pbkdf2-sha256", "iterations": 1000, "hash": "SHA-256"},
		"encrypted_vault_key":          hex.EncodeToString(seal(t, passwordKey, vaultKey)),
		"recovery_key_salt":            hex.EncodeToString(recoverySalt),
		"recovery_key_kdf":             map[string]any{"kdf": "hkdf-sha256"},
		"recovery_encrypted_vault_key": hex.EncodeToString(seal(t, recoveryWrapKey(t, recoveryKey, recoverySalt), vaultKey)),
	}
	var claimed struct {
		Key keyMaterial `json:"key"`
	}
	laptop.expectJSON(http.MethodPut, keyPath, material, &claimed)

	// A phone signs in to the same account. It holds no vault password, so it
	// opens a pairing with a one-time key pair and shows it as a QR code.
	phone := s.login(t, sub, "e2e phone")
	pairingKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var opened struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	resp := phone.api(http.MethodPost, "/api/pairings", map[string]string{
		"public_key":  base64.RawURLEncoding.EncodeToString(pairingKey.PublicKey().Bytes()),
		"device_name": "e2e phone",
		"platform":    "android",
	}, "")
	phone.expect("open pairing", resp, http.StatusCreated)
	decode(t, resp, &opened)
	pairingPath := "/api/pairings/" + opened.ID
	phone.expect("poll before the key is posted", phone.api(http.MethodGet, pairingPath, nil, ""), http.StatusAccepted)

	// The laptop scans the code, seals the vault key to the phone's public
	// key, and posts it. Another account cannot reach the pairing.
	stranger := s.login(t, "e2e-stranger-"+uuid.NewV7().String(), "e2e stranger")
	s.cleanupUser(t, stranger.userID)
	stranger.expect("another account's post", stranger.api(http.MethodPost, pairingPath+"/key",
		map[string]string{"ciphertext": "x"}, ""), http.StatusNotFound)
	sealed := sealTo(t, pairingKey.PublicKey(), vaultKey)
	laptop.expect("post the sealed key", laptop.api(http.MethodPost, pairingPath+"/key",
		map[string]string{"ciphertext": sealed}, ""), http.StatusNoContent)

	// The phone collects it once and opens it with its private key.
	var collected struct {
		Ciphertext string `json:"ciphertext"`
	}
	phone.expectJSON(http.MethodGet, pairingPath, nil, &collected)
	if got := openSealed(t, pairingKey, collected.Ciphertext); !bytes.Equal(got, vaultKey) {
		t.Fatal("the vault key the phone collected differs from the laptop's")
	}
	phone.expect("poll after collection", phone.api(http.MethodGet, pairingPath, nil, ""), http.StatusNotFound)
	if n := s.count(t, `SELECT count(*) FROM pairings WHERE id = $1`, opened.ID); n != 0 {
		t.Fatalf("pairing rows after collection: %d, want 0", n)
	}

	// The phone reads the same key material, recovery envelope included, and
	// the recovery key opens the same vault key.
	var read struct {
		Key keyMaterial `json:"key"`
	}
	phone.expectJSON(http.MethodGet, keyPath, nil, &read)
	if read.Key.KeyUpdatedAt != claimed.Key.KeyUpdatedAt || read.Key.RecoveryEncryptedVaultKey == "" {
		t.Fatalf("key material on the phone = %+v, want the laptop's %+v", read.Key, claimed.Key)
	}
	salt, err := hex.DecodeString(read.Key.RecoveryKeySalt)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := hex.DecodeString(read.Key.RecoveryEncryptedVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := open(t, recoveryWrapKey(t, recoveryKey, salt), wrapped); !bytes.Equal(got, vaultKey) {
		t.Fatal("the recovery key does not open the vault key")
	}

	// A password change that forgets the recovery envelope deletes it.
	delete(material, "recovery_key_salt")
	delete(material, "recovery_key_kdf")
	delete(material, "recovery_encrypted_vault_key")
	material["previous_key_updated_at"] = read.Key.KeyUpdatedAt
	laptop.expectJSON(http.MethodPut, keyPath, material, nil)
	read.Key = keyMaterial{}
	phone.expectJSON(http.MethodGet, keyPath, nil, &read)
	if read.Key.RecoveryKeySalt != "" || read.Key.RecoveryEncryptedVaultKey != "" {
		t.Fatalf("key material after a rotation without the recovery envelope = %+v, want none", read.Key)
	}
}

type keyMaterial struct {
	KeyUpdatedAt              string `json:"key_updated_at"`
	RecoveryKeySalt           string `json:"recovery_key_salt"`
	RecoveryEncryptedVaultKey string `json:"recovery_encrypted_vault_key"`
}

func recoveryWrapKey(t *testing.T, recoveryKey, salt []byte) []byte {
	t.Helper()
	return deriveKey(t, recoveryKey, salt, "futo-notes recovery")
}

// deriveKey derives an AES-256 key from secret with HKDF-SHA256.
func deriveKey(t *testing.T, secret, salt []byte, info string) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, secret, salt, info, 32)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// seal encrypts plaintext under key with AES-256-GCM, nonce first.
func seal(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	aead := gcm(t, key)
	nonce := randomBytes(t, aead.NonceSize())
	return aead.Seal(nonce, nonce, plaintext, nil)
}

func open(t *testing.T, key, sealed []byte) []byte {
	t.Helper()
	aead := gcm(t, key)
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func gcm(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

// sealTo seals plaintext to recipient with an ephemeral X25519 key: the
// ephemeral public key, then the AES-GCM box under their shared secret.
func sealTo(t *testing.T, recipient *ecdh.PublicKey, plaintext []byte) string {
	t.Helper()
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		t.Fatal(err)
	}
	box := append(ephemeral.PublicKey().Bytes(), seal(t, deriveKey(t, shared, nil, "futo-notes pairing"), plaintext)...)
	return base64.RawURLEncoding.EncodeToString(box)
}

func openSealed(t *testing.T, recipient *ecdh.PrivateKey, sealed string) []byte {
	t.Helper()
	box, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(box[:32])
	if err != nil {
		t.Fatal(err)
	}
	shared, err := recipient.ECDH(ephemeral)
	if err != nil {
		t.Fatal(err)
	}
	return open(t, deriveKey(t, shared, nil, "futo-notes pairing"), box[32:])
}
