package report

import (
	"fmt"
	"html"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
)

// MarkdownLimit is the size of a GitHub comment (65,536 characters). The
// markdown report stays below it, counting bytes, which are never fewer than
// characters.
const MarkdownLimit = 65536

// markdownRoutes is how many routes a group lists in markdown.
const markdownRoutes = 3

// Markdown writes the report as a pull-request comment: a one-line verdict,
// a summary table, the explained groups, then every drifted request with up
// to maxDrifts drifts in a collapsed <details>. What does not fit under
// MarkdownLimit is left out, and the comment says so.
func (r *Report) Markdown(maxDrifts int) string {
	var b strings.Builder
	switch {
	case r.Unexpected() > 0 && r.Expected > 0:
		fmt.Fprintf(&b, "### DriftGate: ✗ %d of %d compared requests drifted unexpectedly (%d more as expected)\n\n", r.Unexpected(), r.Compared, r.Expected)
	case r.Drifted > r.Expected:
		fmt.Fprintf(&b, "### DriftGate: ✗ %d of %d compared requests drifted\n\n", r.Drifted, r.Compared)
	case r.Compared == 0:
		fmt.Fprintf(&b, "### DriftGate: ✗ incomplete, nothing compared\n\n")
	case len(r.ReadOnlyBaseline) > 0:
		fmt.Fprintf(&b, "### DriftGate: ✗ incomplete, %s not in the read-only set compared against\n\n", plural(len(r.ReadOnlyBaseline), "request"))
	case r.Incomplete():
		fmt.Fprintf(&b, "### DriftGate: ✗ incomplete, %s not compared here\n\n", plural(len(r.PrivateMissing), "private request"))
	case r.Expected > 0:
		fmt.Fprintf(&b, "### DriftGate: ✓ no unexpected drift in %d requests (%d drifted as expected)%s\n\n", r.Compared, r.Expected, r.retryVerdict())
	default:
		fmt.Fprintf(&b, "### DriftGate: ✓ no drift in %d requests%s\n\n", r.Compared, r.retryVerdict())
	}
	meta := []string{code(r.Tag) + " against " + code(r.Against)}
	if r.Level != "" {
		meta = append(meta, "level "+code(r.Level))
	}
	if r.Mode != "" {
		meta = append(meta, "mode "+code(r.Mode))
	}
	if r.Created != "" {
		meta = append(meta, mdText(r.Created))
	}
	for _, o := range r.Origins {
		meta = append(meta, "origins "+mdText(o))
	}
	b.WriteString(strings.Join(meta, " · ") + "\n\n")
	if r.Expected > 0 || len(r.ExpectedRefs) > 0 {
		b.WriteString("| Compared | Passed | Drifted | Expected | Skipped | Observed |\n|---:|---:|---:|---:|---:|---:|\n")
		fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d |\n", r.Compared, r.Passed, r.Drifted, r.Expected, r.Skipped, r.Observed)
	} else {
		b.WriteString("| Compared | Passed | Drifted | Skipped | Observed |\n|---:|---:|---:|---:|---:|\n")
		fmt.Fprintf(&b, "| %d | %d | %d | %d | %d |\n", r.Compared, r.Passed, r.Drifted, r.Skipped, r.Observed)
	}
	// The expectations not met are named before what may be cut (ten at
	// most): an expectation that held only in part is never left out.
	for _, note := range []string{r.ReadOnlyBaselineNote(), r.NoBaselineNote(), r.PrivateNote(), r.HiddenNote(), r.NothingNote(), r.UnusedNote(), r.ResentNote(), r.ResultNotes(), r.TimingsNote(), r.RetryNote(), r.UnmetNote(), r.KindsNote(), r.DominantNote(), r.NotCovered.CatalogLine()} {
		if note != "" {
			fmt.Fprintf(&b, "\n> %s\n", mdText(note))
		}
	}
	if lines := r.NotCovered.NotCoveredLines(); len(lines) > 0 {
		fmt.Fprintf(&b, "\n> %s\n", mdText(notCoveredTitle))
		for _, l := range lines {
			fmt.Fprintf(&b, "> - %s\n", mdText(l))
		}
	}

	// Everything below is added while it fits, keeping room for the note
	// that says what was left out.
	budget := MarkdownLimit - 1024
	fits := func(chunk string) bool { return b.Len()+len(chunk) <= budget }
	cutGroups, cutRequests, cutOther, cutExpected := 0, 0, false, false
	expectedGroups := r.expectedGroups()

	if len(r.Groups) > 0 && fits("\n#### What changed\n\n") {
		b.WriteString("\n#### What changed\n\n")
	}
	// The default view lists the status changes under the status group and
	// leaves out the groups that only follow from them; the others keep
	// their numbers, which accept takes.
	summary := maxDrifts == DefaultDrifts
	hidden := map[int]bool{}
	if summary {
		hidden = r.hiddenGroups()
	}
	for i, g := range r.Groups {
		if hidden[g.N] {
			continue
		}
		chunk := r.groupMarkdown(g, len(hidden) > 0, summary)
		if expectedGroups[g.N] {
			chunk = strings.Replace(chunk, "\n", " _(expected)_\n", 1)
		}
		if !fits(chunk) {
			cutGroups = len(r.Groups) - i
			break
		}
		b.WriteString(chunk)
	}
	if n := len(hidden); n > 0 && cutGroups == 0 {
		chunk := fmt.Sprintf("\n_%s only in requests whose status changed, summarized there; `driftgate review` lists them._\n", plural(n, "more group"))
		if fits(chunk) {
			b.WriteString(chunk)
		}
	}

	// The requests that fail come first; the expected ones, collapsed,
	// only in the room they leave.
	var drifted []Result
	for _, res := range r.Results {
		if failing(res) {
			drifted = append(drifted, res)
		}
	}
	if len(drifted) > 0 && fits("\n#### Drifted requests\n\n") {
		b.WriteString("\n#### Drifted requests\n\n")
	}
	for i, res := range drifted {
		what := plural(len(res.Drifts), "drift")
		if res.StatusChange != nil && maxDrifts == DefaultDrifts {
			what += ", status " + html.EscapeString(res.StatusChange.text())
		}
		chunk := fmt.Sprintf("<details><summary><code>%s</code> — %s</summary>\n\n%s\n</details>\n\n",
			html.EscapeString(res.ID), what, fence(strings.Join(failingLines(res, maxDrifts, 0), "\n")))
		if !fits(chunk) {
			cutRequests = len(drifted) - i
			break
		}
		b.WriteString(chunk)
	}
	if lines := r.ExpectedLines(); len(lines) > 0 {
		var list strings.Builder
		for _, l := range lines {
			list.WriteString("- " + mdText(l) + "\n")
		}
		chunk := fmt.Sprintf("<details><summary>%s</summary>\n\n%s\n</details>\n\n", mdText(r.expectedSummary()), list.String())
		if cutRequests == 0 && fits(chunk) {
			b.WriteString(chunk)
		} else {
			cutExpected = true
		}
	}

	var other []string
	for _, res := range r.Results {
		if res.Outcome == Skipped || res.Outcome == Observed {
			other = append(other, fmt.Sprintf("%-8s %s (%s%s)", res.Outcome, res.ID, res.Note, res.observedRetry()))
		}
	}
	if len(other) > 0 {
		chunk := fmt.Sprintf("<details><summary>%d skipped or observed</summary>\n\n%s\n</details>\n\n", len(other), fence(strings.Join(other, "\n")))
		if fits(chunk) {
			b.WriteString(chunk)
		} else {
			cutOther = true
		}
	}

	var cut []string
	if cutGroups > 0 {
		cut = append(cut, plural(cutGroups, "group"))
	}
	if cutRequests > 0 {
		cut = append(cut, plural(cutRequests, "drifted request"))
	}
	if cutExpected {
		cut = append(cut, "the requests that drifted as expected")
	}
	if cutOther {
		cut = append(cut, "the skipped and observed requests")
	}
	if len(cut) > 0 {
		fmt.Fprintf(&b, "\n_Not shown, to stay under GitHub's comment limit: %s. Run `driftgate report` for the whole report._\n", strings.Join(cut, ", "))
	}
	return b.String()
}

