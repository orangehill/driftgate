package report

import (
	"fmt"
	"strings"
)

// A read-only request whose first send failed under load (a timeout, a
// reset connection, a 502, 503 or 504) is sent again (target.retry). Its
// response then carries the first send's outcome (snapshot.Retry), and the
// report keeps it apart: a request that passed only after a retry is a
// class of its own in every format, never a plain pass. One that needed a
// retry in the baseline too is said louder: twice is not load.

// retried counts the requests that passed only after a retry
// (PassedAfterRetry), those of them whose baseline response came after one
// too (RetriedInBoth), and the compared requests whose baseline response
// came after one (BaselineRetried). Build and Expect call it.
func (r *Report) retried() {
	r.PassedAfterRetry, r.RetriedInBoth, r.BaselineRetried = nil, nil, nil
	for _, res := range r.Results {
		if res.passedAfterRetry() {
			r.PassedAfterRetry = append(r.PassedAfterRetry, res.ID)
			if res.BaselineRetry.Passed() {
				r.RetriedInBoth = append(r.RetriedInBoth, res.ID)
			}
		}
		if (res.Outcome == Pass || res.Outcome == Drift) && res.BaselineRetry.Passed() {
			r.BaselineRetried = append(r.BaselineRetried, res.ID)
		}
	}
}

// passedAfterRetry reports a request that passes, also as an expected
// drift (driftgate expect), with a response that came only after a retry.
func (res Result) passedAfterRetry() bool {
	return res.Retry.Passed() && res.passes()
}

// passes reports a result that does not fail the comparison: a pass or an
// expected drift.
func (res Result) passes() bool {
	return res.Outcome == Pass || res.Outcome == Drift && len(res.Expected) > 0
}

// passedRetryLine is how a request that does not fail and was sent again
// after a load failure is shown: passed after a retry, or passed although
// every send failed (as in the baseline); empty without a retry.
func (res Result) passedRetryLine() string {
	switch {
	case res.Retry == nil || !res.passes():
		return ""
	case res.Retry.Recovered:
		return "passed " + res.RetryText()
	}
	return "passed; " + res.RetryText()
}

// RetryText says how the sends of a retried request went: after a retry
// when one recovered, else failed in all of them; and that a retry in the
// baseline too is not load.
func (res Result) RetryText() string {
	if !res.Retry.Recovered {
		return fmt.Sprintf("failed in all %d sends: the first gave %s", res.Retry.Sends, res.Retry.First())
	}
	s := fmt.Sprintf("after a retry: the first send gave %s", res.Retry.First())
	if res.BaselineRetry.Passed() {
		s += "; it needed a retry in the baseline too (first " + res.BaselineRetry.First() + "): not load, check it"
	}
	return s
}

// observedRetry is what a skipped or observed request's line adds when its
// response came after a retry, or failed in every send.
func (res Result) observedRetry() string {
	if res.Retry == nil {
		return ""
	}
	return "; " + res.RetryText()
}

// RetryNote names the requests that needed a retry in the baseline and
// now, those that passed only after a retry and those that passed
// although every send failed (with their first outcome, up to ten each),
// and those whose baseline response came after one; empty when there are
// none.
func (r *Report) RetryNote() string {
	var notes []string
	if n := len(r.RetriedInBoth); n > 0 {
		notes = append(notes, fmt.Sprintf("%s needed a retry in the baseline and now, which is not load: check %s",
			plural(n, "request"), listIDs(r.RetriedInBoth)))
	}
	if n := len(r.PassedAfterRetry); n > 0 {
		var list []string
		for _, res := range r.Results {
			if !res.passedAfterRetry() {
				continue
			}
			if len(list) == 10 {
				list = append(list, fmt.Sprintf("and %d more", n-10))
				break
			}
			list = append(list, fmt.Sprintf("%s (first %s)", res.ID, res.Retry.First()))
		}
		notes = append(notes, fmt.Sprintf("%s passed only after a retry, counted as passed: %s", plural(n, "request"), strings.Join(list, ", ")))
	}
	var failed []string
	for _, res := range r.Results {
		if res.Retry != nil && !res.Retry.Recovered && res.passes() {
			failed = append(failed, fmt.Sprintf("%s (first %s)", res.ID, res.Retry.First()))
		}
	}
	if n := len(failed); n > 0 {
		if n > 10 {
			failed = append(failed[:10], fmt.Sprintf("and %d more", n-10))
		}
		notes = append(notes, fmt.Sprintf("%s passed although every send failed under load: %s", plural(n, "request"), strings.Join(failed, ", ")))
	}
	if n := len(r.BaselineRetried); n > 0 {
		notes = append(notes, fmt.Sprintf("the baseline's response of %s came only after a retry: %s", plural(n, "request"), listIDs(r.BaselineRetried)))
	}
	return strings.Join(notes, "; ")
}

// retryVerdict is what the verdict line adds when requests passed only
// after a retry.
func (r *Report) retryVerdict() string {
	n := len(r.PassedAfterRetry)
	switch {
	case n == 0:
		return ""
	case len(r.RetriedInBoth) > 0:
		return fmt.Sprintf(" (%d passed only after a retry, %d of them in the baseline too: check them)", n, len(r.RetriedInBoth))
	}
	return fmt.Sprintf(" (%d passed only after a retry)", n)
}
