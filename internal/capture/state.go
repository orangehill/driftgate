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
	"sync/atomic"
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
	PristineID  string            `json:"pristine_id,omitempty"` // pristineIdentifier; none for other components and in a file of an earlier release
	Fingerprint state.Fingerprint `json:"fingerprint"`
	// RestoreOnly is the component's restore_only at the seed, sorted, and
	// RestoreOnlyFingerprint the fingerprint of those tables in the
	// pristine copy (state.RestoreOnly); a capture with another
	// restore_only asks for a seed.
	RestoreOnly            []string          `json:"restore_only,omitempty"`
	RestoreEmpty           []string          `json:"restore_empty,omitempty"` // as RestoreOnly, for restore_empty (their fingerprint is in RestoreOnlyFingerprint)
	RestoreOnlyFingerprint state.Fingerprint `json:"restore_only_fingerprint,omitempty"`
	// Schema is the pristine copy's schema at the seed (state.Schemer) in
	// SchemaFormat, which spares checkSchema its read when the working
	// schema equals it.
	Schema       state.Fingerprint `json:"schema,omitempty"`
	SchemaFormat int               `json:"schema_format,omitempty"`
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
// (see pristinePath, which also takes over a record of an earlier release),
// and returns its path.
func readPristine(p *config.Project, name, server string) (pristineFile, string, error) {
	var pf pristineFile
	path, err := pristinePath(p, name, server)
	if err != nil {
		return pf, path, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return pf, path, err
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		return pf, path, fmt.Errorf("%s: %w", path, err)
	}
	return pf, path, nil
}

