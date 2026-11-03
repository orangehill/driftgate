package cli

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
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

// sqliteExclude proposes, for the SQLite file of db, to exclude the tables
// the framework keeps its sessions, cache and jobs in, in the same file
// (appconf.SQLiteExclude): they change on every request, and restoring
// them would only log users out and drop jobs. The default is yes (--yes
// says yes); the answer is written to state:.
func (s *setup) sqliteExclude(dr *draft, db appconf.Database) error {
	tables, why := appconf.SQLiteExclude(dr.p.Root, db, os.LookupEnv)
	dr.excludeAsked = true
	if len(tables) == 0 {
		return nil
	}
	q := fmt.Sprintf("Exclude the framework's own tables %s from state: (never compared or restored; %s)?", strings.Join(tables, ", "), why)
	yes := s.a.yes
	for !yes {
		a, ok := s.a.line(q + " [Y/n]: ")
		if !ok {
			return s.a.cannot(q, "pass --yes to exclude them")
		}
		if a = strings.ToLower(a); a == "n" || a == "no" {
			return nil
		}
		yes = a == "" || a == "y" || a == "yes"
	}
	{
		dr.exclude = tables
		s.chosen = append(s.chosen, fmt.Sprintf("exclude: %s (%s)", strings.Join(tables, ", "), why))
	}
	return nil
}

// excludeTables are the tables state: excludes: those setup's question
// took, or for init, which asks nothing, the framework's proposal.
func (dr *draft) excludeTables() []string {
	if dr.excludeAsked || dr.db == nil || dr.db.Driver != appconf.SQLite {
		return dr.exclude
	}
	tables, _ := appconf.SQLiteExclude(dr.p.Root, *dr.db, os.LookupEnv)
	return tables
}

// sqliteMissing stops setup before the pristine copy of an SQLite
// database whose file is not there yet: the application makes it.
func (s *setup) sqliteMissing(p *config.Project, name string, c config.Component) error {
	if _, err := appconf.LocalPath(p.Root, c.Working); err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.HasSuffix(err.Error(), ": not found") {
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
// main worktree's. Without a dsn, what Find noted about this worktree's
// SQLite settings is shown: one may name another file than working.
func (s *setup) adoptSQLite(here, main, name string, c config.Component, found appconf.Found, d appconf.Database, known bool) error {
	var notes []string
	for _, n := range found.Notes {
		if strings.Contains(n.Text, "SQLite") && !controls(n.File, n.Text) {
			notes = append(notes, n.File+": "+n.Text)
		}
	}
	file := c.Working
	switch {
	case !c.DSN.IsZero() && (!known || d.Path == ""):
		why := ""
		if len(notes) > 0 {
			why = " (" + strings.Join(notes, "; ") + ")"
		}
		return configErr("state %s: this worktree's setting %s names no database file of this worktree%s; its runs could restore a file the main worktree uses. Give this worktree a file of its own in its settings, then run %s",
			name, flow(c.DSN), why, s.resume)
	case known:
		file = d.Path
	default:
		for _, n := range notes {
			fmt.Fprintln(s.out, "note: "+n)
		}
	}
	mine, err := appconf.LocalPath(here, file)
	if err != nil {
		if strings.HasSuffix(err.Error(), ": not found") {
			return configErr("state %s: the database file %s is not in this worktree (git does not carry it): copy the main worktree's (%s), or create it with the application's migrations, then run %s",
				name, file, filepath.Join(main, filepath.FromSlash(c.Working)), s.resume)
		}
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

// cleanFile is a file of the configuration with / and cleaned.
func cleanFile(file string) string { return path.Clean(strings.ReplaceAll(file, `\`, "/")) }
