package data

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orangehill/driftgate/internal/lazy"
)

// insertConns is how many connections write rows into the target database
// at once.
const insertConns = 4

// writeQueue is how many statements may wait for the writer: with INSERTs of
// up to about 1 MiB (maxStatement) a few MiB.
const writeQueue = 8

// writer takes the statements of a run on a goroutine of its own, so the
// next rows are read and replaced while the last ones are written. The dump
// gets every statement in the order it was made, as one writer would write
// it. The target database runs the INSERTs of rows on up to insertConns
// connections at once; every other statement (a table, an index, a trigger,
// a session setting) waits until the INSERTs before it have finished and
// runs on the first connection, in order. Every connection has the session
// of the target (no checks of references and uniqueness on MySQL).
//
// The rows of a table go to one connection in the order they were read,
// unless the order they are written in cannot show: a PostgreSQL table is a
// heap, which a query without ORDER BY reads in the order its rows were
// written, and so is a MySQL table that is not InnoDB or has no primary key.
// InnoDB keeps rows in the order of the primary key whatever order they
// arrive in. The target holds the same rows, read back in the same order,
// as with one connection.
type writer struct {
	dump  *dumpSink
	db    *dbWriter // nil without a target
	items chan writeItem
	done  chan struct{}
	// packet is the longest statement the target takes (MySQL's
	// max_allowed_packet, less a margin); 0: no limit known.
	packet int

	mu     sync.Mutex
	err    error
	failed chan struct{} // closed with the first error
	closed bool
}

type writeItem struct {
	st      Stmt
	comment string // a comment of the dump instead of a statement
}

func newWriter(ctx context.Context, dump *dumpSink, conns []*sql.Conn) *writer {
	w := &writer{dump: dump, items: make(chan writeItem, writeQueue), done: make(chan struct{}), failed: make(chan struct{})}
	if len(conns) > 0 {
		w.db = newDBWriter(ctx, conns)
	}
	go w.run()
	return w
}

func (w *writer) run() {
	defer close(w.done)
	for it := range w.items {
		if w.firstErr() != nil {
			continue // drained, so the producer never waits
		}
		if err := w.write(it); err != nil {
			w.fail(err)
		}
	}
	if w.db != nil {
		w.fail(w.db.close())
	}
}

func (w *writer) write(it writeItem) error {
	if it.st.SQL == "" {
		if w.dump == nil {
			return nil
		}
		return w.dump.comment(it.comment)
	}
	if w.dump != nil {
		if err := w.dump.exec(it.st); err != nil {
			return err
		}
	}
	if w.db == nil {
		return nil
	}
	if it.st.rows != nil {
		return w.db.insert(it.st)
	}
	return w.db.other(it.st)
}

func (w *writer) fail(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
		close(w.failed)
	}
}

func (w *writer) firstErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *writer) send(ctx context.Context, it writeItem) error {
	select {
	case w.items <- it:
		return nil
	case <-w.failed:
		return w.firstErr()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// exec queues a statement; an error is the first of the statements before.
func (w *writer) exec(ctx context.Context, st Stmt) error { return w.send(ctx, writeItem{st: st}) }

// comment queues a comment of the dump.
func (w *writer) comment(ctx context.Context, text string) error {
	if w.dump == nil {
		return nil
	}
	return w.send(ctx, writeItem{comment: text})
}

// close waits until every statement has run and returns the first error.
func (w *writer) close() error {
	w.mu.Lock()
	closed := w.closed
	w.closed = true
	w.mu.Unlock()
	if !closed {
		close(w.items)
	}
	<-w.done
	return w.firstErr()
}

// abort stops the statements that wait and those that run, and waits for
// the writer (after an error elsewhere, before the target is dropped).
func (w *writer) abort() {
	if w.db != nil {
		w.db.cancel()
	}
	w.close()
}

// dbWriter runs statements on the connections of the target database.
type dbWriter struct {
	conns   []*sql.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	shared  chan Stmt   // INSERTs any connection may run
	lanes   []chan Stmt // INSERTs of one table that keep their order, by connection
	lane    map[*Table]int
	next    int
	pending sync.WaitGroup // INSERTs queued and not yet run
	workers sync.WaitGroup

	mu  sync.Mutex
	err error
}

func newDBWriter(ctx context.Context, conns []*sql.Conn) *dbWriter {
	d := &dbWriter{conns: conns, shared: make(chan Stmt), lane: map[*Table]int{}}
	d.ctx, d.cancel = context.WithCancel(ctx)
	for _, c := range conns {
		lane := make(chan Stmt, 1)
		d.lanes = append(d.lanes, lane)
		d.workers.Add(1)
		go d.work(c, lane)
	}
	return d
}

func (d *dbWriter) work(c *sql.Conn, lane chan Stmt) {
	defer d.workers.Done()
	shared := d.shared
	for lane != nil || shared != nil {
		var st Stmt
		var ok bool
		select {
		case st, ok = <-lane:
			if !ok {
				lane = nil
				continue
			}
		case st, ok = <-shared:
			if !ok {
				shared = nil
				continue
			}
		}
		if d.firstErr() == nil {
			if err := execRows(d.ctx, c, st); err != nil {
				d.fail(err)
			}
		}
		d.pending.Done()
	}
}

func (d *dbWriter) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
		d.cancel()
	}
}

func (d *dbWriter) firstErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// insert hands an INSERT of rows to a connection.
func (d *dbWriter) insert(st Stmt) error {
	if err := d.firstErr(); err != nil {
		return err
	}
	ch := d.shared
	if st.inOrder {
		i, ok := d.lane[st.rows]
		if !ok {
			i = d.next % len(d.lanes)
			d.lane[st.rows] = i
			d.next++
		}
		ch = d.lanes[i]
	}
	d.pending.Add(1)
	select {
	case ch <- st:
		return nil
	case <-d.ctx.Done():
		d.pending.Done()
		if err := d.firstErr(); err != nil {
			return err
		}
		return d.ctx.Err()
	}
}

