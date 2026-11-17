package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// DashboardParity maps each state-changing dashboard route to the JSON API
// route that does the same thing.
func (s *Server) DashboardParity() map[string]string {
	parity := map[string]string{}
	for _, a := range s.dashboardActions() {
		parity["POST "+a.path] = a.api
	}
	return parity
}

// Routes lists every route the server mounts, as "METHOD pattern".
func (s *Server) Routes() []string {
	var routes []string
	_ = chi.Walk(s.router(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	})
	return routes
}
