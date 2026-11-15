package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRowTrackingIgnore: row_tracking takes plain names and name:
// {ignore: [columns]} side by side, refuses other keys, and writes back
// as it was read. An ignored column is not checked against the database:
// a migration may drop it.
func TestRowTrackingIgnore(t *testing.T) {
	base := "version: 1\nproject: p\nstate:\n  db:\n    driver: mysql\n    dsn: { env: X }\n    working: app\n    row_tracking: "
	for list, want := range map[string]string{
		"[orders]": "orders:",
		"[orders, follows: {ignore: [updated_at, 'x_*']}]": "orders: follows:updated_at,x_*",
		"\n      - follows:\n          ignore: [gone]":     "follows:gone",
		"[follows: {skip: [a]}]":                           "error: takes ignore, not \"skip\"",
		"[follows: {ignore: ['']}]":                        "error: an empty column",
		"[[a]]":                                            "error: a row_tracking table is a name",
	} {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(base+list+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := Load(path)
		if e, ok := strings.CutPrefix(want, "error: "); ok {
			if err == nil || !strings.Contains(err.Error(), e) {
				t.Errorf("%s: err %v, want %s", list, err, e)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", list, err)
		}
		rt := p.State["db"].RowTracking
		var got []string
		for _, r := range rt {
			got = append(got, r.Name+":"+strings.Join(r.Ignore, ","))
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s: %q, want %q", list, got, want)
		}
		raw, err := yaml.Marshal(rt)
		if err != nil {
			t.Fatal(err)
		}
		var back RowTables
		if err := yaml.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, rt) {
			t.Errorf("%s: written as %s, read back %+v (%v)", list, raw, back, err)
		}
	}
	if got := Tables("a", "b").Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("names %v", got)
	}
}
