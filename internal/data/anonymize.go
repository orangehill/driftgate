package data

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/pii"
)

// DumpFormat is the version of the dump format; it changes when the same
// input would give different bytes.
const DumpFormat = 1

// FileValue is one value of a file column: the path the application stored.
type FileValue struct {
	Table, Column string
	Path          string // the original value of the column
}

// FilePlaceholder makes a placeholder for the file a file column names (same
// name, type and dimensions, neutral content) and returns the value the
// column gets in the output. Without one, a file rule stops the run before
// anything is written.
type FilePlaceholder interface {
	Placeholder(ctx context.Context, f FileValue) (string, error)
}

// Filter narrows the rows that are read (data subset). Both methods may
// keep everything: Where returns "" and Keep true.
type Filter interface {
	// Where is an SQL condition on the rows of a table, ANDed to the read.
	Where(t *Table) string
	// Keep decides about one row after the read; values as text, nil for NULL.
	// It may set values to nil (data subset nulls references that leave the
	// sample) before the row is replaced.
	Keep(t *Table, cols []*Column, row [][]byte) bool
}

// preparer is a Filter that reads the source before the copy, in the same
// snapshot, after the rules are checked and before anything is written
// (Subset chooses its sample).
type preparer interface {
	prepare(ctx context.Context, src *sql.Conn, dl dialect, s *Schema, r *Rules, log io.Writer) error
	report(t *Table) *SampleReport
}

// Options configure Anonymize.
type Options struct {
	Source Database // only read, in one read-only snapshot
	// Target is written under a staging name and takes its own name when
	// the run and Verify have passed; nil writes no database. It must be
	// another database than the source (told apart by the servers'
	// identities) and, when it holds objects, one DriftGate wrote.
	Target *Database
	// Replace allows dropping a target database DriftGate wrote that
	// already holds objects.
	Replace bool
	Dump    io.Writer // the deterministic SQL dump; nil writes none
	// Verify checks the output before it is made: the target database under
	// its staging name (nil without a target) and the dump, written and
	// flushed. An error leaves the target as it was and is returned as is.
	Verify func(ctx context.Context, staging *Database) error
	// Index, when not nil, is filled with what data verify looks for, in
	// the run's own pass over the original personal values of the source:
	// Verify with VerifyOptions.Index then reads the source no more.
	Index  *VerifyIndex
	Rules  *Rules
	Key    []byte // the project key; the replacement key is derived from it
	Files  FilePlaceholder
	PII    *pii.Detector // finds personal parts of free text; nil: the default detector
	Filter Filter
	// BatchRows is the most rows per INSERT (default 500); a statement also
	// ends after about 1 MiB.
	BatchRows int
	Log       io.Writer // progress, one line per table; never values
}

// Report says what a run did, in counts only: it never holds a value.
type Report struct {
	Driver  string        `json:"driver"`
	Tables  []TableReport `json:"tables"`
	Rows    int64         `json:"rows"`
	Seconds float64       `json:"seconds"`
	// LeftOut are views and other statements left out because they need a
	// table that is skipped.
	LeftOut []string `json:"left_out,omitempty"`
}

// TableReport is one table of the report.
type TableReport struct {
	Name    string         `json:"name"`
	Action  string         `json:"action"` // copy, empty, skip, or emptied (a serialized PHP column)
	Rows    int64          `json:"rows"`
	Columns []ColumnReport `json:"columns,omitempty"`
	Sample  *SampleReport  `json:"sample,omitempty"` // data subset
}

// ColumnReport counts the replacements in one column.
type ColumnReport struct {
	Name    string `json:"name"`
	Rule    string `json:"rule"`                     // keep, file, serialized-php or the strategy
	Values  int64  `json:"values"`                   // values that are not NULL
	Parts   int64  `json:"parts,omitempty"`          // personal parts replaced in free text, HTML or JSON
	Unknown int64  `json:"unknown_format,omitempty"` // password hashes of no known format, made unusable
}

