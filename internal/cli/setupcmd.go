package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// setupInput is where setup reads its answers: the terminal, or nil when
// stdin is not one (a pipe, a file, a CI job), and nobody can answer.
var setupInput = func() io.Reader {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return os.Stdin
	}
	return nil
}

// asker asks setup's questions on the terminal. With --yes it asks
// nothing: it takes the first option and says yes, and a value it would
// have to guess stops it with what to pass instead.
type asker struct {
	yes bool
	in  *bufio.Reader // nil: no terminal
	out io.Writer
}

// cannot stops setup at a question nobody can answer: no terminal, or its
// input ended.
func (a *asker) cannot(q, instead string) error {
	return configErr("setup has a question and no terminal to ask it on: %s\nrun it in a terminal to answer, or %s", q, instead)
}

// line asks q and reads the answer; false when there is none to read.
func (a *asker) line(q string) (string, bool) {
	if a.in == nil {
		return "", false
	}
	fmt.Fprint(a.out, q)
	s, err := a.in.ReadString('\n')
	if err != nil && s == "" {
		fmt.Fprintln(a.out)
		return "", false
	}
	return strings.TrimSpace(s), true
}

// choose asks for one of options, numbered from 1; Enter (and --yes) takes
// the first. instead says what to pass when nobody can answer.
func (a *asker) choose(q string, options []string, instead string) (int, error) {
	if a.yes || len(options) == 1 {
		return 0, nil
	}
	if a.in == nil {
		return 0, a.cannot(q, instead)
	}
	fmt.Fprintln(a.out, q)
	for i, o := range options {
		fmt.Fprintf(a.out, "  %d. %s\n", i+1, o)
	}
	for {
		s, ok := a.line("number [1]: ")
		if !ok {
			return 0, a.cannot(q, instead)
		}
		if s == "" {
			return 0, nil
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(a.out, "a number from 1 to %d\n", len(options))
	}
}

// confirm asks a yes/no question whose default is no; --yes says yes.
func (a *asker) confirm(q, instead string) (bool, error) {
	if a.yes {
		return true, nil
	}
	for {
		s, ok := a.line(q + " [y/N]: ")
		if !ok {
			return false, a.cannot(q, instead)
		}
		switch strings.ToLower(s) {
		case "y", "yes":
			return true, nil
		case "", "n", "no":
			return false, nil
		}
	}
}

// value asks for a value setup cannot find; with --yes it stops with
// instead, as it never guesses. valid checks an answer.
func (a *asker) value(q, instead string, valid func(string) error) (string, error) {
	if a.yes {
		return "", configErr("setup: %s", instead)
	}
	for {
		s, ok := a.line(q + " ")
		if !ok {
			return "", a.cannot(q, instead)
		}
		if err := valid(s); err != nil {
			fmt.Fprintln(a.out, err)
			continue
		}
		return s, nil
	}
}

// setup is one run of driftgate setup.
type setup struct {
	a        *asker
	out      io.Writer
	errOut   io.Writer
	resume   string   // the command that goes on from where setup stopped
	chosen   []string // what setup chose and why, for the summary
	check    []string // what the user must check by hand
	database string   // the database line of the summary
	adopted  string   // the main worktree whose project this run took (relative), if any
	answered []string // what answered at each address
}

func setupCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	project := fs.String("project", "", "")
	mode := fs.String("baselines", "", "")
	yes := fs.Bool("yes", false, "")
	driver := fs.String("driver", "", "")
	database := fs.String("database", "", "")
	appURL := fs.String("url", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("setup")
	}
	if *driver != "" {
		d := driverOf(*driver)
		if d == "" {
			return 0, configErr("setup: --driver must be %s, got %q", driverChoices, *driver)
		}
		*driver = d
	}
	if *appURL != "" {
		if err := checkURL(*appURL); err != nil {
			return 0, configErr("setup: --url: %v", err)
		}
	}
	if *database != "" {
		if err := databaseName(*database); err != nil {
			return 0, configErr("setup: --database: not %v", err)
		}
	}
	s := &setup{a: &asker{yes: *yes, out: errOut}, out: out, errOut: errOut, resume: "driftgate setup"}
	if in := setupInput(); in != nil {
		s.a.in = bufio.NewReader(in)
	}
	if *cfg != "" {
		s.resume += " --config " + *cfg
	}
	if *yes {
		s.resume += " --yes"
	}
	code, err := s.run(*cfg, *project, *mode, *driver, *database, *appURL)
	if err != nil && len(s.check) > 0 {
		// What setup found so far is worth checking before the next run.
		s.printCheck(s.check)
	}
	return code, err
}

