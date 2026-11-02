package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/secret"
)

// dataKey is the key of the data module: data.key, or the project key.
func dataKey(p *config.Project) ([]byte, error) {
	if p.Data.Key != (secret.Source{}) {
		return secret.Load(p.Project, p.Data.Key, "")
	}
	return p.LoadKey()
}

// dataVerifyCmd checks an output of data anonymize against the source: the
// target database (data.target, or --target), a dump (--dump), or both.
func dataVerifyCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("data verify", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	target := fs.String("target", "", "")
	dump := fs.String("dump", "", "")
	format := fs.String("format", "text", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 || (*format != "text" && *format != "json") {
		return 0, usageErr("data verify")
	}
	p, err := dataProject(*cfg)
	if err != nil {
		return 0, err
	}
	rules, err := data.LoadRules(rulesPath(p))
	if err != nil {
		return 0, configErr("%v", err)
	}
	det, err := piiDetector(p)
	if err != nil {
		return 0, err
	}
	key, err := dataKey(p)
	if err != nil {
		return 0, err
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	src, err := dataDatabase(ctx, p, p.Data.Source, config.Secret{})
	if err != nil {
		return 0, err
	}
	opt := data.VerifyOptions{Source: src, Rules: rules, Key: key, PII: det, Log: errOut}
	if *target != "" || *dump == "" {
		dst, err := dataDatabase(ctx, p, p.Data.Target, p.Data.Source.DSN)
		if err != nil {
			return 0, err
		}
		if *target != "" {
			dst.Name = *target
		}
		opt.Target = &dst
	}
	if *dump != "" {
		f, err := os.Open(*dump)
		if err != nil {
			return 0, envErr(err)
		}
		defer f.Close()
		opt.Dump = f
	}
	rep, err := data.Verify(ctx, opt)
	if err != nil {
		return 0, verifyErr(err)
	}
	if err := writeVerifyReport(out, rep, *format); err != nil {
		return 0, err
	}
	if !rep.Clean() {
		return ExitDrift, nil
	}
	return ExitClean, nil
}

func verifyErr(err error) error { return failed(fmt.Errorf("data verify: %w", err)) }

// verifyOutput runs data verify on what anonymize (or subset) has just
// written, before it is made: the target database under its staging name
// and/or the dump in dumpFile (still under its temporary name). When either
// check finds something, the output is not made: the report is written and the error (exit 1) makes Anonymize drop
// the staging database, leaving the target as it was, and tells the caller
// to remove the dump instead of renaming it into place.
//
// The original values come from the index Anonymize made in its own pass
// over them (opt.Index): from the source, in the snapshot of the copy,
// without a second read.
func verifyOutput(ctx context.Context, opt data.Options, staging *data.Database, dumpFile string, out io.Writer, format string) (*data.VerifyReport, error) {
	vopt := data.VerifyOptions{Source: opt.Source, Target: staging, Rules: opt.Rules, Key: opt.Key, PII: opt.PII, Index: opt.Index, Log: opt.Log}
	if dumpFile != "" {
		f, err := os.Open(dumpFile)
		if err != nil {
			return nil, envErr(err)
		}
		defer f.Close()
		vopt.Dump = f
	}
	rep, err := data.Verify(ctx, vopt)
	if err != nil {
		return nil, verifyErr(fmt.Errorf("%w; the output is not made", err))
	}
	if staging != nil && opt.Target != nil {
		// Findings name the database the output is made as.
		for i := range rep.Findings {
			if rep.Findings[i].Output == "database "+staging.Name {
				rep.Findings[i].Output = "database " + opt.Target.Name
			}
		}
	}
	if rep.Clean() {
		return rep, nil
	}
	if err := writeVerifyReport(out, rep, format); err != nil {
		return nil, err
	}
	return nil, &exitError{ExitDrift, fmt.Errorf("data verify found personal data in the output (%d findings): the output is not made", len(rep.Findings))}
}

func writeVerifyReport(out io.Writer, rep *data.VerifyReport, format string) error {
	if format == "json" {
		b, err := json.MarshalIndent(rep, "", " ")
		if err != nil {
			return err
		}
		_, err = out.Write(append(b, '\n'))
		return err
	}
	for _, f := range rep.Findings {
		where := f.Table
		if f.Column != "" {
			where += "." + f.Column
		}
		what := f.Kind
		if f.Origin != "" {
			what += " from " + f.Origin
		}
		values := "values"
		if f.Values == 1 {
			values = "value"
		}
		line := fmt.Sprintf("%-8s %s: %s in %d %s", f.Check, where, what, f.Values, values)
		if len(f.Rows) > 0 {
			line += " (rows " + strings.Join(f.Rows, " ")
			if f.Values > int64(len(f.Rows)) {
				line += " …"
			}
			line += ")"
		}
		fmt.Fprintf(out, "%s [%s]\n", line, f.Output)
	}
	if rep.Clean() {
		fmt.Fprintf(out, "verified: no personal data found (%d values; %d original values looked for) in %.1f s\n", rep.Values, rep.Originals, rep.Seconds)
	} else {
		fmt.Fprintf(out, "%d findings (%d values; %d original values looked for) in %.1f s\n", len(rep.Findings), rep.Values, rep.Originals, rep.Seconds)
	}
	return nil
}
