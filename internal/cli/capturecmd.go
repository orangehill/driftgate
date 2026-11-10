package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/generate"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/infer"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

func captureCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	only := fs.String("only", "", "")
	force := fs.Bool("force", false, "")
	remote := fs.Bool("allow-remote", false, "")
	remoteMut := fs.Bool("allow-remote-mutations", false, "")
	noRestore := fs.Bool("allow-mutating-without-restore", false, "")
	restore := fs.String("restore", "once", "")
	repeat := fs.Int("repeat", 1, "")
	baseline := fs.String("baseline", "local", "")
	skipFP := fs.Bool("skip-fingerprint-check", false, "")
	quiet := fs.Bool("quiet", false, "")
	calibrate := fs.Bool("calibrate", false, "")
	readOnly := fs.Bool("read-only", false, "")
	targets := targetFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageErr("capture")
	}
	if *repeat < 1 {
		return 0, configErr("--repeat must be at least 1")
	}
	if *calibrate {
		if *only != "" {
			return 0, configErr("--calibrate measures the whole catalog; it takes no --only")
		}
		// The setting is compared with a sequential capture, which must
		// first agree with itself.
		*repeat = max(*repeat, 2)
	}
	p, cat, allow, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	sel, err := selectTargets(p, targets)
	if err != nil {
		return 0, err
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	unlock, err := lockStore(ctx, p, errOut)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return captureSet(ctx, p, sel.requests(cat), allow, pos[0], captureOptions{only: *only, force: *force, allowRemote: *remote,
		allowRemoteMutations: *remoteMut, noRestore: *noRestore, restore: *restore, repeat: *repeat, baseline: *baseline,
		skipFingerprint: *skipFP, quiet: *quiet, calibrate: *calibrate, readOnly: *readOnly}, out, errOut)
}

// captureOptions are the flags of capture.
type captureOptions struct {
	only                 string
	force                bool
	allowRemote          bool
	allowRemoteMutations bool
	noRestore            bool
	restore              string
	repeat               int
	baseline             string
	skipFingerprint      bool
	quiet                bool
	calibrate            bool // decide concurrency: auto (after the repeats agree)
	readOnly             bool // send only the requests that do not change data
	// first, when set, gets the summary of the first capture (run makes
	// the baseline from it).
	first func(*capture.Summary)
}

// runOptions are the options of every capture.Run of a capture command:
// what the flags select (--only, --read-only) and allow goes into each,
// so no run of it (a repeat, the calibrated one) sends more than asked.
func (o captureOptions) runOptions(tag string, base *snapshot.Set, out, log io.Writer) capture.Options {
	return capture.Options{Tag: tag, Only: o.only, ReadOnly: o.readOnly, AllowRemote: o.allowRemote,
		AllowRemoteMutations: o.allowRemoteMutations, AllowMutatingWithoutRestore: o.noRestore, Restore: o.restore,
		Baseline: base, SkipFingerprintCheck: o.skipFingerprint, Out: out, Log: log}
}

// runGap is the least time between the end of a run of a repeated capture
// and the first write of the next (captureSet).
var runGap = time.Second

