package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/bisect"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// Defaults of the bisect hooks' time limits.
const (
	checkoutTimeout = 10 * time.Minute
	readyTimeout    = time.Minute
)

// chase is what bisect looks for: drift groups (by signature, numbered as
// in the last comparison) and the requests that show them.
type chase struct {
	baseline string
	groups   []chasedGroup // empty: the groups found at the bad commit
	requests []catalog.Request
	level    *compare.Level // the level the comparison used, when stricter than the configuration
}

type chasedGroup struct {
	N         int    `json:"n"`
	Signature string `json:"signature"`
}

// bisectCmd finds, for each drift group, the first commit between a good and
// a bad one that shows it. Every commit tested is checked out in a separate
// worktree; the checkout hook puts the application on it; the state is
// restored and the drifted requests are captured and compared with the
// baseline. The user's working tree, branch and baseline are not touched.
func bisectCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("bisect", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	var groupArgs names
	fs.Var(&groupArgs, "group", "")
	only := fs.String("only", "", "")
	baselineFlag := fs.String("baseline", "", "")
	format := fs.String("format", "text", "")
	withExpected := fs.Bool("expected", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 2 {
		return 0, usageErr("bisect")
	}
	if *format != "text" && *format != "json" {
		return 0, configErr("--format must be text or json")
	}
	p, cat, allow, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	// The application runs from the worktree, with the SQLite file of
	// the worktree; a restore would reset this checkout's file, which the
	// application does not use, and every commit would run on what the
	// one before it wrote.
	var sqlite []string
	for n, sc := range p.State {
		if sc.Driver == config.SQLite {
			sqlite = append(sqlite, n)
		}
	}
	if len(sqlite) > 0 {
		sort.Strings(sqlite)
		n := sqlite[0]
		return 0, configErr("bisect does not support SQLite state yet (state %s): the application runs from the worktree in $%s with that worktree's own file, and DriftGate restores %s of this checkout, so the requests of each commit would run on what earlier ones wrote; bisect by hand with git bisect and driftgate run, or use a MySQL, PostgreSQL or SQL Server database", n, bisect.EnvDir, p.State[n].Working)
	}
	if len(p.Bisect.Checkout) == 0 {
		return 0, configErr("bisect needs bisect.checkout in %s: the command that puts the application on the commit checked out in $%s (built, restarted), e.g.\n  bisect:\n    checkout: [\"sh\", \"-c\", \"cd \\\"$%s\\\" && make restart\"]\n    ready: [\"curl\", \"-sf\", \"http://localhost:8000/up\"]", *cfg, bisect.EnvDir, bisect.EnvDir)
	}
	ch, err := chooseChase(p, cat, groupArgs, *only, *baselineFlag, *withExpected)
	if err != nil {
		return 0, err
	}
	base, err := p.Baseline(ch.baseline)
	if err != nil {
		return 0, failed(err)
	}
	if !base.Exists() {
		return 0, configErr("there is no baseline %s", ch.baseline)
	}
	if err := checkBaseline(p, base, ch.baseline); err != nil {
		return 0, err
	}
	ids := make([]string, len(ch.requests))
	for i, r := range ch.requests {
		ids[i] = r.ID
	}
	recs, err := selectRecords(base, ids)
	if err != nil {
		return 0, err
	}
	if err := privateKey(p, "baseline:"+ch.baseline, recs); err != nil {
		return 0, err
	}

	ctx, stop := signalContext(errOut)
	defer stop()
	unlock, err := lockStore(ctx, p, errOut)
	if err != nil {
		return 0, err
	}
	defer unlock()
	began := time.Now()
	repo, err := bisect.Open(ctx, p.Root)
	if err != nil {
		return 0, configErr("%v", err)
	}
	var hashes [2]string
	for i, rev := range pos {
		if hashes[i], err = repo.Resolve(ctx, rev); err != nil {
			return 0, configErr("%v", err)
		}
	}
	path, err := repo.Path(ctx, hashes[0], hashes[1])
	if err != nil {
		return 0, configErr("%v", err)
	}
	rel, err := filepath.Rel(repo.Dir, p.Root)
	if err != nil {
		return 0, err
	}

	progress := out
	if *format == "json" {
		progress = errOut
	}
	fmt.Fprintf(progress, "bisect: %d commits after %s (good) up to %s (bad); %s, %d %s, against baseline %s\n",
		len(path)-1, path[0].Short, path[len(path)-1].Short, describeChase(ch), len(ch.requests), plural(len(ch.requests), "request", "requests"), ch.baseline)

	wt, err := repo.AddWorktree(ctx, path[0].Hash)
	if err != nil {
		return 0, envErr(err)
	}
	defer func() {
		if err := wt.Remove(); err != nil {
			fmt.Fprintf(errOut, "driftgate: removing the worktree %s: %v\n", wt.Dir, err)
		}
	}()
	defer func() {
		// The application last ran a commit of the worktree, which is about
		// to go: finish can put it back on the checkout. It runs after an
		// interrupt too.
		if len(p.Bisect.Finish) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), limitOr(p.Bisect.Timeout, checkoutTimeout))
		defer cancel()
		if err := bisect.Hook(fctx, p.Root, p.Bisect.Finish, []string{bisect.EnvRepo + "=" + repo.Dir}, 0, errOut); err != nil {
			fmt.Fprintf(errOut, "driftgate: bisect.finish: %v\n", err)
		}
	}()
	// prepare and guards run during the capture and see the commit too.
	defer func() {
		for _, k := range []string{bisect.EnvDir, bisect.EnvCommit, bisect.EnvProject, bisect.EnvRepo} {
			os.Unsetenv(k)
		}
	}()

	capCat := captureCatalog(cat, ch.requests)
	results := map[int][]report.Result{} // per tested commit
	s := &bisect.Searcher{Last: len(path) - 1}
	s.Test = func(i int) (bisect.Probe, error) {
		c := path[i]
		fmt.Fprintf(progress, "  %s %s: ", c.Short, shortSubject(c.Subject))
		probe, res, err := testCommit(ctx, p, wt, repo, rel, c, capCat, ch, base, allow, errOut)
		switch {
		case err != nil:
			fmt.Fprintln(progress, "stopped")
			return probe, err
		case probe.Skip != "":
			fmt.Fprintf(progress, "skipped (%s)\n", probe.Skip)
		default:
			results[i] = res
			fmt.Fprintln(progress, presentLine(ch, probe))
		}
		return probe, nil
	}

	good, bad, err := s.Endpoints()
	if errors.Is(err, bisect.ErrEndpoint) {
		which, c, probe := "good", path[0], good
		if bad.Skip != "" {
			which, c, probe = "bad", path[len(path)-1], bad
		}
		return 0, envErr(fmt.Errorf("the %s commit %s cannot be tested: %s", which, c.Short, probe.Skip))
	}
	if err != nil {
		return 0, err
	}
	if len(ch.groups) == 0 {
		// No comparison to start from: the groups are those of the bad
		// commit, numbered as a comparison of it would number them.
		for _, g := range report.Build("", "", results[len(path)-1]).Groups {
			ch.groups = append(ch.groups, chasedGroup{N: g.N, Signature: g.Signature})
		}
		if len(ch.groups) == 0 {
			fmt.Fprintf(progress, "nothing drifts at %s in the requests of --only %s\n", path[len(path)-1].Short, *only)
			return 0, configErr("nothing to bisect: the bad commit does not drift in these requests")
		}
	}

	// Before any search: every group must be clean at good and drift at bad.
	var refused []string
	for _, g := range ch.groups {
		switch {
		case good.Present[g.Signature]:
			refused = append(refused, fmt.Sprintf("[%d] %s drifts already at the good commit %s", g.N, g.Signature, path[0].Short))
		case !bad.Present[g.Signature]:
			refused = append(refused, fmt.Sprintf("[%d] %s does not drift at the bad commit %s", g.N, g.Signature, path[len(path)-1].Short))
		}
	}
	if len(refused) > 0 {
		return 0, configErr("bisect needs every group clean at the good commit and drifting at the bad one:\n  %s\npick other commits, or leave the group out (--group N chooses groups)", strings.Join(refused, "\n  "))
	}
	var answers []bisect.Answer
	for _, g := range ch.groups {
		a, err := s.Search(g.Signature)
		if err != nil {
			return 0, err
		}
		answers = append(answers, a)
	}

	res := bisectResult{Good: path[0], Bad: path[len(path)-1], Commits: len(path) - 1, Baseline: ch.baseline}
	for _, i := range s.Tested() {
		res.Tested = append(res.Tested, path[i].Short)
	}
	for _, i := range s.Skipped() {
		probe, _ := s.Outcome(i)
		res.Skipped = append(res.Skipped, skippedCommit{Commit: path[i].Short, Reason: probe.Skip})
	}
	for k, a := range answers {
		g := bisectGroup{chasedGroup: ch.groups[k], Status: a.Status, at: a.First}
		c := path[a.First]
		g.Commit = &c
		if a.Status == bisect.Range {
			g.Commit = nil
			g.Candidates = append(g.Candidates, path[a.From:a.First+1]...)
		}
		for _, r := range results[a.First] {
			for _, d := range r.Drifts {
				if d.Signature() == g.Signature {
					g.Drifts = append(g.Drifts, d)
				}
			}
		}
		for _, i := range a.Unsteady {
			probe, _ := s.Outcome(i)
			g.Unsteady = append(g.Unsteady, unsteadyCommit{Commit: path[i].Short, Drifts: probe.Present[g.Signature]})
		}
		res.Groups = append(res.Groups, g)
	}
	res.Seconds = time.Since(began).Seconds()
	if *format == "json" {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		return ExitClean, enc.Encode(res)
	}
	res.text(out)
	return ExitClean, nil
}

