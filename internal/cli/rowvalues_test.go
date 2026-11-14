package cli

import (
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// rowSet is a set with one record whose request inserted a tracked row,
// captured with the row values named under id (empty: none recorded),
// and the tables learned from two captures.
func rowSet(t *testing.T, id string, values map[string]string, learned ...string) *snapshot.Set {
	t.Helper()
	s := &snapshot.Set{Dir: t.TempDir()}
	if err := s.WriteMeta(snapshot.Meta{Tag: "x", RowTracking: []string{"db/table:follows"}, RowValues: id}); err != nil {
		t.Fatal(err)
	}
	rec := &snapshot.Record{ID: "follow", Status: 204, Headers: [][2]string{}, Body: snapshot.Body{Storage: snapshot.Public},
		SideEffects: &snapshot.SideEffects{Items: []string{"db/table:follows"}, Rows: []snapshot.RowChange{{Item: "db/table:follows", Key: "id=1", Change: "inserted", Values: values}},
			Learned: learned}}
	if err := s.Write(rec, []byte{}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRowValuesKey: two sets whose row values were named under different
// keys are not compared (an environment error that says what to do);
// one that recorded none is, and the result names what it left out.
func TestRowValuesKey(t *testing.T) {
	v := map[string]string{"target": "x"}
	a, b, none := rowSet(t, "k1", v, "db/table:follows"), rowSet(t, "k2", v, "db/table:follows"), rowSet(t, "", nil)
	err := rowValuesKey(namedSet{a, "baseline:local"}, namedSet{b, "capture:run"})
	if err == nil || classify(err) != ExitEnv || !strings.Contains(err.Error(), "different project keys") || !strings.Contains(err.Error(), secret.EnvKey) {
		t.Errorf("another key: %v", err)
	}
	for _, pair := range [][2]*snapshot.Set{{a, a}, {a, none}, {none, a}} {
		if err := rowValuesKey(namedSet{pair[0], "baseline:local"}, namedSet{pair[1], "capture:run"}); err != nil {
			t.Errorf("one key or none: %v", err)
		}
	}
	if g := rowValuesGap(a, a, "baseline:local", []string{"follow"}); g != nil {
		t.Errorf("learned on both: %+v", g)
	}
	if g := rowValuesGap(none, a, "baseline:local", []string{"follow"}); g == nil || g.Count != 1 || g.Names[0] != "follow" || !strings.Contains(g.Reason, "baseline:local recorded no row values: make it again") {
		t.Errorf("a baseline without values: %+v", g)
	}
	if g := rowValuesGap(a, none, "baseline:local", []string{"follow"}); g == nil || !strings.Contains(g.Reason, "no project key") {
		t.Errorf("a capture without values: %+v", g)
	}
	// A record whose values two captures did not see (captured once, or
	// accepted from one capture) has them not compared, and the result
	// says how to learn them.
	once := rowSet(t, "k1", v)
	if g := rowValuesGap(once, a, "baseline:local", []string{"follow"}); g == nil || !strings.Contains(g.Reason, "--repeat 2, then driftgate baselines rebase") {
		t.Errorf("not learned: %+v", g)
	}
	// A record accepted from a capture without the key, in a baseline
	// that recorded values: not learned, so named too.
	if g := rowValuesGap(rowSet(t, "k1", nil), a, "baseline:local", []string{"follow"}); g == nil || g.Count != 1 {
		t.Errorf("accepted without the key: %+v", g)
	}
}

// TestLearning: two captures learn the tables both recorded values of; a
// record accepted from one capture keeps the learned tables it still
// writes values into and the volatile columns, and learns no new table
// nor one whose values it did not record (no key).
func TestLearning(t *testing.T) {
	v := map[string]string{"c": "x"}
	row := func(item string, values map[string]string) snapshot.RowChange {
		return snapshot.RowChange{Item: item, Key: "id=1", Change: "inserted", Values: values}
	}
	rows := map[string]*rowLearning{}
	first := rowSet(t, "k", v)
	if err := learnRows(rows, "follow", first, rowSet(t, "k", v)); err != nil || strings.Join(rows["follow"].Learned, ",") != "db/table:follows" {
		t.Fatalf("learned %+v, %v", rows["follow"], err)
	}
	if err := learnRows(rows, "follow", first, rowSet(t, "k", nil)); err != nil || len(rows["follow"].Learned) != 0 {
		t.Errorf("a third capture without values: %+v, %v", rows["follow"], err)
	}

	old := &snapshot.SideEffects{Learned: []string{"db/table:a", "db/table:b"}, Volatile: []string{"db/table:a.at"}}
	rec := &snapshot.SideEffects{Rows: []snapshot.RowChange{row("db/table:a", v), row("db/table:b", nil), row("db/table:c", v)}}
	acceptLearning(old, rec)
	if strings.Join(rec.Learned, ",") != "db/table:a" || strings.Join(rec.Volatile, ",") != "db/table:a.at" {
		t.Errorf("accepted from one capture: %+v", rec)
	}
	fresh := &snapshot.SideEffects{Rows: []snapshot.RowChange{row("db/table:c", v)}}
	acceptLearning(nil, fresh)
	if len(fresh.Learned) != 0 {
		t.Errorf("a new request: %+v", fresh)
	}
	own := &snapshot.SideEffects{Rows: []snapshot.RowChange{row("db/table:c", v)}, Learned: []string{"db/table:c"}}
	acceptLearning(old, own)
	if strings.Join(own.Learned, ",") != "db/table:c" || len(own.Volatile) != 0 {
		t.Errorf("accepted from two captures: %+v", own)
	}
}

// TestRebaseRowValues: a capture whose row values were named under
// another key than the project key does not become the baseline.
func TestRebaseRowValues(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "driftgate.yaml"), "version: 1\nproject: rowvalues\nkey: { env: DG_TEST_ROWKEY }\ntarget:\n  base_url: http://127.0.0.1:1\n")
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv("DG_TEST_ROWKEY", base64.StdEncoding.EncodeToString(k))
	p, err := config.Load(filepath.Join(root, "driftgate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	own := secret.Key(k).RowValues().ID()
	if err := rebaseRowValues(p, nil, &snapshot.Meta{RowValues: own}, "c"); err != nil {
		t.Errorf("the project key: %v", err)
	}
	if err := rebaseRowValues(p, nil, &snapshot.Meta{}, "c"); err != nil {
		t.Errorf("no values: %v", err)
	}
	if err := rebaseRowValues(p, nil, &snapshot.Meta{RowValues: "other"}, "c"); err == nil || classify(err) != ExitEnv || !strings.Contains(err.Error(), "another key than the project key") {
		t.Errorf("another key: %v", err)
	}
	// Without the key at hand, the baseline's key decides.
	t.Setenv("DG_TEST_ROWKEY", "")
	p, _ = config.Load(filepath.Join(root, "driftgate.yaml"))
	if err := rebaseRowValues(p, &snapshot.Meta{RowValues: own}, &snapshot.Meta{RowValues: "other"}, "c"); err == nil || !strings.Contains(err.Error(), "the baseline's key") {
		t.Errorf("another key than the baseline's: %v", err)
	}
}

// TestValuesOnly: only a row that changed alike with other values is
// learned as volatile from two captures of one state; another kind or
// other columns still stop the baseline.
func TestValuesOnly(t *testing.T) {
	if !valuesOnly(compare.Drift{Kind: compare.KindSideEffect, RowValues: []string{"db/table:follows.touched"}}) {
		t.Error("values only")
	}
	if valuesOnly(compare.Drift{Kind: compare.KindSideEffect, Baseline: "inserted", Current: "updated target"}) {
		t.Error("another kind")
	}
	if got := mergeVolatile([]string{"b", "a"}, []string{"a", "c"}); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("merge %v", got)
	}
}
