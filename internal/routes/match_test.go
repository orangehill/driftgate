package routes

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
)

// TestMatcherAgreesWithMatch: the index finds, for every request, the
// same route as trying every route does, on random inventories with fixed,
// parameter, optional and catch-all segments, several services and methods.
func TestMatcherAgreesWithMatch(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	words := []string{"api", "v1", "users", "items", "admin", "", "docs", "a.b", "API"}
	segment := func() string {
		switch rng.Intn(10) {
		case 0:
			return "{id}"
		case 1:
			return "{slug?}"
		case 2:
			return "{path*}"
		case 3:
			return "{rest*?}"
		case 4:
			return "v{version}"
		case 5:
			return "{name}.json"
		default:
			return words[rng.Intn(len(words))]
		}
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "*"}
	values := []string{"1", "abc", "x/y", "", "999999999", "a.json", "v2"}
	for round := 0; round < 200; round++ {
		inv := &Inventory{}
		for i := rng.Intn(60); i >= 0; i-- {
			var segs []string
			for j := rng.Intn(4); j >= 0; j-- {
				segs = append(segs, segment())
			}
			path := "/" + strings.Join(segs, "/")
			if rng.Intn(15) == 0 {
				path = "/"
			}
			r := Route{Path: path, Methods: []string{methods[rng.Intn(len(methods))]}}
			if rng.Intn(3) == 0 {
				r.Methods = append(r.Methods, methods[rng.Intn(len(methods))])
			}
			if rng.Intn(4) == 0 {
				r.Target = "web"
			}
			inv.Routes = append(inv.Routes, r)
		}
		m := NewMatcher(inv)
		for i := 0; i < 300; i++ {
			var req catalog.Request
			if rng.Intn(2) == 0 && len(inv.Routes) > 0 {
				// A request made from a route, with values for its parameters.
				r := inv.Routes[rng.Intn(len(inv.Routes))]
				req = catalog.Request{Target: r.Target, Method: methods[rng.Intn(5)], Path: r.Path, Params: map[string]string{}}
				for _, p := range r.Params() {
					req.Params[p.Name] = values[rng.Intn(len(values))]
				}
				for _, name := range []string{"slug?", "path*", "rest*?", "id", "version", "name"} {
					if rng.Intn(3) == 0 {
						req.Path = strings.ReplaceAll(req.Path, "{"+name+"}", values[rng.Intn(len(values))])
					}
				}
			} else {
				var segs []string
				for j := rng.Intn(4); j >= 0; j-- {
					segs = append(segs, words[rng.Intn(len(words))])
				}
				req = catalog.Request{Method: methods[rng.Intn(5)], Path: "/" + strings.Join(segs, "/")}
				if rng.Intn(10) == 0 {
					req.Path = strings.TrimPrefix(req.Path, "/")
				}
				if rng.Intn(5) == 0 {
					req.Path += "?q=1"
				}
			}
			if got, want := m.Match(req), Match(inv, req); got != want {
				t.Fatalf("round %d: %s %q (%v): matcher %v, Match %v", round, req.Method, req.Path, req.Params, describe(got), describe(want))
			}
		}
	}
}

func describe(r *Route) string {
	if r == nil {
		return "none"
	}
	return fmt.Sprintf("%v %s %s", r.Methods, r.Path, r.Target)
}
