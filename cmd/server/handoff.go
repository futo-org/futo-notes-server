package main

import (
	_ "embed"
	"net/http"
	"net/url"
	"path"

	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/handoff"
)

// handoffDoneHTML is the page the browser lands on after a hand-off login,
// telling the person to go back to the app that is polling for the session.
//
//go:embed handoff_done.html
var handoffDoneHTML []byte

// handoffTicketParam names the Login Hand-off ticket in the start route's
// query string, which is how an app's ticket enters the OIDC flow.
const handoffTicketParam = "ticket"

// oidcStartURL builds the URL an app opens for a hand-off ticket: the start
// route, which is the callback's sibling, on the server's own public origin.
// Config guarantees OIDC_REDIRECT_URI is an absolute URL whose path ends in
// the callback route, so the sibling is the right target.
func oidcStartURL(redirectURI, ticket string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	u.Path = path.Dir(u.Path) + "/start"
	u.RawQuery = url.Values{handoffTicketParam: {ticket}}.Encode()
	return u.String(), nil
}

// handleMintHandoff answers an app's unauthenticated request for a Login
// Hand-off: a fresh ticket and the URL to open a browser at.
func handleMintHandoff(cfg config.Config, database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ticket, err := handoff.Mint(r.Context(), database)
		if err != nil {
			serverError(w, r, "minting a login hand-off", err)
			return
		}
		loginURL, err := oidcStartURL(cfg.OIDCRedirectURI, ticket)
		if err != nil {
			serverError(w, r, "minting a login hand-off: building the login URL", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket, "url": loginURL})
	}
}

// handleRedeemHandoff answers an app polling its ticket: 202 while the person
// is still in the browser, the login body exactly once after that, and 404
// for a ticket that is unknown, already redeemed, refused, or expired.
func handleRedeemHandoff(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		redeemed, err := handoff.Redeem(r.Context(), database, r.PathValue("ticket"))
		if err != nil {
			serverError(w, r, "redeeming a login hand-off", err)
			return
		}
		switch redeemed.Code {
		case handoff.RedeemPending:
			w.WriteHeader(http.StatusAccepted)
		case handoff.RedeemOK:
			writeJSON(w, http.StatusOK, map[string]any{"user": redeemed.User, "token": redeemed.Token})
		default:
			writeError(w, http.StatusNotFound, "not found")
		}
	}
}