// groupMarkdown writes a group as an item of a numbered list; bullet writes
// it as a bullet with its number, for a list some groups are left out of
// (markdown renumbers a numbered list). With changes, the status group
// lists the status changes and their requests.
func (r *Report) groupMarkdown(g Group, bullet, changes bool) string {
	var b strings.Builder
	if bullet {
		fmt.Fprintf(&b, "- [%d] %s\n", g.N, mdText(g.text()))
	} else {
		fmt.Fprintf(&b, "%d. %s\n", g.N, mdText(g.text()))
	}
	for i, l := range g.Routes {
		if i == markdownRoutes {
			break
		}
		parts := strings.Split(l.describe("\x00"), "\x00")
		for j := range parts {
			parts[j] = code(parts[j])
		}
		fmt.Fprintf(&b, "   - %s\n", strings.Join(parts, " · "))
	}
	if more := g.RouteCount - min(len(g.Routes), markdownRoutes); more > 0 {
		fmt.Fprintf(&b, "   - and %s\n", plural(more, "more route"))
	}
	if changes && g.Example.Kind == compare.KindStatus {
		for _, s := range r.StatusChanges {
			fmt.Fprintf(&b, "   - %s: %s\n", mdText(s.head()), s.ids(code))
		}
	}
	return b.String()
}

// mdText escapes text so markdown shows it as written.
func mdText(s string) string {
	var b strings.Builder
	for _, r := range strings.Join(strings.Fields(s), " ") {
		switch {
		case r == '@' || r == '#':
			// Values from responses must not mention people or link issues.
			fmt.Fprintf(&b, "&#%d;", r)
			continue
		case strings.ContainsRune("\\`*_[]<>|~&", r):
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// longestRun is the longest run of backticks in s.
func longestRun(s string) int {
	best, n := 0, 0
	for _, r := range s {
		if r == '`' {
			n++
			best = max(best, n)
		} else {
			n = 0
		}
	}
	return best
}

// code writes s as inline code, with more backticks around it than any run
// inside it.
func code(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	ticks := strings.Repeat("`", longestRun(s)+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return ticks + s + ticks
}

// fence writes s as a code block whose fence no line inside can close.
func fence(s string) string {
	f := strings.Repeat("`", max(3, longestRun(s)+1))
	return f + "\n" + s + "\n" + f + "\n"
}
