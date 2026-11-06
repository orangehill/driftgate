package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
)

// topic is the help of a file DriftGate reads, or of the names its reports
// use: one line each, and the section of the documentation to read, under
// docsBase (driftgate help <topic>).
type topic struct {
	name, summary, doc string
	fields             [][2]string // name, what it is
}

// releaseTag is the Version of a release build.
var releaseTag = lazy.Regexp(`^v\d+\.\d+\.\d+$`)

// docsBase is where the documentation is read: the pages of this build's
// release tag, so that a link names a section the release has; main for a
// build that is not a release.
func docsBase() string {
	ref := "main"
	if releaseTag().MatchString(Version) {
		ref = Version
	}
	return "https://github.com/orangehill/driftgate/blob/" + ref + "/docs/"
}

// tasksDoc is the table of what to use for a task, under docsBase.
const tasksDoc = "configuration.md#what-to-use-for-what"

var topics = []topic{
	{name: "catalog", summary: "catalog.json (beside driftgate.yaml: .driftgate/catalog.json, or driftgate/catalog.json with baselines: git) lists the requests every capture sends; generate writes it, entries may be written by hand. Its keys:",
		doc: "configuration.md#catalogjson", fields: [][2]string{
			{"version", "1"},
			{"requests", "the requests, in order; each has the keys below"},
			{"id", "the request's name, unique; reports, --only and globs use it"},
			{"target", "the service, with several targets"},
			{"method, path", "HTTP method and path, with {param} placeholders"},
			{"params", "values of the placeholders"},
			{"params_from", "a placeholder's value from an earlier response: {request, path}"},
			{"headers", "extra headers, [[name, value], …]"},
			{"body", "a JSON body"},
			{"identity", "who sends it (identities and tokens)"},
			{"mutates", "true or false: whether it changes data (default: by method)"},
			{"storage", "public or private: where its response body is kept"},
			{"level", "status, shape, types or values: how far it is compared"},
			{"observe", "measured, never compared"},
			{"multipart", "a multipart body, with fields and files"},
			{"fields", "of multipart: the form fields, [[name, value], …]"},
			{"files", "of multipart: field, and path (a fixture in the uploads folder) with its ref, or synthetic (a kind); name, type"},
			{"repeat", "send it N times and compare the statuses (throttling)"},
			{"command", "a command step instead of a request: argv, never a shell"},
			{"dir, stdin", "a command step's working folder and standard input file"},
			{"output, output_ignore", "compare a command step's output, without lines that match"},
			{"source", "where generate took the entry from"},
			{"sequences", "requests that run in order: id, requests (IDs), restore: before"},
		}},
	{name: "allowlist", summary: "allowlist.yaml (beside driftgate.yaml) names what may change between responses, each rule with a reason. Its keys:",
		doc: "configuration.md#allowlistyaml", fields: [][2]string{
			{"version", "1"},
			{"headers:", "a header's value may change: name, and format or ignore_case"},
			{"body:", "a value in a body may change: path, format, parts; any_length for arrays"},
			{"arrays:", "a JSON array's items: path; key pairs them by a member, any_order lets them come in another order"},
			{"request, target", "on every rule: only these requests (a glob), only this service"},
			{"reason", "on every rule, required"},
		}},
	{name: "kinds", summary: "Every drift has a kind: the first word of a drift line, and \"kind\" in JSON. The names are stable. The kinds:",
		doc: "reference/drift-kinds.md", fields: [][2]string{
			{"status", "the HTTP status changed, or the request got no answer"},
			{"header-missing", "a header of the baseline is gone"},
			{"header-extra", "a header is new"},
			{"header-value", "a header's value changed"},
			{"key-missing", "a JSON key is gone, or an item of an array paired by key"},
			{"key-extra", "a JSON key is new, or an item of an array paired by key"},
			{"key-order", "the same keys in another order"},
			{"array-length", "an array has more or fewer items"},
			{"item-order", "the same items of an array in another order"},
			{"type", "a value's type changed (JSON, or a spreadsheet cell)"},
			{"number-literal", "the same number written otherwise (1.0 for 1)"},
			{"value", "a JSON value changed"},
			{"format", "a value an allowlist rule covers no longer matches its format"},
			{"encoding", "the same values written with other bytes (layout, escapes, byte order mark, charset)"},
			{"body", "a body that is not JSON changed, or JSON became non-JSON"},
			{"element-missing", "an HTML element is gone"},
			{"element-extra", "an HTML element is new"},
			{"element-order", "the same HTML elements in another order"},
			{"attribute-missing", "an attribute of an HTML element is gone"},
			{"attribute-extra", "an attribute of an HTML element is new"},
			{"attribute-value", "an attribute's value changed"},
			{"text", "the text of an HTML element changed"},
			{"doctype", "the document type declaration came, went or changed"},
			{"cell", "a spreadsheet or CSV cell's value changed"},
			{"cell-format", "a cell's number format or style changed"},
			{"cell-formula", "a cell's formula changed"},
			{"sheet-missing", "a worksheet is gone, or was renamed"},
			{"sheet-extra", "a worksheet is new"},
			{"sheet-order", "the worksheets are in another order"},
			{"exit-code", "a command step ended with another exit code"},
			{"repeat", "the statuses of a repeated request changed"},
			{"side-effect", "the request changed other data than in the baseline"},
			{"not-captured", "the baseline has a response, the capture has none"},
		}},
}

func lookupTopic(name string) *topic {
	for i := range topics {
		if topics[i].name == name {
			return &topics[i]
		}
	}
	return nil
}

// writeTopic writes the help of a topic.
func writeTopic(w io.Writer, t *topic) {
	var b strings.Builder
	b.WriteString(wrap(strings.Fields(t.summary), "", "", 80) + "\n")
	width := 0
	for _, f := range t.fields {
		width = max(width, len(f[0]))
	}
	for _, f := range t.fields {
		b.WriteString(wrap(strings.Fields(f[1]), fmt.Sprintf("  %-*s  ", width, f[0]), strings.Repeat(" ", width+4), 80))
	}
	fmt.Fprintf(&b, "\nRead %s; what to use for a task: %s\n", docsBase()+t.doc, docsBase()+tasksDoc)
	fmt.Fprint(w, b.String())
}
