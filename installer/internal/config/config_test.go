package config

import "testing"

func TestGeneratePassword(t *testing.T) {
	a, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-char hex password, got len=%d", len(a))
	}
	b, _ := GeneratePassword()
	if a == b {
		t.Fatalf("two calls returned identical password %q", a)
	}
}
