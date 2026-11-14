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

// TestLearning: two captures learn the tables both recorded values of,
// and a third without values unlearns them; a gap left by a record
// without values names the missing key.
func TestLearning(t *testing.T) {
	v := map[string]string{"c": "x"}
	rows := map[string]*rowLearning{}
	first := rowSet(t, "k", v)
	if err := learnRows(rows, "follow", first, rowSet(t, "k", v)); err != nil || strings.Join(rows["follow"].Learned, ",") != "db/table:follows" {
		t.Fatalf("learned %+v, %v", rows["follow"], err)
	}
	if err := learnRows(rows, "follow", first, rowSet(t, "k", nil)); err != nil || len(rows["follow"].Learned) != 0 {
		t.Errorf("a third capture without values: %+v, %v", rows["follow"], err)
	}

	// A baseline record accepted from a capture without the key: the
	// result names that cause.
	if g := rowValuesGap(rowSet(t, "k1", nil), rowSet(t, "k1", v), "baseline:local", []string{"follow"}); g == nil || !strings.Contains(g.Reason, "without the project key") {
		t.Errorf("accepted without the key: %+v", g)
	}
}

// TestAcceptLearning: a record accepted from a capture made once keeps a
// table's learning (and its volatile columns) only when it changed the
// same rows the same way with their values; one accepted from a capture
// two runs agreed on keeps its own.
func TestAcceptLearning(t *testing.T) {
	v := map[string]string{"c": "x"}
	upd := func(item string, values map[string]string, cols ...string) snapshot.RowChange {
		return snapshot.RowChange{Item: item, Key: "id=1", Change: "updated", Columns: cols, Values: values}
	}
	old := func() *snapshot.SideEffects {
		return &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:orders", v, "status", "updated_at"), upd("db/table:users", v, "name")},
			Learned: []string{"db/table:orders", "db/table:users"}, Volatile: []string{"db/table:orders.updated_at", "db/table:users.seen"}}
	}
	cases := []struct {
		name     string
		rec      *snapshot.SideEffects
		agreed   bool
		learned  string
		volatile string
	}{
		{"the same rows", &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:orders", v, "status", "updated_at")}}, false, "db/table:orders", "db/table:orders.updated_at"},
		// The reviewer's case: an insert instead of the update.
		{"an insert", &snapshot.SideEffects{Rows: []snapshot.RowChange{{Item: "db/table:orders", Key: "id=2", Change: "inserted", Values: v}}}, false, "", ""},
		{"a new column", &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:orders", v, "note", "status", "updated_at")}}, false, "", ""},
		{"another row", &snapshot.SideEffects{Rows: []snapshot.RowChange{{Item: "db/table:orders", Key: "id=9", Change: "updated", Columns: []string{"status", "updated_at"}, Values: v}}}, false, "", ""},
		{"no values (no key)", &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:orders", nil, "status", "updated_at")}}, false, "", ""},
		{"a new table", &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:notes", v, "body")}}, false, "", ""},
		{"agreed: its own", &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:notes", v, "body")}, Learned: []string{"db/table:notes"}, Volatile: []string{"db/table:notes.at"}}, true, "db/table:notes", "db/table:notes.at"},
		{"agreed: learned nothing", &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:orders", v, "status", "updated_at")}}, true, "", ""},
	}
	for _, c := range cases {
		acceptLearning(old(), c.rec, c.agreed)
		if strings.Join(c.rec.Learned, ",") != c.learned || strings.Join(c.rec.Volatile, ",") != c.volatile {
			t.Errorf("%s: learned %v, volatile %v", c.name, c.rec.Learned, c.rec.Volatile)
		}
	}
	fresh := &snapshot.SideEffects{Rows: []snapshot.RowChange{upd("db/table:orders", v, "status")}}
	acceptLearning(nil, fresh, false)
	if len(fresh.Learned) != 0 {
		t.Errorf("a new request: %+v", fresh)
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
