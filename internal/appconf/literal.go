package appconf

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// code stands for a value only running the program could give: a function
// call, a variable, a constant, string interpolation.
type code struct {
	src string // the expression, as written (value)
}

// literal reads the array literals of PHP ([…], array(…), 'k' => v) and
// the dict and list literals of Python ({'k': v}, […]) without running
// anything. Strings, numbers, true/false/null (True/False/None) come back
// as strings ("" for null), arrays and dicts as map[string]any (a list's
// keys are its indexes), anything else as code.
type literal struct {
	s  string
	i  int
	py bool
}

const errLiteral = constError("not a literal the file can be read as (the content is not shown)")

func (l *literal) space() {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			l.i++
		case c == '#' || (!l.py && strings.HasPrefix(l.s[l.i:], "//")):
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case !l.py && strings.HasPrefix(l.s[l.i:], "/*"):
			end := strings.Index(l.s[l.i+2:], "*/")
			if end < 0 {
				l.i = len(l.s)
				return
			}
			l.i += end + 4
		default:
			return
		}
	}
}

// value reads one value and, when an operator follows it (a . or + of a
// concatenation, a call), takes the whole expression for code. A Python
// statement ends at the end of its line.
func (l *literal) value(depth int) (any, error) {
	if depth > 64 {
		return nil, errLiteral
	}
	l.space()
	start := l.i
	v, err := l.primary(depth)
	if err != nil {
		return nil, err
	}
	if _, ok := v.(code); ok {
		v = code{src: strings.TrimSpace(l.s[start:l.i])}
	}
	if l.py && depth == 0 {
		for l.i < len(l.s) && (l.s[l.i] == ' ' || l.s[l.i] == '\t' || l.s[l.i] == '\r') {
			l.i++
		}
		if l.i >= len(l.s) || l.s[l.i] == '\n' || l.s[l.i] == '#' || l.s[l.i] == ';' {
			return v, nil
		}
	}
	l.space()
	if l.i < len(l.s) && !strings.ContainsRune(",)]};:", rune(l.s[l.i])) && !strings.HasPrefix(l.s[l.i:], "=>") {
		if err := l.skipExpr(); err != nil {
			return nil, err
		}
		return code{src: strings.TrimSpace(l.s[start:l.i])}, nil
	}
	return v, nil
}

func (l *literal) primary(depth int) (any, error) {
	l.space()
	if l.i >= len(l.s) {
		return nil, errLiteral
	}
	c := l.s[l.i]
	switch {
	case c == '[':
		l.i++
		return l.items(']', depth)
	case c == '{' && l.py:
		l.i++
		return l.items('}', depth)
	case c == '(' && l.py: // a tuple, or parentheses
		l.i++
		return l.items(')', depth)
	case !l.py && len(l.s)-l.i >= 6 && strings.EqualFold(l.s[l.i:l.i+5], "array") && strings.HasPrefix(strings.TrimLeft(l.s[l.i+5:], " \t\r\n"), "("):
		l.i += 5
		l.space()
		l.i++
		return l.items(')', depth)
	case c == '\'' || c == '"':
		return l.str()
	case l.py && (c == 'r' || c == 'R' || c == 'u' || c == 'U' || c == 'b' || c == 'B' || c == 'f' || c == 'F') && l.i+1 < len(l.s) && (l.s[l.i+1] == '\'' || l.s[l.i+1] == '"'):
		l.i++
		s, err := l.str()
		if err != nil || c == 'f' || c == 'F' {
			return code{}, err
		}
		return s, nil
	case c == '-' || '0' <= c && c <= '9':
		start := l.i
		l.i++
		for l.i < len(l.s) && (isNameByte(l.s[l.i]) || l.s[l.i] == '.') {
			l.i++
		}
		return l.s[start:l.i], nil
	}
	start := l.i
	for l.i < len(l.s) && (isNameByte(l.s[l.i]) || l.s[l.i] == '\\') {
		l.i++
	}
	switch word := l.s[start:l.i]; {
	case word == "":
		if err := l.skipExpr(); err != nil {
			return nil, err
		}
		return code{}, nil
	case l.py && word == "True", !l.py && strings.EqualFold(word, "true"):
		return "true", nil
	case l.py && word == "False", !l.py && strings.EqualFold(word, "false"):
		return "false", nil
	case l.py && word == "None", !l.py && strings.EqualFold(word, "null"):
		return "", nil
	}
	if err := l.skipExpr(); err != nil {
		return nil, err
	}
	return code{}, nil
}

