package data

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DumpFormat is the version of the dump format; it changes when the same
// input would give different bytes. 2: a statement whose last line may end
// in a comment gets its delimiter on a line of its own.
const DumpFormat = 2

// dumpHeader is the first line of a dump; data verify reads no other dump.
func dumpHeader(format int, driver string) string {
	return fmt.Sprintf("-- DriftGate anonymized data, dump format %d (%s)\n", format, driver)
}

// statements reads a dump statement by statement, the way mysql and psql
// split it: quotes, PostgreSQL dollar quotes, comments and the DELIMITER
// lines of MySQL. A statement is at most one INSERT batch (about 1 MiB), so
// memory stays bounded.
type statements struct {
	r      *bufio.Reader
	driver string
	delim  string
	b      []byte
	format int // the dump format its header names; 0: not read
}

func newStatements(r io.Reader, driver string) *statements {
	return &statements{r: bufio.NewReaderSize(r, 1<<20), driver: driver, delim: ";"}
}

// header reads the first line of the dump, which must be the header
// Anonymize writes: the tokenizer follows how mysql and psql split the
// statements this module writes, and a dump of another tool (mysqldump,
// pg_dump's COPY) would not have its rows read as rows.
func (st *statements) header(driver string) error {
	l, err := st.r.ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	// A dump with CRLF line endings (a checkout or an editor on Windows).
	if strings.HasSuffix(l, "\r\n") {
		l = l[:len(l)-2] + "\n"
	}
	for format := 1; format <= DumpFormat; format++ {
		for _, d := range []string{driver, st.driver, "mariadb"} {
			if (d != "mariadb" || st.driver == MySQL) && l == dumpHeader(format, d) {
				st.format = format
				return nil
			}
		}
	}
	return fmt.Errorf("not a dump of driftgate data anonymize or subset for %s (the first line is %.80q)", driver, strings.TrimSuffix(l, "\n"))
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

var errUnterminated = errors.New("the dump ends inside a quoted string or a comment")

var errHiddenDelimiter = errors.New("a statement runs into the next one (a comment at the end of its last line hid its delimiter): make the dump again with this release")

// hidesDelimiter tells whether the line comment l (to its newline) of a
// format 1 dump hid the delimiter the writer put right after a statement,
// so that the statements after it would be read as part of it. The
// comment ends in the delimiter, and what follows is what the writer wrote
// after that delimiter: after ;; (a compound statement) the line
// "DELIMITER ;", after ; the INSERT of rows that starts a line after blank
// and comment lines. Neither follows such a comment inside a statement
// the writer wrote: a trigger may hold INSERTs, but not a line
// "DELIMITER ;" outside a string or comment, and a statement other than a
// compound one holds no INSERT. Format 2 puts the delimiter on a line of
// its own after such a comment; a dump of it is never refused here.
func (st *statements) hidesDelimiter(l string) bool {
	if st.format != 1 || !strings.HasSuffix(strings.TrimRight(l, " \t\r\n"), st.delim) {
		return false
	}
	p, _ := st.r.Peek(1 << 16)
	if st.delim != ";" {
		return bytes.HasPrefix(p, []byte("DELIMITER "))
	}
	for len(p) > 0 {
		switch {
		case p[0] == ' ' || p[0] == '\t' || p[0] == '\r' || p[0] == '\n':
			p = p[1:]
		case bytes.HasPrefix(p, []byte("--")) || (st.driver == MySQL && p[0] == '#'):
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				return false
			}
			p = p[i+1:]
		default:
			return bytes.HasPrefix(p, []byte("INSERT INTO "))
		}
	}
	return false
}

