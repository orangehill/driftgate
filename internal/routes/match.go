package routes

import (
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
)

// Matcher finds the route of many requests in one inventory, as Match does,
// without trying every route for each. A route's path starts with fixed
// segments (/api/users/{id}: api, users), and its pattern matches only a
// path that starts with the same segments; routes sit in a tree by those
// segments, and a request tries only the routes on its own path through
// the tree. The inventory must not change while the matcher is used.
type Matcher struct {
	inv   *Inventory
	spec  [][3]int         // specificity of each route
	roots map[string]*node // by target
}

type node struct {
	routes []int // routes whose fixed segments end here, in inventory order
	next   map[string]*node
}

// NewMatcher indexes the routes of inv.
func NewMatcher(inv *Inventory) *Matcher {
	m := &Matcher{inv: inv, spec: make([][3]int, len(inv.Routes)), roots: map[string]*node{}}
	for i := range inv.Routes {
		r := &inv.Routes[i]
		m.spec[i] = r.specificity()
		n := m.roots[r.Target]
		if n == nil {
			n = &node{}
			m.roots[r.Target] = n
		}
		for _, seg := range strings.Split(strings.TrimPrefix(r.Path, "/"), "/") {
			if strings.Contains(seg, "{") {
				break // a parameter: the fixed part ends
			}
			if n.next[seg] == nil {
				if n.next == nil {
					n.next = map[string]*node{}
				}
				n.next[seg] = &node{}
			}
			n = n.next[seg]
		}
		n.routes = append(n.routes, i)
	}
	return m
}

// Match is Match(inv, req): of the routes whose method and path match, the
// most specific one, the first in the inventory among equals.
func (m *Matcher) Match(req catalog.Request) *Route {
	path := requestPath(req)
	n := m.roots[req.Target]
	if n == nil {
		return nil
	}
	candidates, merged := n.routes, false
	if strings.HasPrefix(path, "/") { // every pattern starts with /
		for _, seg := range strings.Split(path[1:], "/") {
			if n = n.next[seg]; n == nil {
				break
			}
			if len(n.routes) > 0 {
				candidates, merged = append(candidates[:len(candidates):len(candidates)], n.routes...), true
			}
		}
	}
	if merged {
		slices.Sort(candidates) // inventory order: ties go to the earlier route
	}
	best := -1
	for _, i := range candidates {
		r := &m.inv.Routes[i]
		if !r.HasMethod(req.Method) || !r.Pattern().MatchString(path) {
			continue
		}
		if best < 0 || moreSpecific(m.spec[i], m.spec[best]) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &m.inv.Routes[best]
}
