package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// A connection is prepared once, when it is made (initConn), not each time
// it is taken from the pool: one SET statement with every setting of
// mysqlSession, and sql_log_bin = 0 once unlogged has decided so. Over a
// link with latency every statement costs a round trip, and a fingerprint
// or a restore takes several connections.
//
// What changes a connection's session afterwards sets it back, or closes the
// connection when it cannot (discardConn): a connection goes back to the pool
// as it was prepared.

// What new connections do with the binary log (MySQL.logBin).
const (
	logBinUndecided = iota // unlogged has not decided yet
	logBinOff              // sql_log_bin = 0
	logBinOn               // logged: something reads the log, or sql_log_bin cannot be set
)

// sessionSet is the SET statement that prepares a connection: mysqlSession,
// information_schema_stats_expiry = 0 with fresh, sql_log_bin = 0 with
// unlog. A SET that fails changes no variable.
func sessionSet(fresh, unlog bool) string {
	parts := mysqlSession.Assignments()
	if fresh && mysqlSession.FreshStats {
		parts = append(parts, "information_schema_stats_expiry = 0")
	}
	if unlog {
		parts = append(parts, "sql_log_bin = 0")
	}
	return "SET SESSION " + strings.Join(parts, ", ")
}

// unknownVariable reports the server's error for a variable it does not
// have (MariaDB: information_schema_stats_expiry).
func unknownVariable(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1193
}

func serverError(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me)
}

// initConn prepares a new connection of the pool (dbx.MySQLOpen.Init). The
// settings the server refuses are left out from then on: the statistics
// setting a server does not know (as mysqlSession.Apply ignores it), and
// sql_log_bin without the privilege for it, with session's one note.
func (m *MySQL) initConn(ctx context.Context, ex driver.ExecerContext, conn driver.Conn) error {
	m.connMu.Lock()
	fresh, logBin := !m.noFreshStats, m.logBin
	if logBin == logBinUndecided {
		m.early[conn] = true
	}
	m.connMu.Unlock()
	unlog := logBin == logBinOff
	for {
		_, err := ex.ExecContext(ctx, sessionSet(fresh, unlog), nil)
		switch {
		case err == nil:
			return nil
		case fresh && unknownVariable(err):
			fresh = false
			m.connMu.Lock()
			m.noFreshStats = true
			m.connMu.Unlock()
		case unlog && serverError(err):
			unlog = false
			m.setLogBin(logBinOn)
			m.binlog.Do(func() {
				m.logf("state %s: restores are written to the binary log (SET sql_log_bin = 0: %v)\n", m.o.Name, err)
			})
		default:
			m.connMu.Lock()
			delete(m.early, conn)
			m.connMu.Unlock()
			return err
		}
	}
}

// initMulti prepares a new connection of m.multi, which only reads SHOW
// CREATE: the same session as the pool's (its output depends on sql_mode),
// with nothing to keep out of the binary log.
func (m *MySQL) initMulti(ctx context.Context, ex driver.ExecerContext, _ driver.Conn) error {
	m.connMu.Lock()
	fresh := !m.noFreshStats
	m.connMu.Unlock()
	_, err := ex.ExecContext(ctx, sessionSet(fresh, false), nil)
	if err != nil && fresh && unknownVariable(err) {
		m.connMu.Lock()
		m.noFreshStats = true
		m.connMu.Unlock()
		_, err = ex.ExecContext(ctx, sessionSet(false, false), nil)
	}
	return err
}

// setLogBin records what new connections do with the binary log; the
// connections made before the decision are then known.
func (m *MySQL) setLogBin(v int) {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	m.logBin = v
	if v != logBinOff {
		clear(m.early)
	}
}

// madeEarly reports, once, whether c was made before unlogged decided:
// initConn did not set sql_log_bin on it.
func (m *MySQL) madeEarly(c *sql.Conn) bool {
	m.connMu.Lock()
	n := len(m.early)
	m.connMu.Unlock()
	if n == 0 {
		return false
	}
	var key driver.Conn
	c.Raw(func(dc any) error {
		key, _ = dc.(driver.Conn)
		return nil
	})
	m.connMu.Lock()
	defer m.connMu.Unlock()
	if !m.early[key] {
		return false
	}
	delete(m.early, key)
	return true
}

// discardConn closes c instead of putting it back into the pool: its session
// could not be set back.
func discardConn(c *sql.Conn) {
	c.Raw(func(any) error { return driver.ErrBadConn })
	c.Close()
}