// next returns the next statement without its delimiter, or io.EOF.
func (st *statements) next() (string, error) {
	st.b = st.b[:0]
	for {
		// Between statements: blank lines, comment lines, DELIMITER.
		p, err := st.r.Peek(1)
		if err != nil {
			return "", err
		}
		if c := p[0]; c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			st.r.ReadByte()
			continue
		}
		if p, _ := st.r.Peek(2); string(p) == "--" || (st.driver == MySQL && len(p) > 0 && p[0] == '#') {
			if _, err := st.r.ReadString('\n'); err != nil {
				return "", err
			}
			continue
		}
		if p, _ := st.r.Peek(10); st.driver == MySQL && string(p) == "DELIMITER " {
			l, err := st.r.ReadString('\n')
			if err != nil && err != io.EOF {
				return "", err
			}
			st.delim = strings.TrimSpace(l[len("DELIMITER "):])
			continue
		}
		break
	}
	var quote byte // ' " ` or $ for a dollar quote
	var tag string // the dollar-quote tag, with its dollars
	opened := 0    // where the text of the dollar quote starts
	escapes := false
	read := func() (byte, error) {
		c, err := st.r.ReadByte()
		if err == io.EOF {
			err = errUnterminated
		}
		if err == nil {
			st.b = append(st.b, c)
		}
		return c, err
	}
	for {
		c, err := st.r.ReadByte()
		if err == io.EOF {
			if quote != 0 {
				return "", errUnterminated
			}
			if s := strings.TrimSpace(string(st.b)); s != "" {
				return s, nil
			}
			return "", io.EOF
		}
		if err != nil {
			return "", err
		}
		st.b = append(st.b, c)
		n := len(st.b)
		switch {
		case quote == '$':
			if c == '$' && n-len(tag) >= opened && string(st.b[n-len(tag):]) == tag {
				quote = 0
			}
			continue
		case quote != 0:
			if c == '\\' && escapes {
				if _, err := read(); err != nil {
					return "", err
				}
			} else if c == quote {
				if p, _ := st.r.Peek(1); len(p) == 1 && p[0] == quote {
					read()
				} else {
					quote = 0
				}
			}
			continue
		case c == '\'' || c == '"' || (c == '`' && st.driver == MySQL):
			quote = c
			// MySQL escapes with a backslash in strings; PostgreSQL only in E'…'.
			escapes = c != '`' && (st.driver == MySQL ||
				(c == '\'' && n >= 2 && (st.b[n-2] == 'E' || st.b[n-2] == 'e') && (n < 3 || !isIdentByte(st.b[n-3]))))
			continue
		case c == '$' && st.driver == Postgres && (n < 2 || !isIdentByte(st.b[n-2])):
			// $tag$ or $$; not $1.
			p, _ := st.r.Peek(64)
			if end := strings.IndexByte(string(p), '$'); end >= 0 {
				t := p[:end]
				ok := len(t) == 0 || !(t[0] >= '0' && t[0] <= '9')
				for _, b := range t {
					ok = ok && isIdentByte(b)
				}
				if ok {
					st.b = append(st.b, p[:end+1]...)
					st.r.Discard(end + 1)
					quote, tag, opened = '$', "$"+string(t)+"$", len(st.b)
					continue
				}
			}
		case c == '#' && st.driver == MySQL,
			c == '-' && n >= 2 && st.b[n-2] == '-':
			// A comment to the end of the line (MySQL: # or "-- " with a space).
			if p, _ := st.r.Peek(1); c == '#' || st.driver != MySQL || len(p) == 0 || p[0] == ' ' || p[0] == '\t' || p[0] == '\n' || p[0] == '\r' {
				l, err := st.r.ReadString('\n')
				st.b = append(st.b, l...)
				if err != nil && err != io.EOF {
					return "", err
				}
				if st.hidesDelimiter(l) {
					return "", errHiddenDelimiter
				}
				continue
			}
		case c == '*' && n >= 2 && st.b[n-2] == '/':
			for {
				c, err := read()
				if err != nil {
					return "", err
				}
				if c == '/' && st.b[len(st.b)-2] == '*' {
					break
				}
			}
			continue
		}
		if len(st.b) >= len(st.delim) && string(st.b[len(st.b)-len(st.delim):]) == st.delim {
			s := strings.TrimSpace(string(st.b[:len(st.b)-len(st.delim)]))
			st.b = st.b[:0]
			if s == "" {
				continue
			}
			return s, nil
		}
	}
}