// other runs any other statement on the first connection, after the
// INSERTs before it.
func (d *dbWriter) other(st Stmt) error {
	d.pending.Wait()
	if err := d.firstErr(); err != nil {
		return err
	}
	return execTarget(d.ctx, d.conns[0], st)
}

// close waits for the INSERTs and stops the workers.
func (d *dbWriter) close() error {
	d.pending.Wait()
	close(d.shared)
	for _, l := range d.lanes {
		close(l)
	}
	d.workers.Wait()
	return d.firstErr()
}

// maxDeadlocks bounds the runs of an INSERT that InnoDB ends as a deadlock.
const maxDeadlocks = 20

// execRows runs an INSERT of rows. InnoDB may end one of two INSERTs into
// the same table on two connections as a deadlock (the locks of unique
// indexes); the statement is then rolled back as a whole and runs again.
func execRows(ctx context.Context, c *sql.Conn, st Stmt) error {
	for try := 1; ; try++ {
		err := execTarget(ctx, c, st)
		var me *mysql.MySQLError
		if err == nil || try == maxDeadlocks || !errors.As(err, &me) || me.Number != 1213 {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(try) * time.Millisecond):
		}
	}
}

// execTarget runs one statement on the target; an error names the
// statement, never the values of an INSERT, and the server's message
// without the value it quotes (quietError).
func execTarget(ctx context.Context, c *sql.Conn, st Stmt) error {
	if _, err := c.ExecContext(ctx, st.SQL); err != nil {
		err = quietError(err)
		q := st.SQL
		if i := strings.Index(q, " VALUES\n"); strings.HasPrefix(q, "INSERT ") && i > 0 {
			q = q[:i+7] + " …" // never the values
		} else if len(q) > 200 {
			q = q[:200] + "…"
		}
		return fmt.Errorf("target: %w (%s)", err, q)
	}
	return nil
}

// keepsOrder reports whether the rows of a table must be written in the
// order they are read (see writer).
func keepsOrder(driver string, t *Table) bool {
	if driver == Postgres || len(t.PrimaryKey) == 0 || len(t.create) == 0 {
		return true
	}
	def := t.create[len(t.create)-1]
	i := strings.LastIndex(def, ") ENGINE=")
	return i < 0 || !strings.HasPrefix(def[i+len(") ENGINE="):], "InnoDB")
}

// dumpSink writes statements as a SQL script: the same statements the
// target runs, in the same order, so the dump loads into an empty database
// (mysql < dump, psql -f dump).
type dumpSink struct{ w *bufio.Writer }

func (s *dumpSink) exec(st Stmt) error {
	// A comment to the end of the line (--, MySQL's #) on the last line
	// would hide the delimiter: it goes on a line of its own.
	sep := ""
	if last := st.SQL[strings.LastIndexByte(st.SQL, '\n')+1:]; strings.Contains(last, "--") || strings.Contains(last, "#") {
		sep = "\n"
	}
	if st.Compound {
		_, err := s.w.WriteString("DELIMITER ;;\n" + st.SQL + sep + ";;\nDELIMITER ;\n")
		return err
	}
	_, err := s.w.WriteString(st.SQL + sep + ";\n")
	return err
}

func (s *dumpSink) comment(text string) error {
	_, err := s.w.WriteString("\n-- " + text + "\n")
	return err
}

// mysqlValueAt are the places a MySQL or MariaDB message quotes a value of
// a row: Duplicate entry '…' for key, Incorrect integer value: '…' for
// column, Invalid utf8mb4 character string: '…', Illegal double '…' value.
var mysqlValueAt = []string{"entry '", "value: '", "string: '", "double '"}

// postgresValue finds a value PostgreSQL quotes at the end of a message
// (invalid input syntax for type integer: "…") or in "value "…" is out of
// range". A constraint, column or relation name is quoted without a colon
// before it and stays.
var postgresValue = lazy.Regexp(`(?s): ".*"$|value ".*" is out of range`)

// quietError keeps the values of rows out of a database error, which
// reaches the console and logs: the copy's values are pseudonymised, but
// an error may come before the replacement is written, or quote an
// original of a column kept as it is. The error number, SQLSTATE and the
// names of the key or column stay. errors.As still finds the driver's
// error, with its number.
func quietError(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		msg := me.Message
		for _, at := range mysqlValueAt {
			i := strings.Index(msg, at)
			if i < 0 {
				continue
			}
			start := i + len(at)
			end := strings.LastIndex(msg, "' for ")
			if end < start {
				end = strings.LastIndex(msg, "'")
			}
			if end >= start {
				msg = msg[:start] + "…" + msg[end:]
			}
			break
		}
		return &mysql.MySQLError{Number: me.Number, SQLState: me.SQLState, Message: msg}
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		msg := postgresValue().ReplaceAllStringFunc(pe.Message, func(m string) string {
			if strings.HasPrefix(m, ": ") {
				return `: "…"`
			}
			return `value "…" is out of range`
		})
		// Detail, Where and the rest may show the row; Error() does not
		// print them, but a caller could.
		return &pgconn.PgError{Severity: pe.Severity, SeverityUnlocalized: pe.SeverityUnlocalized, Code: pe.Code, Message: msg,
			SchemaName: pe.SchemaName, TableName: pe.TableName, ColumnName: pe.ColumnName, ConstraintName: pe.ConstraintName}
	}
	return err
}