// items reads the items of an array, a dict or a list up to end.
func (l *literal) items(end byte, depth int) (any, error) {
	out := map[string]any{}
	n := 0
	for {
		l.space()
		if l.i >= len(l.s) {
			return nil, errLiteral
		}
		if l.s[l.i] == end {
			l.i++
			return out, nil
		}
		v, err := l.value(depth + 1)
		if err != nil {
			return nil, err
		}
		l.space()
		key := fmt.Sprint(n)
		switch {
		case !l.py && strings.HasPrefix(l.s[l.i:], "=>"), l.py && end == '}' && l.i < len(l.s) && l.s[l.i] == ':':
			if l.s[l.i] == '=' {
				l.i += 2
			} else {
				l.i++
			}
			k, ok := v.(string)
			if !ok {
				k = "\x00code"
			}
			key = k
			if v, err = l.value(depth + 1); err != nil {
				return nil, err
			}
			l.space()
		default:
			n++
		}
		out[key] = v
		switch {
		case l.i < len(l.s) && l.s[l.i] == ',':
			l.i++
		case l.i < len(l.s) && l.s[l.i] == end:
		default: // ; or a bracket that closes nothing here
			return nil, errLiteral
		}
	}
}

// str reads a quoted string. A PHP "…" with a $ in it interpolates, so it
// is code; so is a Python string next to another (implicit concatenation
// is rare in settings and left out).
func (l *literal) str() (any, error) {
	q := l.s[l.i]
	if l.py && strings.HasPrefix(l.s[l.i:], strings.Repeat(string(q), 3)) {
		end := strings.Index(l.s[l.i+3:], strings.Repeat(string(q), 3))
		if end < 0 {
			return nil, errLiteral
		}
		v := l.s[l.i+3 : l.i+3+end]
		l.i += end + 6
		return v, nil
	}
	l.i++
	var b strings.Builder
	interpolated := false
	for {
		if l.i >= len(l.s) {
			return nil, errLiteral
		}
		c := l.s[l.i]
		if c == q {
			l.i++
			break
		}
		if c == '\\' && l.i+1 < len(l.s) {
			e := l.s[l.i+1]
			l.i += 2
			switch {
			case e == q || e == '\\':
				b.WriteByte(e)
			case q == '\'' && !l.py: // PHP '…' knows only \' and \\
				b.WriteByte('\\')
				b.WriteByte(e)
			case e == 'n':
				b.WriteByte('\n')
			case e == 't':
				b.WriteByte('\t')
			case e == 'r':
				b.WriteByte('\r')
			case e == '$' && !l.py:
				b.WriteByte('$')
			default:
				b.WriteByte('\\')
				b.WriteByte(e)
			}
			continue
		}
		if c == '$' && q == '"' && !l.py {
			interpolated = true
		}
		b.WriteByte(c)
		l.i++
	}
	if interpolated {
		return code{}, nil
	}
	return b.String(), nil
}

// skipExpr skips to the end of the expression l is in: the next , or
// closing bracket at its own depth, past strings and nested brackets.
func (l *literal) skipExpr() error {
	depth := 0
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == '\'' || c == '"':
			if _, err := l.str(); err != nil {
				return err
			}
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth == 0 {
				return nil
			}
			depth--
		case (c == ',' || c == ';') && depth == 0:
			return nil
		case c == '=' && depth == 0 && strings.HasPrefix(l.s[l.i:], "=>"):
			return nil
		case c == '\n' && depth == 0 && l.py:
			return nil
		}
		l.i++
	}
	return nil
}

