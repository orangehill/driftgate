package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// concurrency: auto is decided once, when the first
// baseline is made or with capture --calibrate: a probe measures the first
// read-only requests at 1, 2, 4 and 8 at once, and the smallest setting
// within 10% of the best throughput is tried in a capture of its own,
// compared with the sequential capture of the same restored state. The
// setting is kept only when the two are identical; it is recorded with its
// evidence in the baseline's meta.json, and later captures use it.
const (
	probeRequests = 32
	probeMax      = 8
)

// autoServices lists, in name order, the services of the catalog whose
// concurrency is auto.
func autoServices(p *config.Project, cat *catalog.Catalog) []string {
	seen := map[string]bool{}
	var out []string
	for _, req := range cat.Requests {
		name := p.Canonical(req.Target)
		if seen[name] || req.IsCommand() {
			continue
		}
		seen[name] = true
		if t, err := p.TargetFor(name); err == nil && t.Concurrency.Auto() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// concurrencyOf names the setting of a service in the output.
func concurrencyOf(service string) string {
	if service == "" {
		return "concurrency"
	}
	return "concurrency of " + service
}

// decisions reads the decisions of concurrency: auto from a set's meta.
func decisions(set *snapshot.Set) (map[string]snapshot.Concurrency, error) {
	out := map[string]snapshot.Concurrency{}
	if !set.Exists() {
		return out, nil
	}
	meta, err := set.Meta()
	if err != nil || meta == nil {
		return out, err
	}
	for _, c := range meta.Concurrency {
		out[c.Service] = c
	}
	return out, nil
}

// recordDecisions merges decisions into a set's meta, by service in name
// order; a baseline gets its manifest written again.
func recordDecisions(set *snapshot.Set, baseline bool, decided []snapshot.Concurrency) error {
	if baseline {
		// meta.json and the manifest change together, or neither does: a
		// baseline whose manifest does not match is refused.
		return swapBaseline(set, func(work *snapshot.Set) error { return writeDecisions(work, decided) })
	}
	return writeDecisions(set, decided)
}

// writeDecisions merges decided into the concurrency of the set's meta.json.
func writeDecisions(set *snapshot.Set, decided []snapshot.Concurrency) error {
	meta, err := set.Meta()
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("%s has no meta.json", set.Dir)
	}
	byService := map[string]snapshot.Concurrency{}
	for _, c := range append(meta.Concurrency, decided...) {
		byService[c.Service] = c
	}
	meta.Concurrency = make([]snapshot.Concurrency, 0, len(byService))
	for _, c := range byService {
		meta.Concurrency = append(meta.Concurrency, c)
	}
	sort.Slice(meta.Concurrency, func(i, j int) bool { return meta.Concurrency[i].Service < meta.Concurrency[j].Service })
	return set.WriteMeta(*meta)
}

// recordedConcurrency is the concurrency each auto service captures with:
// what the baseline recorded, or one at a time while nothing is decided.
// It prints the setting and why.
func recordedConcurrency(out io.Writer, base *snapshot.Set, baseName string, auto []string) (map[string]int, error) {
	if len(auto) == 0 {
		return nil, nil
	}
	known, err := decisions(base)
	if err != nil {
		return nil, envErr(err)
	}
	used := map[string]int{}
	for _, name := range auto {
		c, ok := known[name]
		if !ok || c.Value < 1 {
			fmt.Fprintf(out, "%s: 1 (auto, not decided yet: driftgate run decides it when it makes the baseline, driftgate capture --calibrate for an existing one)\n", concurrencyOf(name))
			continue
		}
		used[name] = c.Value
		fmt.Fprintf(out, "%s: %d (auto, recorded in baseline %s: %s)\n", concurrencyOf(name), c.Value, baseName, c.Reason)
	}
	return used, nil
}

// fallBack puts the services that captured with more than one request at
// once back to one at a time when the read-only requests changed items no
// single request explains: an effect of the order the decision did not see.
// The baseline records it, so later captures stay sequential. It reports
// whether anything fell back.
func fallBack(p *config.Project, out io.Writer, base *snapshot.Set, baseName string, used map[string]int, sum *capture.Summary) (bool, error) {
	if len(sum.Unattributed) == 0 {
		return false, nil
	}
	known, err := decisions(base)
	if err != nil {
		return false, envErr(err)
	}
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	var back []snapshot.Concurrency
	for _, name := range names {
		n := used[name]
		if n <= 1 {
			continue
		}
		c := known[name]
		c.Service, c.Value, c.Differed = name, 1, append([]string{}, sum.Unattributed...)
		c.Reason = fmt.Sprintf("back to 1 after a capture at %d changed %s, which no single read-only request explains", n, listIDs(sum.Unattributed))
		back = append(back, c)
		fmt.Fprintf(out, "%s: %s; recorded in baseline %s%s, capturing again one request at a time\n", concurrencyOf(name), c.Reason, baseName, commitNote(p, " (commit it)"))
	}
	if len(back) == 0 {
		return false, nil
	}
	return true, recordDecisions(base, true, back)
}

// calibrate decides concurrency: auto for the services in auto, after the
// sequential capture tag (summary seq) agreed with its repeats: it probes,
// captures once more at the chosen settings and compares that capture with
// tag. The decisions go into tag's meta, and into the baseline's when it
// exists.
func calibrate(ctx context.Context, p *config.Project, cat *catalog.Catalog, allow *allowlist.List, tag string, seq *capture.Summary,
	auto []string, base *snapshot.Set, o captureOptions, out, errOut io.Writer) error {
	progress := out
	if o.quiet {
		progress = io.Discard
	}
	fmt.Fprintf(out, "\nconcurrency: auto — measuring up to the first %d read-only requests at 1 to %d at once …\n", probeRequests, probeMax)
	probes, err := capture.Probe(ctx, p, cat, capture.ProbeOptions{Services: auto, Requests: probeRequests, Max: probeMax,
		AllowRemote: o.allowRemote, Out: progress, Log: errOut})
	if err != nil {
		return failed(err)
	}
	decided := map[string]*snapshot.Concurrency{}
	tried := map[string]int{}
	for _, name := range auto {
		c := &snapshot.Concurrency{Service: name, Value: 1}
		decided[name] = c
		runs := probes[name]
		for _, t := range runs {
			c.Probe = append(c.Probe, snapshot.Probe{Concurrency: t.Concurrency, Requests: t.Requests, MS: t.Duration.Milliseconds()})
		}
		switch n := capture.Choose(runs); {
		case len(runs) == 0:
			c.Reason = "fewer than 2 read-only requests to measure"
		case n == 1:
			c.Reason = "more requests at once are not faster (" + throughput(runs) + ")"
		default:
			c.Tried = n
			tried[name] = n
		}
	}

	if len(tried) > 0 {
		ptag := tag + ".concurrency"
		defer func() {
			if set, err := p.Capture(ptag); err == nil {
				os.RemoveAll(set.Dir)
			}
		}()
		fmt.Fprintf(progress, "\n— capture at the measured concurrency —\n")
		par, err := capture.Run(ctx, p, cat, capture.Options{Tag: ptag, Force: true, AllowRemote: o.allowRemote,
			AllowRemoteMutations: o.allowRemoteMutations, AllowMutatingWithoutRestore: o.noRestore, Restore: o.restore,
			Baseline: base, SkipFingerprintCheck: o.skipFingerprint, Out: progress, Log: errOut, Concurrency: tried})
		if err != nil {
			return failed(err)
		}
		differed, err := differences(p, cat, allow, tag, ptag, seq, par, tried)
		if err != nil {
			return err
		}
		for name, n := range tried {
			c := decided[name]
			c.SequentialMS, c.ParallelMS = seq.Time.Total.Milliseconds(), par.Time.Total.Milliseconds()
			if d := differed[name]; len(d) > 0 {
				c.Differed = d
				c.Reason = fmt.Sprintf("at %d at once, %s differed from a sequential capture of the same state", n, listIDs(d))
				continue
			}
			c.Value = n
			c.Reason = fmt.Sprintf("identical to a sequential capture of the same state (%s); %s",
				capturedIn(seq.Time.Total, par.Time.Total, n), throughput(probes[name]))
		}
	}

	var list []snapshot.Concurrency
	for _, name := range auto {
		c := decided[name]
		list = append(list, *c)
		fmt.Fprintf(out, "%s: %d — %s\n", concurrencyOf(name), c.Value, c.Reason)
	}
	set, err := p.Capture(tag)
	if err != nil {
		return failed(err)
	}
	if err := recordDecisions(set, false, list); err != nil {
		return err
	}
	if base.Exists() {
		if err := recordDecisions(base, true, list); err != nil {
			return err
		}
		fmt.Fprintf(out, "recorded in baseline %s%s; later captures use it\n", o.baseline, commitNote(p, " (commit it)"))
	}
	return nil
}

// differences lists, per tried service, what made the capture at its
// concurrency differ from the sequential one: requests whose responses
// differ (at values, the allowlist applied, as for repeats), new writers
// among the read-only requests, and changes no single request explains,
// which count against every tried service.
func differences(p *config.Project, cat *catalog.Catalog, allow *allowlist.List, seqTag, parTag string,
	seq, par *capture.Summary, tried map[string]int) (map[string][]string, error) {
	seqSet, err := p.Capture(seqTag)
	if err != nil {
		return nil, failed(err)
	}
	parSet, err := p.Capture(parTag)
	if err != nil {
		return nil, failed(err)
	}
	found := map[string]map[string]bool{}
	add := func(service, what string) {
		if found[service] == nil {
			found[service] = map[string]bool{}
		}
		found[service][what] = true
	}
	target := map[string]string{}
	for _, req := range cat.Requests {
		name := p.Canonical(req.Target)
		target[req.ID] = name
		if _, ok := tried[name]; !ok {
			continue
		}
		res, err := compareOne(req, seqSet, parSet, nil, allow, compare.LevelValues, true)
		if err != nil {
			return nil, err
		}
		switch {
		case res.Outcome == report.Drift:
			add(name, req.ID)
		case res.Retry.Passed() && !res.BaselineRetry.Passed():
			// A load failure the retry covered, which the sequential
			// capture did not have, is a failure of the setting.
			add(name, req.ID+" (failed at first: "+res.Retry.First()+")")
		}
	}
	for id := range par.Writers {
		if _, ok := seq.Writers[id]; !ok {
			if _, ok := tried[target[id]]; ok {
				add(target[id], id+" (changed data)")
			}
		}
	}
	before := map[string]bool{}
	for _, it := range seq.Unattributed {
		before[it] = true
	}
	for _, it := range par.Unattributed {
		if !before[it] {
			for name := range tried {
				add(name, it+" (no single request explains it)")
			}
		}
	}
	out := map[string][]string{}
	for name, set := range found {
		for what := range set {
			out[name] = append(out[name], what)
		}
		sort.Strings(out[name])
	}
	return out, nil
}

// throughput describes the probe: requests per second at each setting.
func throughput(runs []capture.Throughput) string {
	var parts []string
	for _, t := range runs {
		rate := float64(t.Requests) / max(t.Duration.Seconds(), 1e-6)
		parts = append(parts, fmt.Sprintf("%d: %.1f", t.Concurrency, rate))
	}
	return "requests per second at " + strings.Join(parts, ", ")
}

func capturedIn(seq, par time.Duration, n int) string {
	return fmt.Sprintf("capture %s one at a time, %s at %d", seq.Round(time.Millisecond), par.Round(time.Millisecond), n)
}
