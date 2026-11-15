package cli

import (
	"path/filepath"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestResentCountedWhenNotCompared: a request a capture sent again after a
// restore is counted also when it is observed or has no baseline.
func TestResentCountedWhenNotCompared(t *testing.T) {
	dir := t.TempDir()
	base := &snapshot.Set{Dir: filepath.Join(dir, "base")}
	cur := &snapshot.Set{Dir: filepath.Join(dir, "cur")}
	resent := &snapshot.Resent{After: "the restore of db ended 2 database sessions of the application", First: 500}
	rec := func(id string, again *snapshot.Resent) *snapshot.Record {
		return &snapshot.Record{ID: id, Status: 200, Headers: [][2]string{}, Body: snapshot.Body{Storage: snapshot.Public}, Resent: again}
	}
	for _, w := range []struct {
		set *snapshot.Set
		rec *snapshot.Record
	}{
		{base, rec("observed", nil)}, {cur, rec("observed", resent)},
		{cur, rec("new", resent)},
	} {
		if err := w.set.Write(w.rec, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	for _, req := range []catalog.Request{{ID: "observed", Method: "GET", Path: "/o", Observe: true}, {ID: "new", Method: "GET", Path: "/n"}} {
		res, err := compareOne(req, base, cur, nil, nil, compare.LevelValues, false)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Resent || len(res.Notes) != 1 {
			t.Errorf("%s: %s, resent %t, notes %q", req.ID, res.Outcome, res.Resent, res.Notes)
		}
	}
}
