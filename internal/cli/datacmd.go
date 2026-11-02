package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// filePlaceholders makes the placeholders of file columns. It is nil until
// file placeholders are built; a file rule then stops data anonymize with a
// clear message before anything is written.
var filePlaceholders func(p *config.Project) (data.FilePlaceholder, error)

func dataCmd(args []string, out, errOut io.Writer) (int, error) {
	switch args[0] {
	case "classify":
		return dataClassifyCmd(args[1:], out)
	case "subset":
		return ExitClean, dataAnonymizeCmd("subset", args[1:], out, errOut)
	case "verify":
		return dataVerifyCmd(args[1:], out, errOut)
	case "anonymize":
		return ExitClean, dataAnonymizeCmd("anonymize", args[1:], out, errOut)
	}
	return 0, configErr("unknown data command %q (run 'driftgate help data')", args[0])
}

// dataProject loads the configuration, which must have a data section.
func dataProject(path string) (*config.Project, error) {
	p, err := config.Load(path)
	if err != nil {
		return nil, configErr("%v", err)
	}
	if p.Data.IsZero() {
		return nil, configErr("%s has no data: section (driver, source, target)", config.FileName)
	}
	return p, nil
}

func dataDatabase(ctx context.Context, p *config.Project, db config.DataDB, fallback config.Secret) (data.Database, error) {
	d := p.Data
	src := db.DSN
	if src.IsZero() {
		src = fallback
	}
	dsn, err := src.Read(ctx, p.Root, d.Driver)
	if err != nil {
		return data.Database{}, dsnErr(src, err, fmt.Errorf("data: dsn of %s: %v", db.Database, err))
	}
	return data.Database{Driver: d.Driver, DSN: dsn, Name: db.Database}, nil
}

func rulesPath(p *config.Project) string { return p.Path("classify.yaml") }

func piiDetector(p *config.Project) (*pii.Detector, error) {
	d, err := pii.New(p.PII.Allow)
	if err != nil {
		return nil, configErr("pii.allow: %v", err)
	}
	return d, nil
}

func dataClassifyCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("data classify", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	write := fs.Bool("write", false, "")
	sample := fs.Int("sample", 200, "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 || *sample < 0 {
		return 0, usageErr("data classify")
	}
	p, err := dataProject(*cfg)
	if err != nil {
		return 0, err
	}
	raw, err := os.ReadFile(rulesPath(p))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, configErr("%v", err)
	}
	rules, err := data.LoadRules(rulesPath(p))
	if err != nil {
		return 0, configErr("%v", err)
	}
	det, err := piiDetector(p)
	if err != nil {
		return 0, err
	}
	ctx, stop := signalContext(os.Stderr)
	defer stop()
	src, err := dataDatabase(ctx, p, p.Data.Source, config.Secret{})
	if err != nil {
		return 0, err
	}
	cls, err := data.Classify(ctx, src, rules, det, *sample)
	if err != nil {
		return 0, failed(fmt.Errorf("data classify: %w", err))
	}
	for _, s := range cls.Stale {
		fmt.Fprintf(out, "- %s: no longer in the database\n", s)
	}
	for _, tp := range cls.Tables {
		if tp.Action != "" {
			fmt.Fprintf(out, "+ %s: action %s (%s)\n", tp.Table, tp.Action, tp.Reason)
		}
		for _, cp := range tp.Columns {
			rule := cp.Rule.Kind
			if rule == data.KindPersonal {
				rule += "/" + cp.Rule.Strategy
			}
			fmt.Fprintf(out, "+ %s.%s: %s (%s)\n", tp.Table, cp.Column, rule, cp.Rule.Reason)
		}
	}
	for _, obj := range cls.StaleSchema {
		fmt.Fprintf(out, "- schema %s: nothing to confirm any more\n", obj)
	}
	for _, f := range cls.Schema {
		fmt.Fprintf(out, "+ schema %s: %s (%s)\n", f.Object, strings.Join(f.Kinds, ", "), f.Reason)
	}
	for _, u := range cls.Unconfirmed {
		fmt.Fprintf(out, "? %s: proposed, not confirmed yet\n", u)
	}
	if !cls.Changed() {
		fmt.Fprintf(out, "classify.yaml covers every column (%d proposals waiting for confirmation)\n", len(cls.Unconfirmed))
		return ExitClean, nil
	}
	if !*write {
		fmt.Fprintln(out, "classify.yaml is not up to date; run again with --write, then check every proposed rule")
		return ExitDrift, nil
	}
	merged, err := data.MergeRules(raw, cls)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if err := snapshot.WriteFile(rulesPath(p), merged, 0o644); err != nil {
		return 0, envErr(err)
	}
	fmt.Fprintf(out, "wrote %s: check each rule marked proposed: true, then remove the mark\n", rulesPath(p))
	return ExitClean, nil
}

