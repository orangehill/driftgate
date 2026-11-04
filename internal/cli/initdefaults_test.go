package cli

import (
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
	excl := &routes.Exclusions{Routes: []routes.Exclusion{
		{Route: "/_debugbar/*", Reason: "Laravel Debugbar: a development tool."},
		{Route: "/old", Reason: "gone"},
		{Route: "/horizon", Reason: "my own words"},
	}}
	if got := staleExclusions(excl, []string{"/_debugbar/*", "/old", "/horizon"}); strings.Join(got, ",") != "/old,/horizon" {
		t.Errorf("stale exclusions: %v", got)
	}
	allow, err := allowlist.Parse([]byte("headers:\n" + laravelHeaders + "  - name: x-own\n    format: 'x'\n    reason: mine\n"))
	if err != nil {
		t.Fatal(err)
	}
	unused := []allowlist.Unused{{RuleRef: allowlist.RuleRef{Kind: "header", Rule: 1}}, {RuleRef: allowlist.RuleRef{Kind: "header", Rule: 4}}}
	if got := unusedRules(allow, unused); len(got) != 1 || got[0].Rule != 4 {
		t.Errorf("unused rules: %+v", got)
	}
}
