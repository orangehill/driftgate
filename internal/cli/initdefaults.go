package cli

import (
	"sync"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/routes"
)

// initDefaults are the exclusions and allowlist rules init writes for the
// frameworks it recognises (detectors), each by its route, header name or
// body path and its reason. One that applies to nothing is the framework's
// default for a part the project need not have (Debugbar, Horizon, a
// throttle), not a stale entry: it is not warned about. An entry the user
// changed (another reason) is.
type initDefault struct {
	exclusions, headers, body map[[2]string]bool
}

var initDefaults = sync.OnceValue(func() initDefault {
	d := initDefault{exclusions: map[[2]string]bool{}, headers: map[[2]string]bool{}, body: map[[2]string]bool{}}
	for _, det := range detectors() {
		for _, x := range det.exclude {
			d.exclusions[x] = true
		}
		if l, err := allowlist.Parse([]byte("headers:\n" + det.headers + "body:\n" + det.allow)); err == nil {
			for _, r := range l.Headers {
				d.headers[[2]string{r.Name, r.Reason}] = true
			}
			for _, r := range l.Body {
				d.body[[2]string{r.Path, r.Reason}] = true
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
	defaults := initDefaults()
	var out []allowlist.Unused
	for _, u := range unused {
		i := u.Rule - 1
		switch {
		case u.Kind == "header" && i >= 0 && i < len(allow.Headers) && defaults.headers[[2]string{allow.Headers[i].Name, allow.Headers[i].Reason}]:
		case u.Kind == "body" && i >= 0 && i < len(allow.Body) && defaults.body[[2]string{allow.Body[i].Path, allow.Body[i].Reason}]:
		default:
			out = append(out, u)
		}
	}
	return out
}