// run is setup after its flags are read.
func (s *setup) run(cfg, project, mode, driver, database, appURL string) (int, error) {
	out, errOut := s.out, s.errOut
	// No signal handling of its own: a question or a check of setup is
	// stopped at once; seed and run, which clean up, handle them.
	ctx := context.Background()

	path, err := s.project(ctx, cfg, project, mode, driver, database, appURL)
	if err != nil {
		return 0, err
	}
	p, err := config.Load(path)
	if err != nil {
		return 0, configErr("%v", err)
	}
	d, _ := detect(p.Root, p.Project, nil)
	s.step("key")
	if err := s.key(p); err != nil {
		return 0, err
	}
	if p, err = s.ownDatabase(p, path); err != nil {
		return 0, err
	}
	if len(databases(p)) == 0 {
		if p, err = s.noState(p, path); err != nil {
			return 0, err
		}
	}
	if err := s.pristine(ctx, p, path); err != nil {
		return 0, err
	}
	code := ExitClean
	base, err := p.Baseline("local")
	if err != nil {
		return 0, failed(err)
	}
	if base.Exists() {
		s.step("baseline")
		fmt.Fprintf(out, "baseline local is there already (%s); kept\n", rel(p.Root, base.Dir))
	} else {
		s.step("application")
		if err := s.answering(ctx, p, d); err != nil {
			return 0, err
		}
		if code, err = s.interruptible(func() (int, error) { return runCmd([]string{"--config", path}, out, errOut) }); err != nil {
			return 0, err
		}
		if base.Exists() {
			// Uncovered routes fail a run, not the setup: the summary
			// lists them.
			code = ExitClean
		}
	}
	s.summary(p, d)
	return code, nil
}

func (s *setup) step(name string) { fmt.Fprintf(s.out, "\n== %s ==\n", name) }

// checkURL checks an address given for the application.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http:// or https:// address", raw)
	}
	return nil
}

// project is the configuration setup works on: the one of the project
// when it has one, which setup never changes, else a new one written from
// what setup finds and asks.
func (s *setup) project(ctx context.Context, cfg, project, mode, driver, database, appURL string) (string, error) {
	path := cfg
	if path == "" {
		found, err := config.Find(".")
		if err != nil && !errors.Is(err, config.ErrNoConfig) {
			return "", configErr("%v", err)
		}
		path = found
	} else if _, err := os.Stat(path); err != nil {
		path = ""
	}
	if path == "" && cfg == "" {
		if cwd, err := os.Getwd(); err == nil {
			if here, main := config.MainWorktreeProject(cwd); main != "" {
				if project != "" || mode != "" || driver != "" || database != "" || appURL != "" {
					fmt.Fprintln(s.out, "--project, --baselines, --driver, --database and --url are for a new project; this worktree takes the main worktree's")
				}
				return s.adopt(here, main)
			}
		}
	}
	if path != "" {
		s.step("project")
		fmt.Fprintf(s.out, "%s is there already; setup goes on with it and never changes it\n", path)
		if project != "" || mode != "" || driver != "" || database != "" || appURL != "" {
			fmt.Fprintln(s.out, "--project, --baselines, --driver, --database and --url are for a new project: edit the file instead")
		}
		return path, nil
	}
	s.step("project")
	dr, err := newDraft(cfg, project, mode, nil)
	if err != nil {
		return "", err
	}
	if len(dr.d.kinds) > 0 {
		fmt.Fprintf(s.out, "found %s\n", strings.Join(dr.d.kinds, ", "))
	} else {
		fmt.Fprintln(s.out, "no known framework found: add routes under discover: or write the catalog by hand")
	}
	if err := s.chooseDatabase(dr, driver, database); err != nil {
		return "", err
	}
	if dr.d.targets == "" {
		err = s.chooseURL(ctx, dr, appURL, dr.d.baseURL, "the application")
	} else {
		err = s.chooseTargetURLs(ctx, dr, appURL)
	}
	if err != nil {
		return "", err
	}
	for _, c := range s.chosen {
		fmt.Fprintln(s.out, "chosen: "+c)
	}
	if err := dr.write(); err != nil {
		return "", envErr(err)
	}
	fmt.Fprintf(s.out, "created %s, %s and %s\n", rel(dr.p.Root, dr.p.Layout.Config), rel(dr.p.Root, dr.p.Layout.Catalog), rel(dr.p.Root, dr.p.Layout.Allowlist))
	if !dr.p.Layout.Committed() {
		fmt.Fprintf(s.out, "everything DriftGate keeps is in %s/, which git ignores (.gitignore)\n", rel(dr.p.Root, dr.p.Layout.Dir))
	}
	for _, n := range dr.found.Notes {
		s.check = append(s.check, n.File+": "+n.Text)
	}
	for _, k := range dr.d.unchecked {
		s.check = append(s.check, k+": recognised, but DriftGate has not been checked on such a project yet; check the discover: entry")
	}
	return dr.p.Layout.Config, nil
}

