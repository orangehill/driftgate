//go:build unix

package e2e

import (
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/demo"
)

// The crash tests stop driftgate at a chaos point (internal/chaos) with
// SIGKILL, as the system's out-of-memory killer or a CI timeout does, or
// with SIGTERM, and check that the next command finds everything as it
// was or recovers it. A few cases run with every go test; the whole matrix
// with DRIFTGATE_CHAOS=full (the cases on databases need them:
// DRIFTGATE_TEST_MYSQL, DRIFTGATE_TEST_POSTGRES).

const killed = 128 + 9 // the exit code of a process SIGKILL ended

// full runs the whole matrix, not only its first cases (DRIFTGATE_CHAOS=full).
func full(t *testing.T) {
	if os.Getenv("DRIFTGATE_CHAOS") != "full" {
		t.Skip("DRIFTGATE_CHAOS=full runs the whole crash matrix")
	}
}

// TestCrashCapture: a capture that replaces another is killed while it
// runs. The next capture of the tag puts the previous one back, byte for
// byte, also when it then refuses to replace it without --force; nothing
// written half way stays in the store.
func TestCrashCapture(t *testing.T) {
	for _, tc := range []struct {
		point string
		short bool // in every go test run
	}{
		{"capture.started", true},
		{"capture.record#3", true},
		{"snapshot.rename#5", true},
		{"capture.record#9", false}, // the last record, the private one
		{"snapshot.rename#11", false},
	} {
		t.Run(tc.point, func(t *testing.T) {
			if !tc.short {
				full(t)
			}
			p := memoryProject(t)
			p.must(t, 0, "capture", "c1")
			before := files(t, p.capture("c1"))
			if r := p.run(t, tc.point, "capture", "c1", "--force"); r.code != killed {
				t.Fatalf("not killed at %s: %v", tc.point, r)
			}
			r := p.must(t, 2, "capture", "c1")
			if !strings.Contains(r.out, "put back the previous capture c1") {
				t.Errorf("no word of the previous capture: %v", r)
			}
			if after := files(t, p.capture("c1")); !maps.Equal(before, after) {
				t.Errorf("previous capture changed:\nbefore %v\nafter  %v", before, after)
			}
			p.clean(t)
			p.must(t, 0, "capture", "c1", "--force")
			p.clean(t)
		})
	}
}

// TestCrashCaptureComplete: killed once the new capture is complete, before
// the previous one is removed: the new one stands, whole (its timings
// too), and the next capture removes the previous one.
func TestCrashCaptureComplete(t *testing.T) {
	p := memoryProject(t)
	p.must(t, 0, "capture", "c1")
	if r := p.run(t, "capture.complete", "capture", "c1", "--force"); r.code != killed {
		t.Fatalf("not killed: %v", r)
	}
	if _, err := os.Stat(filepath.Join(p.capture("c1"), "timings.json")); err != nil {
		t.Errorf("a complete capture without its timings: %v", err)
	}
	p.must(t, 2, "capture", "c1")
	p.must(t, 0, "compare", "c1", "--against", "c1")
	p.clean(t)
}

// TestTermCapture: SIGTERM (a cancelled CI job, docker stop) stops a
// capture cleanly: exit 3, the previous capture back in place at once,
// nothing left behind.
func TestTermCapture(t *testing.T) {
	p := memoryProject(t)
	p.must(t, 0, "capture", "c1")
	before := files(t, p.capture("c1"))
	r := p.run(t, "capture.record#2:TERM", "capture", "c1", "--force")
	if r.code != 3 || !strings.Contains(r.err, "terminated: stopping and cleaning up") {
		t.Fatalf("want exit 3 after cleaning up: %v", r)
	}
	if after := files(t, p.capture("c1")); !maps.Equal(before, after) {
		t.Errorf("previous capture changed:\nbefore %v\nafter  %v", before, after)
	}
	p.clean(t)
}

