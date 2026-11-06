package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/lazy"
)

// probeItem is a trigger or view a seed or restore makes again: its kind,
// name, CREATE statement as SHOW CREATE gives it and, for a trigger, its
// table.
type probeItem struct{ kind, name, stmt, table string }

// probeName matches the objects definerProbe makes (dbx.MySQLProbe).
// objects lists none of them, so no fingerprint, copy or restore ever
// covers one, and a seed or restore drops one it finds (left behind by a
// process that was killed).
func probeName(name string) bool { return dbx.MySQLProbe(name) }

// definerAny reads the DEFINER clause of a CREATE statement as SHOW CREATE
// writes it, of an account or (MariaDB) a role.
var definerAny = lazy.Regexp("DEFINER=`(?:[^`]|``)*`(?:@`(?:[^`]|``)*`)?")

// sqlSecurityInvoker finds SQL SECURITY INVOKER in the part of a CREATE
// VIEW statement before " VIEW ", never in the view's SELECT.
var sqlSecurityInvoker = lazy.Regexp(`\bSQL SECURITY INVOKER\b`)

// definerProbe tries, before anything in db is dropped, whether the login
// may create each trigger and view of items there with its DEFINER: whether
// it may name that account (SET_ANY_DEFINER, SYSTEM_USER, SET USER, SUPER)
// and, with the binary log on, create a trigger at all
// (log_bin_trust_function_creators). The privilege is not predicted from
// the grants, which roles make unreliable, but tried: a probe with the
// same DEFINER (and SQL SECURITY) is created under a probe name and dropped
// at once. A refusal stops the seed or restore with createRefused's
// explanation (naming target, the database the objects are made in) while
// db still has every trigger and view it had. The probes stale, left
// behind in db, are dropped first.
//
// The probe of a trigger is made on a table of its own, with a body that
// does nothing: the checks of the DEFINER and the binary log do not depend
// on the table or the body, and a probe on the application's table would
// fire on the application's writes. A login that may not create that table
// (no CREATE), or not a trigger on it (TRIGGER granted on some tables
// only), gets the probe on one of tables, the tables of db: the trigger's
// own table when db has it, else the first. Its body is empty, so it does
// nothing while it exists. Without a table to put it on, nothing is
// probed: the CREATE TRIGGER itself decides. Only when the login may not
// create a table at all (and so not the trigger's table either) is a
// restore into a database without tables refused before any drop. A view's probe selects a
// constant, so that it needs no table. A seed probes in the working
// database (db is not target), where the login may only read: a view it
// may not create there is not probed, and the CREATE in the pristine copy
// says what it lacks. In a restore (db is target) that refusal is the
// CREATE's own, and stops it. A probe that was not made is not recorded
// as passed.
//
// Each DEFINER is tried once per kind and database for the life of m: a
// restore that makes triggers again after the first costs nothing more.
// The probes and the drop of stale ones run under a named lock of db
// (probeLock), so that two processes on one database never drop each
// other's probe.
func (m *MySQL) definerProbe(ctx context.Context, c *sql.Conn, db, target string, tables []string, stale map[string]string, items []probeItem) (err error) {
	type todo struct {
		it               probeItem
		key, clause, sec string
	}
	var work []todo
	m.probeMu.Lock()
	for _, it := range items {
		clause := definerAny().FindString(it.stmt)
		sec := ""
		if it.kind == "view" {
			sec = viewSecurity(it.stmt)
		}
		key := db + "\x00" + it.kind + "\x00" + clause + sec
		if !m.probed[key] && !slices.ContainsFunc(work, func(w todo) bool { return w.key == key }) {
			work = append(work, todo{it, key, clause, sec})
		}
	}
	m.probeMu.Unlock()
	if len(work) == 0 && len(stale) == 0 {
		return nil
	}
	unlock, err := m.probeLock(ctx, c, db)
	if err != nil {
		return err
	}
	defer func() {
		if e := unlock(); e != nil {
			err = errors.Join(err, e)
		}
	}()
	for name, kind := range stale {
		if err := dropObject(ctx, c, db, name, kind); err != nil {
			return fmt.Errorf("a probe left behind, %s %s in %s, cannot be dropped: %w; drop it by hand", kind, name, db, err)
		}
	}
	for _, w := range work {
		made, err := probeOnce(ctx, c, db, target, tables, w.it, w.clause, w.sec)
		if err != nil {
			return err
		}
		if !made {
			continue
		}
		m.probeMu.Lock()
		if m.probed == nil {
			m.probed = map[string]bool{}
		}
		m.probed[w.key] = true
		m.probeMu.Unlock()
	}
	return nil
}

