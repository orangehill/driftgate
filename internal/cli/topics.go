package cli

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
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

// docsBase is where the documentation is read: the pages of the git ref
// docsRef picks for this build.
func docsBase() string {
	module := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		module = info.Main.Version
	}
	return "https://github.com/orangehill/driftgate/blob/" + docsRef(Version, module) + "/docs/"
}

// docsRef is the git ref whose pages a build links, from its Version and
// the module version Go recorded: a release tag in either (a release, or
// go install …@v1.2.3) links that tag, so a link names a section the
// release has; any other build of a commit (dev-…, or dev from a checkout,
// whose module version is "(devel)" or a pseudo-version Go stamped from
// the repository) links development, where the pages of unreleased
// commands are first; any other build (source, a release candidate)
// links main.
func docsRef(version, module string) string {
	switch {
	case releaseTag().MatchString(version):
		return version
	case releaseTag().MatchString(module):
		return module
	case strings.HasPrefix(version, "dev-"), version == "dev":
		return "development"
	}
	return "main"
}

// tasksDoc is the page of what DriftGate can do: every feature, when to
// use it, its guide and its reference, under docsBase.
const tasksDoc = "capabilities.md"

var topics = []topic{
	{name: "catalog", summary: "catalog.json (beside driftgate.yaml: .driftgate/catalog.json, or driftgate/catalog.json with baselines: git) lists the requests every capture sends; generate writes it, entries may be written by hand. Its keys:",
		doc: "reference/catalog-json.md#request-fields", fields: [][2]string{
			{"version", "1"},
			{"requests", "the requests, in order; each has the keys below"},
			{"id", "the request's name, unique; reports, --only and globs use it"},
			{"target", "the service, with several services"},
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
		doc: "reference/allowlist-yaml.md", fields: [][2]string{
			{"version", "1"},
			{"headers:", "a header's value may change: name, and format or ignore_case"},
			{"body:", "a value in a body may change: path, format, parts; any_length for arrays"},
			{"arrays:", "a JSON array's items: path; key pairs them by a member, any_order lets them come in another order"},
			{"request, target", "on every rule: only these requests (a glob), only this service"},
			{"reason", "on every rule, required"},
		}},
	{name: "exclusions", summary: "exclusions.yaml (beside driftgate.yaml) names routes that are deliberately not checked, and requests that fail in the baseline, each with a reason. Its keys:",
		doc: "reference/exclusions-yaml.md", fields: [][2]string{
			{"version", "1"},
			{"routes:", "routes not checked: generate makes no requests for them, coverage counts them as excluded"},
			{"route", "of routes: a METHOD /path glob; without a method it matches every method"},
			{"known_failures:", "requests that fail in the baseline: kept, compared, listed instead of failing coverage"},
			{"request", "of known_failures: a request ID or glob"},
			{"status", "of known_failures: the failure acknowledged (500, 5xx, error, timeout); without it, any failure"},
			{"ref", "of known_failures: a ticket or link"},
			{"target", "of routes: only routes of this service"},
			{"reason", "on every entry, required"},
		}},
	{name: "classify", summary: "classify.yaml (beside driftgate.yaml) holds a rule for every column of the data module's source database; data classify proposes them. Its keys:",
		doc: "reference/classify-yaml.md", fields: [][2]string{
			{"version", "1"},
			{"tables:", "table name → its rule"},
			{"action", "of a table: copy (default), empty or skip; of a serialized-php column: empty-table (default: the rows are left out) or null (the column is NULL)"},
			{"columns", "of a table: column name → its rule, one word (keep, file, a strategy) or a mapping"},
			{"kind", "of a column: keep, personal, serialized-php or file"},
			{"strategy", "of a personal column: universal, email, password, token, free-text, html, json, null, empty"},
			{"paths, other", "of a json column: path glob → strategy or keep; strings under no path"},
			{"kinds", "of a schema entry: the kinds of personal data confirmed there"},
			{"schema:", "schema object → the personal data confirmed in it"},
			{"subset:", "the rows data subset takes: nullable, tables, references"},
			{"reason, proposed", "on tables, columns and schema entries; proposed until a person checks the rule"},
		}},
	{name: "config", summary: "driftgate.yaml (.driftgate/driftgate.yaml, or driftgate.yaml at the root with baselines: git) configures the project. Its top-level keys:",
		doc: "reference/driftgate-yaml.md#top-level-keys", fields: [][2]string{
			{"version, project", "1, and the project's name"},
			{"mode, level", "freeze or evolve; how far a comparison goes (status, shape, types, values)"},
			{"baselines, store, key", "the layout (local or git), the local store, the project key"},
			{"target, targets", "the service, or several services by name"},
			{"identities, tokens", "users and where their tokens come from"},
			{"state", "what is restored before captures: databases, or commands"},
			{"seed, prepare, guards", "load the data; run after every restore; checks before requests that change data"},
			{"discover", "the adapters that find the routes"},
			{"templates, generate, params", "the request variants per route, the identity, the parameter values"},
			{"storage, pii", "public or private bodies; values the personal-data detector lets through"},
			{"observe", "generated requests measured, never compared"},
			{"requests", "whether generated requests change data, and their bodies"},
			{"uploads", "the folder of real files that requests upload"},
			{"bisect", "the hooks of driftgate bisect"},
			{"data", "the data module"},
		}},
	{name: "kinds", summary: "Every drift has a kind: the first word of a drift line, and \"kind\" in JSON. The names are stable. The kinds:",
		doc: "reference/drift-kinds.md", fields: kindFields()},
}

// kindFields are the drift kinds of compare.Kinds(), one line each.
func kindFields() [][2]string {
	all := compare.Kinds()
	out := make([][2]string, len(all))
	for i, k := range all {
		out[i] = [2]string{k.Name, k.Text}
	}
	return out
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