// dataAnonymizeCmd runs data anonymize, or data subset: the same copy of a
// sample with closed references (subset: in classify.yaml). There is no
// subset without anonymization: it would copy personal data.
func dataAnonymizeCmd(name string, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("data "+name, flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	dump := fs.String("dump", "", "")
	noTarget := fs.Bool("no-target", false, "")
	replace := fs.Bool("replace", false, "")
	format := fs.String("format", "text", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || (*format != "text" && *format != "json") {
		return usageErr("data " + name)
	}
	if *noTarget && *dump == "" {
		return configErr("--no-target needs --dump")
	}
	p, err := dataProject(*cfg)
	if err != nil {
		return err
	}
	rules, err := data.LoadRules(rulesPath(p))
	if err != nil {
		return configErr("%v", err)
	}
	det, err := piiDetector(p)
	if err != nil {
		return err
	}
	key, err := p.LoadKey()
	if p.Data.Key != (secret.Source{}) {
		key, err = secret.Load(p.Project, p.Data.Key, "")
	}
	if err != nil {
		return err
	}
	// SIGTERM too (a CI job that is stopped): the staging database is dropped
	// on the way out.
	ctx, stop := signalContext(errOut)
	defer stop()
	src, err := dataDatabase(ctx, p, p.Data.Source, config.Secret{})
	if err != nil {
		return err
	}
	opt := data.Options{Source: src, Replace: *replace, Rules: rules, Key: key, PII: det, Log: errOut, Index: &data.VerifyIndex{}}
	if name == "subset" {
		if rules.Subset == nil {
			return configErr("%s has no subset: section: which rows to start from (tables: posts: { rows: 100 })", rulesPath(p))
		}
		opt.Filter = &data.Subset{}
	}
	if !*noTarget {
		dst, err := dataDatabase(ctx, p, p.Data.Target, p.Data.Source.DSN)
		if err != nil {
			return err
		}
		if err := allowTarget(ctx, p, &dst); err != nil {
			return err
		}
		opt.Target = &dst
	}
	if filePlaceholders != nil {
		if opt.Files, err = filePlaceholders(p); err != nil {
			return configErr("%v", err)
		}
	}
	// The dump is written next to its final name and renamed when complete,
	// so a failed run leaves no partial dump.
	var tmp *os.File
	if *dump != "" {
		if err := os.MkdirAll(filepath.Dir(*dump), 0o755); err != nil {
			return envErr(err)
		}
		if tmp, err = os.CreateTemp(filepath.Dir(*dump), "."+filepath.Base(*dump)+".*"); err != nil {
			return envErr(err)
		}
		defer os.Remove(tmp.Name())
		opt.Dump = tmp
	}
	// Nothing is made before both checks of data verify pass on it.
	var vrep *data.VerifyReport
	var verr error
	opt.Verify = func(ctx context.Context, staging *data.Database) error {
		dumpFile := ""
		if tmp != nil {
			dumpFile = tmp.Name()
		}
		vrep, verr = verifyOutput(ctx, opt, staging, dumpFile, out, *format)
		return verr
	}
	rep, err := data.Anonymize(ctx, opt)
	if tmp != nil {
		if cerr := tmp.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}
	switch {
	case verr != nil:
		return verr
	case errors.Is(err, data.ErrTargetExists):
		return configErr("%v; add --replace to drop it and write it again", err)
	case err != nil:
		return failed(fmt.Errorf("data %s: %w", name, err))
	}
	if opt.Files != nil {
		filesOut := out
		if *format == "json" {
			filesOut = errOut // the JSON report stays the only output
		}
		if err := writePlaceholders(ctx, opt.Files, filesOut); err != nil {
			return err
		}
	}
	if tmp != nil {
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			return envErr(err)
		}
		if err := os.Rename(tmp.Name(), *dump); err != nil {
			return envErr(err)
		}
	}
	if *format == "json" {
		b, err := json.MarshalIndent(struct {
			*data.Report
			Verify *data.VerifyReport `json:"verify"`
		}{rep, vrep}, "", " ")
		if err != nil {
			return err
		}
		_, err = out.Write(append(b, '\n'))
		return err
	}
	writeDataReport(out, rep, opt.Target, *dump)
	return writeVerifyReport(out, vrep, *format)
}

// allowTarget refuses a target outside the allowed hosts: it is dropped and
// written again, like a database under state:. Every host the driver may
// connect to with the DSN is checked (several hosts, fallbacks, PostgreSQL's
// host= parameter), and then every connection, on the address dialled
// (dst.Dial).
func allowTarget(ctx context.Context, p *config.Project, dst *data.Database) error {
	if p.Data.AllowRemote {
		return nil
	}
	dial, err := dbx.AllowDial(p.AllowHostLists(), "data.allow_remote: true")
	if err != nil {
		return configErr("data.target: %v", err)
	}
	dst.Dial = dial
	hosts, err := dst.Hosts()
	if err != nil {
		return configErr("data.target: %v", err)
	}
	for _, host := range hosts {
		if err := allowHost(ctx, p, host); err != nil {
			return err
		}
	}
	return nil
}

// allowHost checks one host of the target as a database under state: is
// checked (dbx.AllowHost): without any allow_hosts only the local system.
func allowHost(ctx context.Context, p *config.Project, host string) error {
	if err := dbx.AllowHost(ctx, p.AllowHostLists(), host, "data.allow_remote: true"); err != nil {
		return configErr("data.target: %v", err)
	}
	return nil
}

func writeDataReport(out io.Writer, rep *data.Report, target *data.Database, dump string) {
	for _, t := range rep.Tables {
		var cols []string
		for _, c := range t.Columns {
			if c.Rule == data.KindKeep {
				continue
			}
			s := fmt.Sprintf("%s %s %d", c.Name, c.Rule, c.Values)
			if c.Parts > 0 {
				s += fmt.Sprintf(" (%d parts)", c.Parts)
			}
			if c.Unknown > 0 {
				s += fmt.Sprintf(" (%d unknown hash format, made unusable)", c.Unknown)
			}
			cols = append(cols, s)
		}
		line := fmt.Sprintf("%-32s %-8s %8d rows", t.Name, t.Action, t.Rows)
		if t.Sample != nil {
			line += fmt.Sprintf(" of %d", t.Sample.Total)
			if t.Sample.Nulled > 0 {
				line += fmt.Sprintf(" (%d references nulled)", t.Sample.Nulled)
			}
			if t.Sample.Dangling > 0 {
				line += fmt.Sprintf(" (%d references to rows missing in the source)", t.Sample.Dangling)
			}
		}
		if len(cols) > 0 {
			line += "  " + strings.Join(cols, ", ")
		}
		fmt.Fprintln(out, line)
	}
	for _, l := range rep.LeftOut {
		fmt.Fprintf(out, "left out: %s (uses a skipped table)\n", l)
	}
	var to []string
	if target != nil {
		to = append(to, "database "+target.Name)
	}
	if dump != "" {
		to = append(to, "dump "+dump)
	}
	fmt.Fprintf(out, "%d rows in %.1f s to %s; pseudonymised with the project key (test password: %q)\n",
		rep.Rows, rep.Seconds, strings.Join(to, " and "), data.TestPassword)
}
