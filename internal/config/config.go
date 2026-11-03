// Package config reads driftgate.yaml and locates the project's files
// (Layout): with baselines: local all of them in the project's .driftgate/
// folder, which git ignores; with baselines: git the driftgate/ folder in
// the repository and the local store outside it.
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/discover"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

const (
	Version  = 1
	FileName = "driftgate.yaml"
	Dir      = "driftgate"
	EnvStore = "DRIFTGATE_STORE"
)

// Modes.
const (
	Freeze = "freeze"
	Evolve = "evolve"
)

// Config is driftgate.yaml. An unknown key is an error, not a warning.
type Config struct {
	Version    int                  `yaml:"version"`
	Project    string               `yaml:"project"`
	Mode       string               `yaml:"mode"`
	Level      string               `yaml:"level"`
	Baselines  string               `yaml:"baselines,omitempty"` // local or git; default: where driftgate.yaml is (Layout)
	Store      string               `yaml:"store,omitempty"`
	Key        secret.Source        `yaml:"key,omitempty"`
	Target     Target               `yaml:"target,omitempty"`  // one service
	Targets    map[string]Target    `yaml:"targets,omitempty"` // several services, by name (an API, a frontend, microservices)
	Identities map[string]Identity  `yaml:"identities,omitempty"`
	Tokens     Tokens               `yaml:"tokens,omitempty"`
	State      map[string]Component `yaml:"state,omitempty"`
	Seed       [][]string           `yaml:"seed,omitempty"`    // load the data into the working state; driftgate seed then copies it to the pristine one
	Prepare    [][]string           `yaml:"prepare,omitempty"` // run after every restore (migrations and the like)
	Guards     []Guard              `yaml:"guards,omitempty"`
	PII        PII                  `yaml:"pii,omitempty"`
	Discover   []discover.Adapter   `yaml:"discover,omitempty"`
	Generate   Generate             `yaml:"generate,omitempty"`
	Templates  []string             `yaml:"templates,omitempty"`
	Params     []Param              `yaml:"params,omitempty"`
	Storage    Storage              `yaml:"storage,omitempty"`
	Observe    []Observe            `yaml:"observe,omitempty"`
	Bisect     Bisect               `yaml:"bisect,omitempty"`
	Data       Data                 `yaml:"data,omitempty"`
	Uploads    Uploads              `yaml:"uploads,omitempty"`
}

// Data configures driftgate data: the database that is read (a copy of
// production data, never written) and the database that receives the
// anonymized copy.
type Data struct {
	Driver string `yaml:"driver,omitempty"` // mysql (also MariaDB) or postgres
	Source DataDB `yaml:"source,omitempty"`
	Target DataDB `yaml:"target,omitempty"`
	// AllowRemote lets the target be on a host outside target.allow_hosts.
	// The target is dropped and created again, so a wrong DSN must not
	// reach a server that is not a copy.
	AllowRemote bool `yaml:"allow_remote,omitempty"`
	// Key replaces values deterministically; default: the project key.
	Key secret.Source `yaml:"key,omitempty"`
	// Files is the folder of uploaded files and the folder that receives
	// their placeholders (same names, neutral content); relative to the
	// project root.
	Files DataFiles `yaml:"files,omitempty"`
}

// DataFiles are the folders of driftgate data's file placeholders.
type DataFiles struct {
	Source string `yaml:"source,omitempty"`
	Target string `yaml:"target,omitempty"`
}

// DataDB is one database of the data module.
type DataDB struct {
	DSN      Secret `yaml:"dsn,omitempty"`      // the server; for the target, default the source's
	Database string `yaml:"database,omitempty"` // the database on it
}

// IsZero lets omitempty skip an unset data section.
func (d Data) IsZero() bool {
	return d.Driver == "" && d.Source.Database == "" && d.Target.Database == ""
}

