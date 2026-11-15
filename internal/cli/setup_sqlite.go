package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/state"
)

// An SQLite database is a file of the project: setup names it by its path
// (working:), grants nothing, and makes the pristine copy in DriftGate's
// store. Each checkout has its own file, so a worktree adopts the main
// worktree's project as it is.

// workingOf is the database a setting names, as state: working takes it:
// the file of an SQLite database, else the name.
func workingOf(d appconf.Database) string {
	if d.Driver == appconf.SQLite {
		return d.Path
	}
	return d.Name
}

// sqliteFile checks a database file given or typed for an SQLite state:
// relative to the project root and inside it.
func sqliteFile(v string) error {
	if v == "" || controls(v) {
		return errors.New("the path of the database file, relative to the project root")
	}
	if err := appconf.CheckFile(v); err != nil {
		return err
	}
	return nil
}

// checkSQLiteDriver refuses a driver that does not fit the setting chosen:
// sqlite for a setting that names a database server (a host, a database
// name), or another driver for one that names an SQLite file. Either
// would write a state: whose dsn the setting cannot fill.
func checkSQLiteDriver(d appconf.Database, driver string) error {
	switch {
	case driver == config.SQLite && d.Driver != appconf.SQLite && (d.Driver != "" || d.Host != "" || d.Name != ""):
		return configErr("setup: --driver sqlite: %s names a database server (%s), not an SQLite file; pass the driver of that server, or choose the setting of the application's SQLite file", where(d), describe(d))
	case driver != "" && driver != config.SQLite && d.Driver == appconf.SQLite:
		return configErr("setup: --driver %s: %s names the SQLite file %s; leave --driver out, or pass --driver sqlite", driver, where(d), d.Path)
	}
	return nil
}