// captureSet captures the catalog under tag, repeated as the options say,
// prints the outcome and returns its exit code.
func captureSet(ctx context.Context, p *config.Project, cat *catalog.Catalog, allow *allowlist.List, tag string, o captureOptions, out, errOut io.Writer) (int, error) {
	base, err := p.Baseline(o.baseline)
	if err != nil {
		return 0, failed(err)
	}
	if base.Exists() {
		if err := checkBaseline(p, base, o.baseline); err != nil {
			return 0, err
		}
	}
	progress := out
	if o.quiet {
		progress = io.Discard
	}
	// Services with concurrency: auto capture as the baseline decided, and
	// one at a time while it is being decided.
	auto := autoServices(p, cat)
	if o.calibrate && len(auto) == 0 {
		return 0, configErr("--calibrate needs a service with concurrency: auto")
	}
	var used map[string]int
	if o.calibrate {
		for _, name := range auto {
			fmt.Fprintf(out, "%s: 1 for now (auto: decided after the sequential captures)\n", concurrencyOf(name))
		}
	} else if used, err = recordedConcurrency(out, base, o.baseline, auto); err != nil {
		return 0, err
	}

	if o.readOnly {
		// Once per command, however many runs it makes (--repeat).
		if len(p.Tokens.Command) > 0 {
			fmt.Fprintln(errOut, "warning: --read-only covers the requests only: tokens.command runs as configured and may write (a login), and connect_to does not apply to it")
		}
		readOnlyNote(out, cat, o.only)
	} else {
		byMethodNote(out, capture.ByMethod(cat, o.only), o.restore)
		literalIDNote(out, cat, o.only)
	}

	tags := []string{tag}
	for i := 2; i <= o.repeat; i++ {
		tags = append(tags, fmt.Sprintf("%s.repeat-%d", tag, i))
	}
	// The extra runs only serve the comparison, also when one of them fails.
	defer func() {
		for _, tag := range tags[1:] {
			if set, err := p.Capture(tag); err == nil {
				os.RemoveAll(set.Dir)
			}
		}
	}()
	code := ExitClean
	var first *capture.Summary
	// Each run writes at least a second after the last one ended: a
	// timestamp the application writes (updated_at) then differs between
	// the runs, so the comparison finds it, whatever the clocks of the
	// application and the database are set to. Two runs within one second
	// would leave it equal, and it would drift on every later run.
	var notBefore time.Time
	for i, tag := range tags {
		if len(tags) > 1 {
			fmt.Fprintf(progress, "\n— run %d of %d —\n", i+1, len(tags))
		}
		opt := o.runOptions(tag, base, progress, errOut)
		opt.Force, opt.Concurrency, opt.NotBefore = o.force || i > 0, used, notBefore
		sum, err := capture.Run(ctx, p, cat, opt)
		if err != nil {
			return 0, failed(err)
		}
		if back, err := fallBack(p, out, base, o.baseline, used, sum); err != nil {
			return 0, err
		} else if back {
			used, opt.Concurrency, opt.Force = nil, nil, true
			if sum, err = capture.Run(ctx, p, cat, opt); err != nil {
				return 0, failed(err)
			}
		}
		notBefore = time.Now().Add(runGap)
		if i == 0 {
			first = sum
			if o.first != nil {
				o.first(sum)
			}
			code = summarize(out, errOut, tag, sum, cat)
			if len(sum.Failed) > 0 {
				// More runs would only fail the same way.
				if len(sum.Failed) == sum.Requests {
					notAnswering(p, errOut)
				}
				return code, nil
			}
		}
	}
	if len(tags) > 1 {
		if err := keepMedianTimings(p, tags); err != nil {
			return 0, err
		}
		c, err := repeatReport(out, p, cat, allow, tags)
		if err != nil {
			return 0, err
		}
		code = max(code, c)
		if c == ExitClean {
			// The capture says it was checked: a rebase takes it as run
			// takes two equal captures.
			if err := markAgreed(p, tags[0], len(tags)); err != nil {
				return 0, err
			}
		}
	}
	if o.calibrate {
		if code != ExitClean {
			fmt.Fprintln(out, "concurrency: auto stays undecided until the sequential captures agree")
			return code, nil
		}
		if err := calibrate(ctx, p, cat, allow, tags[0], first, auto, base, o, out, errOut); err != nil {
			return 0, err
		}
	}
	return code, nil
}

// keepMedianTimings gives the capture kept (the first of tags) the median
// time of each request over all runs, so the times a comparison or a
// baseline made by run reads are not those of one noisy run.
func keepMedianTimings(p *config.Project, tags []string) error {
	var runs []map[string]int64
	for _, tag := range tags {
		set, err := p.Capture(tag)
		if err != nil {
			return failed(err)
		}
		if t := timings(set); t != nil {
			runs = append(runs, t)
		}
	}
	if len(runs) < 2 {
		return nil
	}
	set, err := p.Capture(tags[0])
	if err != nil {
		return failed(err)
	}
	if err := snapshot.WriteJSON(filepath.Join(set.Dir, "timings.json"), report.MedianTimings(runs)); err != nil {
		return envErr(err)
	}
	return nil
}

// markAgreed records in a capture's meta.json how many captures of the
// same state gave its responses.
func markAgreed(p *config.Project, tag string, n int) error {
	set, err := p.Capture(tag)
	if err != nil {
		return failed(err)
	}
	meta, err := set.Meta()
	if err != nil {
		return envErr(err)
	}
	if meta == nil {
		return envErr(fmt.Errorf("capture %s has no meta.json", tag))
	}
	meta.Agreed = n
	if err := set.WriteMeta(*meta); err != nil {
		return envErr(err)
	}
	return nil
}