// chooseDatabase chooses the database of state: among the settings found:
// several are a question (--yes: the best, rankDatabases), and settings
// that disagree on the server or database stop --yes. A driver the
// setting does not give is asked, never guessed; without a database name
// the block is written commented out (--yes), as is a database setup
// cannot tell.
func (s *setup) chooseDatabase(dr *draft, driver, database string) error {
	if len(dr.dbs) == 0 {
		if driver != "" || database != "" {
			return configErr("setup: --driver and --database complete a database setting of the project, and none was found; add state: to the configuration by hand")
		}
		fmt.Fprintln(s.out, "no database settings found: without state:, requests that change data are not sent")
		s.check = append(s.check, "no database settings found: if the application has a database, add state: (configuration.md#state)")
		return nil
	}
	var options []string
	for _, d := range dr.dbs {
		o := describe(d)
		if canCreate(d) {
			o += " — can create the pristine copy"
		}
		options = append(options, o)
	}
	q := "Which database does the application use?"
	if a, b, ok := disagree(dr.dbs); ok {
		if s.a.yes {
			return configErr("setup: the settings name different databases, and which one the application uses is not clear:\n  %s\n  %s\nrun driftgate setup at a terminal to choose, or make the settings agree (remove or fix the one the application does not use)", describe(a), describe(b))
		}
		q = "The settings name different databases: which one does the application use?"
	} else if dr.dbs[len(dr.dbs)-1].Ref.Source == appconf.Compose && server(dr.dbs[len(dr.dbs)-1]) != server(dr.dbs[0]) {
		q = "A compose service names another database than the application's setting: which one does the application use?"
	}
	i, err := s.a.choose(q, options, "pass --yes to take the first: "+options[0])
	if err != nil {
		return err
	}
	db := dr.dbs[i]
	dr.dbIndex = i
	if len(dr.dbs) > 1 {
		why := "you chose it"
		if s.a.yes {
			why = "the application's most specific setting"
			if canCreate(db) {
				why = "the application's setting names it, and its user can create the pristine copy"
			}
		}
		s.chosen = append(s.chosen, fmt.Sprintf("database: %s of %d found (%s); the others are listed in the configuration", where(db), len(dr.dbs), why))
	}
	if db.Note != "" {
		s.check = append(s.check, where(db)+": "+db.Note)
	}
	if err := checkSQLiteDriver(db, driver); err != nil {
		return err
	}
	if driver != "" {
		db.Driver = driver
	}
	if db.Driver == "" {
		v, err := s.a.value("Which database is it, "+driverChoices+"?", "the settings do not say which database it is: pass --driver with "+driverChoices, func(v string) error {
			if driverOf(v) == "" {
				return errors.New(driverChoices)
			}
			return nil
		})
		if err != nil {
			return err
		}
		db.Driver = driverOf(v)
	}
	if err := checkSQLiteDriver(db, db.Driver); err != nil {
		return err
	}
	if db.Driver == appconf.SQLite {
		if err := s.sqliteDatabase(&db, database); err != nil {
			return err
		}
	} else if database != "" {
		db.Name = database
	}
	if db.Name == "" && db.Driver != appconf.SQLite && !s.a.yes {
		v, err := s.a.value("Which database does the application use (its name)?", "the settings name no database: pass --database <name>", databaseName)
		if err != nil {
			return err
		}
		db.Name = v
	}
	if db.HostNeeded != "" {
		// MySQL's localhost or no host, PostgreSQL's no host: the socket or
		// TCP, which DriftGate does not guess.
		instruction := fmt.Sprintf("%s: the host may be the local socket or TCP, which DriftGate does not guess: run driftgate setup at a terminal to give it, or write state: with dsn: %s (or host: <path of the socket>)", where(db), flow(config.SecretFor(appconf.Ref{Source: db.Ref.Source, File: db.Ref.File, Key: db.Ref.Key, Host: db.HostNeeded})))
		v, err := s.a.value("Where does the database listen: host:port, or the path of its socket?", instruction, func(v string) error {
			r := db.Ref
			r.Host = v
			return appconf.CheckRef(r)
		})
		if err != nil {
			return err
		}
		db.Ref.Host, db.HostNeeded = v, ""
	}
	if db.Ref.IsZero() {
		s.check = append(s.check, fmt.Sprintf("%s cannot be read from a file: set %s to the DSN before DriftGate runs (dsn: { env: %s })", where(db), dsnEnv, dsnEnv))
	}
	if db.Driver == appconf.SQLite && db.Path != "" {
		if err := s.sqliteExclude(dr, db); err != nil {
			return err
		}
	}
	dr.db, dr.enable = &db, true
	if workingOf(db) == "" {
		what := "name"
		if db.Driver == appconf.SQLite {
			what = "file"
		}
		s.check = append(s.check, fmt.Sprintf("state: is commented out: the settings name no database the application uses; set working: and uncomment it in %s (or start over with --database <%s>), then run %s", rel(dr.p.Root, dr.p.Layout.Config), what, s.resume))
		fmt.Fprintf(s.out, "state: db %s, commented out: no database named\n", describe(db))
		return nil
	}
	fmt.Fprintf(s.out, "state: db %s\n", describe(db))
	return nil
}

