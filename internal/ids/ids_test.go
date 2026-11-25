package ids

import (
	"strings"
	"testing"
)

func TestIsUUID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical lower case", "f81d4fae-7dec-11d0-a765-00a0c91e6bf6", true},
		{"canonical upper case", "F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6", true},
		{"canonical mixed case", "f81d4fae-7DEC-11d0-A765-00a0c91e6bf6", true},
		{"empty", "", false},
		{"one character short", "f81d4fae-7dec-11d0-a765-00a0c91e6bf", false},
		{"one character long", "f81d4fae-7dec-11d0-a765-00a0c91e6bf60", false},
		{"braces", "{f81d4fae-7dec-11d0-a765-00a0c91e6bf6}", false},
		{"urn", "urn:uuid:f81d4fae-7dec-11d0-a765-00a0c91e6bf6", false},
		{"no hyphens", "f81d4fae7dec11d0a76500a0c91e6bf6", false},
		{"bad hex", "g81d4fae-7dec-11d0-a765-00a0c91e6bf6", false},
		{"misplaced hyphen", "f81d4fa-e7dec-11d0-a765-00a0c91e6bf6", false},
		{"hyphen replaced by hex", "f81d4fae07dec-11d0-a765-00a0c91e6bf6", false},
		{"spaces for hyphens", "f81d4fae 7dec 11d0 a765 00a0c91e6bf6", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsUUID(test.in); got != test.want {
				t.Fatalf("IsUUID(%q) = %t, want %t", test.in, got, test.want)
			}
		})
	}
}

func TestIsOwnBlobKey(t *testing.T) {
	const (
		user  = "018f4b28-11aa-7d3e-9f0a-3c4d5e6f7a8b"
		blob  = "018f4b2b-22bb-7e4f-8a1b-4d5e6f7a8b9c"
		other = "018f4b28-11aa-7d3e-9f0a-3c4d5e6f7aff"
	)
	tests := []struct {
		name        string
		userID, key string
		want        bool
	}{
		{"own key", user, user + "/" + blob, true},
		{"upper case blob id", user, user + "/" + strings.ToUpper(blob), true},
		{"non-UUID user of UUID length", strings.Repeat("x", 36), strings.Repeat("x", 36) + "/" + blob, true},
		{"upper case user id matching the session", strings.ToUpper(user), strings.ToUpper(user) + "/" + blob, true},
		{"other user", user, other + "/" + blob, false},
		{"key scoped to a blob id", user, blob + "/" + blob, false},
		{"case differs from session user", user, strings.ToUpper(user) + "/" + blob, false},
		{"short user id", "short", strings.Repeat("x", 73), false},
		{"short user id matching prefix", "short", "short/" + blob, false},
		{"empty user id", "", "/" + blob, false},
		{"empty key", user, "", false},
		{"missing slash", user, user + "-" + blob, false},
		{"braced blob id", user, user + "/{" + blob + "}", false},
		{"unhyphenated blob id", user, user + "/" + strings.ReplaceAll(blob, "-", ""), false},
		{"bad hex blob id", user, user + "/g" + blob[1:], false},
		{"trailing byte", user, user + "/" + blob + "x", false},
		{"path traversal", user, user + "/../" + blob[3:], false},
		{"nested path", user, user + "/" + blob[:35] + "/", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsOwnBlobKey(test.userID, test.key); got != test.want {
				t.Fatalf("IsOwnBlobKey(%q, %q) = %t, want %t", test.userID, test.key, got, test.want)
			}
		})
	}
}

func TestIsUserID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"f81d4fae-7dec-11d0-a765-00a0c91e6bf6", true},
		{"F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6", false},
		{"f81d4fae-7dec-11d0-a765-00a0c91e6bF6", false},
		{"{f81d4fae-7dec-11d0-a765-00a0c91e6bf6}", false},
		{"f81d4fae7dec11d0a76500a0c91e6bf6", false},
		{"", false},
	}
	for _, test := range tests {
		if got := IsUserID(test.in); got != test.want {
			t.Errorf("IsUserID(%q) = %t, want %t", test.in, got, test.want)
		}
	}
}

func TestIsBlobKey(t *testing.T) {
	const (
		user = "018f4b28-11aa-7d3e-9f0a-3c4d5e6f7a8b"
		blob = "018f4b2b-22bb-7e4f-8a1b-4d5e6f7a8b9c"
	)
	tests := []struct {
		in   string
		want bool
	}{
		{user + "/" + blob, true},
		{user + "/" + strings.ToUpper(blob), true},
		{strings.ToUpper(user) + "/" + blob, false},
		{user, false},
		{user + "/", false},
		{"/" + blob, false},
		{user + "/" + blob + "/" + blob, false},
		{"", false},
	}
	for _, test := range tests {
		if got := IsBlobKey(test.in); got != test.want {
			t.Errorf("IsBlobKey(%q) = %t, want %t", test.in, got, test.want)
		}
	}
}