// at follows a dotted path of keys through nested maps.
func at(v any, keyPath string) (any, bool) {
	for _, k := range strings.Split(keyPath, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// literalFields turns a map of a PHP or Python literal into fields; a key
// whose value is code is listed in coded.
func literalFields(m map[string]any) (f fields, coded []string) {
	f = fields{}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			f[fieldKey(k)] = t
		case code:
			coded = append(coded, k)
		}
	}
	return f, coded
}

// readPHP reads Magento's app/etc/env.php: the array it returns, at key
// (default db.connection.default) with host (also host:port or a socket),
// dbname, username and password; model mysql4 is MySQL.
func readPHP(root, file, key string) (parts, error) {
	raw, err := readFile(root, file)
	if err != nil {
		return parts{}, err
	}
	s := string(raw)
	i := strings.Index(s, "return")
	if i < 0 {
		return parts{}, fmt.Errorf("%s: no return of an array", file)
	}
	l := &literal{s: s, i: i + len("return")}
	v, err := l.value(0)
	if err != nil {
		return parts{}, fmt.Errorf("%s: %w", file, err)
	}
	if key == "" {
		key = "db.connection.default"
	}
	return literalSetting(v, file, key, "PHP")
}

// readPython reads Django's DATABASES dict of a settings file, at key
// (default default) with ENGINE, NAME, USER, PASSWORD, HOST and PORT.
func readPython(root, file, key string) (parts, error) {
	raw, err := readFile(root, file)
	if err != nil {
		return parts{}, err
	}
	v, err := djangoDatabases(string(raw))
	if err != nil {
		return parts{}, fmt.Errorf("%s: %w", file, err)
	}
	if key == "" {
		key = "default"
	}
	return literalSetting(v, file, key, "Python")
}

// djangoDatabases reads DATABASES = {…} at the start of a line; of
// several, the last one counts, as when Python runs the file.
func djangoDatabases(s string) (any, error) {
	var v any
	var err error
	found := false
	for i := 0; ; i += len("DATABASES") {
		j := strings.Index(s[i:], "DATABASES")
		if j < 0 {
			break
		}
		i += j
		if i == 0 || s[i-1] == '\n' {
			rest := strings.TrimLeft(s[i+len("DATABASES"):], " \t")
			if strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==") {
				l := &literal{s: s, i: len(s) - len(rest) + 1, py: true}
				v, err = l.value(0)
				found = true
			}
		}
	}
	if !found {
		return nil, errors.New("no DATABASES = {…}")
	}
	return v, err
}

func literalSetting(v any, file, key, lang string) (parts, error) {
	if _, ok := v.(code); ok {
		return parts{}, fmt.Errorf("%s: the settings are computed by %s code; DriftGate does not run it", file, lang)
	}
	node, ok := at(v, key)
	if !ok {
		return parts{}, fmt.Errorf("%s: %s is not set", file, key)
	}
	m, ok := node.(map[string]any)
	if !ok {
		return parts{}, fmt.Errorf("%s: %s is not an array of settings, or is computed by %s code", file, key, lang)
	}
	f, coded := literalFields(m)
	slices.Sort(coded)
	for _, k := range coded {
		if fieldKey(k) == "name" && sqliteEngine(f) {
			continue // BASE_DIR / 'db.sqlite3' (resolveDjango)
		}
		switch fieldKey(k) {
		case "host", "port", "name", "dbname", "username", "user", "password", "engine", "url":
			return parts{}, fmt.Errorf("%s: %s.%s is computed by %s code; DriftGate does not run it (put the DSN in an environment variable and use dsn: { env: NAME })", file, key, printableName(k), lang)
		}
	}
	return fromFields(f)
}
