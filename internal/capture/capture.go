// Package capture sends the whole catalog and stores the responses as a
// snapshot set: restore of the state and a check of its
// fingerprint, prepare, read-only requests (in parallel, within rate limits)
// and a search for any of them that changed data, guards, another restore,
// and the sequences and requests that change data, one at a time, each with
// the changes it made to the state.
package capture

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/procx"
	"github.com/orangehill/driftgate/internal/scan"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// Error kinds, mapped to exit codes by the command line.
var (
	ErrConfig = errors.New("configuration")
	ErrEnv    = errors.New("environment")
)

func configErr(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrConfig}, a...)...)
}

// configPath names the project's configuration in a message: relative to
// the project folder (.driftgate/driftgate.yaml or driftgate.yaml).
func configPath(p *config.Project) string {
	if r, err := filepath.Rel(p.Root, p.Layout.Config); err == nil {
		return filepath.ToSlash(r)
	}
	return p.Layout.Config
}

func envErr(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrEnv}, a...)...)
}

// Options of one capture.
type Options struct {
	Tag  string
	Only string // glob over request IDs
	// ReadOnly sends only the requests that do not change data
	// (catalog.ReadOnlySkips leaves the others out).
	ReadOnly                    bool
	Force                       bool // replace an existing capture with this tag
	AllowRemote                 bool // allow a target outside allow_hosts
	AllowRemoteMutations        bool // and send requests that change data to it
	AllowMutatingWithoutRestore bool
	Restore                     string        // once (default): before the run and before the requests that change data; each: also before every sequence
	Baseline                    *snapshot.Set // the state must match the one this baseline was captured from
	SkipFingerprintCheck        bool
	Out                         io.Writer // progress
	Log                         io.Writer // output of hook commands
	Env                         []string  // added to the environment of the project's commands (bisect: the commit)
	// Concurrency replaces the configured concurrency of the services it
	// names (the decision of concurrency: auto); it applies to the
	// read-only requests only, as every concurrency does.
	Concurrency map[string]int
	// NotBefore: the prepare commands and the requests start at this
	// moment, not before (a capture repeated for the same baseline, whose
	// timestamps must fall in other seconds than the last run's).
	NotBefore time.Time
}

// Summary of a finished capture.
type Summary struct {
	Requests     int
	Failed       []string                 // requests whose transport failed
	Personal     map[string][]pii.Finding // public requests whose body held personal data or credentials (stored as private)
	Unscanned    map[string]string        // public requests whose body could not be scanned, and why (stored as private)
	Tracked      bool                     // side effects were recorded
	Writers      map[string][]string      // read-only requests that changed the state → items
	Unattributed []string                 // items read-only requests changed that no single request explains
	Time         Time                     // where the time went

	// Retried are the read-only requests answered only after a retry (one
	// that recovered), with the outcome of their first send.
	Retried map[string]*snapshot.Retry
	// Prepared are the items prepare changed after a restore: from the
	// fingerprints taken anyway (with a tokens command, one more after
	// the first restore, and the later restores are not checked).
	Prepared []string
}

// Time is where a capture spent its time.
type Time struct {
	Total, Fingerprint, Restore time.Duration
	Fingerprints, Reused        int // fingerprints taken, and those a check of the write counters made unnecessary
}

// writerSearch is the search for read-only requests that changed the state;
// a test swaps in the previous search to compare the two.
var writerSearch = (*runner).findWriters

// unit is a sequence, or a single request that changes data; units run one
// after another, after the read-only requests.
type unit struct {
	seq  *catalog.Sequence
	reqs []catalog.Request
}

// plan splits the selected requests into read-only requests and units;
// skip are requests left out (a read-only capture's).
func plan(cat *catalog.Catalog, only string, skip map[string]bool) (readonly []catalog.Request, units []unit) {
	selected := map[string]catalog.Request{}
	for _, r := range cat.Requests {
		if (only == "" || glob.Match(only, r.ID)) && !skip[r.ID] {
			selected[r.ID] = r
		}
	}
	inSeq := map[string]*catalog.Sequence{}
	for i := range cat.Sequences {
		for _, id := range cat.Sequences[i].Requests {
			inSeq[id] = &cat.Sequences[i]
		}
	}
	started := map[string]bool{}
	for _, r := range cat.Requests {
		if _, ok := selected[r.ID]; !ok {
			continue
		}
		if s := inSeq[r.ID]; s != nil {
			if !started[s.ID] {
				started[s.ID] = true
				u := unit{seq: s}
				for _, id := range s.Requests {
					if req, ok := selected[id]; ok {
						u.reqs = append(u.reqs, req)
					}
				}
				units = append(units, u)
			}
			continue
		}
		if r.Sequential() {
			units = append(units, unit{reqs: []catalog.Request{r}})
		} else {
			readonly = append(readonly, r)
		}
	}
	return readonly, units
}

