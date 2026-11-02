package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// resolve finds a snapshot set by name. "baseline:x" and "capture:x" choose
// explicitly; a bare name must exist as exactly one of the two.
func resolve(p *config.Project, name string) (*snapshot.Set, string, error) {
	if rest, ok := strings.CutPrefix(name, "baseline:"); ok {
		s, err := p.Baseline(rest)
		if err != nil {
			return nil, "", failed(err)
		}
		return s, name, nil
	}
	if rest, ok := strings.CutPrefix(name, "capture:"); ok {
		s, err := p.Capture(rest)
		if err != nil {
			return nil, "", failed(err)
		}
		return s, name, nil
	}
	b, err := p.Baseline(name)
	if err != nil {
		return nil, "", failed(err)
	}
	c, err := p.Capture(name)
	if err != nil {
		return nil, "", failed(err)
	}
	switch {
	case b.Exists() && c.Exists():
		return nil, "", configErr("%q is both a baseline and a capture; write baseline:%s or capture:%s", name, name, name)
	case b.Exists():
		return b, "baseline:" + name, nil
	case c.Exists():
		return c, "capture:" + name, nil
	}
	return nil, "", configErr("no baseline or capture named %q", name)
}

func loadProject(path string) (*config.Project, *catalog.Catalog, *allowlist.List, error) {
	p, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, configErr("%v", err)
	}
	cat, allow, err := loadFiles(p)
	if err != nil {
		return nil, nil, nil, err
	}
	return p, cat, allow, nil
}

// loadFiles reads the catalog and the allowlist of a loaded project.
func loadFiles(p *config.Project) (*catalog.Catalog, *allowlist.List, error) {
	cat, err := catalog.Load(p.Layout.Catalog)
	if err != nil {
		return nil, nil, configErr("%v", err)
	}
	// Every request names a known service; the empty name means the only one.
	for i := range cat.Requests {
		if _, err := p.TargetFor(cat.Requests[i].Target); err != nil {
			return nil, nil, configErr("catalog: %s: %v", cat.Requests[i].ID, err)
		}
		cat.Requests[i].Target = p.Canonical(cat.Requests[i].Target)
	}
	allow, err := allowlist.Load(p.Layout.Allowlist)
	if err != nil {
		return nil, nil, configErr("%v", err)
	}
	for _, t := range allowTargets(allow) {
		if _, err := p.TargetFor(t); err != nil {
			return nil, nil, configErr("allowlist: %v", err)
		}
	}
	return cat, allow, nil
}

func allowTargets(l *allowlist.List) []string {
	var out []string
	for _, r := range l.Headers {
		if r.Target != "" {
			out = append(out, r.Target)
		}
	}
	for _, r := range l.Body {
		if r.Target != "" {
			out = append(out, r.Target)
		}
	}
	for _, r := range l.Arrays {
		if r.Target != "" {
			out = append(out, r.Target)
		}
	}
	return out
}

func compareCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	against := fs.String("against", "local", "")
	levelFlag := fs.String("level", "", "")
	only := fs.String("only", "", "")
	format := fs.String("format", "text", "")
	maxDrifts := fs.Int("max-drifts", 20, "")
	skipFP := fs.Bool("skip-fingerprint-check", false, "")
	// Reports go to CI logs and pull-request comments: values from private
	// bodies stay out of them unless asked for, on one's own machine.
	showPrivate := fs.Bool("show-private", false, "")
	// A private body is only on the machine that captured it: elsewhere its
	// request is skipped, and the result is incomplete (exit 3) unless a
	// partial check is accepted.
	skipPrivate := fs.Bool("skip-private", false, "")
	targets := targetFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageErr("compare")
	}
	if !report.ValidFormat(*format) {
		return 0, configErr("--format must be one of %s", strings.Join(report.Formats, ", "))
	}
	p, cat, allow, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	sel, err := selectTargets(p, targets)
	if err != nil {
		return 0, err
	}
	cat = sel.requests(cat)
	var override *compare.Level
	if *levelFlag != "" {
		l, err := compare.ParseLevel(*levelFlag)
		if err != nil {
			return 0, configErr("%v", err)
		}
		if l < p.Level {
			return 0, configErr("--level %s is less strict than the configured level %s", l, p.Level)
		}
		override = &l
	}
	return compareSets(p, cat, allow, pos[0], *against, compareOptions{override: override, only: *only, format: *format,
		maxDrifts: driftsShown(fs, *maxDrifts), skipFingerprint: *skipFP, showPrivate: *showPrivate, skipPrivate: *skipPrivate,
		someTargets: sel != nil}, out)
}

