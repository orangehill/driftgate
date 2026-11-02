package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/secret"
)

// TestHelpDescribesEveryFlag: for every command, --help exits 0 with the
// help on stdout, every flag the command parses is in that help, and every
// flag the help describes is parsed.
func TestHelpDescribesEveryFlag(t *testing.T) {
	for i := range commands {
		c := &commands[i]
		args := append(strings.Fields(c.name), "--help")
		_, err := dispatch(args, io.Discard, io.Discard)
		var h *helpRequest
		if !errors.As(err, &h) {
			t.Errorf("%s: no help request: %v", c.name, err)
			continue
		}
		if h.fs.Name() != c.name {
			t.Errorf("%s: flag set named %q", c.name, h.fs.Name())
		}
		code, out, errOut := runCLI(t, args...)
		if code != ExitClean || errOut != "" || !strings.HasPrefix(out, "usage: driftgate "+c.name) {
			t.Errorf("%s --help: exit %d\n%s\n%s", c.name, code, out, errOut)
		}
		described := map[string]bool{}
		for _, f := range c.allFlags() {
			described[f.name] = true
		}
		parsed := map[string]bool{}
		h.fs.VisitAll(func(f *flag.Flag) {
			parsed[f.Name] = true
			if !described[f.Name] || !strings.Contains(out, "--"+f.Name+" ") && !strings.Contains(out, "--"+f.Name+"\n") {
				t.Errorf("%s: --%s is not in the help", c.name, f.Name)
			}
		})
		for name := range described {
			if !parsed[name] {
				t.Errorf("%s: the help describes --%s, which the command does not parse", c.name, name)
			}
		}
	}
}

func TestHelpCommands(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"help", "compare"}, {"help", "data"},
		{"help", "data", "verify"}, {"data", "--help"}, {"har", "-h"}, {"compare", "x", "-h"}} {
		code, out, errOut := runCLI(t, args...)
		if code != ExitClean || out == "" || errOut != "" {
			t.Errorf("%v: exit %d\n%s\n%s", args, code, out, errOut)
		}
	}
	_, all, _ := runCLI(t, "help")
	for _, c := range commands {
		if !strings.Contains(all, "driftgate "+c.name) {
			t.Errorf("driftgate help does not list %s", c.name)
		}
	}
	if _, out, _ := runCLI(t, "help", "data"); !strings.Contains(out, "data anonymize") || !strings.Contains(out, "data verify") {
		t.Errorf("help data:\n%s", out)
	}
	for _, args := range [][]string{{"help", "nope"}, {"version", "x"}, {"key", "create", "x"}, {"data"},
		{"har", "import", "x.har", "--target", "a", "--target", "b"}} {
		if code, _, errOut := runCLI(t, args...); code != ExitConfig {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if _, _, errOut := runCLI(t, "compare"); !strings.Contains(errOut, "usage: driftgate compare <tag> [flags]") || !strings.Contains(errOut, "driftgate help compare") {
		t.Errorf("usage error: %s", errOut)
	}
}

// TestClassify: an error is an environment problem unless it is marked as
// a configuration one: a full disk or a store that cannot be written is
// not a configuration error.
func TestClassify(t *testing.T) {
	t.Setenv(config.EnvStore, "")
	inProject, err := config.New(config.Config{Version: config.Version, Project: "p", Mode: config.Freeze, Level: "values", Store: "store"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, storeErr := inProject.StoreDir()
	if storeErr == nil {
		t.Fatal("store inside the project accepted")
	}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{errors.New("anything"), ExitEnv},
		{&fs.PathError{Op: "write", Path: "/store/x", Err: syscall.ENOSPC}, ExitEnv},
		{fmt.Errorf("copy: %w", fs.ErrPermission), ExitEnv},
		{usageErr("run"), ExitConfig},
		{storeErr, ExitConfig},
		{configErr("bad"), ExitConfig},
		{envErr(errors.New("down")), ExitEnv},
		{&exitError{ExitDrift, errors.New("found")}, ExitDrift},
		{secret.ErrNoKey, ExitEnv},
		{fmt.Errorf("x: %w", secret.ErrNoKey), ExitEnv},
		{fmt.Errorf("%w: no state", capture.ErrConfig), ExitConfig},
		{fmt.Errorf("%w: restore failed", capture.ErrEnv), ExitEnv},
		{fmt.Errorf("data classify: %w", data.ErrUnsupported), ExitConfig},
		{data.ErrNotTarget, ExitConfig},
		{data.ErrTargetExists, ExitConfig},
		{&data.CheckError{}, ExitConfig},
	} {
		if got := classify(tc.err); got != tc.code {
			t.Errorf("classify(%v) = %d, want %d", tc.err, got, tc.code)
		}
	}
	// failed: a known kind keeps its code, anything else is the environment.
	for _, tc := range []struct {
		err  error
		code int
	}{
		{errors.New("connection refused"), ExitEnv},
		{fmt.Errorf("%w: x", capture.ErrConfig), ExitConfig},
		{fmt.Errorf("data anonymize: %w", data.ErrUnsupported), ExitConfig},
		{secret.ErrNoKey, ExitEnv},
		{storeErr, ExitConfig},
	} {
		if got := classify(failed(tc.err)); got != tc.code {
			t.Errorf("classify(failed(%v)) = %d, want %d", tc.err, got, tc.code)
		}
	}
}