// Run captures the catalog into the project's store under opt.Tag.
func Run(ctx context.Context, p *config.Project, cat *catalog.Catalog, opt Options) (_ *Summary, err error) {
	defer httpx.FreePool() // the memory of large bodies, kept for the next one
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	switch opt.Restore {
	case "":
		opt.Restore = "once"
	case "once", "each":
	default:
		return nil, configErr("--restore must be once or each")
	}
	readonly, units, skip, err := selectRequests(p, cat, opt)
	if err != nil {
		return nil, err
	}
	all := append(append([]catalog.Request{}, readonly...), unitRequests(units)...)
	if err := preflight(p, opt, units, all); err != nil {
		return nil, err
	}
	set, err := p.Capture(opt.Tag)
	if err != nil {
		return nil, err // config.ErrInvalid for a tag, else the store's
	}
	if err := recoverAside(set, opt.Out); err != nil {
		return nil, err
	}
	if set.Exists() && !opt.Force {
		return nil, configErr("capture %q already exists; pass --force to replace it", opt.Tag)
	}
	// The previous capture is moved aside at once: removed in the
	// background once this one is complete, put back when it fails, so a
	// capture that cannot run (a stack that is down) loses nothing.
	prev, err := moveAside(set)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			prev.restore()
		} else {
			prev.discard()
		}
	}()
	created := time.Now().UTC().Format(time.RFC3339)
	if err := set.WriteMeta(snapshot.Meta{Tag: opt.Tag, Source: "capture", Created: created, Incomplete: true}); err != nil {
		return nil, err
	}
	chaos.Point("capture.started")
	services, err := newServices(ctx, p, all, opt.AllowRemote, opt.Concurrency)
	if err != nil {
		return nil, err
	}
	detector, err := pii.New(p.PII.Allow)
	if err != nil {
		return nil, configErr("%v", err)
	}
	tr, err := newTracker(ctx, p, opt.Env, opt.Out, opt.Log)
	if err != nil {
		return nil, err
	}
	defer tr.close()
	began := time.Now()
	r := &runner{p: p, set: set, services: services, detector: detector, out: opt.Out, log: opt.Log, env: opt.Env, tr: tr,
		timings: map[string]int64{},
		sum: &Summary{Personal: map[string][]pii.Finding{}, Unscanned: map[string]string{}, Writers: map[string][]string{},
			Retried: map[string]*snapshot.Retry{}}}
	if r.uploads, err = prepareUploads(p, all); err != nil {
		return nil, err
	}

	// A capture stopped before it began (an interrupt, a stack that is
	// down) fails here: the send loop below takes a cancelled context or a
	// free worker, whichever select picks.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Every capture starts from the pristine state, so two captures of the
	// same code see the same data.
	if err := tr.restore(ctx); err != nil {
		return nil, err
	}
	// Sessions the last restore of the previous capture ended fail now.
	takeEnded(p, tr)
	start, err := tr.fingerprint(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := tr.checkWhole(ctx, start); err != nil {
		return nil, err
	}
	r.sum.Tracked = len(start) > 0
	r.start = start
	if err := checkBaseline(p, tr, opt, start); err != nil {
		return nil, err
	}
	if wait := time.Until(opt.NotBefore); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := r.runPrepare(ctx); err != nil {
		return nil, err
	}
	// What prepare changed: the fingerprint after the tokens command is
	// taken anyway; one between the two only when both are configured, so
	// a tokens command that writes (a login) is not taken for prepare.
	var mid states
	if r.sum.Tracked && len(p.Prepare) > 0 && len(p.Tokens.Command) > 0 {
		if mid, err = tr.fingerprint(ctx, nil); err != nil {
			return nil, err
		}
	}
	if err := r.reloadTokens(ctx); err != nil {
		return nil, err
	}
	// The guards are checked once restore and prepare have set up the
	// environment they check, before the first request, so one that fails
	// stops the capture in seconds, not after the read-only requests; and
	// before the fingerprint the read-only phase starts from, so a write
	// of one (guards only check, but a command may log itself) is part of
	// that state and never taken for a request's.
	if len(units) > 0 {
		if err := checkGuards(ctx, p, opt, true); err != nil {
			return nil, err
		}
	}
	before, err := tr.fingerprint(ctx, nil)
	if err != nil {
		return nil, err
	}
	tr.markDirty(diffStates(start, before))
	switch {
	case mid != nil:
		r.prepared(diffStates(start, mid))
	case len(p.Tokens.Command) == 0:
		r.prepared(diffStates(start, before))
	}
	if err := checkIdentities(all, r.tokens); err != nil {
		return nil, err
	}
	r.base = opt.Baseline
	r.retry = newRetrying(readonly)
	if err := r.byService(ctx, readonly, true); err != nil {
		r.retry.release()
		return nil, err
	}
	if err := r.retryDeferred(ctx); err != nil {
		return nil, err
	}
	if r.sum.Tracked && len(readonly) > 0 {
		after, err := tr.fingerprint(ctx, nil)
		if err != nil {
			return nil, err
		}
		if changed := diffStates(before, after); len(changed) > 0 {
			tr.markDirty(changed)
			if err := writerSearch(r, ctx, readonly, before, changed); err != nil {
				return nil, err
			}
		}
	}

	if len(units) > 0 {
		// Again right before the requests that change data, which they
		// guard, when read-only requests ran since the first check: that
		// phase may take long, and the environment may change during it.
		if len(readonly) > 0 {
			if err := checkGuards(ctx, p, opt, false); err != nil {
				return nil, err
			}
		}
		prev, err := r.reset(ctx)
		if err != nil {
			return nil, err
		}
		for i, u := range units {
			if i > 0 && (opt.Restore == "each" || (u.seq != nil && u.seq.Restore == "before") || ownState(units, i)) {
				if prev, err = r.reset(ctx); err != nil {
					return nil, err
				}
			}
			for _, req := range u.reqs {
				if prev, err = r.step(ctx, req, prev); err != nil {
					return nil, err
				}
			}
		}
	}

	// Leave the data as the capture found it after its first restore, so the
	// working copy is ready for the application and the next capture.
	// Every request ran on data checked before it; a pristine copy changed
	// since is found by the next run's first restore, so this restore
	// reads nothing more (no whole-copy read).
	if (len(units) > 0 || len(r.sum.Writers) > 0) && len(p.State) > 0 {
		if err := tr.restore(ctx); err != nil {
			return nil, err
		}
		saveEnded(p, tr)
	}

	if err := r.markComplete(opt, created, skip); err != nil {
		return nil, err
	}
	chaos.Point("capture.complete")
	sort.Strings(r.sum.Failed)
	tr.time.Total = time.Since(began)
	r.sum.Time = *tr.time
	return r.sum, nil
}

