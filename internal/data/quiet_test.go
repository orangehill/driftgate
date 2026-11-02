package data

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestQuietError: a database error keeps its number and the names of the
// key or column, never the value it quotes.
func TestQuietError(t *testing.T) {
	for _, tc := range []struct {
		err       error
		want      string
		secret    string
		mysqlCode uint16
	}{
		{&mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'pera@example.com' for key 'users.email'"}, "Duplicate entry '…' for key 'users.email'", "pera@", 1062},
		{&mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'it' for key 'x' for key 'users.email'"}, "Duplicate entry '…' for key 'users.email'", "'it'", 1062},
		{&mysql.MySQLError{Number: 1366, Message: "Incorrect integer value: 'Pera Perić' for column `db`.`t`.`age` at row 1"}, "Incorrect integer value: '…' for column `db`.`t`.`age` at row 1", "Pera", 1366},
		{&mysql.MySQLError{Number: 1292, Message: "Incorrect datetime value: '1990-13-01' for column 'born' at row 3"}, "value: '…' for column 'born'", "1990", 1292},
		{&mysql.MySQLError{Number: 1300, Message: "Invalid utf8mb4 character string: 'F09F98'"}, "string: '…'", "F09F", 1300},
		{fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}), "Deadlock found", "", 1213},
		{&pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type integer: "Pera Perić"`}, `integer: "…" (SQLSTATE 22P02)`, "Pera", 0},
		{&pgconn.PgError{Severity: "ERROR", Code: "22003", Message: `value "123456789012" is out of range for type integer`}, `value "…" is out of range for type integer`, "1234", 0},
		{&pgconn.PgError{Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "users_email_key"`, Detail: "Key (email)=(pera@example.com) already exists."},
			`unique constraint "users_email_key"`, "pera@", 0},
	} {
		got := quietError(tc.err)
		if !strings.Contains(got.Error(), tc.want) || (tc.secret != "" && strings.Contains(got.Error(), tc.secret)) {
			t.Errorf("%v → %v", tc.err, got)
		}
		var pe *pgconn.PgError
		if errors.As(got, &pe) && pe.Detail != "" {
			t.Errorf("%v keeps the detail", got)
		}
		var me *mysql.MySQLError
		if tc.mysqlCode != 0 && (!errors.As(got, &me) || me.Number != tc.mysqlCode) {
			t.Errorf("%v lost its number", got)
		}
	}
}

// TestExecTargetQuotesNoValue: a duplicate row on a real server gives an
// error with the key, not the value.
func TestExecTargetQuotesNoValue(t *testing.T) {
	var servers []*server
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		servers = append(servers, newServer(t, MySQL, strings.Split(v, ",")[0]))
	}
	if dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES"); dsn != "" {
		servers = append(servers, newServer(t, Postgres, dsn))
	}
	if len(servers) == 0 {
		t.Skip("DRIFTGATE_TEST_MYSQL and DRIFTGATE_TEST_POSTGRES not set")
	}
	for _, s := range servers {
		t.Run(s.driver, func(t *testing.T) {
			const name = "dg_quiet_error"
			s.create(t, name)
			t.Cleanup(func() { s.drop(name) })
			s.exec(t, name, "CREATE TABLE users (email VARCHAR(100) PRIMARY KEY, age INT)")
			d, err := open(s.db(name), name)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx := context.Background()
			c, err := d.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := execTarget(ctx, c, Stmt{SQL: "INSERT INTO users (email, age) VALUES ('pera.peric@example.com', 1)"}); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				"INSERT INTO users (email, age) VALUES\n('pera.peric@example.com', 2)",
				"INSERT INTO users (email, age) VALUES\n('y', 'Pera Peric')",
			} {
				err := execTarget(ctx, c, Stmt{SQL: q})
				if err == nil || strings.Contains(err.Error(), "pera") || strings.Contains(err.Error(), "Pera") {
					t.Errorf("%s: %v", q, err)
				}
				t.Log(err)
			}
		})
	}
}
