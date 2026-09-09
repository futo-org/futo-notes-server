package main

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
)

// Two flows end in a browser the app cannot see: the Login Hand-off, and the
// checkout Polar returns. Both leave the person on a page whose whole job is
// to say the browser is finished and the app is where the rest happens.
// They are one page with two sentences rather than two pages, so they cannot
// drift apart in style.

//go:embed browser_done.html
var browserDoneHTML string

var browserDoneTemplate = template.Must(template.New("browser-done").Parse(browserDoneHTML))

// browserDonePage is what one of those pages says.
type browserDonePage struct {
	Title   string
	Heading string
	Body    string
}

var (
	handoffDonePage = browserDonePage{
		Title:   "Signed in to FUTO Notes",
		Heading: "You're signed in",
		Body:    "Go back to FUTO Notes to finish setting up this device. You can close this tab.",
	}
	billingDonePage = browserDonePage{
		Title:   "Subscribed to FUTO Notes",
		Heading: "You're subscribed",
		Body:    "Go back to FUTO Notes to start syncing. You can close this tab.",
	}
)

// writeBrowserDonePage renders one of them. The page carries no session and
// says nothing about the account, so there is nothing here to get wrong for
// whoever's browser lands on it.
func writeBrowserDonePage(w http.ResponseWriter, page browserDonePage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := browserDoneTemplate.Execute(w, page); err != nil {
		slog.Error("rendering a browser done page", "title", page.Title, "err", err)
	}
}
