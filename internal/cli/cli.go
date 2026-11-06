// Package cli implements the driftgate command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/secret"
)

// Build information, set with -ldflags at release time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Exit codes (docs/reference/cli.md#exit-codes).
const (
	ExitClean  = 0
	ExitDrift  = 1
	ExitConfig = 2
	ExitEnv    = 3
)

// exitError carries an exit code with its message.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func (e *exitError) Unwrap() error { return e.err }

func configErr(format string, a ...any) error {
	return &exitError{ExitConfig, fmt.Errorf(format, a...)}
}

func envErr(err error) error { return &exitError{ExitEnv, err} }

// classify maps an error to an exit code: an exitError carries its own; the
// error kinds of the packages below keep theirs (kind); everything else is
// an environment problem (a full disk, a store that cannot be written): a
// configuration error is marked where it is found.
func classify(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	if code, ok := kind(err); ok {
		return code
	}
	return ExitEnv
}

// kind is the exit code of an error kind the packages below return: a
// missing key or an environment failure of a capture is an environment
// problem; a configuration error of a capture, or what the data module
// does not support or refuses to touch, is a configuration problem.
func kind(err error) (int, bool) {
	var ce *data.CheckError
	switch {
	case errors.Is(err, capture.ErrConfig), errors.Is(err, config.ErrInvalid), errors.Is(err, data.ErrUnsupported), errors.Is(err, data.ErrNotTarget),
		errors.Is(err, data.ErrTargetExists), errors.As(err, &ce):
		return ExitConfig, true
	case errors.Is(err, capture.ErrEnv), errors.Is(err, secret.ErrNoKey):
		return ExitEnv, true
	}
	return 0, false
}

// failed is the error of a step that works with the environment (a capture,
// a seed, a database): an error of a known kind keeps its exit code, any
// other is an environment problem.
func failed(err error) error {
	if code, ok := kind(err); ok {
		return &exitError{code, err}
	}
	return envErr(err)
}

// Main runs the command line and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stderr)
		return ExitConfig
	}
	forgetVerified()
	defer capture.Wait()
	code, err := dispatch(args, stdout, stderr)
	var h *helpRequest
	if errors.As(err, &h) {
		writeHelp(stdout, group(h.fs.Name()))
		return ExitClean
	}
	if err != nil {
		fmt.Fprintln(stderr, "driftgate:", err)
		return classify(err)
	}
	return code
}

// groups are the commands with a subcommand.
var groups = map[string]string{"allowlist": "check", "data": "classify|subset|anonymize|verify", "har": "import", "key": "create"}

// dispatch runs the command named by args[0].
func dispatch(args []string, stdout, stderr io.Writer) (int, error) {
	if sub, ok := groups[args[0]]; ok {
		switch {
		case len(args) < 2:
			return 0, configErr("usage: driftgate %s %s … (run 'driftgate help %s')", args[0], sub, args[0])
		case isHelp(args[1]):
			writeHelp(stdout, group(args[0]))
			return ExitClean, nil
		}
	}
	var code int
	var err error
	switch args[0] {
	case "compare":
		code, err = compareCmd(args[1:], stdout)
	case "report":
		code, err = reportCmd(args[1:], stdout)
	case "capture":
		code, err = captureCmd(args[1:], stdout, stderr)
	case "seed":
		err = seedCmd(args[1:], stdout, stderr)
	case "clean":
		err = cleanCmd(args[1:], stdout, stderr)
	case "run":
		code, err = runCmd(args[1:], stdout, stderr)
	case "discover":
		code, err = discoverCmd(args[1:], stdout, stderr)
	case "generate":
		code, err = generateCmd(args[1:], stdout, stderr)
	case "coverage":
		code, err = coverageCmd(args[1:], stdout)
	case "review":
		code, err = reviewCmd(args[1:], stdout)
	case "accept":
		code, err = acceptCmd(args[1:], stdout)
	case "expect":
		code, err = expectCmd(args[1:], stdout)
	case "verify":
		code, err = verifyCmd(args[1:], stdout)
	case "bisect":
		code, err = bisectCmd(args[1:], stdout, stderr)
	case "data":
		code, err = dataCmd(args[1:], stdout, stderr)
	case "record":
		code, err = recordCmd(args[1:], stdout)
	case "allowlist":
		if args[1] != "check" {
			return 0, usageErr("allowlist check")
		}
		code, err = allowlistCheck(args[2:], stdout)
	case "har":
		if args[1] != "import" {
			return 0, usageErr("har import")
		}
		err = harCmd(args[2:], stdout)
	case "init":
		err = initCmd(args[1:], stdout)
	case "setup":
		code, err = setupCmd(args[1:], stdout, stderr)
	case "baselines":
		if len(args) > 1 && args[1] == "rebase" {
			code, err = rebaseCmd(args[2:], stdout)
			break
		}
		err = baselinesCmd(args[1:], stdout, stderr)
	case "key":
		if args[1] != "create" {
			return 0, usageErr("key create")
		}
		err = keyCreate(args[2:], stdout)
	case "version", "--version":
		err = versionCmd(args[1:], stdout)
	case "help", "-h", "--help":
		err = helpCmd(args[1:], stdout)
	default:
		err = configErr("unknown command %q (run 'driftgate help'; what to use for a task: %s)", args[0], docsBase()+tasksDoc)
	}
	return code, err
}

func versionCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("version")
	}
	commit, date := Commit, Date
	if info, ok := debug.ReadBuildInfo(); ok && commit == "unknown" {
		// A plain go build records the commit itself.
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
			case "vcs.time":
				date = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					commit += " (modified)"
				}
			}
		}
	}
	fmt.Fprintf(out, "driftgate %s (commit %s, built %s)\n", Version, commit, date)
	return nil
}

// parse reads flags that may appear before, between or after positional
// arguments, as in `driftgate compare l56 --against l55`.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, &helpRequest{fs}
			}
			return nil, configErr("%s: %v (run 'driftgate help %s')", fs.Name(), err, fs.Name())
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// Run is the entry point for main.
func Run() { os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr)) }