func (d Data) validate() error {
	switch d.Driver {
	case MySQL, "mariadb", Postgres:
	default:
		return fmt.Errorf("data.driver must be mysql or postgres, got %q", d.Driver)
	}
	if d.Source.DSN.IsZero() {
		return errors.New("data.source needs dsn (" + dsnSources + ")")
	}
	if err := d.Source.DSN.validate(); err != nil {
		return fmt.Errorf("data.source.dsn: %w", err)
	}
	if !d.Target.DSN.IsZero() {
		if err := d.Target.DSN.validate(); err != nil {
			return fmt.Errorf("data.target.dsn: %w", err)
		}
	}
	switch {
	case d.Source.Database == "":
		return errors.New("data.source needs database")
	case d.Target.Database == "":
		return errors.New("data.target needs database")
	case d.Target.Database == d.Source.Database && d.Target.DSN.IsZero():
		return errors.New("data.target must be another database than data.source")
	}
	if (d.Files.Source == "") != (d.Files.Target == "") {
		return errors.New("data.files needs both source and target")
	}
	return nil
}

// Bisect configures driftgate bisect: the commands that put the application
// on a commit checked out in a separate worktree, and wait until it answers.
// They run without a shell, from the project folder, with the worktree and
// the commit in DRIFTGATE_BISECT_DIR and DRIFTGATE_BISECT_COMMIT.
type Bisect struct {
	Checkout     []string `yaml:"checkout,omitempty"`      // builds or restarts the application from the worktree; failing skips the commit
	Ready        []string `yaml:"ready,omitempty"`         // repeated until it succeeds; failing within ready_timeout skips the commit
	Finish       []string `yaml:"finish,omitempty"`        // once at the end, e.g. to start the application from the checkout again
	Timeout      Duration `yaml:"timeout,omitempty"`       // limit of the checkout command; default 10m
	ReadyTimeout Duration `yaml:"ready_timeout,omitempty"` // default 1m
}

// Observe marks generated requests that are measured but never compared:
// their outcome depends on timing or on a cache of the application.
type Observe struct {
	Target string `yaml:"target,omitempty"` // only requests of this service
	Match  string `yaml:"match"`            // glob over "METHOD /route/path", "/route/path" or the request ID
	Reason string `yaml:"reason,omitempty"` // why it cannot be compared
}

// Services returns the targets by name; a configuration with one target has
// it under the empty name.
func (c Config) Services() map[string]Target {
	if len(c.Targets) > 0 {
		return c.Targets
	}
	return map[string]Target{"": c.Target}
}

// AllowHostLists returns the allow_hosts of every service that has them,
// the lists a database under state: or a data target is checked against.
func (c Config) AllowHostLists() [][]string {
	var out [][]string
	for _, t := range c.Services() {
		if len(t.AllowHosts) > 0 {
			out = append(out, t.AllowHosts)
		}
	}
	return out
}

