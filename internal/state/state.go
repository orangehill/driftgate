// Package state restores the data an application runs on and fingerprints it,
// so every capture starts from the same state and every change a request makes
// to the data is seen. A component is one part of that state: a MySQL/MariaDB
// or PostgreSQL database with a pristine copy, or anything else through
// restore and fingerprint commands.
package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/procx"
	"github.com/orangehill/driftgate/internal/secret"
)

// Fingerprint maps the items of a component (tables, views, triggers,
// sequences) to a hash of their definition and content.
type Fingerprint map[string]string

// FingerprintFormat numbers the way fingerprints are made; a fingerprint
// recorded in another format (a pristine copy's, a baseline's) can differ
// from a new one of the same data. 2: MySQL/MariaDB triggers and
// views include the collation they were made with; PostgreSQL adds
// functions, enum, domain and composite types, schemas, materialized
// views, and to a table's definition row security, policies, rules,
// privileges, comments, column collations, UNLOGGED and the replica
// identity, to a view's its rules and privileges, to a sequence its
// parameters and the value setval(…, false) left. Items that did not change keep their hash, so a
// database without such objects fingerprints as before.
const FingerprintFormat = 2

// Formatter is a component that can fingerprint items as an older format
// (1) made them, so a fingerprint recorded in that format can be compared
// with the data now (verify --migrate-state).
type Formatter interface {
	FingerprintAs(ctx context.Context, format int, items []string) (Fingerprint, error)
}

// PristineFingerprinter is a component that can fingerprint items of its
// pristine copy, to tell a restore that went wrong from a pristine copy
// that no longer matches its record.
type PristineFingerprinter interface {
	PristineFingerprint(ctx context.Context, items []string) (Fingerprint, error)
}

// fpMode is what a driver's fingerprint of a database reads.
type fpMode int

const (
	fpRows        fpMode = iota // every item but the restore_only tables, with their rows
	fpRestoreOnly               // the restore_only tables only, with their rows
	fpSchema                    // every item Restore takes, by its definition only (Schemer)
)

// Diff lists the items whose hash differs or that exist on one side only.
func Diff(a, b Fingerprint) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Subset returns the part of f for the given items; an item f does not have
// is left out.
func (f Fingerprint) Subset(items []string) Fingerprint {
	out := Fingerprint{}
	for _, it := range items {
		if v, ok := f[it]; ok {
			out[it] = v
		}
	}
	return out
}

// RowChange is one row of a tracked table that a request inserted, deleted or
// updated. Key is the primary key, written as column=value pairs.
type RowChange struct {
	Table   string   `json:"table"`
	Key     string   `json:"key"`
	Change  string   `json:"change"` // inserted | deleted | updated
	Columns []string `json:"columns,omitempty"`
}

// Rows holds the rows of tracked tables: table → key → column → value hash.
type Rows map[string]map[string]map[string]string