// databaseName checks a database name given or typed.
func databaseName(v string) error {
	if v == "" || controls(v) || strings.ContainsAny(v, " '\"`") {
		return errors.New("a database name")
	}
	return nil
}

// chooseURL chooses the address of the application: the one given, else
// one of those the settings give (several are a question; --yes takes the
// best, rankURLs). Without one, def (the framework's default) is offered,
// never taken unasked.
func (s *setup) chooseURL(ctx context.Context, dr *draft, given, def, what string) error {
	switch {
	case given != "":
		dr.url, dr.urlFrom = given, "--url"
	case len(dr.urls) > 0:
		var options []string
		for _, u := range dr.urls {
			o := u.URL + " (" + u.File + " " + u.Key + ")"
			if !s.a.yes && len(dr.urls) > 1 {
				if status, err := answers(ctx, config.Target{BaseURL: u.URL}); err == nil {
					o += " — answers now: " + status
				}
			}
			options = append(options, o)
		}
		i, err := s.a.choose("Where does "+what+" answer?", options, "pass --url <address> (--yes takes "+dr.urls[0].URL+")")
		if err != nil {
			return err
		}
		dr.url, dr.urlFrom = dr.urls[i].URL, dr.urls[i].File+" "+dr.urls[i].Key
		if len(dr.urls) > 1 {
			s.chosen = append(s.chosen, fmt.Sprintf("base_url: %s of %d addresses found", dr.urlFrom, len(dr.urls)))
		}
	default:
		q := "Where does " + what + " answer (http://…)?"
		if def != "" {
			q = fmt.Sprintf("Where does %s answer? [%s]", what, def)
		}
		instead := "the settings give no address of the application: pass --url <address>, e.g. --url http://localhost:8000"
		if def != "" {
			instead = "the settings give no address of the application, only the framework's default port (" + def + "), which setup does not take unasked: pass --url <address>"
		}
		v, err := s.a.value(q, instead, func(v string) error {
			if v == "" && def != "" {
				return nil
			}
			return checkURL(v)
		})
		if err != nil {
			return err
		}
		if v == "" {
			v = def
		}
		dr.url, dr.urlFrom = v, "your answer"
	}
	fmt.Fprintf(s.out, "base_url %s (%s)\n", dr.url, dr.urlFrom)
	return nil
}

