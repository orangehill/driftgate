package cli

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/capture"
)

// TestMain lets the runs of a repeated capture follow each other at once
// (runGap): the tests do not compare timestamps of the second, and the
// wait would add a second to every first run. TestRepeatedRunsInOtherSeconds
// sets it back. The writes of a capture on one state follow each other
// after a short gap (capture.WriteGap), for the same reason; the capture
// package's TestWriteGap tests the real one.
func TestMain(m *testing.M) {
	runGap = 0
	capture.WriteGap = 10 * time.Millisecond
	os.Exit(m.Run())
}

// noDate serves h without a Date header. Two captures of one state may fall
// in two seconds, and their Date headers then differ: a test that makes a
// baseline from two captures, without a date rule in its allowlist, would
// fail now and then on a drift it is not about.
func noDate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
		h.ServeHTTP(w, r)
	})
}