// selectRequests selects the requests of a capture: those of the catalog
// that --only and --read-only leave, split into read-only requests and
// units (plan), each with its target named as the configuration names it
// (the empty name means the only service). skip are the requests a
// read-only capture leaves out. A selection that is empty, or a
// configuration a read-only capture cannot keep from writing, is refused.
func selectRequests(p *config.Project, cat *catalog.Catalog, opt Options) (readonly []catalog.Request, units []unit, skip map[string]bool, err error) {
	if opt.ReadOnly {
		// --read-only selects requests; the project's own commands run as
		// configured, and connect_to does not apply to them.
		if err := CheckReadOnly(p); err != nil {
			return nil, nil, nil, err
		}
		skip = cat.ReadOnlySkips()
	}
	readonly, units = plan(cat, opt.Only, skip)
	if len(readonly)+len(units) == 0 {
		if opt.ReadOnly {
			return nil, nil, nil, configErr("no catalog request that leaves data unchanged is selected (--read-only)")
		}
		if opt.Only != "" {
			return nil, nil, nil, configErr("no catalog request matches --only %s", opt.Only)
		}
		return nil, nil, nil, configErr("the catalog has no requests")
	}
	for i := range readonly {
		readonly[i].Target = p.Canonical(readonly[i].Target)
	}
	for _, u := range units {
		for i := range u.reqs {
			u.reqs[i].Target = p.Canonical(u.reqs[i].Target)
		}
	}
	return readonly, units, skip, nil
}

// preflight refuses a capture whose requests may not run, before anything
// is changed: a real file uploaded without a restore to take it out,
// requests that change data without a state to restore, requests that
// change data or are repeated sent outside allow_hosts, and a command
// that would stop halfway (checkAllCommands). all are the read-only
// requests and then the requests of units.
func preflight(p *config.Project, opt Options, units []unit, all []catalog.Request) error {
	var mutating, repeats []catalog.Request
	for _, u := range units {
		for _, r := range u.reqs {
			// A command step changes data unless it says mutates: false,
			// which only a read-only capture takes at its word (it sends
			// no other step).
			if r.Mutating() || r.IsCommand() && !opt.ReadOnly {
				mutating = append(mutating, r)
			} else if r.Repeat > 1 {
				repeats = append(repeats, r)
			}
		}
	}
	// An uploaded real file stays in the application's data until a restore
	// takes it out; without one it would stay there after the capture.
	for _, r := range all {
		if r.UsesFiles() && (len(p.State) == 0 || opt.AllowMutatingWithoutRestore) {
			return configErr("%s uploads a file from the uploads folder, which needs a state with a restore command to take it out of the application again (also without --allow-mutating-without-restore)", r.ID)
		}
	}
	if len(mutating) > 0 && len(p.State) == 0 && !opt.AllowMutatingWithoutRestore {
		return configErr("the catalog has requests that change data (%s), but no state with a restore command is configured: configure state: in %s (see driftgate init)%s",
			listRequests(mutating), configPath(p), byMethodHint(mutating))
	}
	// A request sent many times in a row (repeat:) is a burst, like a
	// change of data, to a server outside allow_hosts.
	if len(mutating)+len(repeats) > 0 && opt.AllowRemote && !opt.AllowRemoteMutations {
		return configErr("refusing to send requests that change data or are repeated (%s) to a service outside allow_hosts without --allow-remote-mutations%s",
			listRequests(append(mutating, repeats...)), byMethodHint(mutating))
	}
	return checkAllCommands(p, all, units)
}

// newServices makes the service of every target reqs are sent to (a
// command step is sent to none): a client, rate limits, allowed hosts and
// slots; concurrency sets the slots of the targets it names.
func newServices(ctx context.Context, p *config.Project, reqs []catalog.Request, allowRemote bool, concurrency map[string]int) (map[string]*service, error) {
	services := map[string]*service{}
	for _, req := range reqs {
		if _, ok := services[req.Target]; ok || req.IsCommand() {
			continue
		}
		s, err := newService(ctx, p, req, allowRemote)
		if err != nil {
			return nil, err
		}
		if n, ok := concurrency[req.Target]; ok && n > 0 {
			s.slots = make(chan struct{}, n)
		}
		services[req.Target] = s
	}
	return services, nil
}

