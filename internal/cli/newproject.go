package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// draft is a new project before its files are written: what init and
// setup find in the project folder (the frameworks, and the database and
// address in the application's own settings) and what they chose of it.
type draft struct {
	p     *config.Project // validated, not written yet
	d     detected
	found appconf.Found      // its Notes also say which settings were left out (usable)
	dbs   []appconf.Database // the database settings found, best first (rankDatabases)
	urls  []appconf.AppURL   // the addresses found, best first (rankURLs)

	db      *appconf.Database // state: db; nil for none
	dbIndex int               // the setting of dbs it was made from
	enable  bool              // state: is written on, not commented out (setup)
	url     string            // target.base_url; empty: the framework's default
	urlFrom string            // where url comes from, for the messages
}

// newDraft locates a new project (config.Locate) and reads its folder. A
// project that has a configuration already is refused.
func newDraft(cfg, project, mode string, only []string) (*draft, error) {
	root, _, err := config.Locate(cfg, mode)
	if err != nil {
		return nil, configErr("%v", err)
	}
	if project == "" {
		project = filepath.Base(root)
	}
	p, err := config.Create(config.Config{Version: config.Version, Project: project, Mode: config.Freeze, Level: "values"}, cfg, mode)
	if err != nil {
		return nil, configErr("%v", err)
	}
	d, err := detect(root, project, only)
	if err != nil {
		return nil, err
	}
	dr := &draft{p: p, d: d, found: appconf.Find(root, os.LookupEnv)}
	dr.dbs, dr.urls = rankDatabases(dr.usable()), rankURLs(dr.usableURLs())
	return dr, nil
}

// controls reports a control character (a line break, a NUL) in any of
// values: such a value would change the meaning of driftgate.yaml or of a
// message.
func controls(values ...string) bool {
	for _, v := range values {
		if strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return true
		}
	}
	return false
}

// usable are the database settings found that driftgate.yaml can name. A
// setting with a control character in a value is left out, with a note
// naming the setting, never the value. PostgreSQL's own database (postgres,
// what an image without POSTGRES_DB gives) is never taken for the working
// one: DriftGate connects to it to create and drop databases.
func (dr *draft) usable() []appconf.Database {
	var out []appconf.Database
	for _, d := range dr.found.Databases {
		if controls(d.Driver, d.Host, d.Port, d.Name, d.User, d.File, d.Key, d.Ref.Host) {
			dr.found.Notes = append(dr.found.Notes, appconf.Note{File: fmt.Sprintf("%q", d.File),
				Text: "a value of the database setting holds a control character (a line break); the setting is left out"})
			continue
		}
		if d.Driver == appconf.Postgres && d.Name == "postgres" {
			d.Name = ""
			d.Note = join(d.Note, "the database is postgres, the server's own, which DriftGate uses to create the pristine copy: name the application's database (POSTGRES_DB of a compose service)")
		}
		out = append(out, d)
	}
	return out
}

