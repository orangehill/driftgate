package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// A query of a params sql runs on the pristine copy, which is read-only, in
// a transaction that is rolled back. Neither is a boundary on its own: a
// read-only database still runs EXEC xp_cmdshell, OPENROWSET reads server
// files and other servers, and NEXT VALUE FOR moves a sequence outside the
// transaction. So the query is read before it is sent, by T-SQL's own
// rules (N'…' texts, [names] with ]], "names" with ""): one SELECT (or WITH
// … SELECT) statement, without comments, without INTO, without any other
// statement's keyword (T-SQL needs no semicolon between statements:
// "SELECT 1 DROP TABLE x" is two), without NEXT VALUE FOR, and without the
// system procedures and functions that reach files, programs, other
// servers, other sessions or logins (sp_, xp_, fn_, dm_, OPENROWSET …).
// The login's own permissions remain the real limit (docs/security.md).

// sqlServerDenied may not appear as a bare word anywhere in a query: the
// statements of T-SQL other than SELECT, and words that only those use.
var sqlServerDenied = map[string]bool{
	"into": true, "insert": true, "update": true, "delete": true, "merge": true, "truncate": true,
	"create": true, "alter": true, "drop": true, "exec": true, "execute": true, "declare": true, "set": true,
	"grant": true, "revoke": true, "deny": true, "backup": true, "restore": true, "dbcc": true, "kill": true,
	"shutdown": true, "waitfor": true, "bulk": true, "use": true, "begin": true, "commit": true, "rollback": true,
	"save": true, "transaction": true, "tran": true, "reconfigure": true, "checkpoint": true, "readtext": true,
	"writetext": true, "updatetext": true, "raiserror": true, "throw": true, "print": true, "goto": true,
	"return": true, "while": true, "if": true, "open": true, "close": true, "fetch": true, "deallocate": true,
	"cursor": true, "receive": true, "send": true, "move": true, "get": true, "setuser": true, "revert": true,
	"enable": true, "disable": true, "external": true,
}

// sqlServerSystem are system functions and views a query may not name,
// bare or quoted: they read server files and other servers, and logins'
// passwords; sqlServerSystemPrefixes start the names of system procedures
// and of the functions and views that read files, the log, traces or
// other sessions.
var (
	sqlServerSystem = map[string]bool{
		"openrowset": true, "opendatasource": true, "openquery": true, "openxml": true, "pwdencrypt": true, "pwdcompare": true,
		"sql_logins": true, "syslogins": true, "sysxlogins": true, "linked_logins": true, "credentials": true,
		"database_scoped_credentials": true, "master_key_passwords": true, "server_principal_credentials": true,
	}
	sqlServerSystemPrefixes = []string{"sp_", "xp_", "fn_", "dm_"}
)

// checkSQLServerQuery accepts a single read-only SELECT or WITH statement
// of T-SQL (see above).
func checkSQLServerQuery(query string) error {
	q := strings.TrimSpace(query)
	q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	if q == "" {
		return errors.New("sql: empty query")
	}
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c == '@' || c == '#' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
	}
	nextNonSpace := func(i int) byte {
		for ; i < len(q); i++ {
			if !strings.ContainsRune(" \t\r\n\f\v", rune(q[i])) {
				return q[i]
			}
		}
		return 0
	}
	first, prev := "", ""
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := i + 1
			for {
				k := strings.IndexByte(q[j:], end)
				if k < 0 {
					return errors.New("sql: unterminated quoted text or name")
				}
				j += k + 1
				if j < len(q) && q[j] == end { // doubled: part of the text
					j++
					continue
				}
				break
			}
			if c != '\'' && nextNonSpace(j) == '(' {
				return errors.New("sql: a quoted name cannot be called as a function")
			}
			if c != '\'' {
				if err := sqlServerName(strings.ToLower(q[i+1 : j-1])); err != nil {
					return err
				}
			}
			prev = ""
			i = j
		case c == ';':
			return errors.New("sql: only one statement is accepted")
		case c == '-' && strings.HasPrefix(q[i:], "--"), c == '/' && strings.HasPrefix(q[i:], "/*"):
			return errors.New("sql: comments are not accepted")
		case c == '_' || c == '@' || c == '#' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
			j := i
			for j < len(q) && isIdent(q[j]) {
				j++
			}
			word := strings.ToLower(q[i:j])
			if first == "" {
				first = word
			}
			if prev == "next" && word == "value" {
				return errors.New("sql: NEXT VALUE FOR is not accepted; it moves the sequence")
			}
			if sqlServerDenied[word] {
				return fmt.Errorf("sql: %s is not accepted; only a read-only SELECT is", strings.ToUpper(word))
			}
			if err := sqlServerName(word); err != nil {
				return err
			}
			prev = word
			i = j
		case c >= '0' && c <= '9':
			for i < len(q) && (isIdent(q[i]) || q[i] == '.') { // a number, 1e5, 0x1F
				i++
			}
			prev = ""
		default:
			if !strings.ContainsRune(" \t\r\n\f\v", rune(c)) {
				prev = ""
			}
			i++
		}
	}
	if first != "select" && first != "with" {
		return errors.New("sql: only a SELECT (or WITH … SELECT) statement is accepted")
	}
	return nil
}

// sqlServerName refuses the name (lower case) of a system object a
// query may not use (sqlServerSystem).
func sqlServerName(name string) error {
	denied := sqlServerSystem[name]
	for _, p := range sqlServerSystemPrefixes {
		denied = denied || strings.HasPrefix(name, p)
	}
	if denied {
		return fmt.Errorf("sql: %s is not accepted (system procedures, functions and views that reach files, other servers, sessions or logins are not)", name)
	}
	return nil
}

// Query runs a read-only query (see checkSQLServerQuery) on the pristine
// database, in a transaction that is rolled back.
func (s *SQLServer) Query(ctx context.Context, query string) ([]string, error) {
	if err := checkSQLServerQuery(query); err != nil {
		return nil, err
	}
	if _, err := s.server(ctx); err != nil {
		return nil, err
	}
	d, err := s.database(ctx, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("pristine database %s does not exist (run driftgate seed)", s.o.Pristine)
	}
	c, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return nil, fmt.Errorf("pristine database %s: %w (run driftgate seed)", s.o.Pristine, err)
	}
	defer c.Close()
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return firstColumn(rows)
}