// TargetNames lists the services in name order.
func (c Config) TargetNames() []string {
	var out []string
	for n := range c.Services() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TargetFor returns the target of a request or adapter. With several
// targets the name is required; with one, it must be empty or that name.
func (c Config) TargetFor(name string) (Target, error) {
	if len(c.Targets) == 0 {
		if name != "" {
			return Target{}, fmt.Errorf("target %q: the configuration has one target and no targets: section", name)
		}
		return c.Target, nil
	}
	if name == "" {
		if len(c.Targets) == 1 {
			for _, t := range c.Targets {
				return t, nil
			}
		}
		return Target{}, fmt.Errorf("no target named: there are several (%s)", strings.Join(c.TargetNames(), ", "))
	}
	t, ok := c.Targets[name]
	if !ok {
		return Target{}, fmt.Errorf("unknown target %q (%s)", name, strings.Join(c.TargetNames(), ", "))
	}
	return t, nil
}

// Canonical is the name a request, route or rule should carry: the empty
// name means the only service when there is one.
func (c Config) Canonical(name string) string {
	if name == "" && len(c.Targets) == 1 {
		for n := range c.Targets {
			return n
		}
	}
	return name
}

// Generate configures driftgate generate.
type Generate struct {
	Identity string `yaml:"identity,omitempty"` // for routes that need an authenticated user
}

// Templates of generated requests.
var Templates = []string{"ok", "not-found", "invalid-param", "anonymous", "identities", "json", "empty-body"}

// Param says where the value of a route parameter comes from: a fixed value,
// a list, a read-only query on the pristine database, or another response
// during the capture.
type Param struct {
	Target  string   `yaml:"target,omitempty"` // only routes of this service
	Name    string   `yaml:"name"`
	Route   string   `yaml:"route,omitempty"` // glob over route paths; empty: every route with the parameter
	Value   string   `yaml:"value,omitempty"`
	Values  []string `yaml:"values,omitempty"`
	SQL     string   `yaml:"sql,omitempty"`
	State   string   `yaml:"state,omitempty"` // component the query runs on; default: the only database
	From    *From    `yaml:"from,omitempty"`
	Foreach bool     `yaml:"foreach,omitempty"` // one request per value instead of the first value only
}

// From takes a value from the response of another request.
type From struct {
	Request string `yaml:"request" json:"request"`
	Path    string `yaml:"path" json:"path"` // JSON path in its body, e.g. $.entry.id
}

// Storage of generated requests: private unless a glob over the request ID
// says public.
type Storage struct {
	Default string   `yaml:"default,omitempty"`
	Public  []string `yaml:"public,omitempty"`
}

var orderBy = lazy.Regexp(`(?i)\border\s+by\b`)

func (pr Param) validate() error {
	sources := 0
	for _, set := range []bool{pr.Value != "", len(pr.Values) > 0, pr.SQL != "", pr.From != nil} {
		if set {
			sources++
		}
	}
	switch {
	case pr.Name == "":
		return errors.New("needs name")
	case sources != 1:
		return errors.New("needs exactly one of value, values, sql and from")
	case pr.Foreach && pr.Value != "":
		return errors.New("foreach needs values or sql")
	case pr.Foreach && pr.From != nil:
		return errors.New("foreach cannot take a value from a response")
	case pr.SQL != "" && !orderBy().MatchString(pr.SQL):
		return errors.New("sql needs ORDER BY, so the generated catalog is the same every time")
	case pr.From != nil && (pr.From.Request == "" || !strings.HasPrefix(pr.From.Path, "$")):
		return errors.New("from needs request and a path starting with $")
	}
	return nil
}

// Target is where requests go.
type Target struct {
	BaseURL         string      `yaml:"base_url,omitempty"`
	AllowHosts      []string    `yaml:"allow_hosts,omitempty"` // host names or CIDR ranges; default: local and private
	UserAgent       string      `yaml:"user_agent,omitempty"`
	Timeout         Duration    `yaml:"timeout,omitempty"`
	Concurrency     Concurrency `yaml:"concurrency,omitempty"`
	FollowRedirects bool        `yaml:"follow_redirects,omitempty"`
	RateLimits      []RateLimit `yaml:"rate_limits,omitempty"`
	// Aliases are other origins under which the application writes its
	// own URLs. A comparison of sets captured from different origins takes
	// every side's own origin and these as the same.
	Aliases []string `yaml:"aliases,omitempty"`
}

// Origins returns the origins of a set captured from origin: it and the
// aliases. None when origin is not known.
func (t Target) Origins(origin string) []string {
	if origin == "" {
		return nil
	}
	return append([]string{origin}, t.Aliases...)
}

func checkOrigin(s string) error {
	o, err := compare.Origin(s)
	if err != nil {
		return err
	}
	if strings.TrimSuffix(s, "/") != o {
		return fmt.Errorf("%q is not an origin; write it as %s", s, o)
	}
	return nil
}

// Concurrency is how many read-only requests of a service are sent at once:
// a number (0 and 1: one at a time), or auto, which the first baseline
// decides by measuring and records in its meta.json.
type Concurrency int

// ConcurrencyAuto is concurrency: auto.
const ConcurrencyAuto Concurrency = -1

// Auto reports concurrency: auto.
func (c Concurrency) Auto() bool { return c == ConcurrencyAuto }

// Fixed is the number of requests at once without a decision of auto: auto
// and anything below 1 mean one at a time.
func (c Concurrency) Fixed() int { return max(int(c), 1) }

func (c *Concurrency) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Value == "auto" {
		*c = ConcurrencyAuto
		return nil
	}
	var v int
	if err := n.Decode(&v); err != nil || v < 0 {
		return fmt.Errorf("concurrency must be a number of at least 0, or auto (got %q)", n.Value)
	}
	*c = Concurrency(v)
	return nil
}

func (c Concurrency) MarshalYAML() (any, error) {
	if c.Auto() {
		return "auto", nil
	}
	return int(c), nil
}

// RateLimit caps how often requests whose path matches may be sent.
type RateLimit struct {
	Match     string `yaml:"match"`
	PerMinute int    `yaml:"per_minute"`
}