// checkBaseline refuses a capture whose start state (the fingerprint after
// the first restore) differs from the one opt.Baseline was captured from,
// unless --skip-fingerprint-check: a baseline fingerprinted in an older
// format, items that are restore_only now (the configuration as tr holds
// it now), or other data; each message says how to go on.
func checkBaseline(p *config.Project, tr *tracker, opt Options, start states) error {
	if opt.Baseline == nil || opt.SkipFingerprintCheck {
		return nil
	}
	meta, err := opt.Baseline.Meta()
	if err != nil {
		return err
	}
	if meta == nil || meta.State == nil {
		return nil
	}
	d := flatten(diffStates(fromMeta(meta.State), start))
	if len(d) == 0 {
		return nil
	}
	name := filepath.Base(opt.Baseline.Dir)
	if meta.StateFormat != state.FingerprintFormat {
		return envErr("the state differs from the one the baseline was captured from (%d items: %v), and the baseline's fingerprint has an older format (the current format fingerprints these items another way): driftgate verify --baseline %s --migrate-state checks that the data is the baseline's and records the fingerprint in the new format", len(d), limit(d, 30), name)
	}
	run := "driftgate run"
	if name != "local" && filepath.Dir(opt.Baseline.Dir) == filepath.Clean(p.Layout.Baselines) {
		run += " --baseline " + name
	}
	if tr.restoreOnlyNow(d) {
		return envErr("the state differs from the one the baseline was captured from (%d items): %v; they are restore_only now, which the baseline's state still holds: the configuration changed, so make the baseline again (move %s away; the next %s makes it)", len(d), limit(d, 30), opt.Baseline.Dir, run)
	}
	return envErr("the state differs from the one the baseline was captured from (%d items): %v; the pristine copy holds other data than the baseline's (a seed since, other seed data or migrations): seed the data the baseline was captured from, or make a new baseline of this data (move %s away; the next %s makes it); when the data changed on purpose, you can also capture twice with --skip-fingerprint-check and make that capture the baseline's content: driftgate capture <tag> --repeat 2 --skip-fingerprint-check, then driftgate baselines rebase %s --from <tag> --reason \"…\"", len(d), limit(d, 30), opt.Baseline.Dir, run, name)
}

// markComplete writes the capture's timings, then its meta without
// Incomplete, which marks the capture whole: it is written last. created
// is when the capture began, and skip the requests a read-only capture
// left out (selectRequests).
func (r *runner) markComplete(opt Options, created string, skip map[string]bool) error {
	p := r.p
	meta := snapshot.Meta{Tag: opt.Tag, Source: "capture", Created: created, Unreliable: opt.SkipFingerprintCheck, Roots: projectRoots(p.Root),
		RowTracking: rowTrackingItems(p), ReadOnly: opt.ReadOnly}
	if opt.ReadOnly {
		// What it left out, as the catalog was: a comparison reads the
		// list, not the catalog of its own time.
		meta.ReadOnlySkipped = []string{}
		for id := range skip {
			if opt.Only == "" || glob.Match(opt.Only, id) {
				meta.ReadOnlySkipped = append(meta.ReadOnlySkipped, id)
			}
		}
		sort.Strings(meta.ReadOnlySkipped)
	}
	if r.sum.Tracked {
		meta.State, meta.StateFormat = toMeta(r.start), state.FingerprintFormat
	}
	for name := range r.services {
		t, _ := p.TargetFor(name)
		if o, err := compare.Origin(t.BaseURL); err == nil {
			if meta.Origins == nil {
				meta.Origins = map[string]string{}
			}
			meta.Origins[name] = o
		}
		if t.ConnectTo != "" {
			if meta.ConnectTo == nil {
				meta.ConnectTo = map[string]string{}
			}
			meta.ConnectTo[name] = t.ConnectTo
		}
	}
	if err := snapshot.WriteJSON(filepath.Join(r.set.Dir, "timings.json"), r.timings); err != nil {
		return err
	}
	return r.set.WriteMeta(meta)
}

// projectRoots are the folder of the project and, when it differs, its
// real path (an application reports the folder it runs from resolved:
// /private/tmp for /tmp on macOS).
//
// A folder in a home folder of a form the comparison knows
// (compare.HomeRelative: /Users/<name>, /home/<name>, WSL's
// /mnt/<drive>/Users/<name> …), with two names or more in it, is written
// ~ and the rest: meta.json goes into the repository with baselines: git,
// and names no user that way; the comparison takes it for that path in
// any home folder of those forms. The home folder itself, and one name in
// it, are not written (their path names the user), nor a folder whose
// absolute path would name the user (namesUser); any other folder is
// written as it is.
func projectRoots(root string) []string {
	out := []string{root}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		out = append(out, real)
	}
	var kept []string
	for _, r := range out {
		switch rest, home := compare.HomeRelative(r); {
		case rest != "":
			kept = append(kept, "~"+rest)
		case !home && !namesUser(r):
			kept = append(kept, r)
		}
	}
	return slices.Compact(kept)
}

// homeSpaced is a home folder of a known form whose name has a space
// (/Users/Jane Doe, C:\Users\Jane Doe): ~/… cannot stand for it, as the
// comparison reads a home folder's name up to white space.
var homeSpaced = regexp.MustCompile(`(?i)^(?:/(?:Users|home)/|/mnt/[a-z]/Users/|[a-z]:\\Users\\)[^/\\]*\s`)

// namesUser reports a folder not recorded as ~/… whose absolute path
// would name the user: in a home folder of a known form whose name has a
// space, or in this machine's home when its path has white space (the
// comparison reads a home folder's name up to white space). It is not
// recorded: meta.json goes into the repository with baselines: git. Its
// traces are then compared as they are, which can only make a difference,
// never hide one.
func namesUser(folder string) bool {
	return homeSpaced.MatchString(folder) || compare.InLocalHome(folder)
}

// asidePrefix starts the name of a previous capture of tag moved aside; it
// starts with a dot, which no tag does, so no capture is taken for one.
func asidePrefix(tag string) string { return "." + tag + ".old-" }

// cleanup tracks folders being removed in the background; Wait waits for
// them, so a process ends only once they are gone.
var cleanup sync.WaitGroup

// Wait waits until every previous capture moved aside is removed.
func Wait() { cleanup.Wait() }

// removeLater removes folders in the background.
func removeLater(dirs ...string) {
	if len(dirs) == 0 {
		return
	}
	cleanup.Add(1)
	go func() {
		defer cleanup.Done()
		for _, dir := range dirs {
			os.RemoveAll(dir)
		}
	}()
}

