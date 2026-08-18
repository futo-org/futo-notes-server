package main

import (
	_ "embed"
	"net/http"
)

//go:embed devui.html
var devUIHTML []byte

func handleDevUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(devUIHTML)
}