// splitInsert splits an INSERT of rows at its first " VALUES" line end:
// the head (up to VALUES, as dialect.insert writes it) and the rows.
func splitInsert(q string) (head, rows string, ok bool) {
	i, end := strings.Index(q, " VALUES\n"), len(" VALUES\n")
	if j := strings.Index(q, " VALUES\r\n"); j >= 0 && (i < 0 || j < i) {
		i, end = j, len(" VALUES\r\n") // CRLF line endings
	}
	if i < 0 {
		return "", "", false
	}
	return q[:i+len(" VALUES")], q[i+end:], true
}

// parseRows reads the rows of an INSERT as Anonymize writes them:
// (v, v, …),\n(…) with NULL, numbers, '…' strings (MySQL with backslash
// escapes, PostgreSQL with doubled quotes) and MySQL X'…' bytes.
func parseRows(s string, ncols int, driver string, fn func(vals [][]byte)) error {
	mysql := driver != Postgres
	vals := make([][]byte, 0, ncols)
	// Never nil, so that an empty string is never read as NULL (a value
	// is a slice of b).
	b := make([]byte, 0, 256)
	i := 0
	skip := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t') {
			i++
		}
	}
	for {
		skip()
		if i >= len(s) || s[i] != '(' {
			return fmt.Errorf("a row does not start with ( at byte %d", i)
		}
		i++
		vals = vals[:0]
		b = b[:0]
		for {
			skip()
			if i >= len(s) {
				return errors.New("a row is not closed")
			}
			start := len(b)
			switch {
			case strings.HasPrefix(s[i:], "NULL"):
				i += 4
				vals = append(vals, nil)
			case s[i] == '\'':
				i++
				for {
					if i >= len(s) {
						return errors.New("a string is not closed")
					}
					c := s[i]
					if c == '\\' && mysql && i+1 < len(s) {
						i += 2
						switch e := s[i-1]; e {
						case '0':
							b = append(b, 0)
						case 'n':
							b = append(b, '\n')
						case 'r':
							b = append(b, '\r')
						case 'Z':
							b = append(b, 0x1a)
						default:
							b = append(b, e)
						}
						continue
					}
					if c == '\'' {
						if i+1 < len(s) && s[i+1] == '\'' {
							b = append(b, '\'')
							i += 2
							continue
						}
						i++
						break
					}
					b = append(b, c)
					i++
				}
				vals = append(vals, b[start:len(b):len(b)])
			case mysql && strings.HasPrefix(s[i:], "X'"):
				i += 2
				end := strings.IndexByte(s[i:], '\'')
				if end < 0 || end%2 != 0 {
					return errors.New("a hex literal is not closed")
				}
				for j := i; j < i+end; j += 2 {
					hi, lo := unhex(s[j]), unhex(s[j+1])
					if hi < 0 || lo < 0 {
						return errors.New("a hex literal has a non-hex digit")
					}
					b = append(b, byte(hi<<4|lo))
				}
				i += end + 1
				vals = append(vals, b[start:len(b):len(b)])
			default:
				j := i
				for j < len(s) && s[j] != ',' && s[j] != ')' {
					j++
				}
				b = append(b, strings.TrimSpace(s[i:j])...)
				i = j
				vals = append(vals, b[start:len(b):len(b)])
			}
			skip()
			if i >= len(s) {
				return errors.New("a row is not closed")
			}
			if s[i] == ',' {
				i++
				continue
			}
			if s[i] != ')' {
				return fmt.Errorf("unexpected %q in a row", s[i])
			}
			i++
			break
		}
		if len(vals) != ncols {
			return fmt.Errorf("a row has %d values, the table %d columns", len(vals), ncols)
		}
		fn(vals)
		skip()
		if i >= len(s) {
			return nil
		}
		if s[i] != ',' {
			return fmt.Errorf("unexpected %q after a row", s[i])
		}
		i++
	}
}
