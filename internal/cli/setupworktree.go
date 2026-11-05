package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// A worktree added with git worktree add has no .driftgate/ (git does not
// carry it): setup takes the main worktree's project there (config.Adopt),
// with its baselines, and never captures a baseline of its own, as the
// worktree's code is the change to compare.

// adopt takes the project of the main worktree (main) into this worktree
// (here) and returns its configuration. Every database of the main
// project must be one this worktree's settings name as their own: a
// pristine copy made of the main worktree's database would replace the
// main worktree's; that stops setup before anything is written.
func (s *setup) adopt(here, main string) (string, error) {
	s.step("project")
	fmt.Fprintf(s.out, "taking the project from the main worktree %s\n", rel(here, main))
	src, err := config.Load(filepath.Join(main, config.LocalDir, config.FileName))
	if err != nil {
		return "", configErr("%v", err)
	}
	// The baseline is the main worktree's code: one made here would be of
	// the change itself, and every later run would pass against it.
	if _, err := os.Stat(filepath.Join(src.Layout.Baselines, "local")); err != nil {
		return "", configErr("the main worktree's project (%s) has no baseline yet: make it there first (driftgate setup in %s), then run %s here",
			rel(here, main), main, s.resume)
	}
	found := appconf.Find(here, os.LookupEnv)
	for _, name := range databases(src) {
		c := src.State[name]
		d, known := setting(found, c)
		if c.Driver == config.SQLite {
			if err := s.adoptSQLite(here, main, name, c, found, d, known); err != nil {
				return "", err
			}
			continue
		}
		switch {
		case !known || d.Name == "":
			return "", configErr("state %s: this worktree's settings do not say which database it runs on, and the main worktree's is %s: a pristine copy made here could replace the main worktree's. Give this worktree a database of its own in its settings (e.g. DB_DATABASE in .env), then run %s",
				name, c.Working, s.resume)
		case sameDatabase(c.Driver, d.Name, c.Working):
			return "", configErr("state %s: this worktree's settings name the main worktree's database %s; its runs would restore that database while the main worktree uses it. Give this worktree a database of its own (a copy of %s, e.g. DB_DATABASE=%s_wt in .env), then run %s",
				name, c.Working, c.Working, c.Working, s.resume)
		}
	}
	p, err := config.Adopt(main, here)
	if err != nil {
		return "", configErr("%v", err)
	}
	s.adopted = rel(here, main)
	fmt.Fprintf(s.out, "took %s: its configuration, catalog, allowlist, routes, baselines and key\n", rel(p.Root, p.Layout.Dir))
	return p.Layout.Config, s.ownAddress(p)
}

// ownAddress offers, for a project taken from the main worktree with one
// target, to make the address this worktree's settings give (its .env's
// APP_URL) base_url, as the worktree's application runs there; --yes
// says yes.
func (s *setup) ownAddress(p *config.Project) error {
	dr := &draft{found: appconf.Find(p.Root, os.LookupEnv)}
	urls := rankURLs(dr.usableURLs())
	if len(p.Targets) > 0 || len(urls) == 0 || urls[0].URL == p.Target.BaseURL {
		return nil
	}
	u := urls[0]
	path := rel(p.Root, p.Layout.Config)
	ok, err := s.a.confirm(fmt.Sprintf("%s %s gives this worktree's address %s; the main worktree's is %s. Make it base_url in %s?", u.File, u.Key, u.URL, p.Target.BaseURL, path),
		"pass --yes to change it")
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintf(s.out, "base_url stays %s\n", p.Target.BaseURL)
		}
		return err
	}
	if err := setValue(p.Layout.Config, u.URL, "target", "base_url"); err != nil {
		return configErr("%s: %v; set target.base_url to %s by hand, then run %s", path, err, u.URL, s.resume)
	}
	fmt.Fprintf(s.out, "base_url %s (%s %s)\n", u.URL, u.File, u.Key)
	return nil
}