// chooseTargetURLs chooses the addresses of a project init lays out as
// targets: (a service in a subfolder, or several). A compose port it
// found stays; a framework's default port is asked, never taken unasked.
// --url is the address of a single service, and refused with several.
func (s *setup) chooseTargetURLs(ctx context.Context, dr *draft, given string) error {
	d := &dr.d
	if given != "" && len(d.services) > 1 {
		return configErr("setup: --url is the address of a project with one service, and this one has %d (%s): run driftgate init, set each targets.<name>.base_url in %s, then driftgate setup",
			len(d.services), strings.Join(d.services, ", "), rel(dr.p.Root, dr.p.Layout.Config))
	}
	for _, name := range d.services {
		if given == "" && !slices.Contains(d.defaults, name) {
			continue
		}
		var def string
		if m := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:\n    base_url: (\S+)`).FindStringSubmatch(d.targets); m != nil {
			def = m[1]
		}
		single := &draft{p: dr.p, d: *d}
		if len(d.services) == 1 {
			single.urls = dr.urls
		}
		if err := s.chooseURL(ctx, single, given, def, "service "+name); err != nil {
			return err
		}
		d.targets = regexp.MustCompile(`(?m)^(  `+regexp.QuoteMeta(name)+`:\n    base_url: )\S+(   # from ).*$`).
			ReplaceAllLiteralString(d.targets, "  "+name+":\n    base_url: "+single.url+"   # from "+single.urlFrom)
	}
	return nil
}

// key makes the project key where LoadKey finds it, unless there is one.
func (s *setup) key(p *config.Project) error {
	_, err := p.LoadKey()
	switch {
	case err == nil:
		fmt.Fprintf(s.out, "the key is set up already (from %s); kept\n", p.KeySource())
		if c := p.KeyConflict(); c != "" {
			fmt.Fprintln(s.out, "warning: "+c)
		}
		return nil
	case !errors.Is(err, secret.ErrNoKey):
		return failed(err)
	}
	where, err := p.CreateKey()
	if err != nil {
		return failed(err)
	}
	fmt.Fprintf(s.out, "created a key for %s in %s (keep a copy: without it the private bodies cannot be read)\n", p.Project, where)
	return nil
}

// databases are the state components with a driver, by name.
func databases(p *config.Project) []string {
	var out []string
	for name, c := range p.State {
		if c.Driver != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// pristine makes the pristine copy of every state database, which creates
// a database: it asks first (--yes says yes). A copy made before is kept.
func (s *setup) pristine(ctx context.Context, p *config.Project, path string) error {
	names := databases(p)
	if len(names) == 0 {
		return nil
	}
	s.step("pristine copy")
	found := appconf.Find(p.Root, os.LookupEnv)
	var lines, grants, notes []string
	for _, name := range names {
		c := p.State[name]
		if c.Driver == config.SQLite {
			if err := s.sqliteMissing(p, name, c); err != nil {
				return err
			}
		}
		if _, err := c.ReadDSN(ctx, p.Root); err != nil {
			return dsnErr(c.DSN, err, fmt.Errorf("state %s: dsn: %v\nfix it in %s (or the setting it names), then run %s: it goes on from here", name, err, rel(p.Root, path), s.resume))
		}
		d, known := setting(found, c)
		if known {
			s.database = describe(d)
		} else {
			s.database = fmt.Sprintf("%s %s (dsn: %s)", c.Driver, c.Working, flow(c.DSN))
		}
		pristine := c.Pristine
		if c.Driver == config.SQLite && pristine == "" {
			pristine = "file in DriftGate's store" // capture.sqliteStorePristine
		}
		lines = append(lines, fmt.Sprintf("%s, a copy of %s", pristine, c.Working))
		if g := grant(c, d.User); g != "" && (!known || !canCreate(d)) {
			grants = append(grants, g)
			if c.Driver == config.SQLServer {
				notes = append(notes, sqlServerFast(d.User))
			}
		}
		if c.Driver == config.SQLServer {
			notes = append(notes, sqlServerCopy(c))
		}
	}
	if len(names) > 1 {
		s.database = fmt.Sprintf("%d databases: %s", len(names), strings.Join(names, ", "))
	}
	seeded, err := capture.Seeded(ctx, p)
	if err != nil {
		return err
	}
	if seeded {
		fmt.Fprintln(s.out, "the pristine copy is there already; kept")
		return nil
	}
	if len(grants) > 0 {
		fmt.Fprintln(s.out, "the pristine copy needs a user that may create a database; if the user of the setting may not, grant it (as an administrator): "+strings.Join(grants, " "))
	}
	for _, n := range notes {
		fmt.Fprintln(s.out, n)
	}
	q := "Make the pristine copy now? It creates the database " + strings.Join(lines, "; the database ") + ", and every seed replaces it"
	ok, err := s.a.confirm(q, "pass --yes to make it")
	if err != nil {
		return err
	}
	if !ok {
		return configErr("no pristine copy was made, so nothing was captured: every capture starts from it. To make it: driftgate seed --skip-commands, then %s", s.resume)
	}
	_, err = s.interruptible(func() (int, error) {
		return 0, seedCmd([]string{"--config", path, "--skip-commands"}, s.out, s.errOut)
	})
	if err != nil && len(grants) > 0 && denied().MatchString(err.Error()) {
		var e *exitError
		code := ExitEnv
		if errors.As(err, &e) {
			code = e.code
		}
		return &exitError{code, fmt.Errorf("%w\nthe user may not create the pristine database; grant it (as an administrator): %s Then run %s", err, strings.Join(grants, " "), s.resume)}
	}
	return err
}

// denied matches the errors of a user without the right to create a
// database: MySQL and MariaDB 1044 (database), 1142 (table) and 1227
// (privilege), PostgreSQL 42501, SQL Server's permission errors (229,
// 230, 262, 297, 3110, 5011, 15247) and those of a login without a user
// in the working database (916, 4060, 4063).
// (A pattern built on first use: setup's code must not slow the start of
// every other command.)
var denied = lazy.Regexp(`Error (1044|1142|1227)\b|SQLSTATE 42501|mssql: [^\n]*\((229|230|262|297|916|3110|4060|4063|5011|15247)\)`)

// interruptible runs a command of setup's that handles the stop signals
// itself (seed, run): when one came, setup says how to go on rather than
// the error of what was cut short.
func (s *setup) interruptible(cmd func() (int, error)) (int, error) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, stopSignals...)
	defer signal.Stop(sigs)
	code, err := cmd()
	select {
	case sig := <-sigs:
		return 0, &exitError{signalCode(sig), fmt.Errorf("interrupted (%v); run %s: it goes on from here", sig, s.resume)}
	default:
	}
	return code, err
}

// stateOff is the first line of the state: block init writes commented
// out (stateBlock); the lines after it, up to an empty one, are the block.
const stateOff = "# found in the project's settings: driftgate setup turns it on"

// noState handles a configuration without a state database while the
// project's settings name one: the block init wrote commented out is
// turned on in place, after asking (--yes says yes), and the project
// loaded again; without that block setup stops before routes and
// captures, which would otherwise run without a restore.
func (s *setup) noState(p *config.Project, path string) (*config.Project, error) {
	dbs := (&draft{found: appconf.Find(p.Root, os.LookupEnv)}).usable()
	if len(dbs) == 0 {
		return p, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, envErr(err)
	}
	text := string(raw)
	at := strings.Index(text, stateOff)
	if at < 0 || (at > 0 && text[at-1] != '\n') {
		return nil, configErr("the project's settings name a database (%s), and %s has no state: database: requests would run without restoring it. Add state: (configuration.md#state), or uncomment the block driftgate init wrote, then run %s",
			describe(dbs[0]), rel(p.Root, path), s.resume)
	}
	s.step("state")
	ok, err := s.a.confirm("Turn on state: in "+rel(p.Root, path)+", the database driftgate init found and wrote commented out?", "pass --yes to turn it on")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, configErr("state: stays commented out in %s, so nothing was captured: requests would run without restoring the database. Uncomment it, or run %s again and answer yes", rel(p.Root, path), s.resume)
	}
	lines := strings.Split(text[at:], "\n")
	lines[0] = "# found in the project's settings; turned on by driftgate setup"
	for i := 1; i < len(lines) && lines[i] != ""; i++ {
		lines[i] = strings.TrimPrefix(lines[i], "# ")
	}
	if err := snapshot.WriteFile(path, []byte(text[:at]+strings.Join(lines, "\n")), 0o644); err != nil {
		return nil, envErr(err)
	}
	if p, err = config.Load(path); err != nil {
		return nil, configErr("%v", err)
	}
	if len(databases(p)) == 0 {
		return nil, configErr("%s: the state: block setup turned on names no database; check it, then run %s", rel(p.Root, path), s.resume)
	}
	fmt.Fprintf(s.out, "turned on state: in %s\n", rel(p.Root, path))
	return p, nil
}

