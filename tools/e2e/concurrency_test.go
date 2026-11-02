//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTwoCheckouts: two checkouts of a project (worktrees) share the local
// store and the working database. Commands of both run at the same time:
// the second waits for the store while the first holds it in the middle
// of its capture (held there, so they always overlap), and each gets the
// result it would get alone: had they interleaved, the second's restore
// would undo the first's writes, and the FAQ entries would get other IDs.
func TestTwoCheckouts(t *testing.T) {
	a := mysqlProject(t, "two")
	b := newProject(t, a.url, "state:\n"+mysqlState("two"), stateCatalog, a.store)
	write(t, filepath.Join(b.root, "driftgate", "allowlist.yaml"), allowDate+allowCreated)
	b.env = a.env

	both := func(argsA, argsB []string) (result, result) {
		t.Helper()
		return together(t, a.project, b, "capture.record#2", argsA, argsB)
	}

	a.must(t, 0, "seed")
	// The first runs make a baseline in each checkout from two captures.
	if ra, rb := both([]string{"run"}, []string{"run"}); ra.code != 0 || rb.code != 0 {
		t.Fatalf("first runs:\na: %v\nb: %v", ra, rb)
	}
	// The same code: no drift in either.
	if ra, rb := both([]string{"run"}, []string{"run"}); ra.code != 0 || rb.code != 0 {
		t.Fatalf("runs:\na: %v\nb: %v", ra, rb)
	}
	// Planted drift: both see it; a accepts it while b compares again.
	a.api.SetVersion("v2")
	if ra, rb := both([]string{"run"}, []string{"run"}); ra.code != 1 || rb.code != 1 {
		t.Fatalf("runs on v2:\na: %v\nb: %v", ra, rb)
	}
	if ra, rb := both([]string{"capture", "c1"}, []string{"accept", "*", "--reason", "v2"}); ra.code != 0 || rb.code != 0 {
		t.Fatalf("capture and accept:\na: %v\nb: %v", ra, rb)
	}
	b.must(t, 0, "run")
	a.must(t, 1, "run")
	a.clean(t)
	b.clean(t)
}

// together runs driftgate in a, held at point, then in b until it waits
// for the store, then lets a go on; it returns both results.
func together(t *testing.T, a, b *project, point string, argsA, argsB []string) (result, result) {
	t.Helper()
	hold := filepath.Join(t.TempDir(), "go")
	ra := start(t, a.root, append(a.env, "DG_E2E_CRASH="+point+":HOLD", "DG_E2E_HOLD="+hold), argsA...)
	await(t, "a to hold", func() bool { _, err := os.Stat(hold + ".reached"); return err == nil })
	rb := start(t, b.root, b.env, argsB...)
	await(t, "b to wait", func() bool {
		raw, _ := os.ReadFile(rb.stderr)
		return strings.Contains(string(raw), "is using the store")
	})
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return ra.wait(t), rb.wait(t)
}

// TestAcceptWhileRun: in one checkout, a run started while an accept
// writes the new baseline waits for it, and then compares with the new
// baseline; it does not take the accept's copy for a leftover.
func TestAcceptWhileRun(t *testing.T) {
	p := memoryProject(t)
	p.must(t, 0, "run")
	p.api.SetVersion("v2")
	p.must(t, 1, "run")
	ra, rr := together(t, p, p, "snapshot.rename#1", []string{"accept", "*", "--reason", "v2"}, []string{"run"})
	if ra.code != 0 || rr.code != 0 {
		t.Fatalf("accept: %v\nrun: %v", ra, rr)
	}
	p.clean(t)
}