// driftsShown is the maxDrifts of a report: what --max-drifts asks for, or
// without it the default view, which summarizes what follows from a
// changed status.
func driftsShown(fs *flag.FlagSet, maxDrifts int) int {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "max-drifts" })
	if !given {
		return report.DefaultDrifts
	}
	return maxDrifts
}

// compareOptions are the flags of compare.
type compareOptions struct {
	override        *compare.Level // stricter than the configured level
	only            string
	format          string
	maxDrifts       int
	skipFingerprint bool
	showPrivate     bool
	skipPrivate     bool
	someTargets     bool // --target: the catalog holds the requests of some services
}

// compareSets compares the snapshot set named current with the one named
// against, request by request over the catalog, saves the report as the
// last comparison and writes it; it returns the exit code of the result.
func compareSets(p *config.Project, cat *catalog.Catalog, allow *allowlist.List, current, against string, o compareOptions, out io.Writer) (int, error) {
	base, baseName, err := resolve(p, against)
	if err != nil {
		return 0, err
	}
	cur, curName, err := resolve(p, current)
	if err != nil {
		return 0, err
	}
	for _, s := range []struct {
		set  *snapshot.Set
		name string
	}{{base, baseName}, {cur, curName}} {
		meta, err := s.set.Meta()
		if err != nil {
			return 0, envErr(err)
		}
		if meta != nil && meta.Incomplete {
			return 0, envErr(fmt.Errorf("%s is an incomplete capture (interrupted?); capture it again with --force", s.name))
		}
		if meta != nil && meta.Unreliable && !o.skipFingerprint {
			return 0, configErr("%s was captured with --skip-fingerprint-check; compare it only with the same flag", s.name)
		}
		if name, ok := strings.CutPrefix(s.name, "baseline:"); ok {
			if err := checkBaseline(p, s.set, name); err != nil {
				return 0, err
			}
		}
	}

	origins, notes, err := setOrigins(p, base, cur)
	if err != nil {
		return 0, envErr(err)
	}

	// Every record is read here and again when its request is compared.
	base.CacheRecords()
	cur.CacheRecords()
	// Private bodies are compared by content, so the key must be at hand
	// before anything is compared.
	var ids []string
	for _, req := range cat.Requests {
		if (o.only == "" || glob.Match(o.only, req.ID)) && !req.Observe {
			ids = append(ids, req.ID)
		}
	}
	for _, s := range []struct {
		set  *snapshot.Set
		name string
	}{{base, baseName}, {cur, curName}} {
		recs, err := selectRecords(s.set, ids)
		if err != nil {
			return 0, err
		}
		if err := privateKey(p, s.name, recs); err != nil {
			return 0, err
		}
		if err := checkKey(s.set, recs); err != nil {
			return 0, err
		}
	}

	// Values from private bodies are never kept: with --show-private they
	// are shown, while the saved report (which report, review and accept
	// read, and a CI step may post) holds them hidden.
	var reqs []catalog.Request
	var levels []compare.Level
	for _, req := range cat.Requests {
		if o.only != "" && !glob.Match(o.only, req.ID) {
			continue
		}
		level := p.Level
		if req.Level != "" {
			level, _ = compare.ParseLevel(req.Level)
		}
		if o.override != nil && *o.override > level {
			level = *o.override
		}
		reqs = append(reqs, req)
		levels = append(levels, level)
	}
	// Every compare counts what the allowlist's rules did (unused_rules).
	allow = allow.Track()
	// Requests are compared in parallel; the results keep catalog order.
	var results, shown []report.Result
	if len(reqs) > 0 {
		results = make([]report.Result, len(reqs))
		if o.showPrivate {
			shown = make([]report.Result, len(reqs))
		}
	}
	size := func(i int) int64 {
		bRec, _ := base.Record(reqs[i].ID)
		cRec, _ := cur.Record(reqs[i].ID)
		switch {
		case bRec == nil || cRec != nil && compare.FastPaths && sameBody(bRec, cRec):
			return 0 // no body is read
		case cRec == nil:
			return bRec.Body.Size
		}
		return bRec.Body.Size + cRec.Body.Size
	}
	err = parallel(len(reqs), size, func(i int) error {
		var err error
		if results[i], err = compareOne(reqs[i], base, cur, origins, allow, levels[i], false); err != nil || !o.showPrivate {
			return err
		}
		shown[i], err = compareOne(reqs[i], base, cur, origins, allow, levels[i], true)
		return err
	})
	if err != nil {
		return 0, err
	}
	build := func(results []report.Result) *report.Report {
		r := report.Build(curName, baseName, results)
		r.Baseline, _ = strings.CutPrefix(baseName, "baseline:")
		if r.Baseline == baseName {
			r.Baseline = ""
		}
		r.Level, r.Mode, r.Created = p.Level.String(), p.Mode, time.Now().UTC().Format(time.RFC3339)
		r.SkipPrivate = o.skipPrivate
		r.Origins = notes
		r.UnusedRules = allow.Unused(o.only != "" || o.someTargets)
		r.Timings = report.CompareTimings(results, timings(base), timings(cur))
		if o.override != nil {
			r.Level = o.override.String()
		}
		locateGroups(p, cat, r)
		return r
	}
	r := build(results)
	if r.BaselineDigest, r.CaptureDigest, err = digests(base, baseName, cur, curName); err != nil {
		return 0, err
	}
	// The last comparison names drifts of private bodies: it goes into a
	// store git ignores.
	if _, err := p.WriteStore(); err != nil {
		return 0, failed(err)
	}
	if path, err := lastReport(p); err == nil {
		// review, accept and report read the last comparison.
		if err := r.Save(path); err != nil {
			return 0, err
		}
	}
	if o.showPrivate {
		r = build(shown)
	}
	if err := r.Write(out, o.format, o.maxDrifts); err != nil {
		return 0, err
	}
	return verdict(r), nil
}

