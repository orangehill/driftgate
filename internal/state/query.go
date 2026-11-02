package state

import (
	"errors"
	"fmt"
	"strings"
)

// A read-only transaction is not a boundary: MySQL commits it implicitly
// before DDL, PostgreSQL runs COPY … TO PROGRAM and reads server files in
// one, and functions such as query_to_xml run SQL given as text. So a query
// of a params sql is read before it is sent: one SELECT (or WITH … SELECT)
// statement, without comments, without INTO, without data-changing
// statements in a WITH, and without functions that reach the server's files,
// programs, other connections or settings. Anything the reading cannot tell
// apart for certain (a backslash in a quoted text, a dollar-quoted text, a
// quoted name called as a function) is refused too. The database user's own
// privileges remain the real limit (docs/security.md).

// deniedWords may not appear as a bare word anywhere in a query.
var deniedWords = map[string]bool{
	"into": true, "insert": true, "update": true, "delete": true, "merge": true, "upsert": true,
	"truncate": true, "copy": true, "call": true, "execute": true, "procedure": true,
	"outfile": true, "dumpfile": true, "handler": true,
}

// deniedFuncs may not be called; deniedPrefixes cover families of them
// (PostgreSQL's server functions, large objects, dblink).
var (
	deniedFuncs = map[string]bool{
		"query_to_xml": true, "query_to_xmlschema": true, "query_to_xml_and_xmlschema": true,
		"cursor_to_xml": true, "cursor_to_xmlschema": true,
		"set_config": true, "setval": true, "nextval": true,
		"load_file": true, "sleep": true, "benchmark": true,
		"get_lock": true, "release_lock": true, "release_all_locks": true, "is_free_lock": true, "is_used_lock": true,
		"master_pos_wait": true, "source_pos_wait": true, "sys_exec": true, "sys_eval": true,
	}
	deniedPrefixes = []string{"pg_", "lo_", "dblink"}
)

// CheckQuery accepts a single read-only SELECT or WITH statement.
func CheckQuery(query string) error {
	q := strings.TrimSpace(query)
	q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	if q == "" {
		return errors.New("sql: empty query")
	}
	first := ""
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
	}
	nextNonSpace := func(i int) byte {
		for ; i < len(q); i++ {
			if !strings.ContainsRune(" \t\r\n\f", rune(q[i])) {
				return q[i]
			}
		}
		return 0
	}
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for {
				k := strings.IndexByte(q[j:], c)
				if k < 0 {
					return errors.New("sql: unterminated quoted text")
				}
				j += k + 1
				if j < len(q) && q[j] == c { // a doubled quote stays in the text
					j++
					continue
				}
				break
			}
			if strings.ContainsRune(q[i:j], '\\') {
				// MySQL reads a backslash as an escape, PostgreSQL does not:
				// where the text ends would depend on the server.
				return errors.New("sql: a backslash in quoted text is not accepted")
			}
			if c != '\'' && nextNonSpace(j) == '(' {
				return errors.New("sql: a quoted name cannot be called as a function")
			}
			i = j
		case c == ';':
			return errors.New("sql: only one statement is accepted")
		case c == '#', c == '-' && strings.HasPrefix(q[i:], "--"), c == '/' && strings.HasPrefix(q[i:], "/*"):
			return errors.New("sql: comments are not accepted")
		case c == '$':
			return errors.New("sql: $ (dollar-quoted text or a parameter) is not accepted")
		case c == '\\':
			return errors.New("sql: a backslash is not accepted")
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
			j := i
			for j < len(q) && isIdent(q[j]) {
				j++
			}
			word := strings.ToLower(q[i:j])
			if first == "" {
				first = word
			}
			if deniedWords[word] {
				return fmt.Errorf("sql: %s is not accepted; only a read-only SELECT is", strings.ToUpper(word))
			}
			if nextNonSpace(j) == '(' {
				denied := deniedFuncs[word]
				for _, p := range deniedPrefixes {
					denied = denied || strings.HasPrefix(word, p)
				}
				if denied {
					return fmt.Errorf("sql: the function %s is not accepted", word)
				}
			}
			i = j
		case c >= '0' && c <= '9':
			for i < len(q) && isIdent(q[i]) && q[i] != '$' { // a number, 1e5 or 0x1F
				i++
			}
		default:
			i++
		}
	}
	if first != "select" && first != "with" {
		return errors.New("sql: only a SELECT (or WITH … SELECT) statement is accepted")
	}
	return nil
}
