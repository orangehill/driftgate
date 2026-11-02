package cli

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/routes"
)

// reportCmd writes the last comparison (kept by compare and run in the local
// store) in a format. It reports; the gate is compare or run, so it exits 0
// also when the comparison drifted: a CI step that renders several formats
// after run must not stop at the first. With --exit-code it exits with the
// verdict of the comparison it writes, as compare did.
func reportCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	format := fs.String("format", "text", "")
	maxDrifts := fs.Int("max-drifts", 20, "")
	exitCode := fs.Bool("exit-code", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("report")
	}
	if !report.ValidFormat(*format) {
		return 0, configErr("--format must be one of %s", strings.Join(report.Formats, ", "))
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	path, err := lastReport(p)
	if err != nil {
		return 0, failed(err)
	}
	r, err := report.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, configErr("no comparison yet: run driftgate compare or driftgate run first")
	}
	if err != nil {
		return 0, envErr(err)
	}
	if err := r.Write(out, *format, driftsShown(fs, *maxDrifts)); err != nil {
		return 0, err
	}
	if *exitCode {
		return verdict(r), nil
	}
	return ExitClean, nil
}

// locateGroups attaches to the groups of a report the routes their drifts
// touch, from routes.json: route, action and source file. Without an
// inventory the groups stay as they are.
func locateGroups(p *config.Project, cat *catalog.Catalog, r *report.Report) {
	if len(r.Groups) == 0 {
		return
	}
	inv, err := routes.LoadInventory(p.Path("routes.json"))
	if err != nil || len(inv.Routes) == 0 {
		return
	}
	for i := range inv.Routes {
		inv.Routes[i].Target = p.Canonical(inv.Routes[i].Target)
	}
	byID := map[string]catalog.Request{}
	for _, req := range cat.Requests {
		byID[req.ID] = req
	}
	r.Locate(func(id string) *report.Location {
		req, ok := byID[id]
		if !ok {
			return nil
		}
		route := routes.Match(inv, req)
		if route == nil {
			return nil
		}
		label := req.Method + " " + route.Path
		if route.Target != "" {
			label = route.Target + ": " + label
		}
		return &report.Location{Route: label, Action: route.Action, File: route.File}
	})
}
