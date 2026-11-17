package api_test

import (
	"testing"

	"futo-notes-server/internal/hosted/api"
	"futo-notes-server/internal/hosted/billing"
)

func TestDeviceLabel(t *testing.T) {
	for _, tc := range []struct{ name, ua, want string }{
		{"Pixel 9", "okhttp/4.12.0", "Pixel 9"},
		{"", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", "Chrome on macOS"},
		{"", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/153.0.8010.12 Safari/537.36", "Chrome on macOS"},
		{"", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15", "Safari on macOS"},
		{"", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", "Safari on iPhone"},
		{"", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/131.0.6778.73 Mobile/15E148 Safari/604.1", "Chrome on iPhone"},
		{"", "Mozilla/5.0 (iPad; CPU OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/132.0 Mobile/15E148 Safari/605.1.15", "Firefox on iPad"},
		{"", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0", "Edge on Windows"},
		{"", "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0", "Firefox on Linux"},
		{"", "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36", "Chrome on Android"},
		{"", "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", "Chrome on ChromeOS"},
		{"", "Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/131.0.0.0 Mobile Safari/537.36", "Chrome on Android"},
		{"", "Dalvik/2.1.0 (Linux; U; Android 15; Pixel 9 Build/AP3A)", "Android"},
		{"", "Go-http-client/1.1", "Go-http-client"},
		{"", "reqwest/0.12", "reqwest"},
		{"", "", "Unknown device"},
	} {
		if got := api.DeviceLabel(tc.name, tc.ua); got != tc.want {
			t.Errorf("DeviceLabel(%q, %q) = %q, want %q", tc.name, tc.ua, got, tc.want)
		}
	}
}

func TestStatusLabel(t *testing.T) {
	for _, tc := range []struct {
		status   billing.Status
		entitled bool
		want     string
	}{
		{billing.StatusActive, true, "Active"},
		{billing.StatusActive, false, "Expired"},
		{billing.StatusGrace, true, "Active, payment overdue"},
		{billing.StatusGrace, false, "Expired"},
		{billing.StatusOnHold, false, "On hold, payment failed"},
		{billing.StatusExpired, false, "Expired"},
		{billing.StatusRevoked, false, "Revoked"},
	} {
		if got := api.StatusLabel(tc.status, tc.entitled); got != tc.want {
			t.Errorf("StatusLabel(%s, %t) = %q, want %q", tc.status, tc.entitled, got, tc.want)
		}
	}
}
