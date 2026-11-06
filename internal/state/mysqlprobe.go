package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/lazy"
)

// probeItem is a trigger or view a seed or restore makes again: its kind,
// name and CREATE statement as SHOW CREATE gives it.
type probeItem struct{ kind, name, stmt string }

// probeName matches the objects definerProbe makes. objects lists none of
// them, so no fingerprint, copy or restore ever covers one, and a seed or
// restore drops one it finds (left behind by a process that was killed).
var probeName = lazy.Regexp(`^dgt_probe_[0-9a-f]{16}$`)

// definerAny reads the DEFINER clause of a CREATE statement as SHOW CREATE
// writes it, of an account or (MariaDB) a role.
var definerAny = lazy.Regexp("DEFINER=`(?:[^`]|``)*`(?:@`(?:[^`]|``)*`)?")

// definerProbe tries, before anything in db is dropped, whether the login
// may create each trigger and view of items there with its DEFINER: whether
// it may name that account (SET_ANY_DEFINER, SYSTEM_USER, SET USER, SUPER)
// and, with the binary log on, create a trigger at all
// (log_bin_trust_function_creators). The privilege is not predicted from
// the grants, which roles make unreliable, but tried: a probe with the
// same DEFINER (and SQL SECURITY) is created under a probe name and dropped
// at once. A refusal stops the seed or restore with createRefused's
// explanation (naming target, the database the objects are made in) while
// db still has every trigger and view it had.
//
// The probe of a trigger is made on a table of its own, with a body that
// does nothing: the checks of the DEFINER and the binary log do not depend
// on the table or the body, and a probe on the application's table would
// fire on the application's writes. A view's probe selects a constant, so
// that it needs no table. Each DEFINER is tried once per kind and
// database for the life of m: a restore that makes triggers again after
// the first costs nothing more.
//
// A login that may not create a table (CREATE) but may create triggers
// gets its trigger probes on tables, the first of the database's tables:
// the probe's body is empty, so it does nothing while it exists. A
// database without tables has no trigger to lose, and nothing is probed.
func (m *MySQL) definerProbe(ctx context.Context, c *sql.Conn, db, target string, tables []string, items []probeItem) error {
	for _, it := range items {
		clause := definerAny().FindString(it.stmt)
		security := ""
		if it.kind == "view" {
			security = " SQL SECURITY DEFINER"
			if sqlSecurityInvoker().MatchString(it.stmt) {
				security = " SQL SECURITY INVOKER"
			}
		}
		key := db + "\x00" + it.kind + "\x00" + clause + security
		m.probeMu.Lock()
		done := m.probed[key]
		m.probeMu.Unlock()
		if done {
			continue
		}
		if err := probeOnce(ctx, c, db, target, tables, it, clause, security); err != nil {
			return err
		}
		m.probeMu.Lock()
		if m.probed == nil {
			m.probed = map[string]bool{}
		}
		m.probed[key] = true
		m.probeMu.Unlock()
	}
	return nil
}

var sqlSecurityInvoker = lazy.Regexp(`\bSQL SECURITY INVOKER\b`)

// probeOnce makes and drops one probe of it in db. The probe is dropped
// whatever happens, also when ctx is cancelled; one that a killed process
// leaves behind is dropped by the next seed or restore (probeName).
func probeOnce(ctx context.Context, c *sql.Conn, db, target string, tables []string, it probeItem, clause, security string) error {
	b := make([]byte, 8)
	rand.Read(b)
	name := qi(db) + "." + qi("dgt_probe_"+hex.EncodeToString(b))
	var stmt string
	switch it.kind {
	case "trigger":
		on := name
		_, err := c.ExecContext(ctx, "CREATE TABLE "+name+" (id INT PRIMARY KEY)")
		var me *mysql.MySQLError
		switch {
		case err == nil:
			defer c.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+name) // and its trigger
		case errors.As(err, &me) && (me.Number == 1142 || me.Number == 1044): // no CREATE
			if len(tables) == 0 {
				return nil
			}
			on = qi(db) + "." + qi(tables[0])
			defer c.ExecContext(context.WithoutCancel(ctx), "DROP TRIGGER IF EXISTS "+name)
		default:
			return fmt.Errorf("trigger %s: a probe of its DEFINER: %w", it.name, err)
		}
		stmt = "CREATE " + clause + " TRIGGER " + name + " BEFORE INSERT ON " + on + " FOR EACH ROW BEGIN END"
	case "view":
		defer c.ExecContext(context.WithoutCancel(ctx), "DROP VIEW IF EXISTS "+name)
		stmt = "CREATE " + clause + security + " VIEW " + name + " AS SELECT 1 AS probe"
	default:
		return fmt.Errorf("no probe for a %s", it.kind)
	}
	if _, err := c.ExecContext(ctx, stmt); err != nil {
		return createRefused(ctx, c, it.kind, it.name, target, it.stmt, err)
	}
	return nil
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
