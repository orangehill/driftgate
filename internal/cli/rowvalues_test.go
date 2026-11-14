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
// captured with the row values named under id (empty: none recorded).
func rowSet(t *testing.T, id string, agreed int, values map[string]string) *snapshot.Set {
	t.Helper()
	s := &snapshot.Set{Dir: t.TempDir()}
	if err := s.WriteMeta(snapshot.Meta{Tag: "x", RowTracking: []string{"db/table:follows"}, RowValues: id, Agreed: agreed}); err != nil {
		t.Fatal(err)
	}
	rec := &snapshot.Record{ID: "follow", Status: 204, Headers: [][2]string{}, Body: snapshot.Body{Storage: snapshot.Public},
		SideEffects: &snapshot.SideEffects{Items: []string{"db/table:follows"}, Rows: []snapshot.RowChange{{Item: "db/table:follows", Key: "id=1", Change: "inserted", Values: values}}}}
	if err := s.Write(rec, []byte{}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRowValuesKey: two sets whose row values were named under different
// keys are not compared (an environment error that says what to do);
// one that recorded none is, and the result names what it left out.
func TestRowValuesKey(t *testing.T) {
	a, b, none := rowSet(t, "k1", 2, map[string]string{"target": "x"}), rowSet(t, "k2", 2, map[string]string{"target": "y"}), rowSet(t, "", 2, nil)
	err := rowValuesKey(namedSet{a, "baseline:local"}, namedSet{b, "capture:run"})
	if err == nil || classify(err) != ExitEnv || !strings.Contains(err.Error(), "different project keys") || !strings.Contains(err.Error(), secret.EnvKey) {
		t.Errorf("another key: %v", err)
	}
	for _, pair := range [][2]*snapshot.Set{{a, a}, {a, none}, {none, a}} {
		if err := rowValuesKey(namedSet{pair[0], "baseline:local"}, namedSet{pair[1], "capture:run"}); err != nil {
			t.Errorf("one key or none: %v", err)
		}
	}
	if g := rowValuesGap(a, a, []string{"follow"}); g != nil {
		t.Errorf("both recorded values: %+v", g)
	}
	if g := rowValuesGap(none, a, []string{"follow"}); g == nil || g.Count != 1 || g.Names[0] != "follow" || !strings.Contains(g.Reason, "make the baseline again") {
		t.Errorf("a baseline without values: %+v", g)
	}
	if g := rowValuesGap(a, none, []string{"follow"}); g == nil || !strings.Contains(g.Reason, "no project key") {
		t.Errorf("a capture without values: %+v", g)
	}
	// A set captured once does not know which columns change by
	// themselves: its values are not compared, and the result says so.
	once := rowSet(t, "k1", 0, map[string]string{"target": "x"})
	if g := rowValuesGap(once, a, []string{"follow"}); g == nil || !strings.Contains(g.Reason, "--repeat 2") {
		t.Errorf("captured once: %+v", g)
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "driftgate.yaml"), "version: 1\nproject: rowvalues\ntarget:\n  base_url: http://127.0.0.1:1\n")
	p, err := config.Load(filepath.Join(root, "driftgate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if s, _, err := setOrigins(p, once, a); err != nil || s == nil || !s.noRowValues {
		t.Errorf("captured once: sides %+v, %v", s, err)
	}
	if s, _, err := setOrigins(p, a, once); err != nil || s != nil {
		t.Errorf("the current side captured once: sides %+v, %v", s, err)
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