// removeRecords removes the record of a pristine copy (path) and the
// schemaRecord beside it.
func removeRecords(path string) error {
	for _, f := range []string{path, schemaPath(path)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// recorded reports whether DriftGate's record of a pristine copy names the
// component's pristine database: a seed made it, so a seed may replace it
// even without the mark seeds now leave in it (state.Options.AdoptPristine).
// A record of an earlier release is found too: pristinePath takes it over first.
func recorded(p *config.Project, name, server string, sc config.Component) bool {
	pf, _, err := readPristine(p, name, server)
	return err == nil && pf.Driver == sc.Driver && pf.Working == sc.Working && pf.Pristine == sc.Pristine
}

// tracker restores and fingerprints the state components of a project and
// knows which items changed since the last restore.
type tracker struct {
	time     *Time
	comps    []state.Component
	pristine states              // fingerprint of the pristine copy, per database component
	ro       states              // fingerprint of the restore_only tables of the pristine copy, per component that has them
	formats  map[string]int      // the format the pristine fingerprints were recorded in
	dirty    map[string][]string // changed since the last restore; nil: unknown
	rows     bool                // some component tracks rows
	out      io.Writer
	// resends is how many requests may still be sent again after the last
	// restore (resendAfterRestore): the application's sessions with a
	// database it ended (state.SessionEnder), each a pooled connection
	// that fails once; resentAfter says which restore.
	resends     atomic.Int64
	resentAfter string
	// databases names every database component (endedDatabase); endedBy
	// are those whose last restore ended sessions.
	databases map[string]endedDatabase
	endedBy   []endedDatabase
	// The whole-restore check against the record of the pristine copy
	// (checkWhole, compareWhole), part of the restore's own checks.
	copiedWhole map[string]bool // the components the last restore copied whole (not Partial)
	compared    map[string]bool // the components whose whole copy was compared with the record in this run
	schemaCheck
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
		o := state.Options{Name: n, DSN: dsn, Working: sc.Working, Pristine: sc.Pristine, Exclude: sc.Exclude, RestoreOnly: sc.RestoreOnly, RestoreEmpty: sc.RestoreEmpty, RowTracking: sc.RowTracking, Log: log,
			AdoptPristine: recorded(p, n, servers[n], sc)}
		// The record's path names the pristine file of SQLite and is
		// removed by a seed; MySQL and PostgreSQL need it for neither.
		var path string
		if seed || sc.Driver == config.SQLite {
			if path, err = pristinePath(p, n, servers[n]); err != nil {
				closeAll()
				return nil, nil, err
			}
		}
		if sc.Driver == config.SQLite {
			if o.Working, o.Pristine, err = sqliteFiles(p, sc, servers[n], path); err != nil {
				closeAll()
				return nil, nil, configErr("state %s: %v", n, err)
			}
		}
		if seed {
			// A new pristine copy takes the working schema in: an earlier
			// run's schemaRecord no longer applies.
			o.Replacing = func() error { return removeRecords(path) }
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
	t := &tracker{time: &Time{}, comps: comps, pristine: states{}, ro: states{}, formats: map[string]int{}, out: out, databases: map[string]endedDatabase{},
		compared:    map[string]bool{},
		schemaCheck: schemaCheck{prepare: len(p.Prepare) > 0, config: configPath(p), records: map[string]string{}, schemas: states{}}}
	for _, c := range comps {
		sc := p.State[c.Name()]
		if sc.Driver == "" {
			continue
		}
		if len(sc.RowTracking) > 0 {
			t.rows = true
		}
		pf, path, err := readPristine(p, c.Name(), servers[c.Name()])
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
		// The pristine copy keeps the rows of restore_only tables, not of
		// excluded ones, and its record has their fingerprint apart.
		if !slices.Equal(pf.RestoreOnly, sortedCopy(sc.RestoreOnly)) {
			t.close()
			return nil, envErr("state %s: restore_only changed since the pristine copy was made (it was %v); run driftgate seed", c.Name(), pf.RestoreOnly)
		}
		if !slices.Equal(pf.RestoreEmpty, sortedCopy(sc.RestoreEmpty)) {
			t.close()
			return nil, envErr("state %s: restore_empty changed since the pristine copy was made (it was %v); run driftgate seed", c.Name(), pf.RestoreEmpty)
		}
		if len(sc.RestoreOnly)+len(sc.RestoreEmpty) > 0 {
			if _, ok := c.(state.RestoreOnly); !ok {
				t.close()
				return nil, configErr("state %s: restore_only is not supported by driver %s", c.Name(), sc.Driver)
			}
			t.ro[c.Name()] = pf.RestoreOnlyFingerprint
		}
		// The record is of one pristine copy; a seed that keeps its record
		// in another store (another project or machine on the same server,
		// a checkout with its own DRIFTGATE_STORE) may have replaced it.
		if pi, ok := c.(pristineIdentifier); ok && pf.PristineID != "" {
			id, err := pi.PristineID(ctx)
			if err != nil {
				t.close()
				return nil, envErr("state %s: %v", c.Name(), err)
			}
			if id != pf.PristineID {
				t.close()
				return nil, envErr("state %s: the pristine copy is not the one driftgate seed recorded here: another seed replaced it (from another checkout or project on the same databases); run driftgate seed", c.Name())
			}
		}
		t.records[c.Name()] = schemaPath(path)
		if pf.SchemaFormat == state.SchemaFormat {
			t.schemas[c.Name()] = pf.Schema
		}
		t.databases[c.Name()] = endedDatabase{Component: c.Name(), Working: sc.Working, Server: serverHash(servers[c.Name()])}
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
	t.resends.Store(0)
	t.resentAfter = ""
	t.endedBy = nil
	t.copiedWhole = map[string]bool{}
	ended := 0
	first := t.dirty == nil
	if first {
		if err := t.checkSchema(ctx); err != nil {
			return err
		}
	}
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
		// The restore_only tables are no part of the fingerprints that
		// tell what changed: they are read now, every time.
		var ro []string
		if want, ok := t.ro[c.Name()]; ok {
			cur, err := c.(state.RestoreOnly).RestoreOnlyFingerprint(ctx, false)
			if err != nil {
				return envErr("state %s: fingerprint of the restore_only tables: %v", c.Name(), err)
			}
			ro = state.Diff(cur, want)
		}
		if len(items)+len(ro) == 0 {
			continue
		}
		if len(ro) > 0 {
			fmt.Fprintf(t.out, "restoring %s: %d changed %s, %d restore_only %s …\n", c.Name(), len(items), plural(len(items), "item", "items"), len(ro), plural(len(ro), "table", "tables"))
		} else {
			fmt.Fprintf(t.out, "restoring %s: %d changed %s …\n", c.Name(), len(items), plural(len(items), "item", "items"))
		}
		start := time.Now()
		err := c.Restore(ctx, append(append([]string{}, items...), ro...))
		if e, ok := c.(state.SessionEnder); ok && e.EndedSessions() > 0 {
			n := e.EndedSessions()
			ended += n
			t.resends.Add(int64(n))
			t.endedBy = append(t.endedBy, t.databases[c.Name()])
			t.resentAfter = fmt.Sprintf("the restore of %s ended %d database %s of the application", c.Name(), ended, plural(ended, "session", "sessions"))
		}
		if err != nil {
			return envErr("restore of %s failed: %v", c.Name(), err)
		}
		if !c.Partial() {
			t.copiedWhole[c.Name()] = true
		}
		// The restore_only tables are not checked: the application may
		// write them at any time, and they decide no pass.
		var got state.Fingerprint
		if len(items) > 0 {
			if got, err = c.Fingerprint(ctx, items); err != nil {
				return envErr("state %s: fingerprint: %v", c.Name(), err)
			}
		}
		if d := state.Diff(got, pf.Subset(items)); len(d) > 0 {
			if t.formats[c.Name()] != state.FingerprintFormat {
				return envErr("state %s: re-seed needed: the fingerprint format changed (the current format fingerprints these items another way: %v); run driftgate seed, then, if a capture says the baseline's state differs, driftgate verify --baseline <name> --migrate-state", c.Name(), limit(d, 20))
			}
			return restoreLeft(ctx, c, pf, d)
		}
		fmt.Fprintf(t.out, "restored %s in %s\n", c.Name(), time.Since(start).Round(time.Millisecond))
	}
	t.dirty = map[string][]string{}
	return nil
}

// restoreLeft is the stop of a restore of c that left items d different
// from the record of its pristine copy (pf).
func restoreLeft(ctx context.Context, c state.Component, pf state.Fingerprint, d []string) error {
	return envErr("restore of %s left %d items different from the fingerprint driftgate seed recorded of the pristine copy: %v; the working database now holds what the pristine copy has now. When the pristine copy changed since the seed (or the application wrote while it ran) or the application wrote during this restore, run driftgate seed again while the application does not write, or stop the application's background writers during the capture%s", c.Name(), len(d), limit(d, 20), staleHint(ctx, c, pf, d))
}

// checkWhole compares, for each component the last restore copied whole
// and not compared in this run yet, the fingerprint read right after it
// (now) with the record of its pristine copy (compareWhole).
func (t *tracker) checkWhole(ctx context.Context, now states) error {
	for _, c := range t.comps {
		if t.copiedWhole[c.Name()] && !t.compared[c.Name()] {
			if err := t.compareWhole(ctx, c, now[c.Name()]); err != nil {
				return err
			}
		}
	}
	return nil
}

// wholeFingerprint reads in full each component the last restore copied
// whole and not compared in this run yet, for checkWhole, when commands
// that may write (prepare, tokens) run before the run reads its own.
func (t *tracker) wholeFingerprint(ctx context.Context) (states, error) {
	out := states{}
	for _, c := range t.comps {
		if t.copiedWhole[c.Name()] && !t.compared[c.Name()] {
			f, err := c.Fingerprint(ctx, nil)
			if err != nil {
				return nil, envErr("state %s: fingerprint: %v", c.Name(), err)
			}
			out[c.Name()] = f
		}
	}
	return out, nil
}

// compareWhole compares f, the fingerprint of c right after a restore
// copied its whole database, with the record of its pristine copy, once in
// a run: the restore checks only the items it found different, and copies
// every other item too, as the pristine copy has it now, which another
// seed or a hand may have changed since the record. A record of an older
// format is not compared.
func (t *tracker) compareWhole(ctx context.Context, c state.Component, f state.Fingerprint) error {
	t.compared[c.Name()] = true
	if t.formats[c.Name()] != state.FingerprintFormat {
		return nil
	}
	if d := state.Diff(f, t.pristine[c.Name()]); len(d) > 0 {
		return restoreLeft(ctx, c, t.pristine[c.Name()], d)
	}
	return nil
}

// restoreOnlyNow reports whether every item of d (<component>/<item>) is a
// restore_only table now (its pristine copy's record has it): the
// baseline's state holds it from before it was restore_only.
func (t *tracker) restoreOnlyNow(d []string) bool {
	for _, it := range d {
		comp, item, _ := strings.Cut(it, "/")
		if _, ok := t.ro[comp][item]; !ok {
			return false
		}
	}
	return len(d) > 0
}

// staleHint explains a restore that left items different when the
// pristine copy itself no longer matches its record: a pristine copy seeded
// by v1.0.2 or earlier on MySQL 8 names column collations the working
// database left implicit, and its first restore has already rewritten the
// working tables from it. It is read only on that error path.
func staleHint(ctx context.Context, c state.Component, record state.Fingerprint, items []string) string {
	pc, ok := c.(state.PristineFingerprinter)
	if !ok {
		return ""
	}
	p, err := pc.PristineFingerprint(ctx, items)
	if err != nil || len(state.Diff(p, record.Subset(items))) == 0 {
		return ""
	}
	return "; the pristine copy no longer matches its record (seeded by an earlier release on MySQL 8?): re-create the working database (migrations or your dump), then run driftgate seed"
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
	tr.run = runMigrateState
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
	if err := tr.checkWhole(ctx, now); err != nil {
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

// sortedCopy is list sorted, without changing list; nil for none.
func sortedCopy(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := slices.Clone(list)
	sort.Strings(out)
	return out
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

// seededFingerprinter is a component that fingerprints its pristine copy
// (state.MySQL, state.Postgres, state.SQLite, state.SQLServer).
type seededFingerprinter interface {
	SeededFingerprint(ctx context.Context) (state.Fingerprint, error)
}

// pristineIdentifier is a component that names its pristine copy, new with
// every seed (state.SQLServer: the backup in its mark).
type pristineIdentifier interface {
	PristineID(ctx context.Context) (string, error)
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
	if !opt.SkipCommands {
		if err := checkCommands(p, "seed", p.Seed); err != nil {
			return err
		}
	}
	for _, cmd := range p.Seed {
		if opt.SkipCommands {
			break
		}
		fmt.Fprintf(opt.Out, "seed: %s\n", commandLine(cmd))
		if err := runCommand(ctx, p, cmd, nil, opt.Log); err != nil {
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
		// The record is of the pristine copy itself where the component
		// can read it (all database drivers): the working data may
		// have moved on since the copy was made, while the application
		// kept writing.
		var f state.Fingerprint
		pc, seeded := c.(seededFingerprinter)
		if seeded {
			f, err = pc.SeededFingerprint(ctx)
		} else {
			f, err = c.Fingerprint(ctx, nil)
		}
		if err != nil {
			return envErr("state %s: fingerprint: %v", c.Name(), err)
		}
		if seeded {
			// A working database that differs from the copy just made
			// means the application wrote during the seed or right after
			// it. On MySQL/MariaDB a write during the copy may leave a
			// copy that mixes data from before and after it (each table
			// is copied on its own), which no check can tell from a
			// consistent one; a PostgreSQL template copy, an SQLite
			// VACUUM INTO and a SQL Server backup are of one moment.
			// The check only warns: an error here leaves the copy and its
			// record as they are.
			if w, err := c.Fingerprint(ctx, nil); err != nil {
				fmt.Fprintf(opt.Out, "warning: %s: the working database could not be compared with the pristine copy just made: %v\n", c.Name(), err)
			} else if d := state.Diff(w, f); len(d) > 0 {
				advice := "the copy is of one moment and stays the pristine copy"
				if sc.Driver == config.MySQL {
					advice = "a write during the copy can leave a copy that mixes data from before and after it, so run driftgate seed again while the application does not write"
				}
				fmt.Fprintf(opt.Out, "warning: %s: the working database differs from the pristine copy just made in %d %s (%v): the application wrote during the seed or right after it; %s\n", c.Name(), len(d), plural(len(d), "item", "items"), limit(d, 20), advice)
			}
		}
		pf := pristineFile{Version: 1, Format: state.FingerprintFormat, Component: c.Name(), Driver: sc.Driver, Server: servers[c.Name()], Working: sc.Working, Pristine: sc.Pristine,
			Created: time.Now().UTC().Format(time.RFC3339), Fingerprint: f}
		if len(sc.RestoreOnly)+len(sc.RestoreEmpty) > 0 {
			ro, ok := c.(state.RestoreOnly)
			if !ok {
				return configErr("state %s: restore_only is not supported by driver %s", c.Name(), sc.Driver)
			}
			if pf.RestoreOnlyFingerprint, err = ro.RestoreOnlyFingerprint(ctx, true); err != nil {
				return envErr("state %s: fingerprint of the restore_only tables: %v", c.Name(), err)
			}
			pf.RestoreOnly, pf.RestoreEmpty = sortedCopy(sc.RestoreOnly), sortedCopy(sc.RestoreEmpty)
		}
		if s, ok := c.(state.Schemer); ok {
			if pf.Schema, err = s.SchemaFingerprint(ctx, true); err != nil {
				return envErr("state %s: schema of the pristine copy: %v", c.Name(), err)
			}
			pf.SchemaFormat = state.SchemaFormat
		}
		if pi, ok := c.(pristineIdentifier); ok {
			if pf.PristineID, err = pi.PristineID(ctx); err != nil {
				return envErr("state %s: %v", c.Name(), err)
			}
		}
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
// file: and the real path of working (appconf.SQLitePath: the file the
// system opens), which must exist inside the project.
func serverOf(p *config.Project, sc config.Component, dsn string) (string, error) {
	if sc.Driver == config.SQLite {
		working, err := appconf.SQLitePath(p.Root, sc.Working)
		if err != nil {
			return "", fmt.Errorf("working: %w", err)
		}
		// Stat only: closing a handle of a file SQLite has open in this
		// process would drop its locks.
		if _, err := os.Stat(working); errors.Is(err, os.ErrNotExist) {
			if config.MainCheckout(p.Root) != "" {
				return "", fmt.Errorf("working: %s: not found in this worktree (git does not carry it): driftgate setup makes it a copy of the main worktree's pristine copy, or the application's migrations make it", sc.Working)
			}
			return "", fmt.Errorf("working: %s: not found; the application's migrations make it", sc.Working)
		} else if err != nil {
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

// commandLine is a command of driftgate.yaml as one line, with its stdin
// file and dir, its secrets shown as *** (config.Command.String).
func commandLine(cmd config.Command) string { return cmd.String() }