// TestCrashPrivateBody: killed while it writes a private body into the
// local store (here the first capture of the tag, so nothing is put back).
// The half-written file is removed by the next command that takes the
// store, before that command ends: here an accept that stops at once, as
// no comparison was made.
func TestCrashPrivateBody(t *testing.T) {
	srv := httptest.NewServer(demo.New("v1"))
	defer srv.Close()
	p := newProject(t, srv.URL, "", `{"version":1,"requests":[{"id":"contact","method":"GET","path":"/api/contact","storage":"private"}]}`, "")
	// 1: the meta of the new capture, 2: the body, 3: its record.
	if r := p.run(t, "snapshot.rename#2", "capture", "c1"); r.code != killed {
		t.Fatalf("not killed: %v", r)
	}
	left := leftovers(t, filepath.Join(p.store, "private"))
	if len(left) != 1 {
		t.Fatalf("want the body being written in the store, found %v", left)
	}
	// Killed a while ago: a file younger than a few seconds may be the
	// next command's own (file times are coarse) and is left to a later one.
	ago := time.Now().Add(-time.Minute)
	if err := os.Chtimes(left[0], ago, ago); err != nil {
		t.Fatal(err)
	}
	p.must(t, 2, "accept", "contact", "--reason", "r")
	p.clean(t)
	p.must(t, 0, "capture", "c1") // no record was written: the tag is free
}

// TestCrashAccept: an accept killed while it writes the new baseline (a
// copy of the old one, swapped in at the end). Killed before the swap, the
// baseline is as it was and the next command removes the copy; killed in
// the swap, the old baseline is whole in .tmp-old-*/baseline and every
// command stops until it is restored (no baseline is made again from the
// code under test).
func TestCrashAccept(t *testing.T) {
	for _, tc := range []struct {
		point string
		short bool
	}{
		{"snapshot.rename#2", true}, // a record of the copy
		{"baseline.swap", true},
		{"snapshot.rename#1", false},
	} {
		t.Run(tc.point, func(t *testing.T) {
			if !tc.short {
				full(t)
			}
			p := memoryProject(t)
			p.must(t, 0, "run")
			p.api.SetVersion("v2")
			p.must(t, 1, "run")
			before := files(t, p.baseline("local"))
			if r := p.run(t, tc.point, "accept", "*", "--reason", "v2"); r.code != killed {
				t.Fatalf("not killed at %s: %v", tc.point, r)
			}
			if tc.point == "baseline.swap" {
				r := p.must(t, 3, "run")
				if !strings.Contains(r.err, ".tmp-old-") {
					t.Fatalf("no word of the old baseline: %v", r)
				}
				old, _ := filepath.Glob(filepath.Join(p.root, "driftgate", "baselines", ".tmp-old-*", "baseline"))
				if len(old) != 1 || !maps.Equal(files(t, old[0]), before) {
					t.Fatalf("the old baseline is not whole in %v", old)
				}
				// What the message says: put it back, remove the rest.
				if err := os.Rename(old[0], p.baseline("local")); err != nil {
					t.Fatal(err)
				}
				left, _ := filepath.Glob(filepath.Join(p.root, "driftgate", "baselines", ".tmp-*"))
				for _, dir := range left {
					os.RemoveAll(dir)
				}
			}
			p.must(t, 1, "run")
			if after := files(t, p.baseline("local")); !maps.Equal(before, after) {
				t.Errorf("baseline changed:\nbefore %v\nafter  %v", before, after)
			}
			p.clean(t)
			p.must(t, 0, "accept", "*", "--reason", "v2")
			p.must(t, 0, "run")
			p.must(t, 0, "verify")
			p.clean(t)
		})
	}
}

// TestCrashFirstRun: the first run, killed while it copies its capture to
// make the baseline: the next run removes the copy and makes the baseline.
func TestCrashFirstRun(t *testing.T) {
	p := memoryProject(t)
	if r := p.run(t, "baseline.copy", "run"); r.code != killed {
		t.Fatalf("not killed: %v", r)
	}
	r := p.must(t, 0, "run")
	if !strings.Contains(r.out, "baseline local made") {
		t.Errorf("no baseline made: %v", r)
	}
	p.must(t, 0, "run")
	p.clean(t)
}
