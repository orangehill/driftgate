package cli

import (
	"fmt"
	"io"

	"github.com/orangehill/driftgate/internal/capture"
)

// preparedNote warns that prepare changed the
// state after the restore: it runs after every restore, so every capture
// pays for it, and what it writes anew each time (a time stamp) is a drift
// in every run. A warning, never a failure: a step that works on the
// restored data belongs there.
func preparedNote(out io.Writer, sum *capture.Summary) {
	if len(sum.Prepared) == 0 {
		return
	}
	fmt.Fprintf(out, "\nwarning: the state changed between the restore and the end of prepare: %s\n", listIDs(sum.Prepared))
	fmt.Fprintln(out, "  it runs after every restore, so it does this before every capture, and a value it writes anew (a time stamp) drifts in every run;")
	fmt.Fprintln(out, "  if it does not depend on the restored data, move it to seed: (driftgate seed keeps its result in the pristine copy)")
}
