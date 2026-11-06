package capture

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/procx"
	"github.com/orangehill/driftgate/internal/state"
)

// runCommand runs a command of seed, prepare or guards without a shell:
// from its dir (default the project root), with its stdin file as
// standard input.
func runCommand(ctx context.Context, p *config.Project, c config.Command, env []string, log io.Writer) error {
	dir, stdin, err := p.CommandPaths(c)
	if err != nil {
		return err
	}
	in, closeIn, err := openStdin(stdin)
	if err != nil {
		return fmt.Errorf("%s: %w", procx.Line(c.Run), err)
	}
	defer closeIn()
	return state.RunIn(ctx, dir, in, c.Run, env, log)
}

// openStdin opens the stdin file of a command (CommandPaths): a nil
// reader without one, and the function that closes it.
func openStdin(path string) (io.Reader, func(), error) {
	if path == "" {
		return nil, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stdin: %w", err)
	}
	return f, func() { f.Close() }, nil
}

// checkAllCommands checks the commands a capture runs (checkCommands):
// prepare, the guards when requests change data, and the command steps.
func checkAllCommands(p *config.Project, readonly []catalog.Request, units []unit) error {
	if err := checkCommands(p, "prepare", p.Prepare); err != nil {
		return err
	}
	if len(units) > 0 {
		for _, g := range p.Guards {
			if err := checkCommands(p, fmt.Sprintf("guard %q", g.Name), []config.Command{g.Command}); err != nil {
				return err
			}
		}
	}
	for _, req := range append(append([]catalog.Request{}, readonly...), unitRequests(units)...) {
		if req.IsCommand() {
			if err := checkCommands(p, req.ID, []config.Command{stepCommand(req)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// stepCommand is the command of a command step.
func stepCommand(req catalog.Request) config.Command {
	return config.Command{Run: req.Command, Dir: req.Dir, Stdin: req.Stdin}
}

// checkCommands checks the folders and stdin files of commands before the
// first of them runs: one that is missing stops the command before
// anything was changed, not halfway through.
func checkCommands(p *config.Project, what string, cmds []config.Command) error {
	for _, c := range cmds {
		if _, _, err := p.CommandPaths(c); err != nil {
			return envErr("%s: %v", what, err)
		}
	}
	return nil
}
