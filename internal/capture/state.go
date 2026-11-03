package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// states holds a fingerprint per component.
type states map[string]state.Fingerprint

// diffStates lists the changed items per component.
func diffStates(a, b states) map[string][]string {
	out := map[string][]string{}
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	for n := range names {
		if d := state.Diff(a[n], b[n]); len(d) > 0 {
			out[n] = d
		}
	}
	return out
}

// flatten writes changed items as <component>/<item>, sorted.
func flatten(d map[string][]string) []string {
	out := []string{}
	for n, items := range d {
		for _, it := range items {
			out = append(out, n+"/"+it)
		}
	}
	sort.Strings(out)
	return out
}

func toMeta(s states) map[string]map[string]string {
	out := map[string]map[string]string{}
	for n, f := range s {
		out[n] = f
	}
	return out
}

func fromMeta(m map[string]map[string]string) states {
	out := states{}
	for n, f := range m {
		out[n] = f
	}
	return out
}

// pristineFile records the fingerprint of a component's pristine copy, so a
// restore knows what to bring back without reading the pristine copy.
type pristineFile struct {
	Version     int               `json:"version"`
	Format      int               `json:"format,omitempty"` // state.FingerprintFormat; 0 in a file of format 1
	Component   string            `json:"component"`
	Driver      string            `json:"driver"`
	Server      string            `json:"server,omitempty"` // serverOf; none in a file of an earlier release
	Working     string            `json:"working"`
	Pristine    string            `json:"pristine"`
	Created     string            `json:"created"`
	Fingerprint state.Fingerprint `json:"fingerprint"`
}

