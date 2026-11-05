package cli

import (
	"os"
	"testing"
)

// TestMain lets the runs of a repeated capture follow each other at once
// (runGap): the tests do not compare timestamps of the second, and the
// wait would add a second to every first run. TestRepeatedRunsInOtherSeconds
// sets it back.
func TestMain(m *testing.M) {
	runGap = 0
	os.Exit(m.Run())
}
