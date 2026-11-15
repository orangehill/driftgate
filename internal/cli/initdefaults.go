package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/routes"
)

// initDefaults are the exclusions and allowlist rules init writes for the
// frameworks it recognises (detectors): an exclusion by its route and
// reason, a rule by everything that makes it apply (name or path, request,
// format and its options) and its reason. One that applies to nothing is
// the framework's default for a part the project need not have (Debugbar,
// Horizon, a throttle), not a stale entry: it is not warned about. An
// entry the user changed (another reason, request, format …) is. The
// service (target) is not compared: init writes the same entries for each.
type initDefault struct {
	exclusions    map[[2]string]bool
	headers, body map[string]bool
}

func headerKey(r allowlist.HeaderRule) string {
	return fmt.Sprintf("%q %q %q %t %q", strings.ToLower(r.Name), r.Request, r.Format, r.IgnoreCase, r.Reason)
}

func bodyKey(r allowlist.BodyRule) string {
	return fmt.Sprintf("%q %q %q %t %t %q", r.Path, r.Request, r.Format, r.AnyLength, r.Parts, r.Reason)
}

var initDefaults = sync.OnceValue(func() initDefault {
	d := initDefault{exclusions: map[[2]string]bool{}, headers: map[string]bool{}, body: map[string]bool{}}
	for _, det := range detectors() {
		for _, x := range det.exclude {
			d.exclusions[x] = true
		}
		if l, err := allowlist.Parse([]byte("headers:\n" + det.headers + "body:\n" + det.allow)); err == nil {
			for _, r := range l.Headers {
				d.headers[headerKey(r)] = true
			}
			for _, r := range l.Body {
				d.body[bodyKey(r)] = true
			}
		}
	}
	return d
})

// staleExclusions are the exclusions that match no route (Coverage.Stale),
// without init's defaults.
func staleExclusions(excl *routes.Exclusions, stale []string) []string {
	defaults := initDefaults().exclusions
	reason := map[string][]string{}
	for _, x := range excl.Routes {
		reason[x.Route] = append(reason[x.Route], x.Reason)
	}
	var out []string
	for _, route := range stale {
		dflt := len(reason[route]) > 0
		for _, why := range reason[route] {
			dflt = dflt && defaults[[2]string{route, why}]
		}
		if !dflt {
			out = append(out, route)
		}
	}
	return out
}

// unusedRules are the allowlist rules that applied to no change
// (List.Unused), without init's defaults.
func unusedRules(allow *allowlist.List, unused []allowlist.Unused) []allowlist.Unused {
	var out []allowlist.Unused
	for _, u := range unused {
		if !isInitDefault(allow, u.RuleRef) {
			out = append(out, u)
		}
	}
	return out
}

// isInitDefault reports whether a header or body rule is one init wrote,
// unchanged.
func isInitDefault(allow *allowlist.List, ref allowlist.RuleRef) bool {
	defaults, i := initDefaults(), ref.Rule-1
	switch {
	case ref.Kind == "header" && i >= 0 && i < len(allow.Headers):
		return defaults.headers[headerKey(allow.Headers[i])]
	case ref.Kind == "body" && i >= 0 && i < len(allow.Body):
		return defaults.body[bodyKey(allow.Body[i])]
	}
	return false
}
