package handlers

import (
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every response the other tests receive is checked against the spec by
// specCheck. This checks that the spec and the router describe the same
// operations, so no route goes unchecked.
func TestSpecDescribesEveryRoute(t *testing.T) {
	e := newEnv(t)
	var routes []string
	err := chi.Walk(e.server.routes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	routes = append(routes, http.MethodGet+" "+readyPath)

	var documented []string
	for path, item := range loadSpec(t).Paths.Map() {
		for method := range item.Operations() {
			documented = append(documented, method+" "+path)
		}
	}
	slices.Sort(routes)
	slices.Sort(documented)
	if !slices.Equal(routes, documented) {
		t.Fatalf("router: %v\nspec:   %v", routes, documented)
	}
}
