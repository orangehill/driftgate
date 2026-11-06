package cli

import (
	"path/filepath"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestCompareSameFolderInTwoHomes: a baseline and a capture that both
// record ~/Sites/shop, made in two users' home folders, compare their
// stack traces as the same: equal records of the home folder are no
// reason to compare the bodies as they are.
func TestCompareSameFolderInTwoHomes(t *testing.T) {
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values", Target: config.Target{BaseURL: "http://127.0.0.1:8081"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trace := func(home string) []byte {
		return []byte(`{"message":"Server Error","file":"` + home + `/Sites/shop/app/A.php","line":12}`)
	}
	sets := map[string]*snapshot.Set{}
	for name, home := range map[string]string{"base": "/Users/alice", "cur": "/home/bob"} {
		s := &snapshot.Set{Dir: filepath.Join(dir, name)}
		if err := s.WriteMeta(snapshot.Meta{Tag: name, Origins: map[string]string{"": "http://127.0.0.1:8081"}, Roots: []string{"~/Sites/shop"}}); err != nil {
			t.Fatal(err)
		}
		rec := &snapshot.Record{ID: "news", Status: 500, Headers: [][2]string{{"content-type", "application/json"}}, Body: snapshot.Body{Storage: snapshot.Public}}
		if err := s.Write(rec, trace(home)); err != nil {
			t.Fatal(err)
		}
		sets[name] = s
	}
	sides, _, err := setOrigins(p, sets["base"], sets["cur"])
	if err != nil || sides == nil {
		t.Fatalf("sides of two captures in two homes: %+v %v", sides, err)
	}
	res, err := compareOne(catalog.Request{ID: "news", Method: "GET", Path: "/news"}, sets["base"], sets["cur"], sides, allowlist.Empty(), compare.LevelValues, true)
	if err != nil || len(res.Drifts) != 0 {
		t.Fatalf("drifts %+v %v", res.Drifts, err)
	}
}