// sqliteDatabase completes the SQLite setting db chooseDatabase chose: the
// file given with --database, else asked when the setting names none
// (--yes leaves state: commented out, as for a database name).
func (s *setup) sqliteDatabase(db *appconf.Database, database string) error {
	if database != "" {
		if err := sqliteFile(database); err != nil {
			return configErr("setup: --database: an SQLite database is its file: %v", err)
		}
		db.Path = path.Clean(strings.ReplaceAll(database, `\`, "/"))
	}
	if db.Path == "" && !s.a.yes {
		v, err := s.a.value("Which file is the application's database (relative to the project root)?", "the settings name no database file: pass --database <file>", sqliteFile)
		if err != nil {
			return err
		}
		db.Path = path.Clean(strings.ReplaceAll(v, `\`, "/"))
	}
	return nil
}

// askTables proposes, for the database of db, to leave the tables the
// framework keeps its own state in out of the comparison
// (appconf.FrameworkTables): excluded, as restoring them would log the
// developer out or disturb a running worker, restore_empty (a cache,
// whose entries would carry over between captures), or restore_only
// (kept as seeded at every restore; rows a request writes there are never
// compared). The default is yes
// (--yes says yes); the answer is written to state:.
func (s *setup) askTables(dr *draft, db appconf.Database) error {
	t := appconf.FrameworkTables(dr.p.Root, db, os.LookupEnv)
	dr.tablesAsked = true
	if t.Empty() {
		return nil
	}
	var parts, chosen []string
	if len(t.Exclude) > 0 {
		parts = append(parts, "exclude "+strings.Join(t.Exclude, ", ")+" (never compared or restored)")
		chosen = append(chosen, "exclude: "+strings.Join(t.Exclude, ", "))
	}
	if len(t.RestoreEmpty) > 0 {
		parts = append(parts, "empty "+strings.Join(t.RestoreEmpty, ", ")+" at every restore (a cache; never compared)")
		chosen = append(chosen, "restore_empty: "+strings.Join(t.RestoreEmpty, ", "))
	}
	if len(t.RestoreOnly) > 0 {
		parts = append(parts, "keep "+strings.Join(t.RestoreOnly, ", ")+" as seeded at every restore (rows a request writes there are never compared)")
		chosen = append(chosen, "restore_only: "+strings.Join(t.RestoreOnly, ", "))
	}
	q := fmt.Sprintf("The framework keeps its own state in this database (%s): %s?", t.Why, strings.Join(parts, "; "))
	yes := s.a.yes
	for !yes {
		a, ok := s.a.line(q + " [Y/n]: ")
		if !ok {
			return s.a.cannot(q, "pass --yes to take them")
		}
		if a = strings.ToLower(a); a == "n" || a == "no" {
			return nil
		}
		yes = a == "" || a == "y" || a == "yes"
	}
	dr.tables = t
	s.chosen = append(s.chosen, fmt.Sprintf("%s (%s)", strings.Join(chosen, "; "), t.Why))
	return nil
}

// frameworkTables are the tables state: excludes or restores only: those
// setup's question took, or for init, which asks nothing, the
// framework's proposal.
func (dr *draft) frameworkTables() appconf.Tables {
	if dr.tablesAsked || dr.db == nil {
		return dr.tables
	}
	return appconf.FrameworkTables(dr.p.Root, *dr.db, os.LookupEnv)
}

// sqliteMissing stops setup before the pristine copy of an SQLite
// database whose file is not there yet: the application makes it. In a
// worktree whose main worktree has a pristine copy of it, setup offers to
// make it from there (sqliteFromMain).
func (s *setup) sqliteMissing(p *config.Project, name string, c config.Component) error {
	if _, err := appconf.LocalPath(p.Root, c.Working); err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.HasSuffix(err.Error(), ": not found") {
			if main := config.MainCheckout(p.Root); main != "" {
				src, err := loadIn(main)
				if err == nil && src.State[name].Driver == config.SQLite {
					return s.sqliteFromMain(src, p.Root, main, name, c.Working)
				}
			}
			return configErr("state %s: the database file %s does not exist yet; the application creates it with its migrations (php artisan migrate, bin/rails db:prepare, python manage.py migrate, npx prisma migrate dev): run them, then %s", name, c.Working, s.resume)
		}
		return configErr("state %s: working %s: %v", name, c.Working, err)
	}
	return nil
}

// adoptSQLite checks an SQLite database of the main worktree's project
// before this worktree takes it. working is a path of the checkout, so
// this worktree's own file; with a dsn, the setting it names must name a
// file of this worktree (d, known), which ownDatabase then makes working.
// That file must exist here (git does not carry it) and must not be the
// main worktree's. Without a dsn, a worktree whose SQLite settings Find
// could not take (a file outside it, such as a copied .env's absolute
// path to the main worktree's file) is refused too, unless one of them
// names working: the application may run on the main worktree's file,
// and a run that restores this worktree's would pass without testing it.
// A file that is not here is made from the main worktree's pristine copy
// (src: its project), after asking (sqliteFromMain).
func (s *setup) adoptSQLite(src *config.Project, here, main, name string, c config.Component, found appconf.Found, d appconf.Database, known bool) error {
	var notes []string
	for _, n := range found.Notes {
		if strings.Contains(n.Text, "SQLite") && !controls(n.File, n.Text) {
			notes = append(notes, n.File+": "+n.Text)
		}
	}
	why := ""
	if len(notes) > 0 {
		why = " (" + strings.Join(notes, "; ") + ")"
	}
	file := c.Working
	switch {
	case !c.DSN.IsZero() && (!known || d.Path == ""):
		return configErr("state %s: this worktree's setting %s names no database file of this worktree%s; its runs could restore a file the main worktree uses. Give this worktree a file of its own in its settings, then run %s",
			name, flow(c.DSN), why, s.resume)
	case known:
		file = d.Path
	case len(notes) > 0 && !namesFile(found, c.Working):
		return configErr("state %s: this worktree's settings name no database file of this worktree%s; the application could run on a file the main worktree uses while its runs restore %s. Give this worktree a file of its own in its settings, then run %s",
			name, why, c.Working, s.resume)
	default:
		for _, n := range notes {
			fmt.Fprintln(s.out, "note: "+n)
		}
	}
	mine, err := appconf.LocalPath(here, file)
	if err != nil && strings.HasSuffix(err.Error(), ": not found") {
		if err = s.sqliteFromMain(src, here, main, name, file); err != nil {
			return err
		}
		mine, err = appconf.LocalPath(here, file)
	}
	if err != nil {
		return configErr("state %s: working %s: %v", name, file, err)
	}
	theirs, err := appconf.LocalPath(main, c.Working)
	if err != nil {
		return nil
	}
	a, errA := os.Stat(mine)
	b, errB := os.Stat(theirs)
	if mine == theirs || errA == nil && errB == nil && os.SameFile(a, b) {
		return configErr("state %s: %s in this worktree is the main worktree's database file; its runs would restore it while the main worktree uses it. Give this worktree a file of its own, then run %s", name, file, s.resume)
	}
	return nil
}

// sqliteFromMain makes the database file of this worktree (file, which
// is not there: git does not carry it) a copy of the main worktree's
// pristine copy of state name, the data the main worktree's baseline was
// captured from, so the worktree's runs start from it; after asking
// (--yes says yes). Without a pristine copy there, or refused, setup
// stops and says how to make the file.
func (s *setup) sqliteFromMain(src *config.Project, here, main, name, file string) error {
	missing := configErr("state %s: the database file %s is not in this worktree (git does not carry it): copy the main worktree's (%s), or create it with the application's migrations, then run %s",
		name, file, filepath.Join(main, filepath.FromSlash(src.State[name].Working)), s.resume)
	pristine, err := capture.SQLitePristine(src, name)
	if err != nil || pristine == "" {
		return missing
	}
	s.step("database file")
	ok, err := s.a.confirm(fmt.Sprintf("The database file %s is not in this worktree (git does not carry it). Make it a copy of the main worktree's pristine copy of state %s, the data its baseline was captured from?", file, name),
		"pass --yes to make it")
	if err != nil {
		return err
	}
	if !ok {
		return missing
	}
	// As the system opens it: a folder linked out of the worktree is
	// refused, though the file is not there yet.
	working, err := appconf.SQLitePath(here, file)
	if err != nil {
		return configErr("state %s: working %s: %v", name, file, err)
	}
	if err := state.SQLiteFromPristine(context.Background(), pristine, working); err != nil {
		return configErr("state %s: making %s from the main worktree's pristine copy: %v", name, file, err)
	}
	fmt.Fprintf(s.out, "state %s: %s made from the main worktree's pristine copy\n", name, file)
	s.chosen = append(s.chosen, fmt.Sprintf("%s: a copy of the main worktree's pristine copy", file))
	return nil
}

// loadIn loads the configuration of the project folder dir:
// .driftgate/driftgate.yaml (baselines: local) or driftgate.yaml.
func loadIn(dir string) (*config.Project, error) {
	path := filepath.Join(dir, config.LocalDir, config.FileName)
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(dir, config.FileName)
	}
	return config.Load(path)
}

// namesFile reports whether a setting Find took names the SQLite file
// working.
func namesFile(found appconf.Found, working string) bool {
	for _, d := range found.Databases {
		if d.Driver == appconf.SQLite && d.Path != "" && d.Path == cleanFile(working) {
			return true
		}
	}
	return false
}

// ownSQLiteFile offers, for an SQLite state database whose setting names
// another file than working, to make working that file (ownDatabase).
func (s *setup) ownSQLiteFile(p *config.Project, path, name string, c config.Component, d appconf.Database) (bool, error) {
	if d.Path == "" || controls(d.Path) || d.Path == cleanFile(c.Working) {
		return false, nil
	}
	s.step("working database")
	q := fmt.Sprintf("%s names the database file %s, and state %s works on %s. Make %s working in %s?", where(d), d.Path, name, c.Working, d.Path, rel(p.Root, path))
	ok, err := s.a.confirm(q, "pass --yes to change it")
	if err != nil {
		return false, err
	}
	if !ok {
		return false, configErr("state %s stays on %s, which the application does not use: set working: %s in %s, or change %s; then run %s",
			name, c.Working, d.Path, rel(p.Root, path), where(d), s.resume)
	}
	if err := setValue(path, d.Path, "state", name, "working"); err != nil {
		return false, configErr("%s: %v; set working: %s by hand, then run %s", rel(p.Root, path), err, d.Path, s.resume)
	}
	fmt.Fprintf(s.out, "state %s: working %s\n", name, d.Path)
	if c.Pristine != "" {
		s.check = append(s.check, fmt.Sprintf("state %s: pristine: %s was the copy of %s; check it is a file for %s", name, c.Pristine, c.Working, d.Path))
	}
	return true, nil
}

// sqliteOnly reports whether every database of p is an SQLite file, which
// has no password.
func sqliteOnly(p *config.Project) bool {
	for _, name := range databases(p) {
		if p.State[name].Driver != config.SQLite {
			return false
		}
	}
	return len(databases(p)) > 0
}

// cleanFile is a file of the configuration with / and cleaned.
func cleanFile(file string) string { return path.Clean(strings.ReplaceAll(file, `\`, "/")) }
