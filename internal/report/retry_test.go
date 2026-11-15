package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestPassedAfterRetryIsVisible: a request that passed only after a retry
// is never a plain pass: text, markdown, JSON and JUnit each name it with
// its first outcome, and so a drift that came after a retry.
func TestPassedAfterRetryIsVisible(t *testing.T) {
	r := Build("capture:c", "baseline:local", []Result{
		{ID: "clean", Outcome: Pass},
		{ID: "flaky", Outcome: Pass, Retry: &snapshot.Retry{Sends: 2, Status: 503, MS: 12, Recovered: true}},
		{ID: "moved", Outcome: Drift, Retry: &snapshot.Retry{Sends: 2, Error: "timeout", MS: 60000, Recovered: true},
			Drifts: []compare.Drift{{Request: "moved", Kind: compare.KindStatus, Baseline: "200", Current: "500"}}},
		{ID: "old", Outcome: Pass, BaselineRetry: &snapshot.Retry{Sends: 2, Status: 502, Recovered: true}},
		// Failed in every send: never a pass after a retry, on either side.
		{ID: "down", Outcome: Pass, Retry: &snapshot.Retry{Sends: 2, Status: 503}, BaselineRetry: &snapshot.Retry{Sends: 2, Status: 503}},
		{ID: "gone", Outcome: Drift, Retry: &snapshot.Retry{Sends: 2, Error: "timeout"},
			Drifts: []compare.Drift{{Request: "gone", Kind: compare.KindStatus, Baseline: "200", Current: "error: timeout"}}},
	})
	if r.Passed != 4 || strings.Join(r.PassedAfterRetry, ",") != "flaky" || strings.Join(r.BaselineRetried, ",") != "old" {
		t.Fatalf("passed %d, after retry %v, baseline %v", r.Passed, r.PassedAfterRetry, r.BaselineRetried)
	}

	var text bytes.Buffer
	r.Text(&text, DefaultDrifts)
	for _, want := range []string{
		"RETRY  flaky (passed after a retry: the first send gave 503)",
		"after a retry: the first send gave timeout",
		"1 request passed only after a retry",
		"flaky (first 503)",
		"the baseline's response of 1 request came only after a retry: old",
		"failed in all 2 sends: the first gave timeout",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}

	clean := Build("capture:c", "baseline:local", []Result{r.Results[0], r.Results[1]})
	text.Reset()
	clean.Text(&text, DefaultDrifts)
	if !strings.Contains(text.String(), "No drift ✓ (1 passed only after a retry)") {
		t.Errorf("verdict: %s", text.String())
	}
	if md := clean.Markdown(DefaultDrifts); !strings.Contains(md, "### DriftGate: ✓ no drift in 2 requests (1 passed only after a retry)") ||
		!strings.Contains(md, "flaky (first 503)") {
		t.Errorf("markdown:\n%s", md)
	}
	if md := r.Markdown(DefaultDrifts); !strings.Contains(md, "after a retry: the first send gave timeout") {
		t.Errorf("markdown of a drift after a retry:\n%s", md)
	}

	// It is never counted as passed after a retry, and is said as what it
	// is: passed although every send failed.
	text.Reset()
	r.Text(&text, DefaultDrifts)
	if strings.Contains(r.RetryNote(), "down (first 503), ") || !strings.Contains(text.String(), "RETRY  down (passed; failed in all 2 sends: the first gave 503)") ||
		!strings.Contains(r.RetryNote(), "1 request passed although every send failed under load: down (first 503)") {
		t.Errorf("a request that passed although every send failed:\n%s", text.String())
	}

	var js bytes.Buffer
	if err := r.JSON(&js); err != nil {
		t.Fatal(err)
	}
	var back struct {
		PassedAfterRetry []string `json:"passed_after_retry"`
		BaselineRetried  []string `json:"baseline_retried"`
		Results          []struct {
			ID    string          `json:"id"`
			Retry *snapshot.Retry `json:"retry"`
		} `json:"results"`
	}
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if strings.Join(back.PassedAfterRetry, ",") != "flaky" || strings.Join(back.BaselineRetried, ",") != "old" ||
		back.Results[1].Retry == nil || back.Results[1].Retry.Status != 503 || back.Results[0].Retry != nil {
		t.Errorf("json: %s", js.String())
	}

	var junit bytes.Buffer
	if err := r.JUnit(&junit, DefaultDrifts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(junit.String(), `<testcase name="flaky" classname="driftgate" time="0">`+"\n   <system-out>passed after a retry: the first send gave 503</system-out>") ||
		strings.Count(junit.String(), "<system-out>") != 3 || !strings.Contains(junit.String(), "<system-out>passed; failed in all 2 sends: the first gave 503</system-out>") || !strings.Contains(junit.String(), "after a retry: the first send gave timeout") ||
		!strings.Contains(junit.String(), "the baseline&#39;s response of 1 request came only after a retry: old</system-out>") {
		t.Errorf("junit:\n%s", junit.String())
	}

	// A report without retries reads as before.
	text.Reset()
	plain := Build("capture:c", "baseline:local", []Result{{ID: "clean", Outcome: Pass}})
	plain.Text(&text, DefaultDrifts)
	if plain.RetryNote() != "" || strings.Contains(text.String(), "retry") || !strings.Contains(text.String(), "No drift ✓\n") {
		t.Errorf("without retries:\n%s", text.String())
	}
}

// TestRetriedInBothIsLouder: a request that needed a retry in the baseline
// and now is not load; every format says so.
func TestRetriedInBothIsLouder(t *testing.T) {
	rt := func(status int) *snapshot.Retry { return &snapshot.Retry{Sends: 2, Status: status, Recovered: true} }
	r := Build("capture:c", "baseline:local", []Result{
		{ID: "twice", Outcome: Pass, Retry: rt(503), BaselineRetry: rt(504)},
		{ID: "once", Outcome: Pass, Retry: rt(503)},
	})
	if strings.Join(r.RetriedInBoth, ",") != "twice" {
		t.Fatalf("retried in both: %v", r.RetriedInBoth)
	}
	var text bytes.Buffer
	r.Text(&text, DefaultDrifts)
	for _, want := range []string{
		"RETRY  twice (passed after a retry: the first send gave 503; it needed a retry in the baseline too (first 504): not load, check it)",
		"1 request needed a retry in the baseline and now, which is not load: check twice",
		"No drift ✓ (2 passed only after a retry, 1 of them in the baseline too: check them)",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	if strings.Contains(text.String(), "RETRY  once (passed after a retry: the first send gave 503;") {
		t.Errorf("once is not twice:\n%s", text.String())
	}
	if md := r.Markdown(DefaultDrifts); !strings.Contains(md, "✓ no drift in 2 requests (2 passed only after a retry, 1 of them in the baseline too: check them)") {
		t.Errorf("markdown:\n%s", md)
	}
	var js, junit bytes.Buffer
	r.JSON(&js)
	r.JUnit(&junit, DefaultDrifts)
	if !strings.Contains(js.String(), `"retried_in_both": [`) || !strings.Contains(junit.String(), "not load, check it</system-out>") {
		t.Errorf("json:\n%s\njunit:\n%s", js.String(), junit.String())
	}
}

// TestObservedRetryShown: an observed or skipped request whose response
// came after a retry says so on its line.
func TestObservedRetryShown(t *testing.T) {
	r := Build("capture:c", "baseline:local", []Result{
		{ID: "o", Outcome: Observed, Note: "200, 2 bytes → 200, 2 bytes", Retry: &snapshot.Retry{Sends: 2, Status: 503, Recovered: true}},
		{ID: "p", Outcome: Skipped, Note: NotePrivateMissing, Retry: &snapshot.Retry{Sends: 2, Error: "timeout"}},
	})
	var text bytes.Buffer
	r.Text(&text, DefaultDrifts)
	md := r.Markdown(DefaultDrifts)
	for _, want := range []string{"o (200, 2 bytes → 200, 2 bytes; after a retry: the first send gave 503)", "p (private body not on this machine; failed in all 2 sends: the first gave timeout)"} {
		if !strings.Contains(text.String(), want) || !strings.Contains(md, want) {
			t.Errorf("lacks %q:\n%s\n%s", want, text.String(), md)
		}
	}
}