// Identity is a named user; its token comes from Env or Op, else from Tokens.
type Identity struct {
	Env string `yaml:"env,omitempty"`
	Op  string `yaml:"op,omitempty"`
}

// Tokens is a source of a JSON object {"<identity>": "<token>"}: a command
// that prints it or a file that holds it (kept outside the repository).
type Tokens struct {
	Command []string `yaml:"command,omitempty"`
	File    string   `yaml:"file,omitempty"`
}

// Component is one part of the state the application runs on: a database
// with a built-in driver, or anything else through restore and fingerprint
// commands.
type Component struct {
	Driver      string   `yaml:"driver,omitempty"`   // mysql (also MariaDB), postgres, sqlite or sqlserver (also mssql); empty: commands
	DSN         Secret   `yaml:"dsn,omitempty"`      // SQLite: optional, cross-checked with working
	Working     string   `yaml:"working,omitempty"`  // database the application uses; SQLite: its file, relative to the project root
	Pristine    string   `yaml:"pristine,omitempty"` // its pristine copy; default <working>_pristine (SQLite: a file of the local store)
	Exclude     []string `yaml:"exclude,omitempty"`  // tables never fingerprinted or restored (globs)
	RowTracking []string `yaml:"row_tracking,omitempty"`
	// AllowRemote lets the database be on a host outside target.allow_hosts.
	// A restore rewrites its tables, so a wrong DSN must not reach a server
	// that is not a copy.
	AllowRemote bool     `yaml:"allow_remote,omitempty"`
	Restore     []string `yaml:"restore,omitempty"`
	Fingerprint []string `yaml:"fingerprint,omitempty"`
}

// Drivers.
const (
	MySQL     = "mysql"
	Postgres  = "postgres"
	SQLite    = "sqlite"
	SQLServer = "sqlserver"
)

// drivers names the drivers in messages.
const drivers = "mysql, postgres, sqlite, sqlserver"

func (c Component) validate() error {
	switch c.Driver {
	case "":
		switch {
		case len(c.Restore) == 0:
			return errors.New("needs a driver (" + drivers + ") or a restore command")
		case !c.DSN.IsZero() || c.Working != "" || c.Pristine != "" || len(c.Exclude) > 0 || len(c.RowTracking) > 0:
			return errors.New("dsn, working, pristine, exclude and row_tracking need a driver")
		}
		return nil
	case MySQL, "mariadb", Postgres, SQLServer, "mssql":
		if err := c.validateServer(); err != nil {
			return err
		}
	case SQLite:
		if err := c.validateSQLite(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown driver %q (%s)", c.Driver, drivers)
	}
	if len(c.Restore) > 0 || len(c.Fingerprint) > 0 {
		return errors.New("restore and fingerprint commands are for components without a driver")
	}
	for _, t := range c.RowTracking {
		for _, g := range c.Exclude {
			if glob.Match(g, t) {
				return fmt.Errorf("row_tracking table %s is excluded", t)
			}
		}
	}
	return nil
}

// validateServer checks a component of a database server: MySQL, MariaDB,
// PostgreSQL or SQL Server, whose pristine copy is a second database on it.
func (c Component) validateServer() error {
	if c.DSN.IsZero() {
		return errors.New("needs dsn (" + dsnSources + ")")
	}
	if err := c.DSN.validate(); err != nil {
		return fmt.Errorf("dsn: %w", err)
	}
	switch {
	case c.Working == "":
		return errors.New("needs working (the database the application uses)")
	case c.Pristine == c.Working, c.Driver != Postgres && strings.EqualFold(c.Pristine, c.Working):
		// MySQL and MariaDB on macOS and Windows (lower_case_table_names 1
		// or 2) take App and app for the same database, as SQL Server does
		// under its default, case-insensitive collation.
		return errors.New("pristine must differ from working (MySQL, MariaDB and SQL Server: also in letter case)")
	}
	return nil
}

// Guard is a command that must succeed before any request that changes data.
type Guard struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
}

// PII configures the detector of personal data in public bodies.
type PII struct {
	Allow []string `yaml:"allow,omitempty"` // regular expressions of values that may appear in public bodies
}

// Duration is a time.Duration written as "30s" in YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q", n.Value)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// IsZero lets omitempty skip an unset duration.
func (d Duration) IsZero() bool { return d.Duration == 0 }

