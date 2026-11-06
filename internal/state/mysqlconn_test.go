package state

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// fakeExec runs a new connection's SET statements: fail returns the error
// for a statement, or nil.
type fakeExec struct {
	stmts []string
	fail  func(stmt string) error
}

func (f *fakeExec) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	f.stmts = append(f.stmts, q)
	if err := f.fail(q); err != nil {
		return nil, err
	}
	return driver.RowsAffected(0), nil
}

// TestInitConnLogBinRefused: a server that refuses sql_log_bin = 0 for any
// reason, not only for want of the privilege (1227), leaves the copies in
// the binary log with one note, and new connections are still made, and
// no longer asked for it.
func TestInitConnLogBinRefused(t *testing.T) {
	for _, number := range []uint16{1227, 1231, 1238} {
		var log strings.Builder
		m := &MySQL{o: Options{Name: "db", Log: &log}, early: map[driver.Conn]bool{}, logBin: logBinOff}
		ex := &fakeExec{fail: func(q string) error {
			if strings.Contains(q, "sql_log_bin") {
				return &mysql.MySQLError{Number: number, Message: "refused"}
			}
			return nil
		}}
		for range 2 {
			if err := m.initConn(context.Background(), ex, nil); err != nil {
				t.Fatalf("%d: %v", number, err)
			}
		}
		if m.logBin != logBinOn || strings.Count(log.String(), "restores are written to the binary log") != 1 {
			t.Fatalf("%d: logBin %d, log %q", number, m.logBin, log.String())
		}
		// The first connection tried it once, the second did not.
		if len(ex.stmts) != 3 || strings.Contains(ex.stmts[1], "sql_log_bin") || strings.Contains(ex.stmts[2], "sql_log_bin") {
			t.Fatalf("%d: statements %q", number, ex.stmts)
		}
	}
	// An error of the settings themselves still fails the connection.
	m := &MySQL{o: Options{Name: "db"}, early: map[driver.Conn]bool{}, logBin: logBinOn}
	ex := &fakeExec{fail: func(string) error { return &mysql.MySQLError{Number: 1231, Message: "bad sql_mode"} }}
	if err := m.initConn(context.Background(), ex, nil); err == nil {
		t.Fatal("a refused session setting did not fail the connection")
	}
}
