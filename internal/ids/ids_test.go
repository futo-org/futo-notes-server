package ids

import "testing"

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