// viewSecurity is the SQL SECURITY clause of a CREATE VIEW statement, read
// before " VIEW " only: the view's SELECT may hold the same words.
func viewSecurity(stmt string) string {
	if i := strings.Index(stmt, " VIEW "); i >= 0 {
		stmt = stmt[:i]
	}
	if sqlSecurityInvoker().MatchString(stmt) {
		return " SQL SECURITY INVOKER"
	}
	return " SQL SECURITY DEFINER"
}

// probeLockWait is how many seconds probeLock waits for the lock.
var probeLockWait = 60

// probeLock takes the named lock of db's probes (GET_LOCK, which MySQL
// and MariaDB hold per session) and returns its release. It waits
// probeLockWait seconds, less than the DSN's readTimeout, which would
// otherwise end the wait as a broken connection. A cluster that does not
// support GET_LOCK (lockUnsupported) runs the probes without it, with one
// note. A release that fails discards c, which would otherwise go back to
// the pool holding the lock, and is returned as an error.
func (m *MySQL) probeLock(ctx context.Context, c *sql.Conn, db string) (func() error, error) {
	sum := sha256.Sum256([]byte(db))
	name := "driftgate_probe_" + hex.EncodeToString(sum[:16])
	wait := probeLockWait
	if cfg, err := mysqlDSN(m.o.DSN); err == nil && cfg.ReadTimeout > 0 {
		wait = max(0, min(wait, int(cfg.ReadTimeout/time.Second)-1))
	}
	var got sql.NullInt64
	if err := c.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, wait).Scan(&got); err != nil {
		if lockUnsupported(err) {
			m.lockNote.Do(func() {
				m.logf("state %s: the server does not support GET_LOCK (%v); the probes run without their lock, so do not seed or restore %s from two processes at once\n", m.o.Name, err, db)
			})
			return func() error { return nil }, nil
		}
		return nil, err
	}
	if !got.Valid || got.Int64 != 1 {
		return nil, fmt.Errorf("the lock of the probes of %s (%s) was not free within %d seconds: another DriftGate process restores or seeds it", db, name, wait)
	}
	return func() error {
		var released sql.NullInt64
		if err := c.QueryRowContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil || released.Int64 != 1 {
			discardConn(c)
			return fmt.Errorf("the lock of the probes of %s (%s) could not be released (%v, %v); its connection is closed", db, name, released, err)
		}
		return nil
	}, nil
}

// lockUnsupported reports a GET_LOCK a cluster refuses: MariaDB Galera
// before 10.5 (1235, "doesn't yet support 'GET_LOCK in cluster'"), Percona
// XtraDB Cluster with pxc_strict_mode ENFORCING (1105, "prohibits use of
// GET_LOCK").
func lockUnsupported(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == 1235 || me.Number == 1105 && strings.Contains(me.Message, "GET_LOCK")
}

// probeDenied reports a refusal for want of a privilege on a database or
// table (CREATE, TRIGGER): 1142 or 1044.
func probeDenied(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1142 || me.Number == 1044)
}

