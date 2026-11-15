package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// retriedNote names the read-only requests of a capture that were answered
// only after a retry (target.retry), with how their first send went; up
// to ten.
func retriedNote(out io.Writer, sum *capture.Summary) {
	ids := retriedIDs(sum)
	if len(ids) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%d %s only after a retry (marked in every report; retry: 0 shows each failure):\n",
		len(ids), plural(len(ids), "request was answered", "requests were answered"))
	for i, id := range ids {
		if i == 10 {
			fmt.Fprintf(out, "  and %d more\n", len(ids)-10)
			break
		}
		rt := sum.Retried[id]
		fmt.Fprintf(out, "  %s: first %s after %dms\n", id, rt.First(), rt.MS)
	}
}

// retriedIDs lists the requests of a capture answered only after a retry.
func retriedIDs(sum *capture.Summary) []string {
	ids := make([]string, 0, len(sum.Retried))
	for id := range sum.Retried {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// retriedRecords lists the requests of ids whose response in set came
// only after a retry (snapshot.Record.Retry).
func retriedRecords(set *snapshot.Set, ids []string) ([]string, error) {
	recs, err := selectRecords(set, ids)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rec := range recs {
		if rec != nil && rec.Retry.Passed() {
			out = append(out, rec.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// retriedBaselineNote warns that a baseline now holds responses that came
// only after a retry: they are what later captures are compared with,
// although their first send failed.
func retriedBaselineNote(out io.Writer, name string, ids []string) {
	if len(ids) == 0 {
		return
	}
	fmt.Fprintf(out, "warning: baseline %s holds %d %s that came only after a retry: %s (marked in the records and in every comparison)\n",
		name, len(ids), plural(len(ids), "response", "responses"), listIDs(ids))
}

// reviewRetries lists, for review, the requests sent again after a load
// failure, as compare shows them: passed after a retry (louder when the
// baseline needed one too) or failed in all their sends.
func reviewRetries(out io.Writer, r *report.Report) {
	var lines []string
	for _, res := range r.Results {
		if res.Retry != nil {
			lines = append(lines, fmt.Sprintf("    %-24s %s", res.ID, res.RetryText()))
		}
	}
	if len(lines) > 0 {
		fmt.Fprintf(out, "\nsent again after a load failure (not drifts; nothing to accept):\n%s\n", strings.Join(lines, "\n"))
	}
}