// chooseChase decides which groups and requests to follow. With a last
// comparison: its groups that are still open (--group chooses among them)
// and the requests that drift in them (--only narrows those). Without one,
// --only names the requests and the groups are found at the bad commit.
func chooseChase(p *config.Project, cat *catalog.Catalog, groupArgs []string, only, baselineName string, withExpected bool) (*chase, error) {
	byID := map[string]catalog.Request{}
	for _, r := range cat.Requests {
		byID[r.ID] = r
	}
	path, err := lastReport(p)
	if err != nil {
		return nil, failed(err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if only == "" || len(groupArgs) > 0 {
			return nil, configErr("there is no comparison yet: run driftgate run (or compare) at the bad commit first, so bisect knows the drift groups; or name the requests with --only <glob>")
		}
		ch := &chase{baseline: baselineName}
		if ch.baseline == "" {
			ch.baseline = "local"
		}
		for _, r := range cat.Requests {
			if glob.Match(only, r.ID) && !r.Observe {
				ch.requests = append(ch.requests, r)
			}
		}
		if len(ch.requests) == 0 {
			return nil, configErr("no catalog request matches --only %s", only)
		}
		return ch, nil
	}
	r, _, err := readLastCompare(p)
	if err != nil {
		return nil, err
	}
	if r.Baseline == "" {
		return nil, configErr("the last comparison is %s against %s; bisect follows drifts against a baseline (driftgate run, or compare <tag> --against <baseline>)", r.Tag, r.Against)
	}
	ch := &chase{baseline: r.Baseline}
	if baselineName != "" {
		ch.baseline = baselineName
	}
	if l, err := compare.ParseLevel(r.Level); err == nil && r.Level != "" && l > p.Level {
		ch.level = &l
	}
	chosen := map[string]bool{}
	for _, a := range groupArgs {
		n, err := strconv.Atoi(strings.TrimPrefix(a, "group:"))
		if err != nil || n < 1 || n > len(r.Groups) {
			return nil, configErr("--group %s: the last comparison has groups 1 to %d (driftgate review lists them)", a, len(r.Groups))
		}
		chosen[r.Groups[n-1].Signature] = true
	}
	done := map[string]bool{}
	for _, id := range r.Accepted {
		done[id] = true
	}
	seen := map[string]bool{}
	present := map[string]bool{}
	for _, res := range r.Results {
		if res.Outcome != report.Drift || done[res.ID] || (only != "" && !glob.Match(only, res.ID)) {
			continue
		}
		for _, d := range res.Drifts {
			sig := d.Signature()
			if len(chosen) > 0 && !chosen[sig] || d.Expected != "" && !withExpected {
				continue // a drift a ticket intends is not chased, as an accepted one is not
			}
			present[sig] = true
			if !seen[res.ID] {
				req, ok := byID[res.ID]
				if !ok {
					return nil, configErr("%s drifted in the last comparison but is no longer in the catalog; compare again", res.ID)
				}
				seen[res.ID] = true
				ch.requests = append(ch.requests, req)
			}
		}
	}
	for _, g := range r.Groups {
		if present[g.Signature] {
			ch.groups = append(ch.groups, chasedGroup{N: g.N, Signature: g.Signature})
		}
	}
	if len(ch.groups) == 0 {
		return nil, configErr("nothing to bisect: the last comparison (%s against %s) has no open drift in the groups and requests chosen", r.Tag, r.Against)
	}
	return ch, nil
}

// captureCatalog is the part of the catalog bisect captures: the chased
// requests, with every sequence one of them belongs to in full, since a
// later step can depend on an earlier one.
func captureCatalog(cat *catalog.Catalog, reqs []catalog.Request) *catalog.Catalog {
	want := map[string]bool{}
	for _, r := range reqs {
		want[r.ID] = true
	}
	out := &catalog.Catalog{Version: cat.Version}
	for _, s := range cat.Sequences {
		in := false
		for _, id := range s.Requests {
			in = in || want[id]
		}
		if in {
			out.Sequences = append(out.Sequences, s)
			for _, id := range s.Requests {
				want[id] = true
			}
		}
	}
	for _, r := range cat.Requests {
		if want[r.ID] {
			out.Requests = append(out.Requests, r)
		}
	}
	return out
}

// testCommit puts the application on a commit and compares the chased
// requests with the baseline. A commit whose hooks fail, or where requests
// get no answer, is skipped; an error stops the whole search.
func testCommit(ctx context.Context, p *config.Project, wt *bisect.Worktree, repo bisect.Repo, rel string, c bisect.Commit,
	cat *catalog.Catalog, ch *chase, base *snapshot.Set, allow *allowlist.List, errOut io.Writer) (bisect.Probe, []report.Result, error) {
	if err := wt.Checkout(ctx, c.Hash); err != nil {
		return bisect.Probe{}, nil, envErr(err)
	}
	env := []string{
		bisect.EnvDir + "=" + wt.Dir,
		bisect.EnvCommit + "=" + c.Hash,
		bisect.EnvProject + "=" + filepath.Join(wt.Dir, rel),
		bisect.EnvRepo + "=" + repo.Dir,
	}
	if err := bisect.Hook(ctx, p.Root, p.Bisect.Checkout, env, limitOr(p.Bisect.Timeout, checkoutTimeout), errOut); err != nil {
		if ctx.Err() != nil {
			return bisect.Probe{}, nil, envErr(errors.New("interrupted"))
		}
		return bisect.Probe{Skip: "checkout: " + err.Error()}, nil, nil
	}
	if err := bisect.Wait(ctx, p.Root, p.Bisect.Ready, env, limitOr(p.Bisect.ReadyTimeout, readyTimeout)); err != nil {
		if ctx.Err() != nil {
			return bisect.Probe{}, nil, envErr(errors.New("interrupted"))
		}
		return bisect.Probe{Skip: "ready: " + err.Error()}, nil, nil
	}

	tag := "bisect-" + c.Hash[:12]
	set, err := p.Capture(tag)
	if err != nil {
		return bisect.Probe{}, nil, failed(err)
	}
	// The capture serves only this comparison; the baseline is only read.
	defer os.RemoveAll(set.Dir)
	sum, err := capture.Run(ctx, p, cat, capture.Options{Tag: tag, Force: true, Baseline: base, Log: errOut, Env: env})
	switch {
	case ctx.Err() != nil:
		return bisect.Probe{}, nil, envErr(errors.New("interrupted"))
	case err != nil:
		return bisect.Probe{}, nil, failed(err)
	}
	if len(sum.Failed) > 0 {
		return bisect.Probe{Skip: "no answer to " + listIDs(sum.Failed)}, nil, nil
	}
	origins, _, err := setOrigins(p, base, set)
	if err != nil {
		return bisect.Probe{}, nil, envErr(err)
	}
	if err := rowValuesKey(namedSet{base, "the baseline"}, namedSet{set, "capture:" + tag}); err != nil {
		return bisect.Probe{}, nil, err
	}
	probe := bisect.Probe{Present: map[string]bool{}}
	var results []report.Result
	for _, req := range ch.requests {
		level := p.Level
		if req.Level != "" {
			level, _ = compare.ParseLevel(req.Level)
		}
		if ch.level != nil && *ch.level > level {
			level = *ch.level
		}
		res, err := compareOne(req, base, set, origins, allow, level, false)
		if err != nil {
			return bisect.Probe{}, nil, err
		}
		for _, d := range res.Drifts {
			probe.Present[d.Signature()] = true
		}
		results = append(results, res)
	}
	return probe, results, nil
}

func limitOr(d config.Duration, def time.Duration) time.Duration {
	if d.Duration > 0 {
		return d.Duration
	}
	return def
}

func shortSubject(s string) string {
	if r := []rune(s); len(r) > 50 {
		return string(r[:49]) + "…"
	}
	return s
}

func describeChase(ch *chase) string {
	if len(ch.groups) == 0 {
		return "the groups found at the bad commit"
	}
	if len(ch.groups) == 1 {
		return fmt.Sprintf("group %d", ch.groups[0].N)
	}
	ns := make([]string, len(ch.groups))
	for i, g := range ch.groups {
		ns[i] = strconv.Itoa(g.N)
	}
	return "groups " + strings.Join(ns, ", ")
}

// presentLine says which chased groups drift at a tested commit.
func presentLine(ch *chase, probe bisect.Probe) string {
	if len(ch.groups) == 0 {
		var sigs []string
		for s := range probe.Present {
			sigs = append(sigs, s)
		}
		if len(sigs) == 0 {
			return "no drift"
		}
		sort.Strings(sigs)
		return "drifts: " + strings.Join(sigs, "; ")
	}
	var ns []string
	for _, g := range ch.groups {
		if probe.Present[g.Signature] {
			ns = append(ns, strconv.Itoa(g.N))
		}
	}
	switch len(ns) {
	case 0:
		return "no drift of these groups"
	case 1:
		return "group " + ns[0] + " drifts"
	}
	return "groups " + strings.Join(ns, ", ") + " drift"
}

// bisectResult is the outcome of bisect, also as JSON.
type bisectResult struct {
	Good     bisect.Commit   `json:"good"`
	Bad      bisect.Commit   `json:"bad"`
	Commits  int             `json:"commits"` // after good, up to and with bad
	Baseline string          `json:"baseline"`
	Tested   []string        `json:"tested"` // in the order tested
	Skipped  []skippedCommit `json:"skipped,omitempty"`
	Groups   []bisectGroup   `json:"groups"`
	Seconds  float64         `json:"seconds"`
}

type skippedCommit struct {
	Commit string `json:"commit"`
	Reason string `json:"reason"`
}

type unsteadyCommit struct {
	Commit string `json:"commit"`
	Drifts bool   `json:"drifts"`
}

type bisectGroup struct {
	chasedGroup
	Status     string           `json:"status"`               // found or range
	Commit     *bisect.Commit   `json:"commit,omitempty"`     // found: the first commit with the drift
	Candidates []bisect.Commit  `json:"candidates,omitempty"` // range: one of these, in order
	Drifts     []compare.Drift  `json:"drifts"`               // the group's drifts at the commit (range: at the last candidate)
	Unsteady   []unsteadyCommit `json:"unsteady,omitempty"`   // tested commits that contradict the answer
	at         int              // place of the commit (range: the last candidate) on the path
}

func (r *bisectResult) text(w io.Writer) {
	fmt.Fprintln(w, "\nfirst commit of each drift group:")
	// Commits in the order of history; groups with the same commit are one
	// cause and are shown together.
	groups := append([]bisectGroup{}, r.Groups...)
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].at < groups[j].at })
	var order []string
	byCommit := map[string][]bisectGroup{}
	for _, g := range groups {
		key := "range"
		if g.Commit != nil {
			key = g.Commit.Hash
		}
		if _, ok := byCommit[key]; !ok && key != "range" {
			order = append(order, key)
		}
		byCommit[key] = append(byCommit[key], g)
	}
	for _, key := range order {
		gs := byCommit[key]
		c := gs[0].Commit
		fmt.Fprintf(w, "\n%s  %s  (%s, %s)\n", c.Short, c.Subject, c.Author, c.Date)
		for _, g := range gs {
			groupText(w, g)
		}
	}
	for _, g := range byCommit["range"] {
		fmt.Fprintf(w, "\none of %d commits (those before the last could not be tested):\n", len(g.Candidates))
		for _, c := range g.Candidates {
			fmt.Fprintf(w, "  %s  %s  (%s, %s)\n", c.Short, c.Subject, c.Author, c.Date)
		}
		groupText(w, g)
	}
	fmt.Fprintf(w, "\n%d commits from good to bad, %d tested", r.Commits+1, len(r.Tested))
	if len(r.Skipped) > 0 {
		var cs []string
		for _, s := range r.Skipped {
			cs = append(cs, s.Commit)
		}
		fmt.Fprintf(w, " (%d skipped: %s)", len(r.Skipped), strings.Join(cs, ", "))
	}
	fmt.Fprintf(w, ", in %s; baseline %s unchanged, your working tree untouched\n", (time.Duration(r.Seconds * float64(time.Second))).Round(time.Millisecond), r.Baseline)
}

func groupText(w io.Writer, g bisectGroup) {
	fmt.Fprintf(w, "  [%d] %s\n", g.N, g.Signature)
	const maxDrifts = 5
	for i, d := range g.Drifts {
		if i == maxDrifts {
			fmt.Fprintf(w, "        … %d more\n", len(g.Drifts)-maxDrifts)
			break
		}
		fmt.Fprintf(w, "        %-20s %s\n", d.Request, driftLine(d))
	}
	if len(g.Unsteady) > 0 {
		var parts []string
		for _, u := range g.Unsteady {
			what := "clean"
			if u.Drifts {
				what = "drifts"
			}
			parts = append(parts, u.Commit+" "+what)
		}
		fmt.Fprintf(w, "        the drift comes and goes (%s): this is one commit where it appears, perhaps not the first\n", strings.Join(parts, ", "))
	}
}
