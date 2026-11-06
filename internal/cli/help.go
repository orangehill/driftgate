package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// The help of every command is described once, here: driftgate help lists
// the synopses, driftgate help <command> and <command> --help show one
// command with its flags, and a usage error names the synopsis. A test
// checks that every flag a command parses is described and the other way
// round.

// flagHelp describes one flag.
type flagHelp struct {
	name     string // without dashes
	arg      string // the value, e.g. "<glob>"; empty for a switch
	text     string
	required bool // shown without brackets in the synopsis
}

// commandHelp describes one command.
type commandHelp struct {
	name    string // "compare", "data verify"
	args    string // positional arguments
	summary string // what it does, a sentence or two
	doc     string // its section of the documentation, under docsBase
	flags   []flagHelp
	local   bool // no configuration: no --config
}

// allFlags are the flags of a command with the common --config.
func (c *commandHelp) allFlags() []flagHelp {
	if c.local {
		return c.flags
	}
	return append(append([]flagHelp{}, c.flags...), configFlag)
}

const (
	targetText = "work on this service only (repeatable or comma-separated; targets: in driftgate.yaml)"
	formats    = "text|json|junit|markdown"
	// The default view of a request whose status changed is a summary.
	maxDriftsText = "list up to N drifts per request, every one (default 20; of a request whose status changed, the change, its location and content type and one line for the rest)"
)

var configFlag = flagHelp{name: "config", arg: "<path>", text: "configuration file (default: .driftgate/driftgate.yaml or driftgate.yaml here or in the nearest folder above)"}

