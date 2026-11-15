package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
)

// TestLaravelPrepare: with state: on, setup writes prepare: with Laravel's
// migrate command, so a migration run since the seed is made again after
// every restore; with state: commented out, or for another framework,
// prepare: stays a comment. The configuration loads.
func TestLaravelPrepare(t *testing.T) {
	const prepare = "prepare:                                    # after every restore\n  - [\"php\", \"artisan\", \"migrate\", \"--force\"]"
	for _, c := range []struct {
		kind   string
		enable bool
		want   bool
	}{{"laravel", true, true}, {"laravel", false, false}, {"rails", true, false}} {
		root := t.TempDir()
		dr := &draft{p: &config.Project{Root: root, Config: config.Config{Project: "p"}}, d: detected{kinds: []string{c.kind}},
			db:     &appconf.Database{Driver: appconf.MySQL, Name: "shop", Ref: appconf.Ref{Source: appconf.Dotenv, File: ".env", Key: "DB_DATABASE"}},
			enable: c.enable, tablesAsked: true}
		text := dr.configText()
		if strings.Contains(text, prepare) != c.want {
			t.Errorf("%s, state on %v: prepare: written %v, want %v:\n%s", c.kind, c.enable, !c.want, c.want, text)
		}
		path := filepath.Join(root, "driftgate.yaml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := config.Load(path)
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.kind, err, text)
		}
		if got := len(p.Prepare) > 0; got != c.want {
			t.Errorf("%s, state on %v: prepare: %v", c.kind, c.enable, p.Prepare)
		}
	}
}