// plan is how one column of a copied table is written.
type plan struct {
	col    *Column
	rule   *ColumnRule
	fn     replaceFn // whole-value strategies
	json   *jsonRule
	needle bool // its value is looked for in free text of the same row
	words  bool // word by word, too
	fold   bool
	report *ColumnReport
}

func (p *plan) label() string {
	switch p.rule.Kind {
	case KindPersonal:
		return p.rule.Strategy
	case KindSerializedPHP:
		return KindSerializedPHP + "/" + p.rule.Action
	}
	return p.rule.Kind
}

// engine holds what a run shares between tables.
type engine struct {
	dl    dialect
	r     *Replacer
	text  *textReplacer
	files FilePlaceholder
}

func newEngine(driver string, key []byte, det *pii.Detector, files FilePlaceholder) (*engine, error) {
	dl, err := dialectFor(driver)
	if err != nil {
		return nil, err
	}
	r, err := NewReplacer(key)
	if err != nil {
		return nil, err
	}
	if det == nil {
		if det, err = pii.New(nil); err != nil {
			return nil, err
		}
	}
	return &engine{dl: dl, r: r, text: &textReplacer{r: r, pii: det}, files: files}, nil
}

func (e *engine) plans(t *Table, tr *TableRule) []*plan {
	var out []*plan
	for _, c := range t.Columns {
		if c.Generated {
			continue
		}
		cr := tr.Columns[c.Name]
		p := &plan{col: c, rule: cr}
		p.report = &ColumnReport{Name: c.Name, Rule: p.label()}
		if cr.Kind == KindPersonal {
			switch cr.Strategy {
			case StrategyUniversal:
				p.fn, p.needle, p.words, p.fold = e.r.ForColumn(c, cr.Strategy), true, true, true
			case StrategyEmail:
				p.fn, p.needle, p.fold = e.r.ForColumn(c, cr.Strategy), true, true
			case StrategyToken:
				p.fn, p.needle = e.r.ForColumn(c, cr.Strategy), true
			case StrategyJSON:
				p.json = newJSONRule(cr)
			}
		}
		out = append(out, p)
	}
	return out
}

// text reports whether the column is free text or HTML, whose spans can
// be found ahead.
func (p *plan) text() bool {
	return p.rule.Kind == KindPersonal && (p.rule.Strategy == StrategyFreeText || p.rule.Strategy == StrategyHTML)
}

// needles are the personal values of a row its free text is searched for.
func (e *engine) needles(plans []*plan, vals [][]byte) []needle {
	var needles []needle
	for i, p := range plans {
		if !p.needle || vals[i] == nil || !(p.col.Category == CatText || p.col.Category == CatNumeric && len(vals[i]) >= minLeakDigits) {
			continue
		}
		v := string(vals[i])
		needles = append(needles, needle{text: v, fn: p.fn, fold: p.fold})
		if p.words {
			for _, w := range words(v) {
				needles = append(needles, needle{text: w, fn: e.r.Universal, fold: true})
			}
		}
	}
	return needles
}

// rowSpans are the spans of the free text of one row, found ahead: by
// plan, the texts freeText was asked about, in order.
type rowSpans struct {
	needles []needle
	texts   [][]foundSpans
}

func (f *rowSpans) of(plan int) []foundSpans {
	if f == nil {
		return nil
	}
	return f.texts[plan]
}

// findAhead finds the spans of the free text and HTML of rows, on a worker
// of readAhead: what is to be replaced depends only on the text and the
// row's needles, never on stand-ins made before, so it is found in
// parallel; the replacement itself, whose stand-ins depend on the order of
// values, stays in row order (engine.row).
func (e *engine) findAhead(plans []*plan, rows [][][]byte) []rowSpans {
	out := make([]rowSpans, len(rows))
	for k, vals := range rows {
		f := &out[k]
		f.needles = e.needles(plans, vals)
		f.texts = make([][]foundSpans, len(plans))
		for i, p := range plans {
			if !p.text() || vals[i] == nil {
				continue
			}
			x := &textReplacer{r: e.r, pii: e.text.pii, record: &f.texts[i]}
			if p.rule.Strategy == StrategyHTML {
				x.html(string(vals[i]), f.needles)
			} else {
				x.freeText(string(vals[i]), f.needles)
			}
		}
	}
	return out
}