// Project is a loaded configuration with resolved locations.
type Project struct {
	Config
	Root   string // the project folder; driftgate.yaml is in it or in its .driftgate/
	Level  compare.Level
	Layout Layout // where the project's files are

	storeErr error // why Layout has no store
	ownStore bool  // the store is the layout's own, not one named by store: or DRIFTGATE_STORE
	loaded   bool  // read from Layout.Config (not a configuration yet to be written)

	keyMu sync.Mutex // captures write private bodies from several goroutines
	key   secret.Key
}

// projectName checks a name that names a file or folder (snapshot.SimpleName).
var projectName = simpleName{}

type simpleName struct{}

func (simpleName) MatchString(name string) bool { return snapshot.SimpleName(name) }

// ErrNoConfig means driftgate.yaml was not found.
var ErrNoConfig = errors.New(FileName + " not found")

// Load reads the configuration file; an empty path is the configuration of
// the project the working folder is in (Find).
func Load(path string) (*Project, error) { return load(path, true) }

// load is Load; strict refuses a project with a configuration in both
// places or a switch of baselines: in progress, which only the switch
// itself reads past.
func load(path string, strict bool) (*Project, error) {
	if path == "" {
		var err error
		if path, err = Find("."); err != nil {
			return nil, err
		}
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w (%s)", ErrNoConfig, path)
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	c := Config{Mode: Freeze, Level: "values"}
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !onlyDocument(dec) {
		return nil, fmt.Errorf("%s: only one YAML document is read; remove the --- and what follows, or merge it into the first", path)
	}
	file, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// The place of the file is the mode; baselines: may only confirm it.
	root, mode := locate(file)
	if _, err := configIn(root); err != nil && strict {
		return nil, err
	}
	switch c.Baselines {
	case "", mode:
	case BaselinesLocal, BaselinesGit:
		return nil, fmt.Errorf("%s: baselines: %s, but the file is where baselines: %s keeps it; driftgate baselines %s moves the project's files", path, c.Baselines, mode, c.Baselines)
	default:
		return nil, fmt.Errorf("%s: baselines must be %s or %s, got %q", path, BaselinesLocal, BaselinesGit, c.Baselines)
	}
	p, err := newProject(c, root, mode)
	if err != nil {
		return nil, err
	}
	p.Layout.Config, p.loaded = file, true
	return p, nil
}

// onlyDocument reports whether dec, past its first document, holds nothing
// but empty documents: what follows a --- would otherwise be ignored, with
// every setting in it.
func onlyDocument(dec *yaml.Decoder) bool {
	for {
		var n yaml.Node
		if err := dec.Decode(&n); errors.Is(err, io.EOF) {
			return true
		} else if err != nil || len(n.Content) != 1 || n.Content[0].Tag != "!!null" {
			return false
		}
	}
}

// New validates a configuration for a project rooted at root, laid out as
// its baselines: says (default git, as earlier releases laid out every project).
func New(c Config, root string) (*Project, error) {
	mode := c.Baselines
	if mode == "" {
		mode = BaselinesGit
	}
	return newProject(c, root, mode)
}

func newProject(c Config, root, mode string) (*Project, error) {
	if c.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d (this build reads version %d)", FileName, c.Version, Version)
	}
	if !projectName.MatchString(c.Project) {
		return nil, fmt.Errorf("%s: project must be a simple name (letters, digits, . _ -; not a name Windows reserves such as con or nul), got %q", FileName, c.Project)
	}
	if c.Mode != Freeze && c.Mode != Evolve {
		return nil, fmt.Errorf("%s: mode must be %s or %s", FileName, Freeze, Evolve)
	}
	level, err := compare.ParseLevel(c.Level)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	// Variables that hold a key are never passed on to the commands DriftGate
	// runs.
	secret.HideEnv(c.Key.Env)
	secret.HideEnv(c.Data.Key.Env)
	stateNames := map[string]string{}
	for name, comp := range c.State {
		// The name names files of the local store (pristine/<name>.json).
		if !projectName.MatchString(name) {
			return nil, fmt.Errorf("%s: state name %q must be a simple name (letters, digits, . _ -; not a name Windows reserves such as con or nul)", FileName, name)
		}
		if other, ok := stateNames[snapshot.FoldName(name)]; ok {
			a, b := min(other, name), max(other, name)
			return nil, fmt.Errorf("%s: state names %q and %q differ only in case; on macOS and Windows they would share files", FileName, a, b)
		}
		stateNames[snapshot.FoldName(name)] = name
		if err := comp.validate(); err != nil {
			return nil, fmt.Errorf("%s: state %s: %w", FileName, name, err)
		}
		switch comp.Driver {
		case "mariadb":
			comp.Driver = MySQL
		case "mssql":
			comp.Driver = SQLServer
		}
		// SQLite's pristine copy defaults to a file of the local store,
		// which the capture names (it is not a setting).
		if comp.Driver != "" && comp.Driver != SQLite && comp.Pristine == "" {
			comp.Pristine = comp.Working + "_pristine"
		}
		c.State[name] = comp
	}
	if !c.Data.IsZero() {
		if err := c.Data.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", FileName, err)
		}
		if c.Data.Driver == "mariadb" {
			c.Data.Driver = MySQL
		}
	}
	for _, list := range [][][]string{c.Seed, c.Prepare} {
		for _, cmd := range list {
			if len(cmd) == 0 {
				return nil, fmt.Errorf("%s: seed and prepare commands must not be empty", FileName)
			}
		}
	}
	if (len(c.Bisect.Ready) > 0 || len(c.Bisect.Finish) > 0) && len(c.Bisect.Checkout) == 0 {
		return nil, fmt.Errorf("%s: bisect needs checkout (the command that puts the application on a commit)", FileName)
	}
	if c.Bisect.Timeout.Duration < 0 || c.Bisect.ReadyTimeout.Duration < 0 {
		return nil, fmt.Errorf("%s: bisect timeouts must not be negative", FileName)
	}
	for i, g := range c.Guards {
		if g.Name == "" || len(g.Command) == 0 {
			return nil, fmt.Errorf("%s: guard %d needs name and command", FileName, i+1)
		}
	}
	if len(c.Targets) > 0 && !reflect.DeepEqual(c.Target, Target{}) {
		return nil, fmt.Errorf("%s: use target (one service) or targets (several), not both", FileName)
	}
	for name, t := range c.Services() {
		where := "target"
		if name != "" {
			where = "targets." + name
			if !projectName.MatchString(name) {
				return nil, fmt.Errorf("%s: target name %q must be a simple name (letters, digits, . _ -; not a name Windows reserves such as con or nul)", FileName, name)
			}
		}
		for _, rl := range t.RateLimits {
			if rl.Match == "" || rl.PerMinute <= 0 {
				return nil, fmt.Errorf("%s: %s: rate limit needs match and a positive per_minute", FileName, where)
			}
		}
		// http.Client takes a timeout of 0 or less as none: a request
		// that never answers would hold the run until CI kills it.
		if t.Timeout.Duration < 0 {
			return nil, fmt.Errorf("%s: %s.timeout must be positive (0 or unset: 60s)", FileName, where)
		}
		if t.Concurrency < 0 && !t.Concurrency.Auto() {
			return nil, fmt.Errorf("%s: %s.concurrency must not be negative", FileName, where)
		}
		for _, a := range t.Aliases {
			if err := checkOrigin(a); err != nil {
				return nil, fmt.Errorf("%s: %s.aliases: %w", FileName, where, err)
			}
		}
	}
	for i, a := range c.Discover {
		if _, err := c.TargetFor(a.Target); err != nil {
			return nil, fmt.Errorf("%s: discover %d: %w", FileName, i+1, err)
		}
	}
	for i, o := range c.Observe {
		if o.Target != "" {
			if _, err := c.TargetFor(o.Target); err != nil {
				return nil, fmt.Errorf("%s: observe %d: %w", FileName, i+1, err)
			}
		}
	}
	for i, pr := range c.Params {
		if pr.Target != "" {
			if _, err := c.TargetFor(pr.Target); err != nil {
				return nil, fmt.Errorf("%s: params %d: %w", FileName, i+1, err)
			}
		}
	}
	if c.Tokens.File != "" && len(c.Tokens.Command) > 0 {
		return nil, fmt.Errorf("%s: tokens takes command or file, not both", FileName)
	}
	for i, a := range c.Discover {
		if err := a.Validate(); err != nil {
			return nil, fmt.Errorf("%s: discover %d: %w", FileName, i+1, err)
		}
	}
	for _, t := range c.Templates {
		known := false
		for _, k := range Templates {
			known = known || k == t
		}
		if !known {
			return nil, fmt.Errorf("%s: unknown template %q (%s)", FileName, t, strings.Join(Templates, ", "))
		}
	}
	for i, pr := range c.Params {
		if err := pr.validate(); err != nil {
			return nil, fmt.Errorf("%s: params %d (%s): %w", FileName, i+1, pr.Name, err)
		}
		if pr.State != "" && c.State[pr.State].Driver == "" {
			return nil, fmt.Errorf("%s: params %d: state %s is not a database", FileName, i+1, pr.State)
		}
	}
	if c.Storage.Default != "" && c.Storage.Default != snapshot.Public && c.Storage.Default != snapshot.Private {
		return nil, fmt.Errorf("%s: storage.default must be public or private", FileName)
	}
	if c.Generate.Identity != "" {
		if _, ok := c.Identities[c.Generate.Identity]; !ok && c.Tokens.Command == nil && c.Tokens.File == "" {
			return nil, fmt.Errorf("%s: generate.identity %s is not an identity and no tokens source is configured", FileName, c.Generate.Identity)
		}
	}
	for _, p := range c.PII.Allow {
		if _, err := regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("%s: pii.allow %q: %w", FileName, p, err)
		}
	}
	l, storeErr := NewLayout(c, root, mode)
	if l.Dir == "" {
		return nil, storeErr // an unknown mode
	}
	return &Project{Config: c, Root: root, Level: level, Layout: l, storeErr: storeErr,
		ownStore: os.Getenv(EnvStore) == "" && c.Store == ""}, nil
}

