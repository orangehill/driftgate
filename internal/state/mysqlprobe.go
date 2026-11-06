package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

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
func (m *MySQL) definerProbe(ctx context.Context, c *sql.Conn, db, target string, items []probeItem) error {
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
		if err := probeOnce(ctx, c, db, target, it, clause, security); err != nil {
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
func probeOnce(ctx context.Context, c *sql.Conn, db, target string, it probeItem, clause, security string) error {
	b := make([]byte, 8)
	rand.Read(b)
	name := qi(db) + "." + qi("dgt_probe_"+hex.EncodeToString(b))
	var stmt string
	switch it.kind {
	case "trigger":
		defer c.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+name) // and its trigger
		if _, err := c.ExecContext(ctx, "CREATE TABLE "+name+" (id INT PRIMARY KEY)"); err != nil {
			return fmt.Errorf("trigger %s: a probe of its DEFINER: %w", it.name, err)
		}
		stmt = "CREATE " + clause + " TRIGGER " + name + " BEFORE INSERT ON " + name + " FOR EACH ROW SET @driftgate_probe = 1"
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
