package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/config"
)

// cleanInput is where clean reads its answer: the terminal, or nil when
// stdin is not one (setupInput).
var cleanInput = func() io.Reader { return setupInput() }

// cleanCmd removes what seed made: the pristine copies of the configured
// database components (or of the components named) and their records in
// the local store. It says what it removes and asks first, unless --yes;
// without a terminal to ask on it needs --yes.
func cleanCmd(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	yes := fs.Bool("yes", false, "")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return configErr("%v", err)
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	unlock, err := lockStore(ctx, p, errOut)
	if err != nil {
		return err
	}
	defer unlock()
	items, err := capture.CleanPlan(ctx, p, names)
	if err != nil {
		return failed(err)
	}
	if len(items) == 0 {
		return configErr("no database component under state: in %s; there is no pristine copy to remove", rel(p.Root, p.Layout.Config))
	}
	fmt.Fprintln(out, "clean removes the pristine copies driftgate seed made, and their records (the working databases stay):")
	for _, it := range items {
		fmt.Fprintf(out, "  %s\n", it.What())
	}
	if !*yes {
		in := cleanInput()
		if in == nil {
			return configErr("clean asks before it removes anything, and there is no terminal to ask on; pass --yes")
		}
		fmt.Fprint(out, "remove them? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "nothing removed")
			return nil
		}
	}
	if err := capture.Clean(ctx, p, items, out, errOut); err != nil {
		return failed(err)
	}
	fmt.Fprintln(out, "the next capture needs driftgate seed (run seeds when it finds no pristine copy)")
	return nil
}