// Save writes the configuration.
func (p *Project) Save() error {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(p.Config); err != nil {
		return err
	}
	return snapshot.WriteFile(p.Layout.Config, b.Bytes(), 0o644)
}

// Path joins elements under the folder of the project's files.
func (p *Project) Path(elem ...string) string { return p.Layout.Path(elem...) }

// ErrInvalid marks an error of the configuration that is found only when it
// is used (the cli exits 2 for it, not 3 as for the environment).
var ErrInvalid = errors.New("invalid configuration")

type invalid struct{ error }

func (invalid) Is(target error) bool { return target == ErrInvalid }

// StoreDir is the local store (Layout.Store), made when missing. A store
// folder DriftGate makes is open to its owner only.
func (p *Project) StoreDir() (string, error) {
	if p.storeErr != nil {
		return "", p.storeErr
	}
	dir := p.Layout.Store
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if p.ownStore {
		// The default store is DriftGate's own, also when an older
		// version made it open to others.
		os.Chmod(filepath.Dir(dir), 0o700)
		os.Chmod(dir, 0o700)
	}
	return dir, nil
}

// CheckoutID names the checkout of the project (its folder) in a store
// that several checkouts share: its run capture and last comparison.
func (p *Project) CheckoutID() string {
	sum := sha256.Sum256([]byte(p.Root))
	return fmt.Sprintf("%x", sum[:6])
}