var commands = []commandHelp{
	{name: "init", doc: "reference/commands/project.md#driftgate-init", summary: "writes driftgate.yaml, a catalog and an allowlist for the project in this folder, from the frameworks it finds", flags: []flagHelp{
		{name: "project", arg: "<name>", text: "project name (default: the folder name)"},
		{name: "baselines", arg: "local|git", text: "local (default): everything in .driftgate/, which git ignores; git: driftgate.yaml and driftgate/ in the repository, the store in ~/.driftgate"},
		{name: "service", arg: "<name>", text: "write only this service (repeatable or comma-separated)"},
	}},
	{name: "setup", doc: "reference/commands/project.md#driftgate-setup", summary: "sets up a new project from zero to its first baseline: framework, database and address from the project's own settings, the key, routes, the pristine copy (asked first) and the baseline; a second run goes on where it stopped", flags: []flagHelp{
		{name: "yes", text: "ask nothing: take the best database setting and make the pristine copy; stop with an instruction where a value would have to be guessed"},
		{name: "project", arg: "<name>", text: "project name (default: the folder name)"},
		{name: "baselines", arg: "local|git", text: "local (default): everything in .driftgate/, which git ignores; git: driftgate.yaml and driftgate/ in the repository"},
		{name: "driver", arg: "mysql|postgres|sqlite|sqlserver", text: "the database server, when the settings do not say"},
		{name: "database", arg: "<name>", text: "the database the application uses (SQLite: its file, relative to the project root), when the settings do not say"},
		{name: "url", arg: "<address>", text: "where the application answers (default: from its settings)"},
	}},
	{name: "seed", doc: "reference/commands/state.md#driftgate-seed", summary: "runs the seed: commands and keeps a pristine copy of every state database", flags: []flagHelp{
		{name: "skip-commands", text: "take the data already loaded as pristine, without the seed: commands (a database cache in CI)"},
	}},
	{name: "clean", doc: "reference/commands/state.md#driftgate-clean", args: "[<component>…]", summary: "removes the pristine copies driftgate seed made (of every database component under state:, or of those named) and their records in the local store; never a working database, nor a copy seed did not make", flags: []flagHelp{
		{name: "yes", text: "remove without asking (needed without a terminal)"},
	}},
	{name: "run", doc: "reference/commands/check.md#driftgate-run", summary: "routes, catalog, capture and compare in one command; the first run makes the baseline from two equal captures", flags: []flagHelp{
		{name: "baseline", arg: "<name>", text: "baseline to make or compare with (default local)"},
		{name: "allow-mutating-without-restore", text: "send requests that change data even without a state restore"},
		{name: "skip-private", text: "accept that private bodies not on this machine are not compared (their status, headers and side effects are); without it the result is incomplete (exit 3)"},
		{name: "read-only", text: "capture only the requests that do not change data, as capture --read-only"},
		{name: "target", arg: "<service>", text: targetText + "; the check is then reported as partial"},
	}},
	{name: "discover", doc: "reference/commands/catalog.md#driftgate-discover", summary: "finds the routes of the application and compares them with the route inventory (exit 1 when they changed)", flags: []flagHelp{
		{name: "write", text: "write the routes found as the new inventory"},
		{name: "target", arg: "<service>", text: targetText},
	}},
	{name: "generate", doc: "reference/commands/catalog.md#driftgate-generate", summary: "generates catalog requests for routes the hand-written and imported ones do not cover", flags: []flagHelp{
		{name: "write", text: "write the generated catalog (not with --check)"},
		{name: "check", text: "exit 1 when the catalog is not what generate would write (not with --write)"},
		{name: "all", text: "generate requests for every route, also the covered ones"},
		{name: "target", arg: "<service>", text: targetText},
	}},
	{name: "coverage", doc: "reference/commands/catalog.md#driftgate-coverage", summary: "routes of the inventory without a catalog request that succeeds (exit 1 when there are any)", flags: []flagHelp{
		{name: "format", arg: "text|json", text: "output format (default text)"},
		{name: "baseline", arg: "<name>", text: "baseline whose answers decide which requests succeed (default local, when it exists; none: the catalog alone)"},
		{name: "target", arg: "<service>", text: targetText},
	}},
	{name: "har import", doc: "reference/commands/catalog.md#driftgate-har-import", args: "<file.har>", summary: "adds the requests of a HAR file (recorded in a browser) to the catalog", flags: []flagHelp{
		{name: "target", arg: "<service>", text: "service the requests belong to (once; needed with several services)"},
		{name: "write", text: "write the catalog; without it the requests are only listed"},
	}},
	{name: "record", doc: "reference/commands/catalog.md#driftgate-record", summary: "a recording proxy on 127.0.0.1: use the application through it (a browser, a frontend, an E2E suite) and it proposes catalog requests for what it saw; responses are never kept", flags: []flagHelp{
		{name: "listen", arg: "<addr>", text: "address to listen on (default " + defaultListen + "; loopback only)"},
		{name: "target", arg: "<service>", text: "service to forward to (once; needed with several services)"},
		{name: "write", text: "on stop, add the new requests to the catalog; without it they are only listed"},
		{name: "include", arg: "<glob>", text: "record only paths that match (repeatable or comma-separated)"},
		{name: "exclude", arg: "<glob>", text: "do not record paths that match, besides static files and dev-server noise (repeatable or comma-separated)"},
		{name: "allow-non-loopback", text: "allow a --listen address other than loopback (anyone who reaches it acts as you)"},
	}},
	{name: "capture", doc: "reference/commands/check.md#driftgate-capture", args: "<tag>", summary: "restores the state, sends the catalog and stores the responses under a tag", flags: []flagHelp{
		{name: "only", arg: "<glob>", text: "only requests whose ID matches"},
		{name: "force", text: "replace an existing capture with this tag"},
		{name: "restore", arg: "once|each", text: "restore the state once (default), or also before every sequence"},
		{name: "repeat", arg: "N", text: "capture N times on the same state and propose allowlist entries for values that differ (default 1); the capture keeps the median response time of each request"},
		{name: "baseline", arg: "<name>", text: "the state must match the one this baseline was captured from (default local)"},
		{name: "allow-remote", text: "allow a service outside allow_hosts"},
		{name: "allow-remote-mutations", text: "with --allow-remote, also send requests that change data or are repeated to it"},
		{name: "allow-mutating-without-restore", text: "send requests that change data even without a state restore"},
		{name: "skip-fingerprint-check", text: "do not check the state fingerprint (diagnostics only; the capture is marked unreliable)"},
		{name: "calibrate", text: "decide concurrency: auto: measure, compare a capture at the chosen setting with a sequential one, record it in the baseline (implies --repeat 2)"},
		{name: "read-only", text: "send only the requests that do not change data (GET, HEAD, OPTIONS and mutates: false); a comparison skips the others"},
		{name: "quiet", text: "no progress output"},
		{name: "target", arg: "<service>", text: targetText},
	}},
	{name: "compare", doc: "reference/commands/check.md#driftgate-compare", args: "<tag>", summary: "compares a capture with a baseline or another capture (exit 1 on drift)", flags: []flagHelp{
		{name: "against", arg: "<baseline|tag>", text: "baseline name or capture tag to compare with (default local)"},
		{name: "level", arg: "status|shape|types|values", text: "level for this comparison; only stricter than the configured one"},
		{name: "only", arg: "<glob>", text: "only requests whose ID matches"},
		{name: "format", arg: formats, text: "output format (default text)"},
		{name: "max-drifts", arg: "N", text: maxDriftsText},
		{name: "skip-fingerprint-check", text: "compare captures made with the same flag"},
		{name: "show-private", text: "show values from private bodies, on your own machine only (never kept)"},
		{name: "skip-private", text: "accept that private bodies not on this machine are not compared (their status, headers and side effects are); without it the result is incomplete (exit 3)"},
		{name: "target", arg: "<service>", text: targetText},
	}},
	{name: "report", doc: "reference/commands/check.md#driftgate-report", summary: "the last comparison again, e.g. as a pull-request comment; exits 0 also after drift (compare and run are the gate), unless --exit-code", flags: []flagHelp{
		{name: "format", arg: formats, text: "output format (default text)"},
		{name: "max-drifts", arg: "N", text: maxDriftsText},
		{name: "exit-code", text: "exit with the verdict of the comparison, as compare did: 1 on drift, 3 when incomplete, 0 clean"},
	}},
	{name: "allowlist check", doc: "reference/commands/review.md#driftgate-allowlist-check", summary: "checks the allowlist without capturing: rules for no request, rules that reach nothing in the baseline, any_length over values, $raw for every request, broad and shadowed rules (exit 1 on findings)", flags: []flagHelp{
		{name: "baseline", arg: "<name>", text: "baseline whose responses the rules are checked against (default local; without it, only the catalog)"},
		{name: "format", arg: "text|json", text: "output format (default text)"},
	}},
	{name: "review", doc: "reference/commands/review.md#driftgate-review", summary: "drift groups of the last comparison, with the accept command for each", flags: []flagHelp{
		{name: "group", arg: "N", text: "show this group with every request it touches"},
		{name: "show-private", text: "show values from private bodies, on your own machine only (never kept)"},
	}},
	{name: "accept", doc: "reference/commands/review.md#driftgate-accept", args: "<request|glob|group:N>...", summary: "takes drifts of the last comparison into its baseline (mode evolve), with a reason in the acceptance log", flags: []flagHelp{
		{name: "reason", arg: `"…"`, text: "why the change is right", required: true},
		{name: "ref", arg: "<ticket>", text: "ticket or link kept with the acceptance"},
		{name: "comparison", arg: "<id>", text: "the comparison review showed; accept refuses a newer one"},
	}},
	{name: "expect", doc: "reference/commands/review.md#driftgate-expect", args: "<request|glob|group:N>...", summary: "records drifts of the last comparison that a change intends, by ticket, without changing the baseline (also in mode freeze): compare then passes a request every drift of which is exactly a recorded one, and fails on any other drift", flags: []flagHelp{
		{name: "ref", arg: "<ticket>", text: "ticket or link of the change that intends the drifts", required: true},
		{name: "reason", arg: `"…"`, text: "why the change makes these drifts", required: true},
		{name: "comparison", arg: "<id>", text: "the comparison review showed; expect refuses a newer one"},
		{name: "drop", arg: "<ref>", text: "remove the expectations of this ref instead (no requests; with --reason)"},
		{name: "import", arg: "<file>", text: "take a ref's expectations file from another checkout (a worktree) instead, after checking it"},
		{name: "baseline", arg: "<name>", text: "with --drop or --import: the baseline (default: the last comparison's, else local)"},
	}},
	{name: "verify", doc: "reference/commands/review.md#driftgate-verify", summary: "checks baselines against their manifest and acceptance log (exit 1 when they disagree)", flags: []flagHelp{
		{name: "baseline", arg: "<name>", text: "check this baseline only (default: all)"},
		{name: "adopt", text: "write the manifest of a baseline made before manifests (once)"},
		{name: "migrate-private", text: "keep the personal data of an older baseline out of git: private bodies named by a keyed hash and moved into the local store, public bodies a capture would store private stored private, header values masked (once, with the key)"},
		{name: "migrate-state", text: "check that the data is the one a baseline with an older state fingerprint format was captured from, and record its fingerprint in the current format (once, after driftgate seed)"},
	}},
	{name: "bisect", doc: "reference/commands/review.md#driftgate-bisect", args: "<good> <bad>", summary: "first commit of each drift group of the last comparison (bisect: in driftgate.yaml)", flags: []flagHelp{
		{name: "group", arg: "N", text: "chase this drift group only (repeatable)"},
		{name: "only", arg: "<glob>", text: "chase these requests; without a comparison yet, the requests to chase"},
		{name: "baseline", arg: "<name>", text: "baseline to compare with when there is no comparison yet (default local)"},
		{name: "format", arg: "text|json", text: "output format (default text)"},
		{name: "expected", text: "chase the drifts expectations name too (driftgate expect); without it they are left out, as accepted ones are"},
	}},
	{name: "data classify", doc: "reference/commands/data.md#driftgate-data-classify", summary: "proposes rules for the columns classify.yaml does not cover", flags: []flagHelp{
		{name: "write", text: "add the proposed rules to classify.yaml"},
		{name: "sample", arg: "N", text: "rows sampled per table (default 200)"},
	}},
	{name: "data subset", doc: "reference/commands/data.md#driftgate-data-subset", summary: "data anonymize of a sample with closed references: the rows subset: in classify.yaml chooses, and every row they reference", flags: dataAnonymizeFlags},
	{name: "data anonymize", doc: "reference/commands/data.md#driftgate-data-anonymize", summary: "copies data: (source) into data: (target) and/or a dump, with personal values replaced by classify.yaml; the output is made only if data verify finds nothing in it", flags: dataAnonymizeFlags},
	{name: "data verify", doc: "reference/commands/data.md#driftgate-data-verify", summary: "checks an output of anonymize against the source: the detector over every value, and every original personal value looked for", flags: []flagHelp{
		{name: "target", arg: "<database>", text: "database to check (default data.target)"},
		{name: "dump", arg: "<file>", text: "dump to check"},
		{name: "format", arg: "text|json", text: "output format (default text)"},
	}},
	{name: "baselines", doc: "reference/commands/project.md#driftgate-baselines", args: "local|git", summary: "moves the project to baselines: local (everything in .driftgate/, which git ignores) or git (driftgate.yaml and driftgate/ in the repository, the store in ~/.driftgate); a switch to git first checks the baselines as verify does"},
	{name: "baselines rebase", doc: "reference/commands/review.md#driftgate-baselines-rebase", args: "<name>", summary: "makes a capture the content of a baseline (after a merge), with the checks of a baseline made by run and one line in the acceptance log; closes the expectations the new baseline holds", flags: []flagHelp{
		{name: "from", arg: "<tag>", text: "the capture that becomes the baseline's content (all of the catalog)", required: true},
		{name: "reason", arg: `"…"`, text: "why the baseline takes the capture", required: true},
		{name: "ref", arg: "<ticket>", text: "ticket or link kept with the log line"},
		{name: "allow-freeze", text: "rebase in mode freeze too (for changes reviewed elsewhere, such as a merge)"},
	}},
	{name: "key create", doc: "reference/commands/project.md#driftgate-key-create", summary: "creates the project key: .driftgate/key with baselines: local, the macOS Keychain with baselines: git"},
	{name: "version", doc: "reference/commands/project.md#driftgate-version", summary: "prints the version", local: true},
}

