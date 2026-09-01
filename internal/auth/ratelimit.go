package auth

import (
	"sync"
	"time"
)

// Fixed by the v1 contract: 10 login attempts per 60s per client.
const (
	loginRateLimit  = 10
	loginRateWindow = time.Minute
)

type RateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{windows: make(map[string]*rateWindow)}
}

// Allow records an attempt for key and reports whether it is within the
// limit. When it is not, retryAfter is the time until the window resets.
func (l *RateLimiter) Allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for k, w := range l.windows {
		if now.Sub(w.start) >= loginRateWindow {
			delete(l.windows, k)
		}
	}

	w := l.windows[key]
	if w == nil {
		w = &rateWindow{start: now}
		l.windows[key] = w
	}
	w.count++
	if w.count > loginRateLimit {
		return false, loginRateWindow - now.Sub(w.start)
	}
	return true, 0
}