// Current says whether the project is still laid out as it was loaded: a
// command that waited for the store lock while driftgate baselines moved
// the project must not write into the old place.
func (p *Project) Current() error {
	if j, ok := readJournal(p.Root); ok {
		return invalid{fmt.Errorf("a switch of the project to baselines: %s is in progress; run the command again once it is done", j.mode)}
	}
	if p.loaded && !exists(p.Layout.Config) {
		return invalid{fmt.Errorf("the project moved while this command waited (%s is gone; driftgate baselines?); run the command again", p.Layout.Config)}
	}
	return nil
}

// WriteStore is StoreDir for a command that writes the store: a store in
// the project's own folder is used only once git ignores that folder.
func (p *Project) WriteStore() (string, error) {
	if p.storeErr != nil {
		return "", p.storeErr
	}
	// The key, the configuration and the baselines are in the folder, also
	// when the store is elsewhere (DRIFTGATE_STORE, store:).
	if err := p.Layout.checkIgnored(false); err != nil {
		return "", err
	}
	return p.StoreDir()
}

// repository is the top folder of the git work tree that holds dir: the
// nearest folder with .git (a folder, or a file in a worktree or
// submodule), as git rev-parse --show-toplevel finds it; dir itself when
// there is none.
func repository(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// within reports whether path is dir or inside it, also through symbolic
// links.
func within(dir, path string) bool {
	in := func(dir, path string) bool {
		rel, err := filepath.Rel(dir, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if in(dir, path) {
		return true
	}
	// The path may not exist yet: resolve its longest existing part.
	real := func(p string) string {
		rest := ""
		for {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				return filepath.Join(r, rest)
			}
			parent := filepath.Dir(p)
			if parent == p {
				return p
			}
			rest = filepath.Join(filepath.Base(p), rest)
			p = parent
		}
	}
	return in(real(dir), real(path))
}

func expand(path string) (string, error) {
	if rest, ok := HomeRelative(path); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, rest)
	}
	return filepath.Abs(path)
}

