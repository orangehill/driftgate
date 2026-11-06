package discover

import (
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/routes"
)

// TestMatcherOnRealInventories: on the inventories of real frameworks, the
// route index finds what trying every route finds, for a request to every
// route with values for its parameters and with parameters left out.
func TestMatcherOnRealInventories(t *testing.T) {
	for _, a := range []Adapter{
		{Adapter: "laravel", Command: []string{"cat", "testdata/laravel13.json"}},
		{Adapter: "laravel", Command: []string{"cat", "testdata/laravel-noisy.json"}},
		{Adapter: "symfony", Command: []string{"cat", "testdata/symfony.json"}},
		{Adapter: "rails", Command: []string{"cat", "testdata/rails-routes.txt"}},
	} {
		inv := &routes.Inventory{Routes: run(t, a)}
		m := routes.NewMatcher(inv)
		n := 0
		for _, r := range inv.Routes {
			for _, method := range append([]string{"GET", "POST", "DELETE"}, r.Methods...) {
				for _, value := range []string{"", "12", "a/b", "x.json"} {
					req := catalog.Request{Method: method, Path: r.Path, Params: map[string]string{}}
					for _, p := range r.Params() {
						if value != "" {
							req.Params[p.Name] = value
						}
					}
					req.Path = strings.TrimSuffix(req.Path, "/") + strings.Repeat("/extra", len(value)%2)
					got, want := m.Match(req), routes.Match(inv, req)
					if got != want {
						t.Fatalf("%s %s %s %v: matcher %v, Match %v", a.Command[1], method, req.Path, req.Params, got, want)
					}
					if got != nil {
						n++
					}
				}
			}
		}
		if n == 0 {
			t.Errorf("%s: nothing matched", a.Command[1])
		}
	}
}
