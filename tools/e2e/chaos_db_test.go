//go:build unix

package e2e

import (
	"strings"
	"testing"
)

// TestCrashSeedMySQL: a seed killed while it copies the tables. The
// half-made pristine copy is not trusted: a capture asks for a seed; the
// next seed replaces it (it is marked as DriftGate's), and captures before
// and after the interruption agree.
func TestCrashSeedMySQL(t *testing.T) {
	full(t)
	p := mysqlProject(t, "seed")
	p.must(t, 0, "seed")
	p.must(t, 0, "capture", "c1")
	if r := p.run(t, "state.copy#2", "seed"); r.code != killed {
		t.Fatalf("not killed: %v", r)
	}
	if r := p.must(t, 3, "capture", "c2"); !strings.Contains(r.err, "driftgate seed") {
		t.Errorf("a half-made pristine copy was used: %v", r)
	}
	p.must(t, 0, "seed")
	p.must(t, 0, "capture", "c2", "--force")
	p.must(t, 0, "compare", "c2", "--against", "c1")
	p.clean(t)
}

// TestCrashRestoreMySQL: a capture killed while its first restore copies
// the tables back (SIGKILL), or stopped there (SIGTERM). The next capture
// restores the rest: the application's writes before it are gone, and it
// equals a capture before them.
func TestCrashRestoreMySQL(t *testing.T) {
	for _, point := range []string{"state.copy#1", "state.copy#2:TERM"} {
		t.Run(point, func(t *testing.T) {
			if point != "state.copy#1" {
				full(t)
			}
			p := mysqlProject(t, "restore")
			p.must(t, 0, "seed")
			p.must(t, 0, "capture", "c1")
			// The application writes; the next capture starts from pristine.
			sqlExec(t, p.db, "INSERT INTO faq (created_at) VALUES (NOW()), (NOW())", "UPDATE news SET views = 1000")
			r := p.run(t, point, "capture", "c2")
			if want := map[bool]int{false: killed, true: 3}[strings.HasSuffix(point, ":TERM")]; r.code != want {
				t.Fatalf("want exit %d: %v", want, r)
			}
			p.must(t, 0, "capture", "c2", "--force")
			p.must(t, 0, "compare", "c2", "--against", "c1")
			if n := count(t, p.db, "faq"); n != 0 {
				t.Errorf("%d FAQ rows after the capture, want 0 (pristine)", n)
			}
			p.clean(t)
		})
	}
}

// TestCrashPostgres: PostgreSQL copies a whole database under another name
// and swaps it in. Killed after the copy (before the old database is
// renamed away) or in the swap (old one renamed away, copy not renamed), a
// restore leaves the next capture a working database to restore. A seed
// killed after the copy leaves the old pristine copy and its record, which
// a capture uses; killed in the swap, no pristine copy to trust.
func TestCrashPostgres(t *testing.T) {
	full(t)
	for _, point := range []string{"state.copy", "state.swap"} {
		t.Run("restore/"+point, func(t *testing.T) {
			p := postgresProject(t, "restore")
			p.must(t, 0, "seed")
			sqlExec(t, p.db, "INSERT INTO audit (what) VALUES ('written by the application')")
			if r := p.run(t, point, "capture", "c1"); r.code != killed {
				t.Fatalf("not killed: %v", r)
			}
			p.must(t, 0, "capture", "c1")
			if n := count(t, p.db, "audit"); n != 1 {
				t.Errorf("%d audit rows after the capture, want the pristine 1", n)
			}
			p.clean(t)
		})
		t.Run("seed/"+point, func(t *testing.T) {
			p := postgresProject(t, "seed")
			p.must(t, 0, "seed")
			if r := p.run(t, point, "seed"); r.code != killed {
				t.Fatalf("not killed: %v", r)
			}
			if point == "state.copy" {
				p.must(t, 0, "capture", "c0")
			} else if r := p.must(t, 3, "capture", "c1"); !strings.Contains(r.err, "driftgate seed") {
				t.Errorf("an interrupted seed's copy was used: %v", r)
			}
			p.must(t, 0, "seed")
			p.must(t, 0, "capture", "c1")
			p.clean(t)
		})
	}
}