// row replaces the values of one row in place (nil is NULL). The free text
// of the row looks for the row's own personal values, needles, gathered
// before a Filter nulls any of them (see rows).
//
// found are the spans of the row's free text found ahead (findAhead) with
// the same needles, or nil.
func (e *engine) row(ctx context.Context, t *Table, plans []*plan, vals [][]byte, needles []needle, found *rowSpans) error {
	for i, p := range plans {
		v := vals[i]
		if v == nil {
			continue
		}
		p.report.Values++
		s := string(v)
		var out string
		var err error
		switch p.rule.Kind {
		case KindKeep:
			continue
		case KindSerializedPHP:
			vals[i] = nil // empty-table never reaches a row
			continue
		case KindFile:
			if e.files == nil {
				return errors.New("file placeholders are not available")
			}
			out, err = e.files.Placeholder(ctx, FileValue{Table: t.Name, Column: p.col.Name, Path: s})
		default:
			var n int
			switch p.rule.Strategy {
			case StrategyNull:
				vals[i] = nil
				continue
			case StrategyEmpty:
				out = emptyValue(p.col)
			case StrategyPassword:
				var known bool
				if out, known, err = e.r.Password(s); err == nil && !known {
					p.report.Unknown++
				}
			case StrategyFreeText:
				e.text.replay = found.of(i)
				out, n, err = e.text.freeText(s, needles)
				e.text.replay = nil
			case StrategyHTML:
				e.text.replay = found.of(i)
				out, n, err = e.text.html(s, needles)
				e.text.replay = nil
			case StrategyJSON:
				var nulled bool
				out, nulled, n, err = e.text.json(s, p.json, needles)
				if err == nil && nulled {
					if !p.col.Nullable {
						out = "null"
					} else {
						vals[i] = nil
						p.report.Parts += int64(n)
						continue
					}
				}
			default:
				out, err = p.fn(s)
			}
			p.report.Parts += int64(n)
		}
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name, p.col.Name, err)
		}
		vals[i] = []byte(out)
	}
	return nil
}

