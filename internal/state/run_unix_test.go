//go:build unix

package state

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunTimeoutWithChild: a restore whose child keeps its output open
// stops at the time limit instead of waiting for the child, and only the
// end of a long output is kept for the message.
func TestRunTimeoutWithChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := Run(ctx, t.TempDir(), []string{"sh", "-c", "sleep 30 & sleep 30"}, nil, nil); err == nil {
		t.Fatal("no error after the time limit")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %s", took)
	}
	var log bytes.Buffer
	err := Run(context.Background(), t.TempDir(), []string{"sh", "-c", "yes line | head -c 1000000; echo last; exit 1"}, nil, &log)
	if err == nil || !strings.HasSuffix(strings.TrimSpace(log.String()), "last") || log.Len() > 64<<10 {
		t.Fatalf("%v, %d bytes logged", err, log.Len())
	}
}
