package capture

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the gap between writes on one state (writeGap): the
// tests capture many writes, and only TestWriteGap is about the gap.
func TestMain(m *testing.M) {
	writeGap = 10 * time.Millisecond
	os.Exit(m.Run())
}
