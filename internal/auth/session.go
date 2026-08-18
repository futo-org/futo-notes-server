package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const SessionTTL = 7 * 24 * time.Hour

// GenerateToken returns a raw session token: 32 random bytes, hex-encoded.
func GenerateToken() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// HashToken returns the SHA-256 of the raw (hex-encoded) token. Only this
// hash is stored server-side.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