// dsnErr is the error of a DSN that cannot be read: a configuration
// problem (a file or variable it names is missing, or it names another
// database than working), except for 1Password, where it is the
// environment's (op not signed in).
func dsnErr(dsn config.Secret, cause, err error) error {
	if dsn.Op != "" && !errors.Is(cause, config.ErrInvalid) {
		return envErr(err)
	}
	return &exitError{ExitConfig, err}
}

// setting is the setting of the project a state component names, as
// appconf.Find found it: host, port and user for the messages.
func setting(found appconf.Found, c config.Component) (appconf.Database, bool) {
	for _, d := range found.Databases {
		if !d.Ref.IsZero() && config.SecretFor(d.Ref) == c.DSN {
			return d, true
		}
	}
	return appconf.Database{}, false
}

// driverChoices names the drivers setup takes.
const driverChoices = "mysql, postgres, sqlite or sqlserver"

// driverOf is the driver a --driver value or an answer names: mariadb is
// mysql, mssql is sqlserver; "" for none DriftGate has.
func driverOf(v string) string {
	switch v {
	case config.MySQL, "mariadb":
		return config.MySQL
	case config.Postgres, config.SQLite:
		return v
	case config.SQLServer, "mssql":
		return config.SQLServer
	}
	return ""
}

// grant is the statement that lets user create the pristine copy of c; ""
// when there is none to grant (an SQLite file).
func grant(c config.Component, user string) string {
	if user == "" {
		user = "<user>"
	}
	switch c.Driver {
	case config.Postgres:
		return fmt.Sprintf(`ALTER ROLE "%s" CREATEDB;`, user)
	case config.SQLite:
		return ""
	case config.SQLServer:
		return sqlServerGrant(c, user)
	}
	return fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%';", c.Pristine, user)
}