// RowDiff lists the rows that differ between two reads of the same tables.
func RowDiff(before, after Rows) []RowChange {
	var out []RowChange
	tables := map[string]bool{}
	for t := range before {
		tables[t] = true
	}
	for t := range after {
		tables[t] = true
	}
	for t := range tables {
		b, a := before[t], after[t]
		for key, cols := range a {
			old, ok := b[key]
			if !ok {
				out = append(out, RowChange{Table: t, Key: key, Change: "inserted"})
				continue
			}
			var changed []string
			for c, v := range cols {
				if old[c] != v {
					changed = append(changed, c)
				}
			}
			for c := range old {
				if _, ok := cols[c]; !ok {
					changed = append(changed, c)
				}
			}
			if len(changed) > 0 {
				sort.Strings(changed)
				out = append(out, RowChange{Table: t, Key: key, Change: "updated", Columns: changed})
			}
		}
		for key := range b {
			if _, ok := a[key]; !ok {
				out = append(out, RowChange{Table: t, Key: key, Change: "deleted"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Component is one part of the state.
type Component interface {
	// Name is the component's name in driftgate.yaml.
	Name() string
	// Fingerprint hashes the working state; with items set, only those
	// items. A component that cannot fingerprint returns nil.
	Fingerprint(ctx context.Context, items []string) (Fingerprint, error)
	// Restore brings the given items of the working state back to the
	// pristine copy; nil means everything. A command component always
	// restores everything.
	Restore(ctx context.Context, items []string) error
	// Seed makes the current working state the new pristine copy.
	Seed(ctx context.Context) error
	// Rows reads the rows of the tracked tables among items; nil means all
	// tracked tables.
	Rows(ctx context.Context, items []string) (Rows, error)
	// Partial reports whether Restore can restore single items.
	Partial() bool
	Close() error
}

// RestoreOnly is a database component that restores tables without
// fingerprinting them (Options.RestoreOnly): tables the application writes
// on every request or outside requests (sessions, caches, queues) whose
// rows should still start from the pristine copy's. Fingerprint leaves
// them out as it leaves out excluded tables, so they are no side effect and
// no part of the state a baseline is checked against; Restore takes their
// items as any other.
type RestoreOnly interface {
	// RestoreOnlyFingerprint hashes the restore_only tables of the working
	// state, or with pristine of the pristine copy, as the items Restore
	// takes. It reads them every time: no fingerprint keeps them.
	RestoreOnlyFingerprint(ctx context.Context, pristine bool) (Fingerprint, error)
}

// Schemer hashes the definitions of the items Restore takes, without rows
// or counters: the schema a restore brings back. A working database a
// stopped restore left unreadable gives nil; the pristine copy never does.
type Schemer interface {
	SchemaFingerprint(ctx context.Context, pristine bool) (Fingerprint, error)
}

// SessionEnder is a Component whose Restore may end the application's own
// sessions with the database (SQL Server's restore of the whole database
// takes it offline): the application's pooled connections then fail once
// each, on their next use.
type SessionEnder interface {
	// EndedSessions is how many of them the last Restore ended.
	EndedSessions() int
}

// Command is a component handled entirely by commands of the project.
type Command struct {
	name        string
	root        string
	restore     []string
	fingerprint []string
	env         []string
	log         io.Writer
}

// NewCommand returns a command component. Commands run from root, without a
// shell, with env added to their environment; their output goes to log.
func NewCommand(name, root string, restore, fingerprint, env []string, log io.Writer) *Command {
	if log == nil {
		log = io.Discard
	}
	return &Command{name: name, root: root, restore: restore, fingerprint: fingerprint, env: env, log: log}
}

func (c *Command) Name() string  { return c.name }
func (c *Command) Partial() bool { return false }
func (c *Command) Close() error  { return nil }

func (c *Command) Seed(context.Context) error { return nil }

func (c *Command) Rows(context.Context, []string) (Rows, error) { return Rows{}, nil }

func (c *Command) Restore(ctx context.Context, _ []string) error {
	return Run(ctx, c.root, c.restore, c.env, c.log)
}

// Fingerprint hashes the output of the fingerprint command as one item named
// after the component.
func (c *Command) Fingerprint(ctx context.Context, items []string) (Fingerprint, error) {
	if len(c.fingerprint) == 0 {
		return nil, nil
	}
	if items != nil && !contains(items, c.name) {
		return Fingerprint{}, nil
	}
	cmd := procx.Command(ctx, c.fingerprint[0], c.fingerprint[1:]...)
	cmd.Dir = c.root
	cmd.Env = append(secret.Environ(), c.env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, c.log
	if err := procx.Run(cmd); err != nil {
		return nil, fmt.Errorf("fingerprint of %s: %w", c.name, err)
	}
	return Fingerprint{c.name: hash(out.Bytes())}, nil
}

// Run runs a command of the project without a shell, from root, with env
// added to its environment (procx: a time limit or an interrupt stops what
// it started too). Its output is shown only when it fails (the last lines),
// so a working restore or migration does not fill the report with its own
// messages; only the end of it is kept.
func Run(ctx context.Context, root string, argv, env []string, log io.Writer) error {
	return RunIn(ctx, root, nil, argv, env, log)
}

// RunIn is Run from dir, with stdin (nil: none) as standard input.
func RunIn(ctx context.Context, dir string, stdin io.Reader, argv, env []string, log io.Writer) error {
	if len(argv) == 0 {
		return nil
	}
	cmd := procx.Command(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Env = append(secret.Environ(), env...)
	out := &tail{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := procx.Run(cmd); err != nil {
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		if log != nil && out.Len() > 0 {
			fmt.Fprintln(log, strings.Join(lines, "\n"))
		}
		return fmt.Errorf("%s: %w", procx.Line(argv), err)
	}
	return nil
}

// tail keeps the last max bytes written to it (at least; it is cut when it
// holds twice that).
type tail struct {
	b   []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) Len() int { return len(t.b) }

func (t *tail) String() string { return string(t.b) }

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Querier runs a read-only query on the pristine copy and returns the first
// column of every row as text.
type Querier interface {
	Query(ctx context.Context, query string) ([]string, error)
}

func firstColumn(rows interface {
	Next() bool
	Scan(...any) error
	Columns() ([]string, error)
	Err() error
}) ([]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("sql: the query returns no columns")
	}
	vals := make([]any, len(cols))
	var first sqlText
	vals[0] = &first
	for i := 1; i < len(cols); i++ {
		vals[i] = new(any)
	}
	var out []string
	for rows.Next() {
		if err := rows.Scan(vals...); err != nil {
			return nil, err
		}
		out = append(out, string(first))
	}
	return out, rows.Err()
}

// sqlText scans any column value as text; NULL becomes "".
type sqlText string

func (t *sqlText) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*t = ""
	case []byte:
		*t = sqlText(x)
	case string:
		*t = sqlText(x)
	default:
		*t = sqlText(fmt.Sprint(x))
	}
	return nil
}
