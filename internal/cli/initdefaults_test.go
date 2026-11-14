package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/routes"
)

// TestInitDefaults: every detector's allowlist entries parse, and an
// exclusion or rule init wrote is not warned about when it matched
// nothing; one with the user's own reason is.
func TestInitDefaults(t *testing.T) {
	for _, det := range detectors() {
		if _, err := allowlist.Parse([]byte("headers:\n" + det.headers + "body:\n" + det.allow)); err != nil {
			t.Errorf("%s: %v", det.kind, err)
		}
	}
	// Laravel Dusk's routes log in as any user: a testing tool, never the
	// API, and their error pages differ on every run.
	laravel := slices.IndexFunc(detectors(), func(d detector) bool { return d.kind == "laravel" })
	if laravel < 0 || !slices.ContainsFunc(detectors()[laravel].exclude, func(x [2]string) bool { return x[0] == "/_dusk/*" }) {
		t.Error("init does not exclude /_dusk/* for Laravel")
	}
	excl := &routes.Exclusions{Routes: []routes.Exclusion{
		{Route: "/_debugbar/*", Reason: "Laravel Debugbar: a development tool."},
		{Route: "/old", Reason: "gone"},
		{Route: "/horizon", Reason: "my own words"},
	}}
	if got := staleExclusions(excl, []string{"/_debugbar/*", "/old", "/horizon"}); strings.Join(got, ",") != "/old,/horizon" {
		t.Errorf("stale exclusions: %v", got)
	}
	// Rules 1–3 are init's; 4 is the user's own; 5 is rule 1 with another
	// format, 6 with a request of its own: both the user's.
	allow, err := allowlist.Parse([]byte("headers:\n" + laravelHeaders + `  - name: x-own
    format: 'x'
    reason: mine
  - name: x-ratelimit-remaining
    format: '.*'
    reason: Laravel throttle; depends on the requests sent before in the same minute.
  - name: x-ratelimit-remaining
    request: 'api-*'
    format: '\d+'
    reason: Laravel throttle; depends on the requests sent before in the same minute.
`))
	if err != nil {
		t.Fatal(err)
	}
	var unused []allowlist.Unused
	for i := range allow.Headers {
		unused = append(unused, allowlist.Unused{RuleRef: allowlist.RuleRef{Kind: "header", Rule: i + 1}})
	}
	var got []int
	for _, u := range unusedRules(allow, unused) {
		got = append(got, u.Rule)
	}
	if fmt.Sprint(got) != "[4 5 6]" {
		t.Errorf("unused rules reported: %v, want [4 5 6]", got)
	}
}
