package main

import (
	_ "embed"
	"net/http"
)

// accountPath serves a bare page for signing in and managing a subscription
// from a browser, without the app. The page is static and says nothing about
// the account itself; it reads everything through the /api routes the app
// calls, on the session cookie a browser login sets.
const accountPath = "/account"

//go:embed account.html
var accountHTML []byte

func handleAccountPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(accountHTML)
}