// byMethodNote names the requests that change data only by their method
// (the catalog does not say mutates), once per command: they run one at a
// time after the read-only requests, each on what the writes before it
// left, so a search sent as POST that only reads sees data the other
// requests changed. A catalog of an earlier release has many.
func byMethodNote(out io.Writer, ids []string, restore string) {
	if len(ids) == 0 {
		return
	}
	on := "the data the requests before it changed (--restore once)"
	if restore == "each" {
		on = "a restored state (--restore each)"
	}
	fmt.Fprintf(out, "%d %s data only by their method, as the catalog does not say mutates: %s\n"+
		"  each runs one at a time after the read-only requests, on %s;\n"+
		"  say mutates: false for those that only read (a search sent as POST), mutates: true for the others,\n"+
		"  under requests: in driftgate.yaml (match: the request ID), then driftgate generate --write; an entry written by hand says it in the catalog\n",
		len(ids), plural(len(ids), "request changes", "requests change"), listIDs(ids), on)
}

// readOnlyNote says how many selected requests a read-only capture
// leaves out.
func readOnlyNote(out io.Writer, cat *catalog.Catalog, only string) {
	n := 0
	for id := range cat.ReadOnlySkips() {
		if only == "" || glob.Match(only, id) {
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(out, "--read-only: %d %s that change data (or share a sequence with one) not sent\n", n, plural(n, "request", "requests"))
	}
}

// literalIDNote warns, one line per request, of a request that changes
// data with a fixed id in its path (PUT /api/users/17): on another copy
// of the data that id may be a real record, and the status would not
// show it. An id from params_from or a parameter is not warned of.
func literalIDNote(out io.Writer, cat *catalog.Catalog, only string) {
	for _, r := range cat.Requests {
		if r.Command != nil || !r.Mutating() || (only != "" && !glob.Match(only, r.ID)) {
			continue
		}
		if id := catalog.LiteralID(r.Path); id != "" {
			fmt.Fprintf(out, "warning: %s (%s %s) changes data with the fixed id %s in its path; on another copy of the data it may be a real record: take it from a response with params_from\n",
				r.ID, r.Method, r.Path, id)
		}
	}
}

// markWriters says how to mark read-only requests that changed data as
// changing it, so they run one at a time on a known state: a generated
// one under requests: in driftgate.yaml, by its route (what generate
// matches; generate --write then writes it), one written by hand in the
// catalog.
func markWriters(out io.Writer, ids []string, cat *catalog.Catalog) {
	byID := map[string]catalog.Request{}
	if cat != nil {
		for _, r := range cat.Requests {
			byID[r.ID] = r
		}
	}
	var entries, hand []string
	seen := map[string]bool{}
	for _, id := range ids {
		r, ok := byID[id]
		if !ok || !generate.IsGenerated(r) {
			hand = append(hand, id)
			continue
		}
		_, path, _ := strings.Cut(r.Source.Route, " ")
		e := fmt.Sprintf("    - match: %q\n", r.Method+" "+path)
		if r.Target != "" {
			e += fmt.Sprintf("      target: %s\n", r.Target)
		}
		if !seen[e] {
			seen[e] = true
			entries = append(entries, e+"      mutates: true\n")
		}
	}
	fmt.Fprintln(out, "mark them as changing data, so they run one at a time on a known state:")
	if len(entries) > 0 {
		fmt.Fprint(out, "  in driftgate.yaml, then driftgate generate --write:\n  requests:\n"+strings.Join(entries, ""))
	}
	if len(hand) > 0 {
		fmt.Fprintf(out, "  in the catalog, written by hand: \"mutates\": true on %s\n", strings.Join(hand, ", "))
	}
}

// summarize prints the outcome of a capture and returns its exit code.
func summarize(out, errOut io.Writer, tag string, sum *capture.Summary, cat *catalog.Catalog) int {
	fmt.Fprintf(out, "\ncaptured %d requests into %s\n", sum.Requests, tag)
	if t := sum.Time; t.Fingerprints > 0 {
		fmt.Fprintf(out, "time %s: state fingerprints %s (%d, %d of them unnecessary after a check of the write counters), restores %s, the rest requests\n",
			t.Total.Round(time.Millisecond), t.Fingerprint.Round(time.Millisecond), t.Fingerprints, t.Reused, t.Restore.Round(time.Millisecond))
	}
	code := ExitClean
	if len(sum.Writers) > 0 {
		ids := make([]string, 0, len(sum.Writers))
		for id := range sum.Writers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		what := "read-only requests change"
		if len(ids) == 1 {
			what = "read-only request changes"
		}
		fmt.Fprintf(out, "\n%d %s data (recorded as side effects):\n", len(ids), what)
		for _, id := range ids {
			fmt.Fprintf(out, "  %s → %s\n", id, strings.Join(sum.Writers[id], ", "))
		}
		markWriters(out, ids, cat)
	}
	if len(sum.Unattributed) > 0 {
		fmt.Fprintf(out, "\nread-only requests changed %s, but no single request does it alone (an order effect or background work)\n", strings.Join(sum.Unattributed, ", "))
	}
	retriedNote(out, sum)
	preparedNote(out, sum)
	if len(sum.Personal) > 0 {
		ids := make([]string, 0, len(sum.Personal))
		for id := range sum.Personal {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintf(errOut, "\npersonal data or credentials found in %d responses marked public; they were stored as private (encrypted, not in git):\n", len(ids))
		for _, id := range ids {
			f := sum.Personal[id][0]
			fmt.Fprintf(errOut, "  %s: %s at %s (%s)", id, f.Kind, report.Printable(f.Where), report.Printable(f.Sample))
			if n := len(sum.Personal[id]); n > 1 {
				fmt.Fprintf(errOut, " and %d more", n-1)
			}
			fmt.Fprintln(errOut)
		}
		fmt.Fprintln(errOut, "mark them storage: private in the catalog, or allow the values in pii.allow if they are public")
		code = ExitConfig
	}
	if len(sum.Unscanned) > 0 {
		ids := make([]string, 0, len(sum.Unscanned))
		for id := range sum.Unscanned {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintf(errOut, "\n%d responses marked public cannot be scanned for personal data; they were stored as private (encrypted, not in git):\n", len(ids))
		for _, id := range ids {
			fmt.Fprintf(errOut, "  %s: %s\n", id, report.Printable(sum.Unscanned[id]))
		}
		fmt.Fprintln(errOut, "mark them storage: private in the catalog")
	}
	if len(sum.Failed) > 0 {
		fmt.Fprintf(errOut, "\n%d requests failed (connection, timeout or host not allowed): %v\n", len(sum.Failed), sum.Failed)
		code = ExitEnv
	}
	return code
}

// notAnswering says how to start the application when no request of a
// capture got an answer.
func notAnswering(p *config.Project, errOut io.Writer) {
	var urls []string
	for _, name := range p.TargetNames() {
		if t, err := p.TargetFor(name); err == nil {
			urls = append(urls, t.BaseURL)
		}
	}
	d, _ := detect(p.Root, p.Project, nil)
	fmt.Fprintf(errOut, "nothing answered: is the application running at %s? %s, or set the address in %s; then run again\n",
		strings.Join(urls, ", "), startHint(p.Root, d.kinds), rel(p.Root, p.Layout.Config))
}

// repeatReport compares every repeated run with the first one. Values that
// changed although the state was the same are not deterministic: for each,
// it proposes an allowlist entry with a format that matches every value seen.
func repeatReport(out io.Writer, p *config.Project, cat *catalog.Catalog, allow *allowlist.List, tags []string) (int, error) {
	first, err := p.Capture(tags[0])
	if err != nil {
		return 0, failed(err)
	}
	first.CacheRecords()
	values := map[key][]string{}
	var keys []key
	headers := map[string][][]string{} // name → values at each position, over all runs
	var headerNames []string
	var other []compare.Drift
	for _, tag := range tags[1:] {
		set, err := p.Capture(tag)
		if err != nil {
			return 0, failed(err)
		}
		set.CacheRecords()
		// The requests are compared in parallel; their results are taken
		// below in catalog order.
		results := make([]report.Result, len(cat.Requests))
		size := func(i int) int64 {
			var n int64
			for _, s := range []*snapshot.Set{first, set} {
				if r, _ := s.Record(cat.Requests[i].ID); r != nil {
					n += r.Body.Size
				}
			}
			return n
		}
		if err := parallel(len(cat.Requests), size, func(i int) error {
			var err error
			results[i], err = compareOne(cat.Requests[i], first, set, nil, allow, compare.LevelValues, true) // infer needs the values; it prints formats only
			return err
		}); err != nil {
			return 0, err
		}
		for ri, req := range cat.Requests {
			res := results[ri]
			hidden := res.Drifts
			if rec, err := first.Record(req.ID); err == nil && isPrivate(rec) {
				hidden = append([]compare.Drift{}, res.Drifts...)
				hidePrivateValues(hidden)
			}
			for i, d := range res.Drifts {
				switch {
				case d.Kind == compare.KindHeaderValue && len(d.Values[0]) == len(d.Values[1]):
					// Values are matched by position: the n-th Set-Cookie of
					// one run with the n-th of the other.
					name := strings.TrimPrefix(d.Where, "header:")
					if _, ok := headers[name]; !ok {
						headerNames = append(headerNames, name)
						headers[name] = make([][]string, len(d.Values[0]))
						for i, v := range d.Values[0] {
							headers[name][i] = []string{v}
						}
					}
					for i, v := range d.Values[1] {
						if i < len(headers[name]) {
							headers[name][i] = append(headers[name][i], v)
						}
					}
				case (d.Kind == compare.KindValue || d.Kind == compare.KindText || d.Kind == compare.KindAttributeValue || d.Kind == compare.KindCell) && d.Texts != [2]string{}:
					k := key{d.Request, indexes().ReplaceAllString(d.Where, "[*]")}
					if _, ok := values[k]; !ok {
						keys = append(keys, k)
					}
					values[k] = append(values[k], d.Texts[0], d.Texts[1])
				default:
					other = append(other, hidden[i])
				}
			}
		}
	}
	if len(keys) == 0 && len(headerNames) == 0 && len(other) == 0 {
		fmt.Fprintf(out, "\n%d runs on the same state gave the same responses ✓\n", len(tags))
		return ExitClean, nil
	}
	fmt.Fprintf(out, "\n%d runs on the same state gave different responses.\n", len(tags))
	mutating := false
	for _, r := range cat.Requests {
		mutating = mutating || r.Mutating()
	}
	if mutating && len(p.State) == 0 {
		fmt.Fprintln(out, "Warning: requests change data and no state is restored between the runs, so some differences may come from the data, not from the application. Configure state: (a restore command) before trusting these proposals.")
	}
	if len(keys) > 0 || len(headerNames) > 0 {
		fmt.Fprintln(out, "Proposed allowlist entries (check every format and write the reason):")
		const reason = "    reason: TODO — differs between captures of the same state"
		if len(headerNames) > 0 {
			fmt.Fprintln(out, "headers:")
			for _, n := range headerNames {
				fmt.Fprintf(out, "  - name: %s\n    format: %s\n%s\n", report.Printable(n), report.Printable(yamlQuote(headerFormat(n, headers[n]))), reason)
			}
		}
		if len(keys) > 0 {
			fmt.Fprintln(out, "body:")
			for _, r := range mergeRules(cat, keys, values) {
				fmt.Fprintf(out, "  - request: %s\n    path: %s\n    format: %s\n%s\n", yamlQuote(r.request), report.Printable(yamlQuote(r.path)), report.Printable(yamlQuote(r.format)), reason)
			}
		}
	}
	if len(other) > 0 {
		fmt.Fprintf(out, "\nDifferences without a proposal (structure, types, side effects or a body that is neither JSON nor HTML):\n")
		for _, d := range other {
			b, c := report.DriftPair(d, 100)
			fmt.Fprintf(out, "  %s  %s %s  %s → %s\n", d.Request, d.Kind, report.Printable(d.Where), report.Printable(b), report.Printable(c))
		}
		fmt.Fprintln(out, "A response that differs by itself and cannot be allow-listed can be marked observe: true in the catalog (measured, never compared), or excluded.")
	}
	return ExitDrift, nil
}

var indexes = lazy.Regexp(`\[\d+\]`)

type rule struct{ request, path, format string }

// key is a proposal: one request and one body path.
type key struct{ request, path string }

// mergeRules turns per-request proposals into rules. Requests with the same
// path and format share one rule when a glob over their common ID prefix
// matches exactly them in the catalog, never more.
func mergeRules(cat *catalog.Catalog, keys []key, values map[key][]string) []rule {
	type group struct {
		path, format string
		requests     []string
	}
	var groups []*group
	index := map[[2]string]*group{}
	for _, k := range keys {
		f := infer.Pattern(values[k])
		g := index[[2]string{k.path, f}]
		if g == nil {
			g = &group{path: k.path, format: f}
			index[[2]string{k.path, f}] = g
			groups = append(groups, g)
		}
		g.requests = append(g.requests, k.request)
	}
	var out []rule
	for _, g := range groups {
		if len(g.requests) > 1 {
			prefix := g.requests[0]
			for _, r := range g.requests[1:] {
				for !strings.HasPrefix(r, prefix) {
					prefix = prefix[:len(prefix)-1]
				}
			}
			members := map[string]bool{}
			for _, r := range g.requests {
				members[r] = true
			}
			exact := len(prefix) >= 3
			for _, req := range cat.Requests {
				if strings.HasPrefix(req.ID, prefix) && !members[req.ID] {
					exact = false
				}
			}
			if exact {
				out = append(out, rule{prefix + "*", g.path, g.format})
				continue
			}
		}
		for _, r := range g.requests {
			out = append(out, rule{r, g.path, g.format})
		}
	}
	return out
}

// headerFormat is one format for every value of a header: a pattern per
// position, joined with | when they differ. A Set-Cookie header has one
// pattern per cookie name, whatever its position.
// A header is recorded masked where it may carry a credential (letters
// become x and X, digits 0: httpx.MaskHeader), and no format is derived
// from what the mask left: a cookie's value is any text up to the first
// ;, and a header masked whole is any text.
func headerFormat(name string, positions [][]string) string {
	if name == "set-cookie" {
		return strings.Join(cookieFormats(slices.Concat(positions...)), "|")
	}
	var parts []string
	seen := map[string]bool{}
	for _, vals := range positions {
		p := ".+"
		if !httpx.MasksWhole(name) {
			p = valuesFormat(vals)
		}
		if !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "|")
}

// valuesFormat is a pattern for values, or the value itself when it did
// not change.
func valuesFormat(vals []string) string {
	if p := infer.Pattern(vals); p != "" {
		return p
	}
	return regexp.QuoteMeta(vals[0])
}

// cookieFormats are the formats of Set-Cookie values, one per cookie
// group: the values of one cookie name with the same attributes in the
// same order (their keys and flags as sent), of one kind (a cookie set, or
// one deleted: emptied or with a Max-Age of 0 or less) and one form of
// expiry. The name is as it is, the value (masked when recorded, which
// keeps its length) is text without a ; of the lengths seen, and each
// attribute takes cookieAttrFormat. A deleted cookie does not match a
// group of set ones, nor a set one a group of deleted ones. A delete that
// keeps a value, has no Max-Age and has an expiry in the past shares the
// group of the set cookie, as before.
func cookieFormats(vals []string) []string {
	type groupKey struct {
		name, skeleton, expires string
		deleted                 bool
	}
	type group struct {
		groupKey
		lo, hi int        // shortest and longest value, in characters
		vals   []string   // as recorded
		rests  []string   // the attributes as one text, from the first ;
		attrs  [][]string // the attributes, split at ;
	}
	var groups []*group
	index := map[groupKey]*group{}
	for _, v := range vals {
		first, rest, _ := strings.Cut(v, ";")
		cookie, value, ok := strings.Cut(first, "=")
		if !ok {
			return []string{valuesFormat(vals)} // not a cookie: as recorded
		}
		var attrs, skeleton []string
		if strings.Contains(v, ";") {
			attrs = strings.Split(rest, ";")
			rest = ";" + rest
		}
		k := groupKey{name: cookie, deleted: value == ""}
		for _, a := range attrs {
			key, val, isValue := strings.Cut(a, "=")
			if isValue {
				key += "="
			}
			skeleton = append(skeleton, key)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "max-age=":
				n, err := strconv.Atoi(val)
				k.deleted = k.deleted || err == nil && n <= 0
			case "expires=":
				k.expires = expiresForm(val)
			}
		}
		k.skeleton = strings.Join(skeleton, ";")
		n := utf8.RuneCountInString(value)
		g := index[k]
		if g == nil {
			g = &group{groupKey: k, lo: n, hi: n}
			index[k] = g
			groups = append(groups, g)
		}
		g.lo, g.hi = min(g.lo, n), max(g.hi, n)
		g.vals, g.rests, g.attrs = append(g.vals, v), append(g.rests, rest), append(g.attrs, attrs)
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		length := fmt.Sprintf("{%d}", g.lo)
		switch {
		case g.hi == 0:
			length = "{0}" // only ever empty
		case g.lo != g.hi:
			length = fmt.Sprintf("{%d,%d}", max(g.lo, 1), g.hi)
		}
		head := regexp.QuoteMeta(g.name) + "=[^;]" + length
		var b strings.Builder
		for i, a := range g.attrs[0] {
			key, _, isValue := strings.Cut(a, "=")
			b.WriteString(";" + regexp.QuoteMeta(key))
			if !isValue {
				continue // a flag (secure, httponly), as sent
			}
			col := make([]string, len(g.attrs))
			for j, attrs := range g.attrs {
				_, col[j], _ = strings.Cut(attrs[i], "=")
			}
			b.WriteString("=" + cookieAttrFormat(strings.ToLower(strings.TrimSpace(key)), col, g.expires, g.deleted))
		}
		f := head + b.String()
		// Each part matches its values; should the whole still not match
		// one, the attributes are inferred as one text.
		re, err := regexp.Compile(`^(?:` + f + `)$`)
		for _, v := range g.vals {
			if err != nil || !re.MatchString(v) {
				f = head + grouped(valuesFormat(g.rests))
				break
			}
		}
		out = append(out, f)
	}
	return out
}

// cookieAttrFormat is the format of the values of one cookie attribute in
// a group. A cookie that is set gets an expiry of its form (any HTTP date,
// any dashed date: runs within the same second agree on it only by chance)
// and a Max-Age of the widths seen (one width less from a power of ten:
// 1000 is 999 a second later), never 0 or less. In a deleted cookie
// Max-Age is one of the values seen, so that it never matches a set one;
// its expiry, which frameworks put a year or more before the time of the
// response, takes its form as well. Other attributes are inferred.
func cookieAttrFormat(key string, vals []string, expires string, deleted bool) string {
	switch {
	case key == "expires" && expires == "http":
		return httpDate
	case key == "expires" && expires == "dashed":
		return dashedDate
	case key == "max-age" && deleted:
		var seen []string
		for _, v := range vals {
			if q := regexp.QuoteMeta(v); !slices.Contains(seen, q) {
				seen = append(seen, q)
			}
		}
		return grouped(strings.Join(seen, "|"))
	case key == "max-age":
		short, lo, hi := vals[0], len(vals[0]), 0
		for _, v := range vals {
			// Not a plain count (a leading 0, or too long for a
			// repeat count): inferred as any other value.
			if !allDigits(v) || v[0] == '0' || len(v) > 18 {
				return grouped(valuesFormat(vals))
			}
			if len(v) < len(short) || len(v) == len(short) && v < short {
				short = v
			}
			lo, hi = min(lo, len(v)), max(hi, len(v))
		}
		if len(short) > 1 && strings.TrimRight(short, "0") == "1" {
			lo-- // a power of ten
		}
		// The first digit is never 0, so the format never takes 0.
		switch {
		case hi == 1:
			return `[1-9]`
		case lo == hi:
			return fmt.Sprintf(`[1-9]\d{%d}`, hi-1)
		}
		return fmt.Sprintf(`[1-9]\d{%d,%d}`, lo-1, hi-1)
	}
	return grouped(valuesFormat(vals))
}

// grouped is a format that may hold a | of its own (an alternation),
// inside (?:…) so that the text around it stays outside it; one without
// a | stays bare.
func grouped(p string) string {
	if !strings.Contains(p, "|") {
		return p
	}
	return "(?:" + p + ")"
}

// expiresForm is the form of a cookie's expiry: an HTTP date, a dashed
// date, or other.
func expiresForm(v string) string {
	switch {
	case httpDateValue().MatchString(v):
		return "http"
	case dashedDateValue().MatchString(v):
		return "dashed"
	}
	return "other"
}

// dashedDate is the format of a cookie's expiry as PHP's setcookie and
// other Netscape-style writers send it (Thu, 01-Jan-1970 00:00:01 GMT).
const dashedDate = `[A-Z][a-z]{2}, \d{2}-[A-Z][a-z]{2}-\d{4} \d{2}:\d{2}:\d{2} GMT`

var (
	httpDateValue   = lazy.Regexp(`^(?:` + httpDate + `)$`)
	dashedDateValue = lazy.Regexp(`^(?:` + dashedDate + `)$`)
)

// allDigits reports a run of digits only (as infer's allDigits).
func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

var yamlQuote = compare.YAMLQuote

func seedCmd(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	skip := fs.Bool("skip-commands", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("seed")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return configErr("%v", err)
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	unlock, err := lockStore(ctx, p, errOut)
	if err != nil {
		return err
	}
	defer unlock()
	err = capture.Seed(ctx, p, capture.SeedOptions{Out: out, Log: errOut, SkipCommands: *skip})
	if err != nil {
		return failed(err)
	}
	return nil
}

const initConfig = `# DriftGate configuration (driftgate help lists the commands).
version: 1
project: %s
mode: freeze            # freeze: every drift fails; evolve: drifts may be accepted with a reason
level: values           # status | shape | types | values
%s
target:
  base_url: http://localhost:8080
  # allow_hosts: [localhost, 10.0.0.0/8]   # default: localhost and private addresses
  # user_agent: driftgate/1
  # timeout: 60s
  # concurrency: 1                          # parallel read-only requests; auto: measured once, with the first baseline
  # follow_redirects: false
  # rate_limits:
  #   - match: "/api/throttled/*"
  #     per_minute: 50

# tokens:                                   # JSON object {"<identity>": "<token>"}
#   command: ["php", "artisan", "driftgate:tokens"]
#   file: ~/secrets/tokens.json
# identities:
#   admin: { env: DG_TOKEN_ADMIN }

# state:                                    # restored at the start and before requests that change data
#   db:
#     driver: mysql                         # or postgres
#     dsn: { env: DG_DB_DSN }               # mysql://user:pass@host:3306/ (never written here)
#     working: app                          # the database the application uses
#     pristine: app_pristine                # its pristine copy, made by driftgate seed
#     exclude: [sessions, cache, jobs]
#     row_tracking: [faq]                   # also record which rows change
#     # allow_remote: true                  # only for a copy on a host outside allow_hosts
#   redis:
#     restore: ["redis-cli", "FLUSHALL"]
#     fingerprint: ["redis-cli", "DBSIZE"]
# seed:                                     # loads the data into the working database
#   - ["sh", "-c", "gunzip -c dump.sql.gz | mysql app"]
# prepare:                                  # after every restore
#   - ["php", "artisan", "migrate", "--force"]

# guards:                                   # must succeed before requests that change data
#   - name: mail driver is log
#     command: ["sh", "-c", "grep -q '^MAIL_MAILER=log' .env"]

# pii:
#   allow: ['[a-z.]+@example\.com']         # values allowed in public bodies

# bisect:                                   # driftgate bisect <good> <bad>: the first commit of each drift group
#   # puts the application on the commit checked out in $DRIFTGATE_BISECT_DIR (a separate worktree)
#   checkout: ["sh", "-c", "cd \"$DRIFTGATE_BISECT_DIR\" && make restart"]
#   ready: ["curl", "-sf", "http://localhost:8080/"]   # repeated until the application answers
#   finish: ["make", "restart"]             # at the end: back on this checkout
`

func initCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	project := fs.String("project", "", "")
	mode := fs.String("baselines", "", "")
	var only names
	fs.Var(&only, "service", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *cfg == "" {
		if cwd, err := os.Getwd(); err == nil {
			if _, main := config.MainWorktreeProject(cwd); main != "" {
				return configErr("this is a worktree of a project whose main worktree has its DriftGate project (%s): run driftgate setup here, which takes it with its baselines and key, rather than starting an empty one (--config makes a separate project)", main)
			}
		}
	}
	dr, err := newDraft(*cfg, *project, *mode, only)
	if err != nil {
		return err
	}
	// init asks nothing: the best of what the settings give, to check in
	// the file.
	if len(dr.dbs) > 0 {
		dr.db = &dr.dbs[0]
	}
	if len(dr.urls) > 0 {
		dr.url, dr.urlFrom = dr.urls[0].URL, dr.urls[0].File
	}
	if err := dr.write(); err != nil {
		return err
	}
	root, l, d := dr.p.Root, dr.p.Layout, dr.d
	found := "no known project type found; add routes under discover: or write the catalog by hand"
	if len(d.kinds) > 0 {
		found = "found " + strings.Join(d.kinds, ", ") + "; routes come from discover: in " + rel(root, l.Config)
	}
	if len(d.unchecked) > 0 {
		found += "\n" + strings.Join(d.unchecked, ", ") + ": recognised, but DriftGate has not been checked on such a project yet; check the discover: entry"
	}
	if len(d.found) > 1 && len(only) == 0 {
		found += "\nevery run checks all of them; init --service <name> (repeatable) keeps only the services you name"
	}
	if dr.db != nil {
		found += "\nstate: db " + describe(*dr.db) + ", commented out in " + rel(root, l.Config) + ": driftgate setup turns it on (it asks before it creates the pristine copy), or uncomment it"
		if len(dr.dbs) > 1 {
			found += fmt.Sprintf(" (%d database settings found; the others are listed above state:)", len(dr.dbs))
		}
	}
	if dr.urlFrom != "" && d.targets == "" {
		found += "\nbase_url " + dr.url + " from " + dr.urlFrom
	}
	fmt.Fprintf(out, `created %s, %s and %s
%s

Next steps:
  1. driftgate key create   (key for private bodies; DRIFTGATE_KEY takes precedence unless key: is set)
  2. check target.base_url and start the application
  3. driftgate run          (routes, catalog and the first baseline; later runs compare with it)
`, rel(root, l.Config), rel(root, l.Catalog), rel(root, l.Allowlist), found)
	if l.Committed() {
		fmt.Fprintf(out, `
Protect the baselines with a CODEOWNERS entry, so every change to them needs
your review:

  /%s/  @<your-github-user>
`, filepath.ToSlash(rel(root, l.Baselines)))
	} else {
		fmt.Fprintf(out, "\nEverything DriftGate keeps is in %s/, which git ignores (.gitignore).\n", rel(root, l.Dir))
	}
	return nil
}

// rel is path relative to root, for messages.
func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}

// httpDate is the format of an HTTP date (Sun, 09 Nov 2026 01:23:45 GMT).
const httpDate = `[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT`

// initAllowlist allows the Date header, which every response carries and
// which changes every second.
const initAllowlist = `version: 1
headers:
  - name: date
    format: '` + httpDate + `'
    reason: Time of the response; changes with every request.
body: []
`
