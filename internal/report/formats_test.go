package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestEveryClassInEveryFormat: a comparison with every kind of result
// that is not a plain pass or drift (passed after a retry, also in the
// baseline and as an expected drift; failed in every send, failing and
// passing, also as an expected drift; sent again
// after a restore; an expected drift; an expectation met in part; an
// observed request after a retry; a slower request) says each of them in
// text, JSON, markdown and JUnit.
func TestEveryClassInEveryFormat(t *testing.T) {
	retried := &snapshot.Retry{Sends: 2, Status: 503, MS: 12, Recovered: true}
	failedAll := &snapshot.Retry{Sends: 2, Status: 504, MS: 12}
	exp := drifted("exp", "sha256:1")
	exp.Retry = retried
	failing := drifted("fail-all", "sha256:9")
	failing.Retry = failedAll
	// Failed in every send, as in the baseline: it passes, and says so.
	expFailed := drifted("exp-down", "sha256:5")
	expFailed.Retry = failedAll
	r := Build("capture:c", "baseline:local", []Result{
		{ID: "ok-retry", Outcome: Pass, Retry: retried},
		{ID: "ok-both", Outcome: Pass, Retry: retried, BaselineRetry: retried},
		{ID: "down", Outcome: Pass, Retry: failedAll},
		expFailed,
		{ID: "resent", Outcome: Pass, Resent: true, Notes: []string{"sent again in the capture: the first answer was 500, after the restore of db ended 1 database session of the application"}},
		failing, exp, drifted("partly", "sha256:6", "sha256:7"),
		{ID: "slow", Outcome: Pass},
		{ID: "obs", Outcome: Observed, Note: "observed", Retry: retried},
	})
	r.Timings = CompareTimings(r.Results, map[string]int64{"slow": 10, "ok-retry": 10}, map[string]int64{"slow": 900, "ok-retry": 10})
	r.Expect([]Expectation{
		{Ref: "T-1", Reason: "x", Request: "exp", Drifts: []string{"sha256:1"}},
		{Ref: "T-1", Reason: "x", Request: "exp-down", Drifts: []string{"sha256:5"}},
		{Ref: "T-2", Reason: "y", Request: "partly", Drifts: []string{"sha256:6", "sha256:8"}},
	}, func(string) bool { return true })
	if strings.Join(r.PassedAfterRetry, ",") != "ok-retry,ok-both,exp" {
		t.Fatalf("passed after a retry: %v", r.PassedAfterRetry)
	}
	want := map[string][]string{
		"text": {"RETRY  ok-retry (passed after a retry: the first send gave 503)", "RETRY  exp (passed after a retry", "not load, check it",
			"RETRY  down (passed; failed in all 2 sends: the first gave 504)", "RETRY  exp-down (passed; failed in all 2 sends", "2 requests passed although every send failed under load: down (first 504), exp-down (first 504)",
			"failed in all 2 sends: the first gave 504", "1 request was sent again after a restore", "resent: sent again in the capture",
			"T-1 (x): 2 requests: exp-down, exp", "T-2 partly (partly met: 1 of 2 drifts)", "slow 10 → 900 ms", "OBSERVE obs (observed; after a retry", "4 drifted (2 expected)"},
		"json": {`"passed_after_retry": [`, `"retried_in_both": [`, `"baseline_retried": [`, `"recovered": false`, `"resent": true`, `"resent": [`,
			`"expected": [`, `"why": "partly met: 1 of 2 drifts"`, `"slower": [`},
		"markdown": {"3 requests passed only after a retry", "2 requests passed although every send failed under load: down (first 504), exp-down (first 504)", "not load", "failed in all 2 sends", "sent again after a restore", "T-1 (x): 2 requests: exp-down, exp",
			"partly met: 1 of 2 drifts", "slow 10 → 900 ms", "obs (observed; after a retry", "| Expected |"},
		"junit": {`<system-out>passed after a retry: the first send gave 503</system-out>`, `<system-out>passed; failed in all 2 sends: the first gave 504</system-out>`, "not load, check it", "failed in all 2 sends: the first gave 504",
			`<property name="resent" value="1">`, "sent again in the capture", "expected drift (T-1), 1 drift:&#xA;header-extra header:x-a   → [&#34;1&#34;]&#xA;after a retry",
			"expectation T-2 not met: partly met: 1 of 2 drifts", `<property name="slower:slow" value="10 → 900 ms">`,
			`<skipped message="observed: observed; after a retry: the first send gave 503">`, "3 requests passed only after a retry"},
	}
	for _, f := range []string{"text", "json", "markdown", "junit"} {
		var b bytes.Buffer
		if err := r.Write(&b, f, DefaultDrifts); err != nil {
			t.Fatal(err)
		}
		for _, w := range want[f] {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s without %q:\n%s", f, w, b.String())
			}
		}
	}
}

// TestJUnitIncomplete: an incomplete result (exit 3) is an error in the
// JUnit file, with why, so a CI that reads only the file does not see it
// green; a complete one has none.
func TestJUnitIncomplete(t *testing.T) {
	for name, c := range map[string]struct {
		results []Result
		want    string
	}{
		"private":   {[]Result{{ID: "a", Outcome: Pass}, {ID: "me", Outcome: Skipped, Note: NotePrivateMissing}}, "the result is incomplete: 1 private request could not be compared here"},
		"read-only": {[]Result{{ID: "a", Outcome: Pass}, {ID: "post", Outcome: Skipped, Note: NoteReadOnlyBaseline}}, "incomplete: 1 request not checked: capture:ro was captured with --read-only"},
		"nothing":   {[]Result{{ID: "a", Outcome: Skipped, Note: NoteNoBaseline}}, "nothing was compared"},
		"complete":  {[]Result{{ID: "a", Outcome: Pass}}, ""},
	} {
		r := Build("cur", "capture:ro", c.results)
		var b strings.Builder
		if err := r.JUnit(&b, DefaultDrifts); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if c.want == "" {
			if !strings.Contains(out, `errors="0"`) || strings.Contains(out, "<error") {
				t.Errorf("%s: %s", name, out)
			}
			continue
		}
		if strings.Count(out, `errors="1"`) != 2 || !strings.Contains(out, `<testcase name="incomplete"`) ||
			!strings.Contains(out, `type="incomplete"`) || !strings.Contains(out, c.want) {
			t.Errorf("%s: %s", name, out)
		}
	}
}