// startHints say how each framework's application is started.
var startHints = map[string]string{
	"laravel": "php artisan serve", "symfony": "symfony server:start", "spring": "./mvnw spring-boot:run (or ./gradlew bootRun)",
	"quarkus": "./mvnw quarkus:dev", "micronaut": "./mvnw mn:run", "aspnet": "dotnet run", "fastapi": "uvicorn main:app",
	"django": "python manage.py runserver", "rails": "bin/rails server", "nestjs": "npm run start:dev",
	"nextjs": "npm run dev", "nuxt": "npm run dev",
}

// answering checks that the application answers at every target before
// the capture, and stops with how to start it.
func (s *setup) answering(ctx context.Context, p *config.Project, d detected) error {
	for _, name := range p.TargetNames() {
		t, err := p.TargetFor(name)
		if err != nil {
			return configErr("%v", err)
		}
		status, err := answers(ctx, t)
		if err == nil {
			// Any answer counts: the summary says what answered, in case
			// it is not the application.
			fmt.Fprintf(s.out, "something answers at %s: %s\n", t.BaseURL, status)
			s.answered = append(s.answered, t.BaseURL+" ("+status+" when setup checked it)")
			continue
		}
		start := startHint(p.Root, d.kinds)
		field := "target.base_url"
		if name != "" {
			field = "targets." + name + ".base_url"
		}
		return envErr(fmt.Errorf("the application does not answer at %s: %v\n%s, or set %s in %s to where it answers; then run %s: it goes on from here",
			t.BaseURL, err, start, field, rel(p.Root, p.Layout.Config), s.resume))
	}
	return nil
}