var dataAnonymizeFlags = []flagHelp{
	{name: "dump", arg: "<file>", text: "also write a dump to this file"},
	{name: "no-target", text: "write only the dump (needs --dump)"},
	{name: "replace", text: "drop an existing target database and write it again"},
	{name: "format", arg: "text|json", text: "output format (default text)"},
}

const helpIntro = "driftgate — a regression gate for HTTP APIs\n\nUsage:\n"

const helpOutro = `
Several services (targets: in driftgate.yaml): every command checks all of
them; --target <service> works on the named ones only, and run says so.

Common flags:
  --config <path>   configuration file (default: .driftgate/driftgate.yaml or driftgate.yaml here or in the nearest folder above)

driftgate help <command> (or <command> --help) describes a command and its flags;
driftgate help catalog and help allowlist the fields of those files;
driftgate help kinds the kinds of drift.

Exit codes: 0 clean, 1 drift (also: changed routes, uncovered routes, a stale
catalog with --check, findings of allowlist check), 2 configuration error, 3 environment error or an incomplete
result (an interrupted capture; private requests whose body is not on this
machine, without --skip-private).
`

func lookupCommand(name string) *commandHelp {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// group lists the commands under a name: the command itself, or every
// command of a group such as "data".
func group(name string) []*commandHelp {
	if c := lookupCommand(name); c != nil {
		return []*commandHelp{c}
	}
	var out []*commandHelp
	for i := range commands {
		if strings.HasPrefix(commands[i].name, name+" ") {
			out = append(out, &commands[i])
		}
	}
	return out
}

// synopsis is the command line of a command, without the common --config,
// in parts that are not broken across lines.
func (c *commandHelp) synopsis() []string {
	parts := []string{"driftgate", c.name}
	if c.args != "" {
		parts = append(parts, c.args)
	}
	for _, f := range c.flags {
		s := "--" + f.name
		if f.arg != "" {
			s += " " + f.arg
		}
		if !f.required {
			s = "[" + s + "]"
		}
		parts = append(parts, s)
	}
	return parts
}

// wrap breaks words into lines of at most width: the first after first, the
// others after rest.
func wrap(words []string, first, rest string, width int) string {
	var b strings.Builder
	line, empty := first, true
	for _, w := range words {
		switch {
		case empty:
		case len(line)+1+len(w) > width:
			b.WriteString(line + "\n")
			line = rest
		default:
			line += " "
		}
		line += w
		empty = false
	}
	b.WriteString(line + "\n")
	return b.String()
}

// writeUsage writes the synopsis of every command.
func writeUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString(helpIntro)
	for i := range commands {
		c := &commands[i]
		b.WriteString(wrap(c.synopsis(), "  ", "      ", 80))
		b.WriteString(wrap(strings.Fields(c.summary), "        ", "        ", 80))
	}
	b.WriteString(helpOutro)
	fmt.Fprint(w, b.String())
}