// setValue sets the scalar at keys of the configuration file to v,
// changing only that value.
func setValue(path, v string, keys ...string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	n := doc.Content[0]
	for _, k := range keys {
		n = mapValue(n, k)
	}
	if n == nil {
		return errors.New("no " + strings.Join(keys, "."))
	}
	lines := strings.Split(string(raw), "\n")
	if err := replaceScalar(lines, n, v); err != nil {
		return err
	}
	return snapshot.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// ownDatabase offers, for every state database whose setting names
// another database than working (a worktree on its own database, a
// changed .env), to make working and pristine that database in the
// configuration, comments kept; --yes says yes. Refused, setup stops:
// every command refuses a working database the application does not use.
func (s *setup) ownDatabase(p *config.Project, path string) (*config.Project, error) {
	found := appconf.Find(p.Root, os.LookupEnv)
	changed := false
	for _, name := range databases(p) {
		c := p.State[name]
		d, known := setting(found, c)
		if c.Driver == config.SQLite {
			if known {
				own, err := s.ownSQLiteFile(p, path, name, c, d)
				if err != nil {
					return nil, err
				}
				changed = changed || own
			}
			continue
		}
		if !known || d.Name == "" || d.Name == c.Working || controls(d.Name) {
			continue
		}
		s.step("working database")
		q := fmt.Sprintf("%s names the database %s, and state %s works on %s. Make %s the working database (pristine: %s_pristine) in %s?",
			where(d), d.Name, name, c.Working, d.Name, d.Name, rel(p.Root, path))
		ok, err := s.a.confirm(q, "pass --yes to change it")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, configErr("state %s stays on %s, which the application does not use: set working: %s (and pristine:) in %s, or change %s; then run %s",
				name, c.Working, d.Name, rel(p.Root, path), where(d), s.resume)
		}
		if err := setWorking(path, name, d.Name); err != nil {
			return nil, configErr("%s: %v; set working: %s (and pristine:) by hand, then run %s", rel(p.Root, path), err, d.Name, s.resume)
		}
		fmt.Fprintf(s.out, "state %s: working %s, pristine %s_pristine\n", name, d.Name, d.Name)
		changed = true
	}
	if !changed {
		return p, nil
	}
	q, err := config.Load(path)
	if err != nil {
		return nil, configErr("%v", err)
	}
	return q, nil
}

// setWorking sets working: of the state component name in the
// configuration file to db, and pristine: (when the file sets it) to
// db_pristine, changing only those values: comments and layout stay.
func setWorking(path, name, db string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	comp := mapValue(mapValue(doc.Content[0], "state"), name)
	working := mapValue(comp, "working")
	if working == nil {
		return errors.New("no working: in state " + name)
	}
	lines := strings.Split(string(raw), "\n")
	for _, v := range []struct {
		n   *yaml.Node
		new string
	}{{working, db}, {mapValue(comp, "pristine"), db + "_pristine"}} {
		if v.n == nil {
			continue
		}
		if err := replaceScalar(lines, v.n, v.new); err != nil {
			return err
		}
	}
	return snapshot.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// mapValue is the value of key in the mapping n; nil when there is none.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// replaceScalar writes v in place of the scalar n on its line, as it was
// written there (plain or quoted).
func replaceScalar(lines []string, n *yaml.Node, v string) error {
	if n.Kind != yaml.ScalarNode || n.Line < 1 || n.Line > len(lines) {
		return errors.New("working: and pristine: must be plain values")
	}
	line, col := lines[n.Line-1], n.Column-1
	for _, written := range []string{scalar(n.Value), "'" + n.Value + "'", `"` + n.Value + `"`, n.Value} {
		if col >= 0 && strings.HasPrefix(line[col:], written) {
			lines[n.Line-1] = line[:col] + scalar(v) + line[col+len(written):]
			return nil
		}
	}
	return fmt.Errorf("line %d: %s is not written as setup can change it", n.Line, n.Value)
}
