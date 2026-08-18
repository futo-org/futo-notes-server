package auth

import (
	"testing"
	"time"
)

func TestRateLimiterAllowsUpToLimit(t *testing.T) {
	l := NewRateLimiter()
	now := time.Now()

	for i := 0; i < loginRateLimit; i++ {
		if ok, _ := l.Allow("a", now); !ok {
			t.Fatalf("attempt %d: denied, want allowed", i+1)
		}
	}
	ok, retryAfter := l.Allow("a", now)
	if ok {
		t.Fatal("attempt 11: allowed, want denied")
	}
	if retryAfter != loginRateWindow {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, loginRateWindow)
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	l := NewRateLimiter()
	now := time.Now()

	for i := 0; i < loginRateLimit+1; i++ {
		l.Allow("a", now)
	}
	if ok, _ := l.Allow("b", now); !ok {
		t.Fatal("key b denied by key a's attempts")
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	l := NewRateLimiter()
	now := time.Now()

	for i := 0; i < loginRateLimit+1; i++ {
		l.Allow("a", now)
	}
	if ok, _ := l.Allow("a", now.Add(loginRateWindow)); !ok {
		t.Fatal("denied after window elapsed, want allowed")
	}
}

func TestRateLimiterRetryAfterShrinks(t *testing.T) {
	l := NewRateLimiter()
	now := time.Now()

	for i := 0; i < loginRateLimit; i++ {
		l.Allow("a", now)
	}
	_, retryAfter := l.Allow("a", now.Add(45*time.Second))
	if retryAfter != 15*time.Second {
		t.Fatalf("retryAfter = %v, want 15s", retryAfter)
	}
}