// pristinePath is the pristine file of a database component on a server.
// The store is shared by every checkout of the project (worktrees), which
// may each use their own database server: the file is named by a hash of
// what identifies the databases, pristine/<name>-<hash>.json, so one
// checkout never takes the fingerprint of another's database.
//
// A file of an earlier release, pristine/<name>.json, records no server: when
// there is no file for this server yet, one for the same driver and
// databases is taken over (renamed) once. Should it have been made on
// another server, the first restore finds the database different from it
// and stops, and driftgate seed makes the right one.
func pristinePath(p *config.Project, name, server string) (string, error) {
	if _, err := p.StoreDir(); err != nil {
		return "", err
	}
	sc := p.State[name]
	sum := sha256.Sum256([]byte(strings.Join([]string{sc.Driver, server, sc.Working, sc.Pristine}, "\x00")))
	path := filepath.Join(p.Layout.Pristine, fmt.Sprintf("%s-%x.json", name, sum[:6]))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	legacy := filepath.Join(p.Layout.Pristine, name+".json")
	var pf pristineFile
	if raw, err := os.ReadFile(legacy); err == nil && json.Unmarshal(raw, &pf) == nil &&
		pf.Server == "" && pf.Driver == sc.Driver && pf.Working == sc.Working && pf.Pristine == sc.Pristine {
		if err := os.Rename(legacy, path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return path, nil
}

// readPristine reads the record of a component's pristine copy on a server
// (see pristinePath, which also takes over a record of an earlier release).
func readPristine(p *config.Project, name, server string) (pristineFile, error) {
	var pf pristineFile
	path, err := pristinePath(p, name, server)
	if err != nil {
		return pf, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return pf, err
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		return pf, fmt.Errorf("%s: %w", path, err)
	}
	return pf, nil
}

// recorded reports whether DriftGate's record of a pristine copy names the
// component's pristine database: a seed made it, so a seed may replace it
// even without the mark seeds now leave in it (state.Options.AdoptPristine).
// A record of an earlier release is found too: pristinePath takes it over first.
func recorded(p *config.Project, name, server string, sc config.Component) bool {
	pf, err := readPristine(p, name, server)
	return err == nil && pf.Driver == sc.Driver && pf.Working == sc.Working && pf.Pristine == sc.Pristine
}

// tracker restores and fingerprints the state components of a project and
// knows which items changed since the last restore.
type tracker struct {
	time     *Time
	comps    []state.Component
	pristine states              // fingerprint of the pristine copy, per database component
	formats  map[string]int      // the format the pristine fingerprints were recorded in
	dirty    map[string][]string // changed since the last restore; nil: unknown
	rows     bool                // some component tracks rows
	out      io.Writer
}

// openComponents connects to every state component, in name order.
func openComponents(ctx context.Context, p *config.Project, env []string, log io.Writer) ([]state.Component, error) {
	comps, _, err := openServers(ctx, p, env, log, false)
	return comps, err
}

// openServers is openComponents that also returns the server of every
// database component (serverOf), by name. For a seed, a component
// removes the record of its pristine copy right before it replaces that
// copy (state.Options.Replacing).
func openServers(ctx context.Context, p *config.Project, env []string, log io.Writer, seed bool) ([]state.Component, map[string]string, error) {
	names := make([]string, 0, len(p.State))
	for n := range p.State {
		names = append(names, n)
	}
	sort.Strings(names)
	var comps []state.Component
	servers := map[string]string{}
	closeAll := func() {
		for _, c := range comps {
			c.Close()
		}
	}
	for _, n := range names {
		sc := p.State[n]
		if sc.Driver == "" {
			comps = append(comps, state.NewCommand(n, p.Root, sc.Restore, sc.Fingerprint, env, log))
			continue
		}
		dsn, err := sc.ReadDSN(ctx, p.Root)
		if err != nil {
			closeAll()
			return nil, nil, dsnErr(sc.DSN, n, err)
		}
		if servers[n], err = serverOf(p, sc, dsn); err != nil {
			closeAll()
			return nil, nil, configErr("state %s: %v", n, err)
		}
		o := state.Options{Name: n, DSN: dsn, Working: sc.Working, Pristine: sc.Pristine, Exclude: sc.Exclude, RowTracking: sc.RowTracking, Log: log,
			AdoptPristine: recorded(p, n, servers[n], sc)}
		path, err := pristinePath(p, n, servers[n])
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		if sc.Driver == config.SQLite {
			if o.Working, o.Pristine, err = sqliteFiles(p, sc, servers[n], path); err != nil {
				closeAll()
				return nil, nil, configErr("state %s: %v", n, err)
			}
		}
		if seed {
			o.Replacing = func() error {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			}
		}
		// A restore rewrites the tables: the database must be on an allowed
		// host, and every host the driver may connect to is checked, first
		// by name and then at every connection, on the address dialled. An
		// SQLite database is a file of the project, on no host.
		if !sc.AllowRemote && sc.Driver != config.SQLite {
			const hint = "allow_remote: true for this component"
			if err := dbx.AllowHosts(ctx, sc.Driver, dsn, p.AllowHostLists(), hint); err != nil {
				closeAll()
				return nil, nil, configErr("state %s: %v", n, err)
			}
			if o.Dial, err = dbx.AllowDial(p.AllowHostLists(), hint); err != nil {
				closeAll()
				return nil, nil, configErr("state %s: %v", n, err)
			}
		}
		var c state.Component
		switch sc.Driver {
		case config.Postgres:
			c, err = state.NewPostgres(o)
		case config.SQLite:
			c, err = state.NewSQLite(o)
		case config.SQLServer:
			c, err = state.NewSQLServer(o)
		default:
			c, err = state.NewMySQL(o)
		}
		if err != nil {
			closeAll()
			return nil, nil, configErr("state %s: %v", n, err)
		}
		comps = append(comps, c)
	}
	return comps, servers, nil
}

func newTracker(ctx context.Context, p *config.Project, env []string, out, log io.Writer) (*tracker, error) {
	comps, servers, err := openServers(ctx, p, env, log, false)
	if err != nil {
		return nil, err
	}
	t := &tracker{time: &Time{}, comps: comps, pristine: states{}, formats: map[string]int{}, out: out}
	for _, c := range comps {
		sc := p.State[c.Name()]
		if sc.Driver == "" {
			continue
		}
		if len(sc.RowTracking) > 0 {
			t.rows = true
		}
		pf, err := readPristine(p, c.Name(), servers[c.Name()])
		if errors.Is(err, os.ErrNotExist) {
			t.close()
			return nil, envErr("state %s has no pristine copy yet; run driftgate seed", c.Name())
		}
		if err != nil {
			t.close()
			return nil, err
		}
		if pf.Driver != sc.Driver || pf.Working != sc.Working || pf.Pristine != sc.Pristine {
			t.close()
			return nil, envErr("state %s: the pristine copy was made for another configuration; run driftgate seed", c.Name())
		}
		t.pristine[c.Name()] = pf.Fingerprint
		t.formats[c.Name()] = max(pf.Format, 1)
	}
	return t, nil
}

func (t *tracker) close() {
	for _, c := range t.comps {
		c.Close()
	}
}

// fingerprint hashes every component, or only the given items. Components
// that cannot fingerprint are left out.
func (t *tracker) fingerprint(ctx context.Context, items map[string][]string) (states, error) {
	defer func(start time.Time) {
		t.time.Fingerprint += time.Since(start)
		t.time.Fingerprints++
	}(time.Now())
	out := states{}
	for _, c := range t.comps {
		var its []string
		if items != nil {
			if its = items[c.Name()]; len(its) == 0 {
				continue
			}
		}
		f, err := c.Fingerprint(ctx, its)
		if err != nil {
			return nil, envErr("state %s: fingerprint: %v", c.Name(), err)
		}
		if r, ok := c.(interface{ Reused() bool }); ok && r.Reused() {
			t.time.Reused++
		}
		if f != nil {
			out[c.Name()] = f
		}
	}
	return out, nil
}

func (t *tracker) markDirty(d map[string][]string) {
	if t.dirty == nil {
		return // unknown already means everything
	}
	for n, items := range d {
		seen := map[string]bool{}
		for _, it := range t.dirty[n] {
			seen[it] = true
		}
		for _, it := range items {
			if !seen[it] {
				t.dirty[n] = append(t.dirty[n], it)
				seen[it] = true
			}
		}
	}
}

// restore brings every component back to its pristine state. A database
// restores only the items that changed since the last restore (all of them
// on the first restore, found by fingerprint) and must then match its
// pristine fingerprint; a command component always runs its restore command.
func (t *tracker) restore(ctx context.Context) error {
	defer func(start time.Time) { t.time.Restore += time.Since(start) }(time.Now())
	for _, c := range t.comps {
		pf, isDB := t.pristine[c.Name()]
		if !isDB {
			fmt.Fprintf(t.out, "restoring %s …\n", c.Name())
			if err := c.Restore(ctx, nil); err != nil {
				return envErr("restore of %s failed: %v", c.Name(), err)
			}
			continue
		}
		var items []string
		if t.dirty == nil {
			cur, err := c.Fingerprint(ctx, nil)
			if err != nil {
				return envErr("state %s: fingerprint: %v", c.Name(), err)
			}
			items = state.Diff(cur, pf)
		} else {
			items = t.dirty[c.Name()]
		}
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(t.out, "restoring %s: %d changed %s …\n", c.Name(), len(items), plural(len(items), "item", "items"))
		start := time.Now()
		if err := c.Restore(ctx, items); err != nil {
			return envErr("restore of %s failed: %v", c.Name(), err)
		}
		got, err := c.Fingerprint(ctx, items)
		if err != nil {
			return envErr("state %s: fingerprint: %v", c.Name(), err)
		}
		if d := state.Diff(got, pf.Subset(items)); len(d) > 0 {
			if t.formats[c.Name()] != state.FingerprintFormat {
				return envErr("state %s: re-seed needed: the fingerprint format changed (the current format fingerprints these items another way: %v); run driftgate seed, then, if a capture says the baseline's state differs, driftgate verify --baseline <name> --migrate-state", c.Name(), limit(d, 20))
			}
			return envErr("restore of %s left %d items different from the pristine copy: %v", c.Name(), len(d), limit(d, 20))
		}
		fmt.Fprintf(t.out, "restored %s in %s\n", c.Name(), time.Since(start).Round(time.Millisecond))
	}
	t.dirty = map[string][]string{}
	return nil
}

// MigrateState proves that the state now is the one a baseline whose
// fingerprint has an older format (meta.StateFormat) was captured from,
// and returns the fingerprint in the current format for the baseline. The
// working state is restored from the pristine copy (which must have been
// seeded in the current format) and fingerprinted; the items whose hash
// differs from the baseline's are fingerprinted again as the baseline's
// format made them (state.Formatter), and each must then have the
// baseline's hash, or be missing from both. Anything else is other data.
func MigrateState(ctx context.Context, p *config.Project, meta *snapshot.Meta, out, log io.Writer) (map[string]map[string]string, error) {
	tr, err := newTracker(ctx, p, nil, out, log)
	if err != nil {
		return nil, err
	}
	defer tr.close()
	for _, c := range tr.comps {
		if f, ok := tr.formats[c.Name()]; ok && f != state.FingerprintFormat {
			return nil, envErr("state %s: the record of the pristine copy has the older fingerprint format too; run driftgate seed first", c.Name())
		}
	}
	if err := tr.restore(ctx); err != nil {
		return nil, err
	}
	now, err := tr.fingerprint(ctx, nil)
	if err != nil {
		return nil, err
	}
	then := fromMeta(meta.State)
	var other []string
	for _, c := range tr.comps {
		items := state.Diff(then[c.Name()], now[c.Name()])
		if len(items) == 0 {
			continue
		}
		f, ok := c.(state.Formatter)
		if !ok {
			for _, it := range items {
				other = append(other, c.Name()+"/"+it)
			}
			continue
		}
		old, err := f.FingerprintAs(ctx, max(meta.StateFormat, 1), items)
		if err != nil {
			return nil, envErr("state %s: fingerprint: %v", c.Name(), err)
		}
		for _, it := range state.Diff(then[c.Name()].Subset(items), old) {
			other = append(other, c.Name()+"/"+it)
		}
	}
	for name := range then { // a component no longer configured
		if !slices.ContainsFunc(tr.comps, func(c state.Component) bool { return c.Name() == name }) {
			other = append(other, name+"/*")
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return nil, envErr("the data differs from the data the baseline was captured from, also as its own fingerprint format hashes it (%d items: %v): seed the data the baseline was made from, or make a new baseline",
			len(other), limit(other, 30))
	}
	return toMeta(now), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func limit(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(items[:n:n], fmt.Sprintf("and %d more", len(items)-n))
}

// readRows reads the tracked tables of every component.
func (t *tracker) readRows(ctx context.Context) (map[string]state.Rows, error) {
	if !t.rows {
		return nil, nil
	}
	out := map[string]state.Rows{}
	for _, c := range t.comps {
		r, err := c.Rows(ctx, nil)
		if err != nil {
			return nil, envErr("state %s: rows: %v", c.Name(), err)
		}
		out[c.Name()] = r
	}
	return out, nil
}

func rowChanges(before, after map[string]state.Rows) []snapshot.RowChange {
	var out []snapshot.RowChange
	names := make([]string, 0, len(after))
	for n := range after {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, ch := range state.RowDiff(before[n], after[n]) {
			out = append(out, snapshot.RowChange{Item: n + "/table:" + ch.Table, Key: ch.Key, Change: ch.Change, Columns: ch.Columns})
		}
	}
	return out
}

// SeedOptions configure driftgate seed.
type SeedOptions struct {
	// SkipCommands copies the working state as it is, without the seed:
	// commands: the data was loaded another way (a cached dump in CI).
	SkipCommands bool
	Out, Log     io.Writer
}

// Seed runs the seed commands, which load the data into the working state,
// then makes that state the pristine copy of every database component and
// records its fingerprint.
func Seed(ctx context.Context, p *config.Project, opt SeedOptions) error {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	if len(p.State) == 0 {
		return configErr("no state components are configured")
	}
	for _, cmd := range p.Seed {
		if opt.SkipCommands {
			break
		}
		fmt.Fprintf(opt.Out, "seed: %s\n", cmd[0])
		if err := state.Run(ctx, p.Root, cmd, nil, opt.Log); err != nil {
			return envErr("seed: %v", err)
		}
	}
	comps, servers, err := openServers(ctx, p, nil, opt.Log, true)
	if err != nil {
		return err
	}
	defer func() {
		for _, c := range comps {
			c.Close()
		}
	}()
	for _, c := range comps {
		sc := p.State[c.Name()]
		if sc.Driver == "" {
			continue
		}
		start := time.Now()
		// The component removes the record right before it replaces the
		// pristine copy (openServers): a seed that stops after that leaves a
		// pristine database no restore may trust, and without the record
		// none does; one that fails before keeps the old copy, its record,
		// and the adoption of an earlier release's copy for the next seed.
		path, err := pristinePath(p, c.Name(), servers[c.Name()])
		if err != nil {
			return err
		}
		if err := c.Seed(ctx); err != nil {
			return envErr("seed of %s failed: %v", c.Name(), err)
		}
		f, err := c.Fingerprint(ctx, nil)
		if err != nil {
			return envErr("state %s: fingerprint: %v", c.Name(), err)
		}
		pf := pristineFile{Version: 1, Format: state.FingerprintFormat, Component: c.Name(), Driver: sc.Driver, Server: servers[c.Name()], Working: sc.Working, Pristine: sc.Pristine,
			Created: time.Now().UTC().Format(time.RFC3339), Fingerprint: f}
		if err := snapshot.WriteJSON(path, pf); err != nil {
			return err
		}
		pristine := sc.Pristine
		if sc.Driver == config.SQLite && pristine == "" {
			pristine = sqliteStorePristine(path)
		}
		fmt.Fprintf(opt.Out, "pristine copy of %s: %s (%d items, %s)\n", c.Name(), pristine, len(f), time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// Queries opens the database components of a project for read-only queries
// on their pristine copies; close releases them.
func Queries(ctx context.Context, p *config.Project, log io.Writer) (query func(ctx context.Context, name, sql string) ([]string, error), close func(), err error) {
	comps, err := openComponents(ctx, p, nil, log)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]state.Component{}
	for _, c := range comps {
		byName[c.Name()] = c
	}
	query = func(ctx context.Context, name, sql string) ([]string, error) {
		q, ok := byName[name].(state.Querier)
		if !ok {
			return nil, fmt.Errorf("state %s cannot run queries", name)
		}
		return q.Query(ctx, sql)
	}
	close = func() {
		for _, c := range comps {
			c.Close()
		}
	}
	return query, close, nil
}

// dsnErr is the error of reading the DSN of state name: of the
// configuration when it names the wrong database or a file or variable that
// is missing, except for 1Password, where it is the environment's (op not
// signed in).
func dsnErr(dsn config.Secret, name string, err error) error {
	if dsn.Op != "" && !errors.Is(err, config.ErrInvalid) {
		return envErr("state %s: dsn: %v", name, err)
	}
	return configErr("state %s: dsn: %v", name, err)
}

// serverOf names the server of the database component sc whose DSN is dsn
// (dbdsn.Server). An SQLite database has none: it is named by its file,
// file: and the real path of working, which must exist inside the project.
func serverOf(p *config.Project, sc config.Component, dsn string) (string, error) {
	if sc.Driver == config.SQLite {
		working, err := appconf.LocalPath(p.Root, sc.Working)
		if err != nil {
			return "", fmt.Errorf("working: %w", err)
		}
		return sqliteServerPrefix + working, nil
	}
	return dbdsn.Server(sc.Driver, dsn)
}

// Seeded reports whether every database component has a pristine copy on
// the server its DSN names now.
func Seeded(ctx context.Context, p *config.Project) (bool, error) {
	for name, sc := range p.State {
		if sc.Driver == "" {
			continue
		}
		dsn, err := sc.ReadDSN(ctx, p.Root)
		if err != nil {
			return false, dsnErr(sc.DSN, name, err)
		}
		server, err := serverOf(p, sc, dsn)
		if err != nil {
			return false, configErr("state %s: %v", name, err)
		}
		path, err := pristinePath(p, name, server)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
	}
	return true, nil
}
