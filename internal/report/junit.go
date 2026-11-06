package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// JUnit XML as GitHub (test-reporter actions), GitLab (artifacts:reports:junit)
// and Jenkins (junit step) read it: one test case per request; a drifted
// request fails with its drifts, a skipped or observed one is skipped. An
// incomplete result (exit 3) is one more case with an error naming why, so
// a reader of the file alone never sees it green.

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Time       string          `xml:"time,attr"`
	Timestamp  string          `xml:"timestamp,attr,omitempty"`
	Properties []junitProperty `xml:"properties>property,omitempty"`
	Cases      []junitCase     `xml:"testcase"`
	SystemOut  string          `xml:"system-out,omitempty"` // the report's notes: information, never a failure
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Error     *junitFailure `xml:"error,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	// SystemOut is the result's notes and, for a request that passed only
	// after a retry, that: JUnit has no outcome for it, and every reader
	// shows the output of a test case.
	SystemOut string `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// JUnit writes the report as JUnit XML, with up to maxDrifts drifts per
// failed request.
func (r *Report) JUnit(w io.Writer, maxDrifts int) error {
	name := "driftgate " + r.Tag + " against " + r.Against
	suite := junitSuite{Name: name, Time: "0", Timestamp: strings.TrimSuffix(r.Created, "Z")}
	expected := ""
	if r.Expected > 0 {
		expected = r.expectedSummary() + ": " + strings.Join(r.ExpectedLines(), "; ")
	}
	for _, p := range [][2]string{{"tag", r.Tag}, {"against", r.Against}, {"level", r.Level}, {"mode", r.Mode}, {"expected", expected}, {"unmet", r.UnmetNote()}} {
		if p[1] != "" {
			suite.Properties = append(suite.Properties, junitProperty{Name: p[0], Value: p[1]})
		}
	}
	var notes []string
	if n := len(r.Resent); n > 0 {
		suite.Properties = append(suite.Properties, junitProperty{Name: "resent", Value: fmt.Sprint(n)})
		notes = append(notes, r.ResentNote())
	}
	// Response times are information: properties and the suite's output,
	// never a failed case.
	if t := r.Timings; t != nil {
		suite.Properties = append(suite.Properties,
			junitProperty{Name: "p50_ms", Value: fmt.Sprintf("%d → %d", t.Baseline.P50, t.Current.P50)},
			junitProperty{Name: "p95_ms", Value: fmt.Sprintf("%d → %d", t.Baseline.P95, t.Current.P95)})
		for _, x := range t.Slower {
			suite.Properties = append(suite.Properties, junitProperty{Name: "slower:" + x.ID, Value: fmt.Sprintf("%d → %d ms", x.Baseline, x.Current)})
		}
		notes = append(notes, r.TimingsNote())
	}
	for _, note := range []string{r.ReadOnlyBaselineNote(), r.RetryNote(), r.DominantNote(), r.NotCovered.CatalogLine(), r.NotCovered.NotCoveredNote()} {
		if note != "" {
			notes = append(notes, note)
		}
	}
	suite.SystemOut = strings.Join(notes, "\n")
	// An expectation of a request not met (or met in part) is also in the
	// output of its test case, where a reader of the case looks.
	unmet := map[string][]string{}
	for _, u := range r.Unmet {
		unmet[u.Request] = append(unmet[u.Request], fmt.Sprintf("expectation %s not met: %s", u.Ref, u.Why))
	}
	for _, res := range r.Results {
		c := junitCase{Name: res.ID, Classname: "driftgate", Time: "0"}
		var out []string // the case's system-out, its notes last
		switch {
		case res.Outcome == Drift && len(res.Expected) > 0:
			// It passes: its drifts are exactly the ones expected.
			lines := driftLines(res, maxDrifts, 0)
			out = append(out, fmt.Sprintf("expected drift (%s), %s:\n%s", expectedNames(res), plural(len(res.Drifts), "drift"), strings.Join(lines, "\n")))
		case res.Outcome == Drift:
			lines := failingLines(res, maxDrifts, 0)
			c.Failure = &junitFailure{
				Message: fmt.Sprintf("%s: %s", plural(len(res.Drifts), "drift"), strings.Join(lines, "; ")),
				Type:    "drift",
				Text:    strings.Join(lines, "\n"),
			}
			suite.Failures++
		case res.Outcome == Skipped || res.Outcome == Observed:
			c.Skipped = &junitSkipped{Message: res.Outcome + ": " + res.Note + res.observedRetry()}
			suite.Skipped++
		case res.Outcome == Pass && res.Retry != nil:
			out = append(out, res.passedRetryLine())
		}
		out = append(append(out, unmet[res.ID]...), res.Notes...)
		c.SystemOut = strings.Join(out, "\n")
		suite.Tests++
		suite.Cases = append(suite.Cases, c)
	}
	if r.Incomplete() {
		var why []string
		for _, note := range []string{r.NothingNote(), r.PrivateNote(), r.ReadOnlyBaselineNote()} {
			if note != "" {
				why = append(why, note)
			}
		}
		if len(why) == 0 {
			why = append(why, "the result is incomplete")
		}
		suite.Cases = append(suite.Cases, junitCase{Name: "incomplete", Classname: "driftgate", Time: "0",
			Error: &junitFailure{Message: why[0], Type: "incomplete", Text: strings.Join(why, "\n")}})
		suite.Tests++
		suite.Errors++
	}
	doc := junitSuites{Name: name, Tests: suite.Tests, Failures: suite.Failures, Errors: suite.Errors, Skipped: suite.Skipped, Time: "0", Suites: []junitSuite{suite}}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", " ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