// timings reads the response times a capture keeps (timings.json, in ms
// per request); nil when there are none.
func timings(set *snapshot.Set) map[string]int64 {
	raw, err := os.ReadFile(filepath.Join(set.Dir, "timings.json"))
	if err != nil {
		return nil
	}
	var ms map[string]int64
	if json.Unmarshal(raw, &ms) != nil {
		return nil
	}
	return ms
}

// verdict is the exit code of a comparison: 3 when it is incomplete
// (private requests not compared here, or nothing compared at all), 1 on
// drift, 0 clean. compare and run
// exit with it, and report with --exit-code.
func verdict(r *report.Report) int {
	switch {
	case r.Incomplete():
		return ExitEnv
	case r.Drifted > 0:
		return ExitDrift
	}
	return ExitClean
}

// lastReport is where compare keeps its last report, in the local store:
// one per checkout, so a comparison in one worktree is never accepted in
// another that shares the store.
func lastReport(p *config.Project) (string, error) {
	dir, err := p.StoreDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "last-compare-"+checkoutID(p)+".json"), nil
}

// checkoutID names the checkout of the project (its folder) in the shared
// local store.
func checkoutID(p *config.Project) string { return p.CheckoutID() }

var quotedKey = lazy.Regexp(`\["(?:[^"\\]|\\.)*"\]`)

// itemKey is the key of an array item in a path ([id=42], [slug="a"]), its
// name quoted when it is not plain (["@id"="a"]).
var itemKey = lazy.Regexp(`\[([\w.$-]+|"(?:[^"\\]|\\.)*")=(?:-?\d+|"(?:[^"\\]|\\.)*")\]`)

// hidePath hides the parts of a path into a private body that may be
// data: keys that are not plain names, and the keys of array items.
func hidePath(path string) string {
	path = quotedKey().ReplaceAllString(path, `["(private)"]`)
	if !strings.Contains(path, "=") {
		return path // no item key: the common case, without a second scan
	}
	return itemKey().ReplaceAllString(path, `[$1=(private)]`)
}

func isPrivate(r *snapshot.Record) bool { return r != nil && r.Body.Storage == snapshot.Private }

// hidePrivateValues keeps the kind and place of drifts in a private body but
// not its values; type names say nothing about the data and stay.
func hidePrivateValues(drifts []compare.Drift) {
	hideErrors(drifts)
	for i, d := range drifts {
		switch d.Kind {
		case compare.KindStatus, compare.KindHeaderMissing, compare.KindHeaderExtra, compare.KindHeaderValue, compare.KindSideEffect,
			compare.KindExit, compare.KindRepeat:
			continue
		}
		// A key that is not a plain name may be data (an e-mail address
		// used as a key): such keys are hidden in the path.
		drifts[i].Where = hidePath(d.Where)
		drifts[i].Texts = [2]string{}
		if d.Kind == compare.KindType {
			continue
		}
		if d.Baseline != "" {
			drifts[i].Baseline = "(private)"
		}
		if d.Current != "" {
			drifts[i].Current = "(private)"
		}
	}
}

