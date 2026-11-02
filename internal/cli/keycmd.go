package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/orangehill/driftgate/internal/config"
)

func keyCreate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("key create", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("key create")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return configErr("%v", err)
	}
	where, err := p.CreateKey()
	if err != nil {
		return failed(err)
	}
	fmt.Fprintf(out, "created a key for %s in %s\n", p.Project, where)
	return nil
}
