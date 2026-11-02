package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/orangehill/driftgate/internal/config"
)

// baselinesCmd moves the project to baselines: local or git: its files,
// its store and its configuration. A switch to git first checks the
// baselines as verify does, since they go into the repository.
func baselinesCmd(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("baselines", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("baselines")
	}
	s, err := config.PrepareSwitch(*cfg, pos[0])
	if errors.Is(err, config.ErrSwitched) {
		fmt.Fprintf(out, "the project is baselines: %s already\n", pos[0])
		return nil
	}
	if err != nil {
		return configErr("%v", err)
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	enterGit := func(p *config.Project) error {
		names, err := baselineNames(p)
		if err != nil || len(names) == 0 {
			return err
		}
		code, err := verifyBaselines(p, names, out)
		if err != nil {
			return err
		}
		if code != ExitClean {
			return configErr("nothing was moved: the baselines would put into git what must stay out of it, or changed outside driftgate accept (above); fix that, then run driftgate baselines git again")
		}
		return nil
	}
	var e *exitError
	if err := s.Run(ctx, enterGit, out, errOut); errors.As(err, &e) {
		return err
	} else if err != nil {
		return failed(err)
	}
	return nil
}