// previous is a capture moved aside while a new one of its tag is taken.
type previous struct {
	set   *snapshot.Set
	aside string // "" when there was none
}

// asides lists the previous captures of set's tag moved aside, oldest
// first (their names end in the time they were moved).
func asides(set *snapshot.Set) []string {
	parent, tag := filepath.Split(set.Dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutPrefix(e.Name(), asidePrefix(tag)); ok && n != "" && strings.Trim(n, "0123456789") == "" {
			out = append(out, filepath.Join(parent, e.Name()))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// recoverAside puts back the previous capture of a capture that was killed
// (SIGKILL, out of memory, a CI timeout) before it could do so itself: the
// capture of the tag is incomplete or missing and an earlier one is aside.
// The newest one aside is the last complete capture; any older one is
// removed. It runs under the store's lock, so no other capture is taking
// this tag.
func recoverAside(set *snapshot.Set, out io.Writer) error {
	old := asides(set)
	if len(old) == 0 {
		return nil
	}
	if !complete(set) {
		newest := old[len(old)-1]
		old = old[:len(old)-1]
		if err := os.RemoveAll(set.Dir); err != nil {
			return err
		}
		if err := os.Rename(newest, set.Dir); err != nil {
			return err
		}
		fmt.Fprintf(out, "put back the previous capture %s: the capture that replaced it did not finish\n", filepath.Base(set.Dir))
	}
	removeLater(old...)
	return nil
}

// complete reports a capture that finished: its meta.json is not marked
// incomplete.
func complete(set *snapshot.Set) bool {
	m, err := set.Meta()
	return err == nil && m != nil && !m.Incomplete
}

// moveAside renames the existing capture of set (if any) out of the way.
// It stays there until the new capture is complete (discard): a capture
// that is killed leaves it for the next one to put back (recoverAside).
func moveAside(set *snapshot.Set) (*previous, error) {
	parent, tag := filepath.Split(set.Dir)
	prev := &previous{set: set}
	if set.Exists() {
		prev.aside = filepath.Join(parent, fmt.Sprintf("%s%d", asidePrefix(tag), time.Now().UnixNano()))
		if err := os.Rename(set.Dir, prev.aside); err != nil {
			return nil, err
		}
	}
	return prev, nil
}

// discard removes the previous capture in the background: the new one is
// complete.
func (p *previous) discard() {
	if p.aside != "" {
		removeLater(p.aside)
	}
}

// restore puts the previous capture back in place of a new one that failed;
// without a previous capture the failed one stays, marked incomplete.
func (p *previous) restore() {
	if p.aside == "" {
		return
	}
	if err := os.RemoveAll(p.set.Dir); err != nil {
		return
	}
	os.Rename(p.aside, p.set.Dir)
}

// listRequests names requests by ID, at most ten of them.
func listRequests(reqs []catalog.Request) string {
	ids := make([]string, 0, len(reqs))
	for _, r := range reqs {
		ids = append(ids, r.ID)
	}
	return strings.Join(limit(ids, 10), ", ")
}

// byMethodHint names the requests that change data only by their method
// (catalog.Request.MutatingByMethod), for a message that refuses them: a
// search sent as POST only reads, and says so with "mutates": false.
func byMethodHint(reqs []catalog.Request) string {
	var by []catalog.Request
	for _, r := range reqs {
		if r.MutatingByMethod() {
			by = append(by, r)
		}
	}
	if len(by) == 0 {
		return ""
	}
	return fmt.Sprintf(`; %s change data only by their method (the catalog does not say mutates): say mutates: false for those that only read (a search sent as POST), under requests: in driftgate.yaml, then driftgate generate --write (an entry written by hand: "mutates": false in the catalog)`, listRequests(by))
}

// ByMethod lists the selected requests (only: a glob over IDs, or empty)
// that change data only by their method (catalog.Request.MutatingByMethod).
func ByMethod(cat *catalog.Catalog, only string) []string {
	var ids []string
	for _, r := range cat.Requests {
		if r.MutatingByMethod() && (only == "" || glob.Match(only, r.ID)) {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// CheckReadOnly refuses a configuration a read-only capture cannot keep
// from writing: --read-only selects requests, and prepare: runs as
// configured. With state: it should write only to data the restore brings back.
func CheckReadOnly(p *config.Project) error {
	if len(p.Prepare) > 0 && len(p.State) == 0 {
		return configErr("--read-only covers the requests only, not the project's commands: prepare: may write, and without state: nothing restores what it writes; leave it out of the configuration for a read-only check")
	}
	return nil
}

// checkGuards runs the guards; say prints each that succeeded.
func checkGuards(ctx context.Context, p *config.Project, opt Options, say bool) error {
	for _, g := range p.Guards {
		if err := runCommand(ctx, p, g.Command, opt.Env, opt.Log); err != nil {
			return envErr("guard %q failed: %v", g.Name, err)
		}
		if say {
			fmt.Fprintf(opt.Out, "guard ok: %s\n", g.Name)
		}
	}
	return nil
}

func unitRequests(units []unit) []catalog.Request {
	var out []catalog.Request
	for _, u := range units {
		out = append(out, u.reqs...)
	}
	return out
}

// service is where the requests of one target go.
type service struct {
	client *httpx.Client
	limits *limiter
	slots  chan struct{}
	// remote is a target outside its allow_hosts (--allow-remote): every
	// send keeps the rate limits, the sends of a repeated request too.
	remote bool
	// retries is how many times a read-only request that failed under
	// load is sent again (target.retry); timeout is the time limit of a
	// request.
	retries int
	timeout time.Duration
}

// newService makes the client, rate limits and slots of the service of req.
func newService(ctx context.Context, p *config.Project, req catalog.Request, allowRemote bool) (*service, error) {
	t, err := p.TargetFor(req.Target)
	if err != nil {
		return nil, configErr("%s: %v", req.ID, err)
	}
	if t.BaseURL == "" {
		return nil, configErr("%s: the service has no base_url", req.ID)
	}
	allow, err := t.Allow(allowRemote)
	if err != nil {
		return nil, configErr("%v", err)
	}
	// Every result is released once its body is stored or dropped.
	client, err := httpx.New(httpx.Options{BaseURL: t.BaseURL, UserAgent: t.UserAgent, Timeout: t.Timeout.Duration,
		FollowRedirects: t.FollowRedirects, Allow: allow, OffHeap: true})
	if err != nil {
		return nil, configErr("%v", err)
	}
	remote := false
	if allowRemote {
		strict, err := t.Allow(false)
		if err != nil {
			return nil, configErr("%v", err)
		}
		u, err := url.Parse(t.BaseURL)
		remote = err != nil || strict.Check(ctx, u.Hostname()) != nil
	}
	return &service{client: client, limits: newLimiter(t.RateLimits), slots: make(chan struct{}, t.Concurrency.Fixed()), remote: remote,
		retries: t.Retries(), timeout: cmp.Or(t.Timeout.Duration, httpx.DefaultTimeout)}, nil
}

type runner struct {
	p        *config.Project
	set      *snapshot.Set
	services map[string]*service // by target name
	detector *pii.Detector
	tr       *tracker
	start    states        // the restored state, before prepare
	base     *snapshot.Set // the baseline, if any: its records name the read-only requests that wrote
	tokens   map[string]string
	uploads  map[string]multipartBody // requests with files, by ID
	tainted  bool                     // a real file was uploaded since the last restore
	retry    *retrying                // retries of the read-only phase; nil: none
	out, log io.Writer
	env      []string // added to the environment of the project's commands

	mu      sync.Mutex
	timings map[string]int64
	sum     *Summary
}

// prepare runs the prepare commands and reloads the tokens, which a restore
// or a prepare may have changed.
func (r *runner) prepare(ctx context.Context) error {
	if err := r.runPrepare(ctx); err != nil {
		return err
	}
	return r.reloadTokens(ctx)
}

// prepared adds what prepare changed after a restore to the summary.
func (r *runner) prepared(changed map[string][]string) {
	if len(r.p.Prepare) == 0 || len(changed) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, it := range r.sum.Prepared {
		seen[it] = true
	}
	for _, it := range flatten(changed) {
		if !seen[it] {
			r.sum.Prepared = append(r.sum.Prepared, it)
		}
	}
	sort.Strings(r.sum.Prepared)
}

// runPrepare runs the prepare commands.
func (r *runner) runPrepare(ctx context.Context) error {
	for _, cmd := range r.p.Prepare {
		fmt.Fprintf(r.out, "prepare: %s\n", commandLine(cmd))
		if err := runCommand(ctx, r.p, cmd, r.env, r.log); err != nil {
			return envErr("prepare: %v", err)
		}
	}
	return r.tr.checkPrepared(ctx)
}

// reloadTokens reads the tokens again, which a restore or a prepare may
// have changed.
func (r *runner) reloadTokens(ctx context.Context) error {
	tokens, err := loadTokens(ctx, r.p, r.env)
	if err != nil {
		return err
	}
	r.tokens = tokens
	return nil
}

// reset restores the state, prepares it again and returns its fingerprint.
// Everything that differs from the restored state, prepare's changes
// included, is what the next restore brings back.
func (r *runner) reset(ctx context.Context) (states, error) {
	if err := r.tr.restore(ctx); err != nil {
		return nil, err
	}
	// A whole copy is compared with its record before prepare and the
	// tokens command, which may write; without them, on the fingerprint
	// read below.
	if len(r.p.Prepare)+len(r.p.Tokens.Command) > 0 {
		whole, err := r.tr.wholeFingerprint(ctx)
		if err != nil {
			return nil, err
		}
		if err := r.tr.checkWhole(ctx, whole); err != nil {
			return nil, err
		}
	}
	r.tainted = false
	if err := r.prepare(ctx); err != nil {
		return nil, err
	}
	if !r.sum.Tracked {
		return states{}, nil
	}
	now, err := r.tr.fingerprint(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := r.tr.checkWhole(ctx, now); err != nil {
		return nil, err
	}
	changed := diffStates(r.start, now)
	r.tr.markDirty(changed)
	if len(r.p.Tokens.Command) == 0 {
		r.prepared(changed) // the restore brought back r.start: prepare made the rest
	}
	return now, nil
}

// byService sends the read-only requests of every service at the same time,
// each service with its own concurrency; with concurrency 1 a service gets
// its requests strictly in catalog order, whatever the other services do.
// Without record the responses are dropped (a run of the writer search).
func (r *runner) byService(ctx context.Context, reqs []catalog.Request, record bool) error {
	groups := map[string][]catalog.Request{}
	var names []string
	for _, req := range reqs {
		if _, ok := groups[req.Target]; !ok {
			names = append(names, req.Target)
		}
		groups[req.Target] = append(groups[req.Target], req)
	}
	var mem *budget
	if record {
		mem = newBudget(queueBudget)
	}
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var q *writeQueue
			if record {
				q = newWriteQueue(r, cap(r.services[name].slots), mem)
			}
			errs[i] = r.parallel(ctx, groups[name], cap(r.services[name].slots), q)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// parallel sends read-only requests with the given number of workers; with
// one worker they go strictly in order. The responses go to the write queue
// q, which parallel closes; without one they are dropped.
func (r *runner) parallel(ctx context.Context, reqs []catalog.Request, workers int, q *writeQueue) error {
	jobs := make(chan catalog.Request)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := range jobs {
				if q != nil && len(req.ParamsFrom) > 0 {
					if err := q.drain(); err != nil {
						errs <- err
						return
					}
				}
				if q == nil {
					r.send(ctx, req).Release()
					continue
				}
				res, ok := r.sendRead(ctx, req)
				if !ok {
					continue // sent again after the read-only phase
				}
				if r.sum.Tracked {
					res.Record.SideEffects = &snapshot.SideEffects{Items: []string{}}
				}
				if err := q.add(req, res); err != nil {
					res.Release()
					errs <- err
					return
				}
			}
		}()
	}
	var firstErr error
send:
	for _, req := range reqs {
		// Cancelled, no request more is handed out: select would pick a
		// ready worker as often as the cancellation.
		if firstErr = ctx.Err(); firstErr != nil {
			break
		}
		select {
		case jobs <- req:
		case firstErr = <-errs:
			break send
		case <-ctx.Done():
			firstErr = ctx.Err()
			break send
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)
	if firstErr == nil {
		firstErr = <-errs
	}
	if q != nil {
		if err := q.close(); firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// step sends one request of a unit and records what it changed in the state.
func (r *runner) step(ctx context.Context, req catalog.Request, prev states) (states, error) {
	var rowsBefore map[string]state.Rows
	var err error
	if r.sum.Tracked {
		if rowsBefore, err = r.tr.readRows(ctx); err != nil {
			return nil, err
		}
	}
	res := r.do(ctx, req)
	defer res.Release()
	cur := prev
	if r.sum.Tracked {
		if cur, err = r.tr.fingerprint(ctx, nil); err != nil {
			return nil, err
		}
		changed := diffStates(prev, cur)
		r.tr.markDirty(changed)
		se := &snapshot.SideEffects{Items: flatten(changed)}
		if len(changed) > 0 && rowsBefore != nil {
			rowsAfter, err := r.tr.readRows(ctx)
			if err != nil {
				return nil, err
			}
			se.Rows = rowChanges(rowsBefore, rowsAfter)
		}
		res.Record.SideEffects = se
	}
	size := int64(len(res.Body))
	err = r.write(req, res)
	collect(res, size)
	return cur, err
}

// rowTrackingItems are the tables whose rows the capture tracks
// (meta.RowTracking), from the configuration, named as the rows of a side
// effect name them (the drivers' Rows): PostgreSQL's in public and SQL
// Server's in dbo when row_tracking names no schema. [] for none: every
// request, read-only or not and whichever ran, has its rows recorded of
// these tables only.
func rowTrackingItems(p *config.Project) []string {
	out := []string{}
	for name, sc := range p.State {
		for _, t := range sc.RowTracking {
			switch {
			case strings.Contains(t, "."):
			case sc.Driver == config.Postgres:
				t = "public." + t
			case sc.Driver == config.SQLServer || sc.Driver == "mssql":
				t = "dbo." + t
			}
			out = append(out, name+"/table:"+t)
		}
	}
	sort.Strings(out)
	return out
}

// send sends a request within the rate limits. Parameters that come from an
// earlier response are filled first; when that value is missing, the request
// is not sent and its record says why.
func (r *runner) send(ctx context.Context, req catalog.Request) httpx.Result {
	if len(req.ParamsFrom) > 0 {
		params := map[string]string{}
		for k, v := range req.Params {
			params[k] = v
		}
		for name, f := range req.ParamsFrom {
			v, err := r.valueFrom(f)
			if err != nil {
				rec := &snapshot.Record{ID: req.ID, Headers: [][2]string{}, Body: snapshot.Body{Storage: req.Storage},
					Error: fmt.Sprintf("parameter %s: %v", name, err)}
				return httpx.Result{Record: rec}
			}
			params[name] = v
		}
		req.Params = params
	}
	if u, ok := r.uploads[req.ID]; ok {
		var err error
		if req, err = u.apply(req); err != nil {
			rec := &snapshot.Record{ID: req.ID, Headers: [][2]string{}, Body: snapshot.Body{Storage: req.Storage}, Error: err.Error()}
			return httpx.Result{Record: rec}
		}
	}
	s := r.services[req.Target]
	s.slots <- struct{}{} // at most the service's concurrency at a time
	defer func() { <-s.slots }()
	if u, err := s.client.URL(req); err == nil && (!unlimited(ctx) || s.remote) {
		if pu, err := url.Parse(u); err == nil {
			s.limits.wait(ctx, req.Method+" "+pu.Path)
		}
	}
	return s.client.Do(ctx, req, r.tokens[req.Identity])
}

// valueFrom reads a value from the response of an earlier request of this
// capture.
func (r *runner) valueFrom(f catalog.From) (string, error) {
	rec, err := r.set.Record(f.Request)
	if err != nil {
		return "", err
	}
	if rec == nil || rec.Error != "" {
		return "", fmt.Errorf("%s has no response (it must run earlier in the same sequence)", f.Request)
	}
	body, err := r.set.Body(rec)
	if err != nil {
		return "", err
	}
	v, err := jsonx.Parse(body)
	if err != nil {
		return "", fmt.Errorf("%s: body is not JSON", f.Request)
	}
	at, err := v.At(f.Path)
	if err != nil {
		return "", fmt.Errorf("%s: %v", f.Request, err)
	}
	if at.Kind == jsonx.Object || at.Kind == jsonx.Array || at.Kind == jsonx.Null {
		return "", fmt.Errorf("%s: %s is %s, not a value", f.Request, f.Path, at.Kind)
	}
	return at.FormatText(), nil
}

// write stores a response; a public body with personal data or a
// credential, in the body or in a header, or one that cannot be scanned
// (scan.Public), is stored as private instead. Personal data in a header value
// is hidden in every record, since the record goes into git.
func (r *runner) write(req catalog.Request, res httpx.Result) error {
	var findings []pii.Finding
	unscanned := ""
	if res.Record.Error == "" {
		findings, res.Record.Headers = r.detector.Headers(res.Record.Headers)
		if req.Storage != snapshot.Public {
			findings = nil
		} else {
			body, why := scan.Public(r.detector, res.Record.Headers, res.Body)
			if findings, unscanned = append(body, findings...), why; len(findings) > 0 || unscanned != "" {
				res.Record.Body.Storage = snapshot.Private // encrypted, never in plain text
			}
		}
	}
	var body []byte
	if res.Record.Error == "" {
		body = res.Body
		if body == nil {
			body = []byte{}
		}
	}
	// Nothing reads the body after this: a later request that needs a value
	// from it reads the stored record.
	if err := r.set.WriteOwned(res.Record, body); err != nil {
		return fmt.Errorf("%s: %w", req.ID, err)
	}
	chaos.Point("capture.record")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sum.Requests++
	r.timings[req.ID] = res.Duration.Milliseconds()
	status := fmt.Sprint(res.Record.Status)
	if res.Record.Exit != nil {
		status = fmt.Sprintf("exit %d", *res.Record.Exit)
	}
	if res.Record.Error != "" {
		r.sum.Failed = append(r.sum.Failed, req.ID)
		status = "ERR"
	}
	if len(findings) > 0 {
		r.sum.Personal[req.ID] = findings
	}
	if unscanned != "" {
		r.sum.Unscanned[req.ID] = unscanned
	}
	if rt := res.Record.Retry; rt != nil {
		what := fmt.Sprintf("failed in all %d sends", rt.Sends)
		if rt.Recovered {
			r.sum.Retried[req.ID] = rt
			what = "after a retry"
		}
		fmt.Fprintf(r.out, "%-4s %s  %dms  (%s: the first send gave %s after %dms)\n", status, req.ID, res.Duration.Milliseconds(), what, rt.First(), rt.MS)
		return nil
	}
	fmt.Fprintf(r.out, "%-4s %s  %dms\n", status, req.ID, res.Duration.Milliseconds())
	return nil
}

// loadTokens reads the token source, then the identities' own sources.
// LoadTokens reads the tokens of every identity.
func LoadTokens(ctx context.Context, p *config.Project) (map[string]string, error) {
	return loadTokens(ctx, p, nil)
}

func loadTokens(ctx context.Context, p *config.Project, env []string) (map[string]string, error) {
	tokens := map[string]string{}
	var raw []byte
	var err error
	switch {
	case len(p.Tokens.Command) > 0:
		cmd := procx.Command(ctx, p.Tokens.Command[0], p.Tokens.Command[1:]...)
		cmd.Dir = p.Root
		cmd.Env = append(secret.Environ(), env...)
		cmd.Stderr = io.Discard
		if raw, err = procx.Output(cmd); err != nil {
			return nil, envErr("tokens command: %v", err)
		}
	case p.Tokens.File != "":
		path := p.Tokens.File
		if rest, ok := config.HomeRelative(path); ok {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, rest)
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(p.Root, path)
		}
		if raw, err = os.ReadFile(path); err != nil {
			return nil, envErr("tokens file: %v", err)
		}
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return nil, envErr("tokens must be a JSON object of identity → token: %v", err)
		}
	}
	for name, id := range p.Identities {
		switch {
		case id.Env != "":
			if v := os.Getenv(id.Env); v != "" {
				tokens[name] = v
			}
		case id.Op != "":
			// Not in a process group of its own: op may ask at the
			// terminal to sign in, which a background group cannot.
			cmd := exec.CommandContext(ctx, "op", "read", id.Op)
			cmd.Env = secret.Environ()
			out, err := cmd.Output()
			if err != nil {
				return nil, envErr("identity %s: op read: %v", name, err)
			}
			tokens[name] = strings.TrimSpace(string(out))
		}
	}
	return tokens, nil
}

func checkIdentities(reqs []catalog.Request, tokens map[string]string) error {
	missing := map[string]bool{}
	for _, r := range reqs {
		if r.Identity != "" && tokens[r.Identity] == "" {
			missing[r.Identity] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var names []string
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	return envErr("no token for identity %s", strings.Join(names, ", "))
}

// limiter spaces requests whose "METHOD path" matches a rate-limit rule.
type limiter struct {
	mu    sync.Mutex
	rules []config.RateLimit
	next  []time.Time
}

func newLimiter(rules []config.RateLimit) *limiter {
	return &limiter{rules: rules, next: make([]time.Time, len(rules))}
}

func (l *limiter) wait(ctx context.Context, methodPath string) {
	path := methodPath[strings.Index(methodPath, " ")+1:]
	for i, rule := range l.rules {
		if !glob.Match(rule.Match, path) && !glob.Match(rule.Match, methodPath) {
			continue
		}
		l.mu.Lock()
		now := time.Now()
		at := l.next[i]
		if at.Before(now) {
			at = now
		}
		l.next[i] = at.Add(time.Minute / time.Duration(rule.PerMinute))
		l.mu.Unlock()
		select {
		case <-time.After(time.Until(at)):
		case <-ctx.Done():
		}
	}
}