// probeOnce makes and drops one probe of it in db, and reports whether it
// made one (made is false when nothing could be probed). The probe is dropped
// whatever happens, also when ctx is cancelled; a drop that fails is
// reported with the probe's name (a login without DROP leaves it behind),
// and the next seed or restore drops it (probeName).
func probeOnce(ctx context.Context, c *sql.Conn, db, target string, tables []string, it probeItem, clause, security string) (made bool, err error) {
	name := probeID()
	q := qi(db) + "." + qi(name)
	var drops []string // what removes the probe, last first
	defer func() {
		for i := len(drops) - 1; i >= 0; i-- {
			if _, e := c.ExecContext(context.WithoutCancel(ctx), drops[i]); e != nil {
				err = errors.Join(err, fmt.Errorf("the probe %s is left in %s, as it could not be dropped (%v): drop it by hand, or the next seed or restore by a login that may drops it", name, db, e))
				return
			}
		}
	}()
	create := func(on string) error {
		_, e := c.ExecContext(ctx, "CREATE "+clause+" TRIGGER "+q+" BEFORE INSERT ON "+on+" FOR EACH ROW BEGIN END")
		if e == nil && on != q { // on the probe's table, its drop takes the trigger
			drops = append(drops, "DROP TRIGGER IF EXISTS "+q)
		}
		return e
	}
	switch it.kind {
	case "trigger":
		_, e := c.ExecContext(ctx, "CREATE TABLE "+q+" (id INT PRIMARY KEY)")
		if e != nil && !probeDenied(e) {
			return false, fmt.Errorf("trigger %s: a probe of its DEFINER: %w", it.name, e)
		}
		tableMade := e == nil
		if tableMade {
			drops = append(drops, "DROP TABLE IF EXISTS "+q) // and its trigger
			if e = create(q); e == nil {
				return true, nil
			}
		}
		// No CREATE, or TRIGGER on some tables only.
		if !probeDenied(e) {
			return false, createRefused(ctx, c, it.kind, it.name, target, it.stmt, e)
		}
		on := ""
		switch {
		case slices.Contains(tables, it.table):
			on = it.table
		case len(tables) > 0:
			on = tables[0]
		case tableMade || db != target:
			// No table to put the probe on. TRIGGER granted on some tables
			// only (the probe's table was made): the CREATE TRIGGER on the
			// trigger's own table decides. A seed probing in the working
			// database: it has no trigger to copy.
			return false, nil
		default:
			// A restore into a database without tables, by a login that may
			// create none: the trigger's table is copied first, which needs
			// that CREATE too.
			return false, fmt.Errorf("trigger %s cannot be tried before the restore: this login may not create a table in %s (%v), which the copy of the trigger's table needs too, and %s has no table to try it on; give the login CREATE on %s", it.name, db, e, db, db)
		}
		if e := create(qi(db) + "." + qi(on)); e != nil {
			return false, createRefused(ctx, c, it.kind, it.name, target, it.stmt, e)
		}
	case "view":
		_, e := c.ExecContext(ctx, "CREATE "+clause+security+" VIEW "+q+" AS SELECT 1 AS probe")
		switch {
		case e == nil:
			drops = append(drops, "DROP VIEW IF EXISTS "+q)
		case probeDenied(e) && db != target:
			return false, nil
		default:
			return false, createRefused(ctx, c, it.kind, it.name, target, it.stmt, e)
		}
	default:
		return false, fmt.Errorf("no probe for a %s", it.kind)
	}
	return true, nil
}

// probeID is a new probe name (probeName).
func probeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "dgt_probe_" + hex.EncodeToString(b)
}

// noteProbe logs, once per object, a probe left behind that objects found.
func (m *MySQL) noteProbe(db, name string) {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	if m.probeNoted[db+"."+name] {
		return
	}
	if m.probeNoted == nil {
		m.probeNoted = map[string]bool{}
	}
	m.probeNoted[db+"."+name] = true
	m.logf("state %s: %s in %s is a probe a stopped seed or restore left behind; it is in no fingerprint, and the next seed or restore drops it\n", m.o.Name, name, db)
}

// dropItem is an object copyObjects drops before it copies: its kind,
// name and, for a trigger, its DEFINER (user@host).
type dropItem struct{ kind, name, definer string }

// dropFirstOthers puts first the drops of the views and triggers whose
// DEFINER is not the login: a drop the server may refuse (MySQL: an object
// whose DEFINER has SYSTEM_USER), which then happens before anything of
// db is dropped. The order of the others is kept. The DEFINERs of views
// are read only when a view is dropped.
func (m *MySQL) dropFirstOthers(ctx context.Context, c *sql.Conn, db string, drops []dropItem) error {
	views := false
	for _, d := range drops {
		views = views || d.kind == "view"
	}
	if len(drops) < 2 || !views && !slices.ContainsFunc(drops, func(d dropItem) bool { return d.kind == "trigger" }) {
		return nil
	}
	if views {
		rows, err := c.QueryContext(ctx, "SELECT TABLE_NAME, DEFINER FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?", db)
		if err != nil {
			return err
		}
		definers := map[string]string{}
		for rows.Next() {
			var name, definer string
			if err := rows.Scan(&name, &definer); err != nil {
				rows.Close()
				return err
			}
			definers[name] = definer
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, d := range drops {
			if d.kind == "view" {
				drops[i].definer = definers[d.name]
			}
		}
	}
	m.probeMu.Lock()
	login := m.login
	m.probeMu.Unlock()
	if login == "" {
		if err := c.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&login); err != nil {
			return err
		}
		m.probeMu.Lock()
		m.login = login
		m.probeMu.Unlock()
	}
	other := func(d dropItem) bool { return d.definer != "" && !sameAccount(d.definer, login) }
	slices.SortStableFunc(drops, func(a, b dropItem) int {
		switch {
		case other(a) && !other(b):
			return -1
		case other(b) && !other(a):
			return 1
		}
		return 0
	})
	return nil
}

// sameAccount reports whether two accounts written user@host are one: the
// user as written, the host in any letter case.
func sameAccount(a, b string) bool {
	i, j := strings.LastIndexByte(a, '@'), strings.LastIndexByte(b, '@')
	if i < 0 || j < 0 {
		return a == b
	}
	return a[:i] == b[:j] && strings.EqualFold(a[i+1:], b[j+1:])
}