// startHint says how to start the application of a project with the
// frameworks kinds: its compose file, else the framework's own command.
func startHint(root string, kinds []string) string {
	if f := composeFile(root); f != "" {
		return "start it (docker compose -f " + f + " up -d)"
	}
	for _, k := range kinds {
		if h, ok := startHints[k]; ok {
			return "start it (" + h + ")"
		}
	}
	return "start it"
}

// composeFile is the compose file of the project, if any.
func composeFile(root string) string {
	return exists(root, "compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml")
}

// answers checks that something answers HTTP at t's address, and says
// what: the status and the Server header. Any response counts; whether it
// is the application, setup cannot tell without guessing. Only an allowed
// host is asked (target.allow_hosts, by default local and private
// addresses), as a capture would.
func answers(ctx context.Context, t config.Target) (string, error) {
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not an address", t.BaseURL)
	}
	allow, err := httpx.NewAllow(t.AllowHosts, false)
	if err != nil {
		return "", err
	}
	if err := allow.Check(ctx, u.Hostname()); err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DialContext: allow.Dial},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", err
	}
	resp.Body.Close()
	status := "HTTP " + strconv.Itoa(resp.StatusCode)
	if srv := resp.Header.Get("Server"); srv != "" && !controls(srv) {
		status += ", server " + strconv.Quote(srv)
	}
	return status, nil
}

// summary says what setup found and made, and what to check by hand.
func (s *setup) summary(p *config.Project, d detected) {
	s.step("summary")
	row := func(k, v string) { fmt.Fprintf(s.out, "%-11s %s\n", k+":", v) }
	if len(d.kinds) > 0 {
		row("framework", strings.Join(d.kinds, ", "))
	} else {
		row("framework", "none recognised")
	}
	if s.database != "" {
		note := "; the password stays where it is"
		if sqliteOnly(p) {
			note = ""
		}
		row("database", s.database+note)
	} else {
		row("database", "none (no state:)")
	}
	var urls []string
	for _, name := range p.TargetNames() {
		if t, err := p.TargetFor(name); err == nil {
			urls = append(urls, t.BaseURL)
		}
	}
	if len(s.answered) > 0 {
		urls = s.answered
	}
	row("app", strings.Join(urls, ", "))
	row("routes", s.coverage(p))
	base := filepath.Join(p.Layout.Baselines, "local")
	if _, err := os.Stat(base); err == nil {
		if s.adopted != "" {
			row("baseline", "local in "+rel(p.Root, base)+", the main worktree's ("+s.adopted+"): driftgate run compares this worktree's code against it")
		} else {
			row("baseline", "local in "+rel(p.Root, base))
		}
	} else {
		row("baseline", "not made yet (see above)")
	}
	for _, c := range s.chosen {
		row("chosen", c)
	}
	check := s.check
	if len(databases(p)) > 0 {
		check = append(check, "guards: the application must not send mail or payments while DriftGate sends requests that change data (configuration.md: seed, prepare, guards)")
	}
	s.printCheck(check)
	fmt.Fprintln(s.out, "\nnext: driftgate run after every upgrade step compares with the baseline")
}

// printCheck lists what the user must check by hand.
func (s *setup) printCheck(check []string) {
	if len(check) == 0 {
		return
	}
	fmt.Fprintln(s.out, "\ncheck by hand:")
	for _, c := range check {
		fmt.Fprintf(s.out, "  - %s\n", c)
	}
}

// coverage is how many routes the catalog covers.
func (s *setup) coverage(p *config.Project) string {
	inv, err := routes.LoadInventory(p.Path("routes.json"))
	if err != nil || len(inv.Routes) == 0 {
		return "none found (discover: in the configuration finds them)"
	}
	_, excl, err := withExclusions(p, inv, "routes.json")
	if err != nil {
		return err.Error()
	}
	cat, err := catalog.Load(p.Layout.Catalog)
	if err != nil {
		return err.Error()
	}
	c := routes.Check(inv, cat, excl)
	line := fmt.Sprintf("%d found: %d covered, %d excluded, %d uncovered", len(inv.Routes), len(c.Covered), len(c.Excluded), len(c.Uncovered))
	if len(c.Uncovered) > 0 {
		s.check = append(s.check, fmt.Sprintf("uncovered routes (%d): driftgate coverage lists them; give their parameters under params: or exclude them with a reason", len(c.Uncovered)))
	}
	return line
}
