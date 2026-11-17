package api

import (
	_ "embed"
	"net/http"
)

//go:embed devui.html
var devUIHTML []byte

// handleDevUI serves a page for exercising the hosted API by hand: dashboard
// and app sign-in, account, region, devices, deletion, storage tokens and
// receipt-backed object writes. DEV_UI enables it; never in production.
func handleDevUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(devUIHTML)
}
