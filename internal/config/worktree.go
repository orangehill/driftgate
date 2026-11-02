package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// MainWorktreeProject finds, for a folder of a worktree added with git
// worktree add, the baselines: local project of the main worktree that it
// is a checkout of: .driftgate/ is not in git, so a new worktree has none.
// here is the project folder in this worktree, main the one in the main
// worktree; both are "" when there is no such project, or when this
// worktree has a configuration of its own.
func MainWorktreeProject(dir string) (here, main string) {
	start, err := filepath.Abs(dir)
	if err != nil {
		return "", ""
	}
	top := repository(start)
	common, linked := commonDir(top)
	if !linked || filepath.Base(common) != ".git" {
		return "", "" // the main worktree itself, or a bare repository
	}
	mainTop := filepath.Dir(common)
	for d := start; ; d = filepath.Dir(d) {
		if exists(filepath.Join(d, FileName)) || exists(filepath.Join(d, LocalDir, FileName)) {
			return "", ""
		}
		r, err := filepath.Rel(top, d)
		if err != nil {
			return "", ""
		}
		if m := filepath.Join(mainTop, r); exists(filepath.Join(m, LocalDir, FileName)) {
			return d, m
		}
		if d == top {
			return "", ""
		}
	}
}

// Adopt makes the project of the main worktree (main, as
// MainWorktreeProject finds it) this worktree's (here): its configuration,
// catalog, allowlist, routes, exclusions, classify rules, baselines with
// their acceptance logs, the key, and the private bodies those baselines
// name. Not its captures, pristine records or other private bodies: they
// are of the main worktree's code and databases. Git must ignore here's
// .driftgate/ first (the ignore files are written). The configuration is
// copied as it is, its state: too: a worktree that runs on another
// database must say so in it.
func Adopt(main, here string) (*Project, error) {
	src, err := Load(filepath.Join(main, LocalDir, FileName))
	if err != nil {
		return nil, err
	}
	if existing, err := configIn(here); err != nil || existing != "" {
		if err == nil {
			err = fmt.Errorf("%s already exists", existing)
		}
		return nil, err
	}
	c := src.Config
	c.Baselines = BaselinesLocal
	p, err := New(c, here)
	if err != nil {
		return nil, err
	}
	l := p.Layout
	if err := l.WriteIgnores(); err != nil {
		return nil, err
	}
	if err := l.checkIgnored(true); err != nil {
		return nil, err
	}
	// The configuration goes last: until it is there, this is no project
	// and Adopt can run again.
	for _, name := range projectFiles {
		from, to := filepath.Join(src.Layout.Dir, name), filepath.Join(l.Dir, name)
		fi, err := os.Stat(from)
		if err != nil {
			continue
		}
		if err := os.RemoveAll(to); err != nil {
			return nil, err
		}
		if fi.IsDir() {
			err = copyDir(from, to)
		} else {
			err = copyFile(from, to, fi)
		}
		if err != nil {
			return nil, err
		}
	}
	if exists(src.Layout.Key) && !exists(l.Key) {
		k, err := secret.ReadFile(src.Layout.Key)
		if err != nil {
			return nil, err
		}
		if err := secret.WriteFile(l.Key, k); err != nil {
			return nil, err
		}
	}
	if err := adoptPrivate(src, p); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(src.Layout.Config)
	if err != nil {
		return nil, err
	}
	if err := snapshot.WriteFile(l.Config, raw, 0o644); err != nil {
		return nil, err
	}
	return Load(l.Config)
}

// adoptPrivate copies the private bodies the baselines of p name from the
// store of src.
func adoptPrivate(src, p *Project) error {
	if src.Layout.Private == p.Layout.Private {
		return nil // one store (DRIFTGATE_STORE)
	}
	sets, err := os.ReadDir(p.Layout.Baselines)
	if err != nil {
		return nil // no baselines
	}
	if _, err := p.StoreDir(); err != nil {
		return err
	}
	for _, e := range sets {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		recs, err := (&snapshot.Set{Dir: filepath.Join(p.Layout.Baselines, e.Name())}).Records()
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.Body.Storage != snapshot.Private {
				continue
			}
			name := r.Body.Ref
			if name == "" {
				name = r.Body.SHA256
			}
			from, to := filepath.Join(src.Layout.Private, name+".enc"), filepath.Join(p.Layout.Private, name+".enc")
			fi, err := os.Stat(from)
			if name == "" || err != nil || exists(to) {
				continue // not on this machine: compare says so, as on any other
			}
			if err := copyFile(from, to, fi); err != nil {
				return err
			}
		}
	}
	return nil
}