// separator is filepath.Separator; a test sets Windows' one.
var separator = filepath.Separator

// HomeRelative reports a path written from the home folder, ~/… or, on
// Windows, also ~\…, and returns the rest after the ~ and the separator.
func HomeRelative(path string) (string, bool) {
	if strings.HasPrefix(path, "~/") || (separator == '\\' && strings.HasPrefix(path, `~\`)) {
		return path[2:], true
	}
	return "", false
}

// LoadKey returns the project key (key: in the configuration, then
// DRIFTGATE_KEY, then the layout's key file or, without one, the macOS
// Keychain); it is loaded once, on first use.
func (p *Project) LoadKey() (secret.Key, error) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.key == nil {
		k, err := secret.Load(p.Project, p.Key, p.Layout.Key)
		if errors.Is(err, secret.ErrNoKey) {
			if n := p.sealedBodies(); n > 0 {
				err = noKey{fmt.Errorf("no encryption key, and the store holds %d private bodies sealed with the project key (%s): put that key back (%s, DRIFTGATE_KEY or key: in %s); driftgate key create would make another key, which cannot read them", n, p.Layout.Private, keyHome(p.Layout), FileName)}
			}
		}
		if err != nil {
			return nil, err
		}
		p.key = k
	}
	return p.key, nil
}

// CreateKey makes the project key where LoadKey finds it: in the layout's
// key file (once git ignores its folder), else in the macOS Keychain. It
// returns where it is.
func (p *Project) CreateKey() (string, error) {
	if p.Layout.Key == "" {
		if _, err := p.LoadKey(); errors.Is(err, secret.ErrNoKey) {
			if err := p.orphans(); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("the macOS Keychain (service driftgate, account %s)", p.Project), secret.Create(p.Project)
	}
	if err := p.Layout.checkIgnored(true); err != nil {
		return "", err
	}
	if !exists(p.Layout.Key) {
		if err := p.orphans(); err != nil {
			return "", err
		}
	}
	return p.Layout.Key, secret.CreateFile(p.Layout.Key)
}

// Private is the local store of encrypted private bodies, of captures and
// baselines alike: a private body never enters the repository.
func (p *Project) Private() (*snapshot.PrivateStore, error) {
	if _, err := p.StoreDir(); err != nil {
		return nil, err
	}
	return &snapshot.PrivateStore{Dir: p.Layout.Private, Ext: ".enc", Key: p.LoadKey, KeyHint: p.keyHint}, nil
}

// Capture is a snapshot set in the local store. An invalid tag is
// ErrInvalid; any other error is one of the store (it cannot be made).
func (p *Project) Capture(tag string) (*snapshot.Set, error) {
	if !projectName.MatchString(tag) {
		return nil, invalid{fmt.Errorf("invalid tag %q (letters, digits, . _ -; not a name Windows reserves such as con or nul)", tag)}
	}
	priv, err := p.Private()
	if err != nil {
		return nil, err
	}
	return &snapshot.Set{Dir: filepath.Join(p.Layout.Captures, tag), Private: priv}, nil
}

// Baseline is a snapshot set in the repository; its private bodies are in
// the local store, so another machine has only their refs. An invalid name
// is ErrInvalid; any other error is one of the store.
func (p *Project) Baseline(name string) (*snapshot.Set, error) {
	if !projectName.MatchString(name) {
		return nil, invalid{fmt.Errorf("invalid baseline name %q (letters, digits, . _ -; not a name Windows reserves such as con or nul)", name)}
	}
	priv, err := p.Private()
	if err != nil {
		return nil, err
	}
	return &snapshot.Set{Dir: filepath.Join(p.Layout.Baselines, name), Private: priv}, nil
}