// Anonymize copies the source database into the target database and/or a
// dump, with every personal value replaced by its rule. The source is read
// in one read-only snapshot, table by table in name order and row by row in
// primary-key order, streaming: no table is held in memory, and no original
// value is written anywhere. The same source, key and rules give the same
// output, byte for byte.
//
// Before anything is written the rules are checked against the schema; a
// column without a rule is a *CheckError. The target database is written
// under a staging name and replaces the target only after the run and
// opt.Verify have passed; on any error, also a cancelled context, the
// staging database is dropped and an existing target stays as it was.
func Anonymize(ctx context.Context, opt Options) (rep *Report, err error) {
	start := time.Now()
	if opt.Target == nil && opt.Dump == nil {
		return nil, errors.New("nothing to write: no target database and no dump")
	}
	if opt.Rules == nil {
		return nil, errors.New("no rules")
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	if opt.BatchRows <= 0 {
		opt.BatchRows = 500
	}
	e, err := newEngine(opt.Source.Driver, opt.Key, opt.PII, opt.Files)
	if err != nil {
		return nil, err
	}
	if opt.Target != nil {
		if opt.Target.Driver != opt.Source.Driver {
			return nil, errors.New("the target must use the same driver as the source")
		}
		if err := checkNotSource(ctx, e.dl, opt.Source, *opt.Target); err != nil {
			return nil, err
		}
	}

	srcDB, err := open(opt.Source, opt.Source.Name)
	if err != nil {
		return nil, err
	}
	defer srcDB.Close()
	// The source phase: a snapshot, the schema, the checks, the stand-ins
	// reserved for the originals, the index of originals and the sample.
	// Nothing is written before it ends. A try the server stops for a lock
	// (retryRead) starts again from a new snapshot and a new engine, index
	// and sample, so nothing of it reaches the next try.
	var src *sql.Conn
	defer func() {
		if src != nil {
			src.ExecContext(context.Background(), "ROLLBACK")
			src.Close()
		}
	}()
	var schema *Schema
	var x *leakIndex
	var sampleLog bytes.Buffer
	det := e.text.pii
	err = retryRead(ctx, opt.Log, "source", func() error {
		if src != nil {
			src.ExecContext(context.Background(), "ROLLBACK")
			src.Close()
			src = nil
		}
		sampleLog.Reset()
		var err error
		if e, err = newEngine(opt.Source.Driver, opt.Key, det, opt.Files); err != nil {
			return err
		}
		if src, err = srcDB.Conn(ctx); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if err := e.dl.session(ctx, src); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if err := e.dl.snapshot(ctx, src); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if schema, err = e.dl.schema(ctx, src, opt.Source.Name); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if err := opt.Rules.Check(schema, opt.Files != nil); err != nil {
			return err
		}
		if err := opt.Rules.CheckSchema(schema, e.text.pii); err != nil {
			return err
		}
		x = nil
		if opt.Index != nil {
			if x, err = newLeakIndex(opt.Key, e.text.pii); err != nil {
				return err
			}
		}
		if err := e.r.prepare(ctx, src, e.dl, e.text.pii, schema, opt.Rules, x); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if p, ok := opt.Filter.(preparer); ok {
			if err := p.prepare(ctx, src, e.dl, schema, opt.Rules, &sampleLog); err != nil {
				return err
			}
		}
		return fault("source")
	})
	opt.Log.Write(sampleLog.Bytes())
	if err != nil {
		return nil, err
	}
	if x != nil {
		*opt.Index = VerifyIndex{key: opt.Key, x: x, schema: schema}
	}

	var staging *Database
	var conns []*sql.Conn
	closeTarget := func() {}
	defer func() { closeTarget() }()
	if opt.Target != nil {
		if staging, err = e.stage(ctx, opt.Source, *opt.Target, opt.Replace); err != nil {
			if errors.Is(err, ErrTargetExists) || errors.Is(err, ErrNotTarget) {
				return nil, err
			}
			return nil, fmt.Errorf("target: %w", err)
		}
		// Dropped unless published (then only MySQL's emptied one is left).
		defer func() {
			closeTarget()
			switch derr := e.discard(*staging); {
			case derr == nil:
			case err != nil:
				err = fmt.Errorf("%w; the staging database %s could not be dropped: %v", err, staging.Name, derr)
			default:
				fmt.Fprintf(opt.Log, "the staging database %s could not be dropped: %v\n", staging.Name, derr)
			}
		}()
		tdb, err := open(*staging, staging.Name)
		if err != nil {
			return nil, err
		}
		closeTarget = func() {
			for _, c := range conns {
				c.Close()
			}
			tdb.Close()
			closeTarget = func() {}
		}
		for range insertConns {
			tc, err := tdb.Conn(ctx)
			if err != nil {
				return nil, fmt.Errorf("target: %w", err)
			}
			conns = append(conns, tc)
			if err := e.dl.session(ctx, tc); err != nil {
				return nil, fmt.Errorf("target: %w", err)
			}
		}
	}
	var dump *dumpSink
	if opt.Dump != nil {
		w := bufio.NewWriterSize(opt.Dump, 1<<20)
		dump = &dumpSink{w: w}
		fmt.Fprintf(w, "-- DriftGate anonymized data, dump format %d (%s)\n", DumpFormat, schema.Driver)
		fmt.Fprintf(w, "-- Pseudonymised: personal values replaced by rules in classify.yaml under a project key.\n\n")
		for _, st := range schema.header {
			if err := dump.exec(st); err != nil {
				return nil, err
			}
		}
	}

	out := newWriter(ctx, dump, conns)
	defer out.abort()
	if len(conns) > 0 && staging.Driver != Postgres {
		packet, err := maxPacket(ctx, conns[0])
		if err != nil {
			return nil, fmt.Errorf("target: %w", err)
		}
		out.packet = packet - packetMargin
	}
	rep, err = e.copy(ctx, src, schema, opt, out)
	if err != nil {
		return nil, err
	}
	if err := out.close(); err != nil {
		return nil, err
	}
	if dump != nil {
		if err := dump.w.Flush(); err != nil {
			return nil, err
		}
	}
	closeTarget()
	if opt.Verify != nil {
		if err := opt.Verify(ctx, staging); err != nil {
			return nil, err
		}
	}
	if staging != nil {
		if err := e.publish(ctx, *staging, *opt.Target, opt.Replace); err != nil {
			if errors.Is(err, ErrTargetExists) || errors.Is(err, ErrNotTarget) {
				return nil, err
			}
			return nil, fmt.Errorf("target: %w", err)
		}
	}
	rep.Seconds = time.Since(start).Seconds()
	return rep, nil
}

// copy writes the schema and the rows to the sinks.
func (e *engine) copy(ctx context.Context, src *sql.Conn, schema *Schema, opt Options, out *writer) (*Report, error) {
	rep := &Report{Driver: schema.Driver}
	skipped := map[string]bool{}
	for _, t := range schema.Tables {
		if opt.Rules.Tables[t.Name].action() == ActionSkip {
			skipped[t.Name] = true
		}
	}
	usable := func(st Stmt) bool {
		for _, u := range st.uses {
			if skipped[u] {
				return false
			}
		}
		return true
	}
	if len(schema.before) > 0 {
		if err := out.comment(ctx, "Schema"); err != nil {
			return nil, err
		}
	}
	for _, st := range schema.before {
		if err := out.exec(ctx, st); err != nil {
			return nil, err
		}
	}
	for _, t := range schema.Tables {
		tr := opt.Rules.Tables[t.Name]
		tw := TableReport{Name: t.Name, Action: tr.action()}
		if skipped[t.Name] {
			rep.Tables = append(rep.Tables, tw)
			fmt.Fprintf(opt.Log, "%-40s skipped\n", t.Name)
			continue
		}
		if err := out.comment(ctx, "Table "+t.Name); err != nil {
			return nil, err
		}
		for _, q := range t.create {
			if err := out.exec(ctx, Stmt{SQL: q}); err != nil {
				return nil, err
			}
		}
		if tr.emptied() {
			if tw.Action == ActionCopy {
				tw.Action = "emptied"
			}
			rep.Tables = append(rep.Tables, tw)
			fmt.Fprintf(opt.Log, "%-40s %s\n", t.Name, tw.Action)
			continue
		}
		plans := e.plans(t, tr)
		n, err := e.rows(ctx, src, t, plans, opt, out)
		if err != nil {
			return nil, err
		}
		tw.Rows = n
		for _, p := range plans {
			tw.Columns = append(tw.Columns, *p.report)
		}
		rep.Tables = append(rep.Tables, tw)
		rep.Rows += n
		fmt.Fprintf(opt.Log, "%-40s %d rows\n", t.Name, n)
	}

	if p, ok := opt.Filter.(preparer); ok {
		for i := range rep.Tables {
			rep.Tables[i].Sample = p.report(schema.Table(rep.Tables[i].Name))
		}
	}

	if err := out.comment(ctx, "After the rows"); err != nil {
		return nil, err
	}
	for _, sq := range schema.sequences {
		if !usable(Stmt{uses: sq.uses}) {
			continue
		}
		rows, err := queryStrings(ctx, src, sq.read)
		if err != nil || len(rows) != 1 {
			return nil, fmt.Errorf("sequence %s: %v", sq.name, err)
		}
		for _, set := range sq.set(rows[0]) {
			if err := out.exec(ctx, Stmt{SQL: set}); err != nil {
				return nil, err
			}
		}
	}
	for _, st := range schema.after {
		if !usable(st) {
			if st.view != "" {
				rep.LeftOut = append(rep.LeftOut, "view "+st.view)
			}
			continue
		}
		if err := out.exec(ctx, st); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// maxStatement is about where an INSERT statement ends.
const maxStatement = 1 << 20

// packetMargin is what the protocol adds to a statement within MySQL's
// max_allowed_packet.
const packetMargin = 1024

// maxPacket reads the longest packet a MySQL or MariaDB target takes
// (tests set a smaller one).
var maxPacket = func(ctx context.Context, c *sql.Conn) (n int, err error) {
	err = c.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&n)
	return n, err
}

// rowTooLong is the error of a row whose INSERT alone is longer than the
// target takes; it names the longest value's column, never a value.
func (e *engine) rowTooLong(t *Table, cols []*Column, vals [][]byte, size, packet int) error {
	longest, at := 0, 0
	for i, v := range vals {
		if n := len(e.dl.literal(cols[i], v)); n > longest {
			longest, at = n, i
		}
	}
	return fmt.Errorf("%s: a row takes an INSERT of %d bytes (column %s: %d), more than the target's max_allowed_packet allows (%d); raise max_allowed_packet on the target server", t.Name, size, cols[at].Name, longest, packet+packetMargin)
}

// rows streams one table: read, replace, write in batches.
func (e *engine) rows(ctx context.Context, src *sql.Conn, t *Table, plans []*plan, opt Options, out *writer) (int64, error) {
	cols := make([]*Column, len(plans))
	var work func(*rowBatch) error
	for i, p := range plans {
		cols[i] = p.col
		if p.text() {
			// The spans of free text are found ahead (see findAhead).
			work = func(rb *rowBatch) error {
				rb.work = e.findAhead(plans, rb.rows)
				return nil
			}
		}
	}
	where := ""
	if opt.Filter != nil {
		where = opt.Filter.Where(t)
	}
	head := e.dl.insert(t, cols)
	inOrder := keepsOrder(opt.Source.Driver, t)
	var b strings.Builder
	inBatch := 0
	flush := func() error {
		if inBatch == 0 {
			return nil
		}
		err := out.exec(ctx, Stmt{SQL: b.String(), rows: t, inOrder: inOrder})
		b.Reset()
		inBatch = 0
		return err
	}
	var n int64
	var stop error // an error of the copy, returned as it is
	add := func(vals [][]byte, found *rowSpans) error {
		// The needles are the values as read: a value a Filter nulls (a
		// reference outside the subset) is still the row's own and stays
		// out of its free text.
		var needles []needle
		if found != nil {
			needles = found.needles
		} else {
			needles = e.needles(plans, vals)
		}
		if opt.Filter != nil && !opt.Filter.Keep(t, cols, vals) {
			return nil
		}
		if err := e.row(ctx, t, plans, vals, needles, found); err != nil {
			return err
		}
		mark := b.Len()
		if inBatch == 0 {
			b.WriteString(head)
			b.WriteString("\n(")
		} else {
			b.WriteString(",\n(")
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(e.dl.literal(cols[i], v))
		}
		b.WriteByte(')')
		if out.packet > 0 && b.Len() > out.packet {
			// The target would refuse the statement: the rows before this
			// one go alone, and a row too long for a statement of its own
			// stops the run, saying where.
			if inBatch == 0 {
				return e.rowTooLong(t, cols, vals, b.Len(), out.packet)
			}
			whole := b.String()
			b.Reset()
			b.WriteString(whole[:mark])
			if err := flush(); err != nil {
				return err
			}
			b.WriteString(head)
			b.WriteString("\n")
			b.WriteString(whole[mark+2:])
			if b.Len() > out.packet {
				return e.rowTooLong(t, cols, vals, b.Len(), out.packet)
			}
		}
		inBatch++
		n++
		if inBatch >= opt.BatchRows || b.Len() >= maxStatement {
			return flush()
		}
		return nil
	}
	err := readAhead(ctx, src, e.dl.selectRows(t, cols, where), len(cols), work, func(rb *rowBatch) error {
		found, _ := rb.work.([]rowSpans)
		for k, vals := range rb.rows {
			var f *rowSpans
			if found != nil {
				f = &found[k]
			}
			if stop = add(vals, f); stop != nil {
				return stop
			}
		}
		return nil
	})
	switch {
	case stop != nil:
		return 0, stop
	case err != nil:
		return 0, fmt.Errorf("source %s: %w", t.Name, err)
	}
	return n, flush()
}
