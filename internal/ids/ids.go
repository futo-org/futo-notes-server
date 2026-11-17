// Package ids validates the identifiers clients send.
package ids

import "uuid"

// IsUUID reports whether s is a UUID in canonical 8-4-4-4-12 form, in either
// hex case, which is what Postgres accepts as a uuid parameter without a cast
// error. uuid.Parse also accepts braced, urn:uuid:, and unhyphenated forms,
// which all differ in length from the canonical 36 characters.
func IsUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}
