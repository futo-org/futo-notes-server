// Package ids validates the identifiers clients send.
package ids

import (
	"strings"
	"uuid"
)

// IsUUID reports whether s is a UUID in canonical 8-4-4-4-12 form, in either
// hex case, which is what Postgres accepts as a uuid parameter without a cast
// error. uuid.Parse also accepts braced, urn:uuid:, and unhyphenated forms,
// which all differ in length from the canonical 36 characters.
func IsUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}

// IsUserID reports whether s is a hosted user ID: a canonical UUID in lower
// case. Storage keys are minted from the token's sub, which is lower case, and
// S3 keys are case-sensitive, so an upper case ID would match nothing.
func IsUserID(s string) bool {
	return IsUUID(s) && s == strings.ToLower(s)
}

// IsBlobKey reports whether key is a key storage mints: a user ID, a slash,
// and a blob UUID.
func IsBlobKey(key string) bool {
	userID, blobID, ok := strings.Cut(key, "/")
	return ok && IsUserID(userID) && IsUUID(blobID)
}

// IsOwnBlobKey reports whether key is a blob key scoped to userID: the
// 36-character user ID, a slash, and a blob UUID.
func IsOwnBlobKey(userID, key string) bool {
	return len(userID) == 36 && len(key) == 73 && key[:36] == userID && key[36] == '/' && IsUUID(key[37:])
}