// usableURLs are the addresses found that are http:// or https://
// addresses without control characters.
func (dr *draft) usableURLs() []appconf.AppURL {
	var out []appconf.AppURL
	for _, u := range dr.found.AppURLs {
		if controls(u.URL, u.File, u.Key) || checkURL(u.URL) != nil {
			dr.found.Notes = append(dr.found.Notes, appconf.Note{File: fmt.Sprintf("%q", u.File), Text: "an address of the application is not an http:// or https:// address; it is left out"})
			continue
		}
		out = append(out, u)
	}
	return out
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// canCreate reports a database user that can make the pristine copy for
// certain: the root user of a compose MySQL or MariaDB service, the user
// of a compose PostgreSQL service, which the image makes a superuser, or
// sa of a compose SQL Server service.
func canCreate(d appconf.Database) bool {
	if d.Ref.Source != appconf.Compose {
		return false
	}
	switch d.Driver {
	case appconf.Postgres:
		return true
	case appconf.SQLServer:
		return strings.EqualFold(d.User, "sa")
	case appconf.SQLite:
		return false
	}
	return d.User == "root"
}

// server is the server and database a setting names, as this machine
// reaches them: the loopback addresses are one host, and a missing port is
// the driver's default. MySQL's localhost and no host are the local socket
// for most of its clients, not TCP; PostgreSQL's no host too. An SQLite
// database is its file.
func server(d appconf.Database) string {
	if d.Driver == appconf.SQLite {
		return "sqlite:" + d.Path
	}
	host, port := d.Host, d.Port
	switch {
	case d.Driver == appconf.SQLServer && (host == "" || host == "localhost"):
		host = "local" // SQL Server takes TCP only
	case host == "" || host == "localhost" && d.Driver != appconf.Postgres:
		host = "socket"
	case host == "localhost", host == "127.0.0.1", host == "::1":
		host = "local"
	}
	if port == "" {
		port = map[string]string{appconf.MySQL: "3306", appconf.Postgres: "5432", appconf.SQLServer: "1433"}[d.Driver]
	}
	return d.Driver + "://" + host + ":" + port + "/" + d.Name
}

// rankDatabases orders the settings found for state:, best first: the
// order of appconf.Find (the application's own settings, the most specific
// file first), except that a compose service whose user can create the
// pristine database comes first when it is the server and database the
// application's first setting names. A compose service of another
// database comes last: its root user is no reason to reset a database the
// application does not use.
func rankDatabases(dbs []appconf.Database) []appconf.Database {
	var app, compose []appconf.Database
	for _, d := range dbs {
		if d.Ref.Source == appconf.Compose {
			compose = append(compose, d)
		} else {
			app = append(app, d)
		}
	}
	if len(app) == 0 {
		return compose
	}
	var first, last []appconf.Database
	for _, c := range compose {
		if canCreate(c) && server(c) == server(app[0]) {
			first = append(first, c)
		} else {
			last = append(last, c)
		}
	}
	return append(append(first, app...), last...)
}

// disagree finds two of the application's settings that name different
// servers or databases (the environment and a .env, .env.local and .env):
// which one the application uses is then not clear. A compose service is
// the application's only when its setting names it (rankDatabases).
func disagree(dbs []appconf.Database) (a, b appconf.Database, ok bool) {
	for i, x := range dbs {
		for _, y := range dbs[i+1:] {
			if x.Ref.Source != appconf.Compose && y.Ref.Source != appconf.Compose && server(x) != server(y) {
				return x, y, true
			}
		}
	}
	return a, b, false
}

// rankURLs orders the addresses found, best first, each once: a port a
// compose service publishes (how this machine reaches a container) before
// the address the application's settings give itself.
func rankURLs(urls []appconf.AppURL) []appconf.AppURL {
	var out []appconf.AppURL
	for _, compose := range []bool{true, false} {
		for _, u := range urls {
			isCompose := strings.HasPrefix(u.Key, "services.")
			if isCompose == compose && !slices.ContainsFunc(out, func(o appconf.AppURL) bool { return o.URL == u.URL }) {
				out = append(out, u)
			}
		}
	}
	return out
}

// where names the setting a database was found in.
func where(d appconf.Database) string {
	switch {
	case d.Ref.Source == appconf.Env || (d.Ref.IsZero() && d.Key == ""):
		return "environment variable " + d.File
	case d.Ref.Source == appconf.Compose:
		return d.File + ", service " + d.Key
	case d.Key == "":
		return d.File
	}
	return d.File + " " + d.Key
}

// describe is a database setting in one line, without its password.
func describe(d appconf.Database) string {
	driver, name := d.Driver, d.Name
	if driver == "" {
		driver = "driver unknown"
	}
	if name == "" {
		name = "(no database named)"
	}
	s := driver + " " + name
	if d.Host != "" {
		s += " on " + d.Host
		if d.Port != "" {
			s += ":" + d.Port
		}
	}
	if d.User != "" {
		if strings.ContainsAny(d.User, "=;") {
			s += ", user (not shown: it holds = or ;, so it may be a part of a password)"
		} else {
			s += ", user " + d.User
		}
	}
	return s + " (" + where(d) + ")"
}

// dsnEnv is the variable state: names when a setting cannot be referenced.
const dsnEnv = "DG_DB_DSN"

// stateBlock is state: for driftgate.yaml with the database d: a reference
// to the setting, never its value. A setting that cannot be referenced
// (only in this shell's environment) takes the DSN from dsnEnv. The block
// is commented out unless on, and when the setting gives no driver or
// database name, with what to do.
func stateBlock(d appconf.Database, on bool) string {
	dsn := flow(config.SecretFor(d.Ref))
	comment := "from " + where(d) + ", read when a command runs; the password stays there"
	if d.Ref.IsZero() {
		dsn = flow(config.Secret{Env: dsnEnv})
		comment = "set " + dsnEnv + " to the DSN (mysql://user:pass@host:3306/ or postgres://…)"
	}
	working := d.Name
	if d.Driver == appconf.SQLite {
		working = d.Path
	}
	lines := []string{
		"state:                                    # restored at the start and before requests that change data",
		"  db:",
		"    driver: " + d.Driver,
		"    dsn: " + dsn + "   # " + comment,
		"    working: " + scalar(working),
	}
	switch {
	case d.Driver == appconf.SQLite:
		// The pristine copy is a file of the store; the DSN only
		// cross-checks working, so none is needed without a setting.
		if d.Ref.IsZero() {
			lines = slices.Delete(lines, 3, 4)
		}
		lines = append(lines, "    # pristine: a file of DriftGate's store by default, made by driftgate seed")
	case d.Name != "":
		lines = append(lines, "    pristine: "+scalar(d.Name+"_pristine")+"   # its pristine copy, made by driftgate seed")
	}
	lines = append(lines, "    # exclude: [sessions, cache, jobs]    # tables never restored")
	var missing []string
	if d.Driver == "" {
		missing = append(missing, "driver (mysql, postgres, sqlite or sqlserver)")
	}
	if working == "" {
		missing = append(missing, "working (the database the application uses)")
	}
	var b strings.Builder
	if d.Note != "" {
		b.WriteString("# " + where(d) + ": " + d.Note + "\n")
	}
	off := len(missing) > 0 || !on
	switch {
	case len(missing) > 0:
		b.WriteString("# set " + strings.Join(missing, " and ") + ", then uncomment:\n")
	case !on:
		b.WriteString("# found in the project's settings: driftgate setup turns it on (it asks before it creates the pristine copy), or uncomment it\n")
	}
	for _, l := range lines {
		if off && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			l = "# " + l
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// scalar is s as a YAML scalar, quoted where YAML needs it.
func scalar(s string) string {
	raw, err := yaml.Marshal(s)
	if err != nil {
		return `""`
	}
	return strings.TrimSpace(string(raw))
}

// flow is v as a YAML flow mapping: { dotenv: .env, host: … }.
func flow(v any) string {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return "{}"
	}
	n.Style = yaml.FlowStyle
	raw, err := yaml.Marshal(&n)
	if err != nil {
		return "{}"
	}
	return strings.TrimSpace(string(raw))
}

// configText is the text of driftgate.yaml.
func (dr *draft) configText() string {
	p, d := dr.p, dr.d
	var baselines string
	if p.Baselines != "" {
		baselines = fmt.Sprintf("baselines: %-12s # everything in .driftgate/, which git ignores; driftgate baselines git puts it into the repository\n", p.Baselines)
	}
	text := fmt.Sprintf(initConfig, p.Project, baselines)
	if d.targets != "" {
		// Several services: their targets replace the single target.
		start := strings.Index(text, "target:\n")
		end := strings.Index(text, "\n# tokens:")
		text = text[:start] + d.targets + text[end:]
	} else if url := dr.baseURL(); url != "" {
		text = strings.Replace(text, "base_url: http://localhost:8080", "base_url: "+url, 1)
	}
	if dr.db != nil {
		// The database found replaces the commented example of state:.
		start := strings.Index(text, "# state:")
		end := strings.Index(text, "# seed:")
		var others strings.Builder
		for i, o := range dr.dbs {
			if i != dr.dbIndex {
				others.WriteString("# also found: " + describe(o) + "\n")
			}
		}
		text = text[:start] + others.String() + stateBlock(*dr.db, dr.enable) + "\n" + text[end:]
	}
	if d.block != "" {
		text += "\n# Found: " + strings.Join(d.kinds, ", ") + "\n" + d.block
	}
	return text
}

// baseURL is the address of the single target: the one chosen, else the
// framework's default.
func (dr *draft) baseURL() string {
	if dr.url != "" {
		return dr.url
	}
	return dr.d.baseURL
}

// write writes the new project's files: the ignore entries first, then
// driftgate.yaml, an empty catalog, the allowlist and the exclusions.
func (dr *draft) write() error {
	l, d := dr.p.Layout, dr.d
	if err := l.WriteIgnores(); err != nil {
		return err
	}
	if err := snapshot.WriteFile(l.Config, []byte(dr.configText()), 0o644); err != nil {
		return err
	}
	if err := (&catalog.Catalog{Requests: []catalog.Request{}}).Save(l.Catalog); err != nil {
		return err
	}
	allow := initAllowlist
	if d.allow != "" {
		allow = strings.Replace(allow, "body: []\n", "body:\n"+d.allow, 1)
	}
	allow = strings.Replace(allow, "body:", strings.TrimSuffix(d.headers, "\n")+"\nbody:", 1)
	allow = strings.Replace(allow, "\n\nbody:", "\nbody:", 1)
	if err := snapshot.WriteFile(l.Allowlist, []byte(allow), 0o644); err != nil {
		return err
	}
	if len(d.excluded)+len(d.excludedFor) > 0 {
		var ex strings.Builder
		ex.WriteString("version: 1\nroutes:\n")
		for _, e := range d.excluded {
			fmt.Fprintf(&ex, "  - route: %s\n    reason: %s\n", yamlQuote(e[0]), yamlQuote(e[1]))
		}
		for _, e := range d.excludedFor {
			fmt.Fprintf(&ex, "  - target: %s\n    route: %s\n    reason: %s\n", e[0], yamlQuote(e[1]), yamlQuote(e[2]))
		}
		if err := snapshot.WriteFile(l.Path("exclusions.yaml"), []byte(ex.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}
