package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// JUnit XML as GitHub (test-reporter actions), GitLab (artifacts:reports:junit)
// and Jenkins (junit step) read it: one test case per request; a drifted
// request fails with its drifts, a skipped or observed one is skipped.

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
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
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
	for _, p := range [][2]string{{"tag", r.Tag}, {"against", r.Against}, {"level", r.Level}, {"mode", r.Mode}} {
		if p[1] != "" {
			suite.Properties = append(suite.Properties, junitProperty{Name: p[0], Value: p[1]})
		}
	}
	for _, res := range r.Results {
		c := junitCase{Name: res.ID, Classname: "driftgate", Time: "0"}
		switch res.Outcome {
		case Drift:
			lines := driftLines(res, maxDrifts, 0)
			c.Failure = &junitFailure{
				Message: fmt.Sprintf("%s: %s", plural(len(res.Drifts), "drift"), strings.Join(lines, "; ")),
				Type:    "drift",
				Text:    strings.Join(lines, "\n"),
			}
			suite.Failures++
		case Skipped, Observed:
			c.Skipped = &junitSkipped{Message: res.Outcome + ": " + res.Note}
			suite.Skipped++
		}
		suite.Tests++
		suite.Cases = append(suite.Cases, c)
	}
	doc := junitSuites{Name: name, Tests: suite.Tests, Failures: suite.Failures, Skipped: suite.Skipped, Time: "0", Suites: []junitSuite{suite}}
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
