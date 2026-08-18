// Package auth verifies login credentials.
package auth

import (
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// Stored hashes carry N/r/p in their prefix, but the contract is to verify
// with these fixed constants regardless of what the string says.
const (
	scryptN      = 16384
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 64
)

func VerifyPlaintext(password, configured string) bool {
	return subtle.ConstantTimeCompare([]byte(password), []byte(configured)) == 1
}

// VerifyScrypt checks a password against the self-describing stored form
// "scrypt:N=16384,r=8,p=1:salt_hex:hash_hex".
func VerifyScrypt(password, stored string) (bool, error) {
	parts := strings.Split(stored, ":")
	if len(parts) != 4 || parts[0] != "scrypt" {
		return false, fmt.Errorf("unrecognized password hash format")
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false, fmt.Errorf("decoding salt: %w", err)
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false, fmt.Errorf("decoding hash: %w", err)
	}
	derived, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(derived, want) == 1, nil
}