// writeHelp writes the help of a command, or of every command in a group.
func writeHelp(w io.Writer, cmds []*commandHelp) {
	var b strings.Builder
	for i, c := range cmds {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(wrap(c.synopsis(), "usage: ", "    ", 80))
		b.WriteString("\n" + wrap(strings.Fields(c.summary), "", "", 80))
		if flags := c.allFlags(); len(flags) > 0 {
			b.WriteString("\nFlags:\n")
			width := 0
			for _, f := range flags {
				width = max(width, len(f.label()))
			}
			for _, f := range flags {
				b.WriteString(wrap(strings.Fields(f.text), fmt.Sprintf("  %-*s  ", width, f.label()), strings.Repeat(" ", width+4), 80))
			}
		}
		b.WriteString("\nDocs: " + docsBase() + c.doc + "\n")
	}
	fmt.Fprint(w, b.String())
}

func (f flagHelp) label() string {
	if f.arg == "" {
		return "--" + f.name
	}
	return "--" + f.name + " " + f.arg
}

// helpCmd is driftgate help [<command>].
func helpCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		writeUsage(out)
		return nil
	}
	name := strings.Join(args, " ")
	cmds := group(name)
	if t := lookupTopic(name); t != nil {
		// A file's fields, then the commands of the same name.
		writeTopic(out, t)
		if len(cmds) > 0 {
			fmt.Fprintln(out)
			writeHelp(out, cmds)
		}
		return nil
	}
	if len(cmds) == 0 {
		return configErr("unknown command or topic %q (run 'driftgate help'; what to use for a task: %s)", name, docsBase()+tasksDoc)
	}
	writeHelp(out, cmds)
	return nil
}

// helpRequest is the error parse returns for -h or --help; Main writes the
// command's help to stdout and exits 0.
type helpRequest struct{ fs *flag.FlagSet }

func (h *helpRequest) Error() string { return "help requested" }

// usageErr is a configuration error with the short synopsis of a command
// (its arguments and required flags) and where the flags are described.
func usageErr(name string) error {
	c := lookupCommand(name)
	parts := []string{"driftgate", c.name}
	if c.args != "" {
		parts = append(parts, c.args)
	}
	optional := false
	for _, f := range c.flags {
		switch {
		case f.required:
			parts = append(parts, f.label())
		default:
			optional = true
		}
	}
	if optional {
		parts = append(parts, "[flags]")
	}
	return configErr("usage: %s (run 'driftgate help %s')", strings.Join(parts, " "), c.name)
}

// isHelp is -h or --help.
func isHelp(arg string) bool { return arg == "-h" || arg == "--help" || arg == "-help" }