// hideErrors keeps that a request failed but not why: the error of a
// request whose response or path holds private data is not shown.
func hideErrors(drifts []compare.Drift) {
	for i, d := range drifts {
		if d.Kind != compare.KindStatus {
			continue
		}
		for _, v := range []*string{&drifts[i].Baseline, &drifts[i].Current} {
			if strings.HasPrefix(*v, "error: ") {
				*v = "error (private)"
			}
		}
	}
}

// compareOne compares one request of two sets; origins are the sets' own
// origins per service (setOrigins), nil for two captures of one origin.
func compareOne(req catalog.Request, base, cur *snapshot.Set, origins map[string][2][]string, allow *allowlist.List, level compare.Level, showPrivate bool) (report.Result, error) {
	id := req.ID
	res := report.Result{ID: id, Level: level.String()}
	bRec, err := base.Record(id)
	if err != nil {
		return res, envErr(err)
	}
	if bRec == nil {
		res.Outcome, res.Note = report.Skipped, report.NoteNoBaseline
		return res, nil
	}
	cRec, err := cur.Record(id)
	if err != nil {
		return res, envErr(err)
	}
	if req.Observe {
		res.Outcome, res.Note = report.Observed, observation(bRec, cRec)
		return res, nil
	}
	// Never by the ref alone: without the body there is nothing to compare
	// in it. The rest of the record (status, headers, side effects) is in
	// the repository and is compared: a request that went from 200 to 500
	// drifts also here. The note says the body was not compared; the
	// request counts as private-missing either way (report.Build).
	noBody := missingPrivate(base, bRec) || missingPrivate(cur, cRec)
	b := compare.Side{Record: bRec, NoBody: noBody}
	c := compare.Side{Record: cRec, NoBody: noBody}
	if o, ok := origins[req.Target]; ok {
		b.Origins, c.Origins = o[0], o[1]
	}
	// Two bodies with one name are the same bytes, which is all the
	// comparison needs to know of them: neither is read.
	if !noBody && (cRec == nil || !compare.FastPaths || !sameBody(bRec, cRec)) {
		if b.Body, err = base.Body(bRec); err != nil {
			return res, envErr(fmt.Errorf("%s: %w", id, err))
		}
		if cRec != nil {
			if c.Body, err = cur.Body(cRec); err != nil {
				return res, envErr(fmt.Errorf("%s: %w", id, err))
			}
		}
	}
	res.Drifts, res.Notes = compare.OneNotes(id, b, c, allow.ForTarget(req.Target), level)
	if !showPrivate && (isPrivate(bRec) || isPrivate(cRec)) {
		hidePrivateValues(res.Drifts)
		for i, n := range res.Notes {
			res.Notes[i] = hidePath(n)
		}
	} else if !showPrivate && len(req.ParamsFrom) > 0 {
		hideErrors(res.Drifts) // its path may hold values of a private body
	}
	res.Outcome = report.Pass
	if len(res.Drifts) > 0 {
		res.Outcome = report.Drift
	}
	if noBody {
		res.Note = report.NotePrivateMissing
		if res.Outcome == report.Pass {
			res.Outcome = report.Skipped // the body, the main part, was not compared
		}
	}
	return res, nil
}

// sameBody reports two records whose bodies have the same name: the same
// SHA-256 of a public body, or the same ref of a private one (a keyed hash
// of its content), and so the same bytes.
func sameBody(a, b *snapshot.Record) bool {
	x, y := a.Body, b.Body
	if a.Error != "" || b.Error != "" || x.Storage != y.Storage {
		return false
	}
	switch x.Storage {
	case snapshot.Public:
		return x.SHA256 != "" && x.SHA256 == y.SHA256
	case snapshot.Private:
		return x.Ref != "" && x.Ref == y.Ref
	}
	return false
}

func observation(base, cur *snapshot.Record) string {
	desc := func(r *snapshot.Record) string {
		switch {
		case r == nil:
			return "not captured"
		case r.Error != "":
			return "error"
		}
		return fmt.Sprintf("%d, %d bytes", r.Status, r.Body.Size)
	}
	return desc(base) + " → " + desc(cur)
}
