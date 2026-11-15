package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

// TestAllowlistCheck lints an allowlist against the catalog and a baseline
// with public and private bodies, with and without the project key.
func TestAllowlistCheck(t *testing.T) {
	project, cfg := setsFixture(t)
	key := os.Getenv(secret.EnvKey)
	// The key from a variable of its own: without it nothing asks the Keychain.
	raw, _ := os.ReadFile(cfg)
	write(t, cfg, string(raw)+"key:\n  env: DG_LINT_KEY\n")
	t.Setenv(secret.EnvKey, "")
	t.Setenv("DG_LINT_KEY", key)

	allow := filepath.Join(project, "driftgate", "allowlist.yaml")
	write(t, allow, `version: 1
headers:
  - name: date
    format: '\d+'
    reason: time
  - name: x-gone
    format: '\d+'
    reason: a header no response has
body:
  - request: mut-login
    path: $.token
    format: '[a-z]+\.[a-z]+'
    reason: in a private body
  - path: $.entries[*].date
    format: '\d{4}-\d{2}-\d{2} [\d:]+'
    reason: present
  - request: get-gone
    path: $.x
    format: '\d+'
    reason: a request that is gone
  - path: $.entries[*].renamed
    format: '\d+'
    reason: a key that is gone
  - path: $.entries*
    any_length: true
    reason: reaches values too
  - path: $raw
    format: '(?s)<html>.*'
    reason: every request, and no page anywhere
  - path: '*updated_at'
    format: '\d+'
    reason: broad
`)
	findings := func(out string) []string {
		var res lintResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		var got []string
		for _, f := range res.Findings {
			got = append(got, f.Kind+" "+strconv.Itoa(f.Rule)+" "+f.Check)
		}
		sort.Strings(got)
		return got
	}
	code, out, errOut := runCLI(t, "allowlist", "check", "--config", cfg, "--format", "json")
	want := []string{
		"body 3 no-request", "body 4 no-place", "body 5 any-length-values", "body 6 no-place", "body 6 raw-without-request",
		"body 7 broad", "body 7 no-place", "header 2 no-place",
	}
	if got := findings(out); code != ExitDrift || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("with the key: exit %d %s\ngot  %q\nwant %q", code, errOut, got, want)
	}
	code, out, _ = runCLI(t, "allowlist", "check", "--config", cfg)
	if code != ExitDrift || !strings.Contains(out, "body rule 4 ($.entries[*].renamed): no-place: ") ||
		!strings.Contains(out, "against the catalog and baseline local: 8 findings") {
		t.Errorf("text: %d\n%s", code, out)
	}

	// Without the key the private bodies are not read: a rule that could
	// reach a place only there is not reported, and a note says why.
	t.Setenv("DG_LINT_KEY", "")
	code, out, _ = runCLI(t, "allowlist", "check", "--config", cfg)
	if code != ExitDrift || strings.Contains(out, "body rule 6 ($raw): no-place") || strings.Contains(out, "request mut-login): ") ||
		!strings.Contains(out, "note: 2 response bodies not read from the baseline: api-me (a private body, and the project key is not at hand), mut-login") ||
		!strings.Contains(out, "or no response yet): body rule 1 ($.token, request mut-login); body rule 4 ($.entries[*].renamed); body rule 6 ($raw)") {
		t.Errorf("without the key: %d\n%s", code, out)
	}

	// Clean: exit 0.
	write(t, allow, "version: 1\nbody:\n  - path: $.entries[*].date\n    format: '[\\d :-]+'\n    reason: time\n")
	if code, out, _ := runCLI(t, "allowlist", "check", "--config", cfg); code != ExitClean || !strings.Contains(out, "no findings") {
		t.Errorf("clean: %d\n%s", code, out)
	}
	// Array rules: one reaches an array, one does not.
	t.Setenv("DG_LINT_KEY", key)
	write(t, allow, "version: 1\narrays:\n  - path: $.entries\n    key: id\n    reason: r\n  - path: $.items\n    key: id\n    reason: r\n")
	if code, out, _ := runCLI(t, "allowlist", "check", "--config", cfg); code != ExitDrift ||
		!strings.Contains(out, "array rule 2 ($.items): no-place") || strings.Contains(out, "array rule 1") {
		t.Errorf("array rules: %d\n%s", code, out)
	}
	// Init's own rules for a part the baseline does not reach (a rate
	// limit reached) are not findings; they are listed as not checked. The
	// same rule with the user's own reason is a finding.
	write(t, allow, "version: 1\nheaders:\n"+strings.Replace(laravelHeaders, "seconds until the limit resets.", "mine", 1))
	code, out, _ = runCLI(t, "allowlist", "check", "--config", cfg, "--format", "json")
	if got := findings(out); code != ExitDrift || strings.Join(got, ",") != "header 3 no-place" {
		t.Errorf("init's rules: exit %d %q\n%s", code, got, out)
	}
	if _, out, _ := runCLI(t, "allowlist", "check", "--config", cfg); !strings.Contains(out, "header rule 1 (x-ratelimit-remaining); header rule 2 (x-ratelimit-reset)") {
		t.Errorf("init's rules not listed as not checked:\n%s", out)
	}
	// A rule for a request that is gone.
	write(t, allow, "version: 1\nbody:\n  - request: gone\n    path: $.a\n    format: x\n    reason: r\n")
	if code, out, _ := runCLI(t, "allowlist", "check", "--config", cfg, "--format", "json"); code != ExitDrift || !strings.Contains(out, `"check": "no-request"`) {
		t.Errorf("no-request: %d\n%s", code, out)
	}
	// Configuration errors: exit 2.
	for _, args := range [][]string{{"--baseline", "nope"}, {"--format", "xml"}, {"extra"}} {
		if code, _, _ := runCLI(t, append([]string{"allowlist", "check", "--config", cfg}, args...)...); code != ExitConfig {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	write(t, allow, "version: 1\nbody:\n  - path: $.a\n    reason: no format\n")
	if code, _, _ := runCLI(t, "allowlist", "check", "--config", cfg); code != ExitConfig {
		t.Errorf("invalid allowlist: exit %d", code)
	}
	if code, _, _ := runCLI(t, "allowlist", "lint", "--config", cfg); code != ExitConfig {
		t.Errorf("unknown subcommand: exit %d", code)
	}
}
