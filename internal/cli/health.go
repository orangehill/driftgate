package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// answerRecord is the part of a response record that says how the request
// fared; the body is not needed.
type answerRecord struct {
	ID      string      `json:"id"`
	Status  int         `json:"status"`
	Headers [][2]string `json:"headers"`
	Error   string      `json:"error"`
	Repeat  string      `json:"repeat"`
	Exit    *int        `json:"exit"`
}

// readAnswers reads what baseline name recorded for each request (ids nil:
// every request), from the baseline's folder alone: it needs neither the
// local store nor the key. nil when the baseline does not exist. A file
// that vanishes while it is read (accept replacing the baseline) makes it
// read again.
func readAnswers(p *config.Project, name string, ids []string) (map[string]routes.Answer, error) {
	if !snapshot.SimpleName(name) {
		return nil, configErr("invalid baseline name %q (letters, digits, . _ -; not a name Windows reserves such as con or nul)", name)
	}
	dir := filepath.Join(p.Layout.Baselines, name, "responses")
	for attempt := 0; ; attempt++ {
		answers, err := readAnswersOnce(dir, ids)
		if err == nil || !errors.Is(err, os.ErrNotExist) || attempt == 3 {
			if err != nil {
				return nil, envErr(err)
			}
			return answers, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readAnswersOnce(dir string, ids []string) (map[string]routes.Answer, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if ids == nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
				ids = append(ids, id)
			}
		}
	}
	recs := make([]*answerRecord, len(ids))
	err := snapshot.Each(len(ids), func(i int) error {
		if !snapshot.SimpleName(ids[i]) {
			return nil
		}
		raw, err := os.ReadFile(filepath.Join(dir, ids[i]+".json"))
		if errors.Is(err, os.ErrNotExist) {
			if _, derr := os.Stat(dir); derr != nil {
				return err // the folder went away: read again
			}
			return nil // no record for this request
		}
		if err != nil {
			return err
		}
		var r answerRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s: %w", ids[i], err)
		}
		recs[i] = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	answers := make(map[string]routes.Answer, len(recs))
	for _, r := range recs {
		if r == nil || r.Exit != nil {
			continue // a command step has no answer to judge
		}
		answers[r.ID] = answerOf(r.Status, r.Error, r.Repeat, r.Headers)
	}
	return answers, nil
}

// section prints a titled list, and says whether it did: nothing when it
// is empty.
func section(out io.Writer, title string, items []string) bool {
	if len(items) == 0 {
		return false
	}
	fmt.Fprintf(out, "\n%s:\n", title)
	for _, it := range items {
		fmt.Fprintf(out, "  %s\n", it)
	}
	return true
}

func failureLines(fs []routes.Failure) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.String()
	}
	return out
}

func failedRouteLines(rs []routes.FailedRoute) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Route + " ← " + strings.Join(failureLines(r.Requests), "; ")
	}
	return out
}

const (
	knownTitle      = "Known failures (known_failures: in exclusions.yaml; still compared)"
	staleKnownTitle = "Known failures that are stale (remove them from exclusions.yaml)"
)

// fixAdvice is how the baseline takes the answers of requests fixed in
// mode: evolve accepts them; freeze accepts nothing, so a rebase does. A
// rebase takes the whole capture, drifts included (as JoinAdvice says).
func fixAdvice(mode string) string {
	if mode == config.Freeze {
		return `rebase the baseline on a capture made twice with no other drift: driftgate capture <tag> --repeat 2 --force, then driftgate baselines rebase <baseline> --from <tag> --reason "…" --allow-freeze`
	}
	return "accept the new answers"
}

func failedRoutesTitle(mode string) string {
	return "Routes whose requests meant to succeed all failed in the baseline (not covered: acknowledge them under known_failures: in exclusions.yaml with a reason, or fix them and " + fixAdvice(mode) + ")"
}

func failedRequestsTitle(mode string) string {
	return "Requests meant to succeed that failed in the baseline (errors the baseline keeps green: fix them and " + fixAdvice(mode) + ")"
}

// baselineHealth checks a baseline just made (ids nil) or the requests just
// accepted into it (ids): requests meant to succeed whose answer is an
// error are listed, and with a route inventory (inv) so are the routes they
// leave uncovered, which exits 1 as coverage then does. It never fails:
// the baseline is written already, so a problem reading it is a warning.
func baselineHealth(p *config.Project, inv *routes.Inventory, excl *routes.Exclusions, cat *catalog.Catalog, name string, ids []string, out io.Writer) (int, []string) {
	if excl == nil {
		var err error
		if excl, err = routes.LoadExclusions(p.Path("exclusions.yaml")); err != nil {
			fmt.Fprintf(out, "\nwarning: %v; known failures are not read\n", err)
			excl = &routes.Exclusions{}
		}
	}
	if inv != nil && ids != nil {
		// Only the routes the accepted requests reach, with every request
		// on them: their answers decide.
		inv, cat, ids = reachedBy(inv, cat, ids)
	}
	answers, err := readAnswers(p, name, ids)
	if err != nil {
		fmt.Fprintf(out, "\nwarning: baseline %s was not checked for errors kept in it: %v\n", name, err)
		return ExitClean, nil
	}
	if answers == nil {
		return ExitClean, nil
	}
	var h *routes.Health
	var failed []routes.FailedRoute
	if inv != nil {
		c := routes.CheckWith(inv, cat, excl, answers)
		h, failed = c.Health, c.Failed
	} else {
		h = routes.CheckHealth(cat, answers, excl.KnownFailures)
	}
	section(out, fmt.Sprintf("%s, baseline %s", failedRequestsTitle(p.Mode), name), failureLines(h.Failed))
	section(out, knownTitle, failureLines(h.Known))
	if len(failed) == 0 {
		return ExitClean, nil
	}
	section(out, failedRoutesTitle(p.Mode), failedRouteLines(failed))
	return ExitDrift, []string{"requests meant to succeed failed in the baseline (see above)"}
}

// reachedBy narrows an inventory and catalog to the route methods the
// requests ids reach and the requests that reach those route methods.
func reachedBy(inv *routes.Inventory, cat *catalog.Catalog, ids []string) (*routes.Inventory, *catalog.Catalog, []string) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	type unit struct {
		route  *routes.Route
		method string
	}
	m := routes.NewMatcher(inv)
	units := make([]unit, len(cat.Requests))
	keep := map[unit]bool{}
	for i, req := range cat.Requests {
		if req.IsCommand() {
			continue
		}
		if r := m.Match(req); r != nil {
			units[i] = unit{r, r.UnitOf(req.Method)}
			if want[req.ID] {
				keep[units[i]] = true
			}
		}
	}
	kept := map[*routes.Route]bool{}
	for u := range keep {
		kept[u.route] = true
	}
	sub := *inv
	sub.Routes = nil
	for i := range inv.Routes {
		if kept[&inv.Routes[i]] {
			sub.Routes = append(sub.Routes, inv.Routes[i])
		}
	}
	subCat := &catalog.Catalog{Version: cat.Version}
	var read []string
	for i, req := range cat.Requests {
		if want[req.ID] || keep[units[i]] {
			subCat.Requests = append(subCat.Requests, req)
			read = append(read, req.ID)
		}
	}
	return &sub, subCat, read
}
