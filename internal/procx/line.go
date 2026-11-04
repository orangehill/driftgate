package procx

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
)

// Line is a command (argv) as one line, for messages and logs: an argument
// with a space, quote or backslash (or an empty one) quoted, and every
// password, secret, token, key, cookie or signature in it shown as ***, in
// an argument of its own and inside a shell script (sh -c "…") alike.
//
// DriftGate runs argv as it is, never through a shell: a $ in an argument
// is part of the value. Only in a script that a shell runs (the argument
// after sh -c, bash -lc …) is $NAME or ${NAME} a reference to a variable,
// which holds no value and stays; anything else in a secret's place ($ in
// single quotes or escaped, a value around a reference, $(command)) is ***.
// What is not a secret stays as written; when in doubt, a value is hidden.
func Line(argv []string) string {
	ws := make([]word, len(argv))
	for i, a := range argv {
		ws[i] = word{raw: a, value: a}
	}
	shown := redactWords(ws)
	parts := make([]string, len(argv))
	for i, a := range shown {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			a = strconv.Quote(a)
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// word is an argument of a command: as written (in a script, with its
// quotes) and as the program receives it. In a script's word, a $ that the
// shell expands is refMark in value.
type word struct {
	raw, value string
	op         bool // an operator of a script: ; & | && || ( ) < > or a newline
	space      bool // what separates words in a script
}

const refMark = "\x00"

// redactWords returns every word as it is shown: as written when it holds
// no secret, its value with the secrets as *** otherwise.
func redactWords(ws []word) []string {
	out := make([]string, len(ws))
	start := 0 // the first word of the simple command
	var prev *word
	for i := range ws {
		w := &ws[i]
		out[i] = w.raw
		if w.space {
			continue
		}
		if w.op {
			if w.value != "<" && w.value != ">" && w.value != ">>" {
				start = i + 1
			}
			prev = nil
			continue
		}
		cmd := words(ws[start:i])
		v := w.value
		var shown string
		if isScript(cmd) {
			shown = redactScript(strings.ReplaceAll(v, refMark, "$"))
		} else {
			p := ""
			if prev != nil {
				p = prev.value
			}
			shown = redactWord(cmd, p, v)
		}
		if shown != v {
			out[i] = render(w, shown)
		}
		prev = w
	}
	return out
}

// words are the values of the words, without spaces and operators.
func words(ws []word) []string {
	var out []string
	for _, w := range ws {
		if !w.space && !w.op {
			out = append(out, w.value)
		}
	}
	return out
}

// render shows a word whose value changed: plain in an argument of its own
// (Line quotes it), in double quotes inside a script when it needs them.
func render(w *word, v string) string {
	v = strings.ReplaceAll(v, refMark, "$")
	if w.raw == w.value || !strings.ContainsAny(v, " \t\n\"'\\;&|<>()`") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`")
	return `"` + r.Replace(v) + `"`
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "mksh": true, "su": true}

var scriptFlag = lazy.Regexp(`^-[a-z]*c[a-z]*$`)

// isScript reports whether the next word of the command (the words before
// it) is a script a shell runs: sh -c, bash -lc, docker exec x sh -c.
func isScript(cmd []string) bool {
	n := len(cmd)
	if n < 2 || !scriptFlag().MatchString(cmd[n-1]) {
		return false
	}
	for _, a := range cmd[:n-1] {
		if shells[filepath.Base(a)] {
			return true
		}
	}
	return false
}

// redactScript shows a shell script with its secrets as ***.
func redactScript(s string) string {
	ws := lex(s)
	var b strings.Builder
	for _, a := range redactWords(ws) {
		b.WriteString(a)
	}
	return b.String()
}

// lex splits a shell script into words, the spaces between them and its
// operators, enough to find secrets: quotes and escapes are removed from a
// word's value and a $ that the shell expands is refMark.
func lex(s string) []word {
	var out []word
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			out = append(out, word{raw: s[i:j], space: true})
			i = j
			continue
		case c == '\n' || c == ';' || c == '(' || c == ')':
			out = append(out, word{raw: s[i : i+1], value: s[i : i+1], op: true})
			i++
			continue
		case c == '&' || c == '|' || c == '<' || c == '>':
			j := i + 1
			if j < len(s) && s[j] == c {
				j++
			}
			out = append(out, word{raw: s[i:j], value: s[i:j], op: true})
			i = j
			continue
		}
		var v strings.Builder
		j := i
	word:
		for j < len(s) {
			switch c := s[j]; c {
			case ' ', '\t', '\n', ';', '(', ')', '&', '|', '<', '>':
				break word
			case '\\':
				if j+1 < len(s) {
					v.WriteByte(s[j+1])
					j += 2
				} else {
					j++
				}
			case '\'':
				k := strings.IndexByte(s[j+1:], '\'')
				if k < 0 {
					v.WriteString(s[j+1:])
					j = len(s)
				} else {
					v.WriteString(s[j+1 : j+1+k])
					j += k + 2
				}
			case '"':
				j++
				for j < len(s) && s[j] != '"' {
					switch {
					case s[j] == '\\' && j+1 < len(s) && strings.IndexByte("$`\"\\\n", s[j+1]) >= 0:
						v.WriteByte(s[j+1])
						j += 2
					case s[j] == '$' || s[j] == '`':
						v.WriteString(refMark)
						if s[j] == '`' {
							v.WriteByte('`')
						}
						j++
					default:
						v.WriteByte(s[j])
						j++
					}
				}
				j = min(j+1, len(s)) // the closing quote, if there is one
			case '$':
				v.WriteString(refMark)
				j++
				if j < len(s) && s[j] == '(' {
					// $(command): to its closing parenthesis, whole.
					k, depth := j, 0
					for ; k < len(s); k++ {
						if s[k] == '(' {
							depth++
						} else if s[k] == ')' {
							depth--
							if depth == 0 {
								k++
								break
							}
						}
					}
					v.WriteString(s[j:min(k, len(s))])
					j = k
				}
			case '`':
				k := strings.IndexByte(s[j+1:], '`')
				end := len(s)
				if k >= 0 {
					end = j + 2 + k
				}
				v.WriteString(refMark + s[j:end])
				j = end
			default:
				v.WriteByte(c)
				j++
			}
		}
		out = append(out, word{raw: s[i:j], value: v.String()})
		i = j
	}
	return out
}

// ref is a value that is only a reference to a variable: $NAME, ${NAME}.
var ref = lazy.Regexp(`^` + refMark + `(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\}|[0-9])$`)

// hide is *** for a value that is not empty and not a reference.
func hide(v string) string {
	if v == "" || ref().MatchString(v) {
		return v
	}
	return "***"
}

var (
	// secretName is in the name of an option, variable, key or header whose
	// value is a secret.
	secretName = lazy.Regexp(`(?i)pass|pwd|secret|token|key|auth|cookie|session|credential|signature|^-*sig$|[_-]sig$`)
	// notSecretKey are names with key that name a column or an order.
	notSecretKey = lazy.Regexp(`(?i)(?:primary|foreign|sort|partition|unique|cache)[_-]?keys?$`)
)

func isSecretName(name string) bool {
	return secretName().MatchString(name) && !notSecretKey().MatchString(name)
}

var (
	optionName = lazy.Regexp(`^--?[A-Za-z0-9_-]+$`)
	// passOption are options of one dash that start like -p with a
	// password glued to it, whose next argument is the secret.
	passOption = lazy.Regexp(`^-pass(?:in|out|word)?$`)
)

// programs whose -p takes a port (or a port mapping) or nothing; any other
// program's -p takes a password.
var (
	portP = map[string]bool{"psql": true, "pg_dump": true, "pg_dumpall": true, "pg_restore": true, "pg_isready": true, "createdb": true, "dropdb": true, "redis-cli": true, "docker": true, "podman": true, "ssh": true, "nc": true}
	flagP = map[string]bool{"mkdir": true}
	// passwordP are programs whose -p takes a password, found before the
	// ones above (docker exec db mysql -p …).
	passwordP = map[string]bool{"mysql": true, "mysqldump": true, "mysqladmin": true, "mariadb": true, "mariadb-dump": true, "sshpass": true, "sqlcmd": true}
	port      = lazy.Regexp(`^(?:[0-9.]+:)?[0-9]+(?:-[0-9]+)?(?::[0-9]+(?:-[0-9]+)?)?(?:/(?:tcp|udp|sctp))?$`)
)

// program is the nearest program of the command before the word with
// a -p of its own meaning, or "".
func program(cmd []string) string {
	for i := len(cmd) - 1; i >= 0; i-- {
		if b := filepath.Base(cmd[i]); portP[b] || flagP[b] || passwordP[b] {
			return b
		}
	}
	return ""
}

func has(cmd []string, prog string) bool {
	for _, a := range cmd {
		if filepath.Base(a) == prog {
			return true
		}
	}
	return false
}

// consoleCommand is a framework console's command (key:generate,
// secrets:list), after artisan or console.
var consoleCommand = lazy.Regexp(`^[a-z][a-z0-9-]*(?::[a-z][a-z0-9-]*)+$`)

// redactWord shows a word's value with its secrets as ***; cmd are the
// words of the command before it, prev the one just before it.
func redactWord(cmd []string, prev, v string) string {
	if p := filepath.Base(prev); (p == "artisan" || p == "console") && consoleCommand().MatchString(v) {
		return v
	}
	prog := program(cmd)
	switch {
	case prev == "-p" && flagP[prog]:
	case prev == "-p" && portP[prog] && port().MatchString(v):
	case prev == "-p" || prev == "-P":
		return hide(v)
	case prev == "-a" && has(cmd, "redis-cli"):
		return hide(v)
	case optionName().MatchString(prev) && isSecretName(prev):
		return hide(v)
	case prev == "-u" || prev == "--user":
		if user, pw, ok := strings.Cut(v, ":"); ok {
			return user + ":" + hide(pw)
		}
	}
	// -pvalue, -Pvalue (mysql, sqlcmd)
	if len(v) > 2 && (strings.HasPrefix(v, "-p") || strings.HasPrefix(v, "-P")) && v[2] != '=' {
		switch {
		case passOption().MatchString(v): // openssl -passin, -passout
		case v[1] == 'p' && flagP[prog]:
		case v[1] == 'p' && portP[prog] && port().MatchString(v[2:]):
		default:
			return v[:2] + hide(v[2:])
		}
	}
	// -uuser:password, --user=user:password
	if m := userOption().FindStringSubmatch(v); m != nil {
		return m[1] + hide(m[2])
	}
	// --name=value with a secret's name: all of the rest
	if name, val, ok := strings.Cut(v, "="); ok && optionName().MatchString(name) && isSecretName(name) {
		return name + "=" + hide(val)
	}
	// Name: value, a header with a secret's name: all of the rest but an
	// authorization's scheme.
	if m := header().FindStringSubmatch(v); m != nil && isSecretName(m[1]) {
		return m[1] + ":" + m[2] + m[3] + hide(m[4])
	}
	return assignments(url(v))
}

var (
	userOption = lazy.Regexp(`^((?:-u|--user=)[^:\s]*:)(.*)$`)
	header     = lazy.Regexp(`(?s)^([A-Za-z][A-Za-z0-9_-]*):(\s*)((?i:(?:bearer|basic|token|digest|negotiate)\s+)?)(.*)$`)
	urlPass    = lazy.Regexp(`(://[^\s:/@]*:)\S*@`)
)

// url shows a URL's password (scheme://user:password@host) as ***, up to
// the last @ of the word: a password may hold @ or /.
func url(v string) string {
	return urlPass().ReplaceAllStringFunc(v, func(m string) string {
		i := strings.Index(m, "://")
		j := strings.IndexByte(m[i+3:], ':') + i + 3
		pw := m[j+1 : len(m)-1]
		return m[:j+1] + hide(pw) + "@"
	})
}

// assignName is a name followed by = or : (with a JSON key's quote).
var assignName = lazy.Regexp(`([A-Za-z0-9_.+-]+)["']?[ \t]*([=:])[ \t]*`)

// assignments shows the value of every name=value or name: value with a
// secret's name as ***: environment-like (PGPASSWORD=…), a DSN's
// (password=… dbname=…), a connection string's (Password=a b;), a URL's
// query (?sig=…&), JSON ("token": "…"). A value runs to the closing quote
// or brace when it starts with one, else to the next ; in a connection
// string, else to the next space, ; or &.
func assignments(v string) string {
	conn := connString().MatchString(v)
	var b strings.Builder
	last := 0
	for pos := 0; pos < len(v); {
		m := assignName().FindStringSubmatchIndex(v[pos:])
		if m == nil {
			break
		}
		for k := range m {
			m[k] += pos
		}
		name, sep := v[m[2]:m[3]], v[m[4]:m[5]]
		start := m[1]
		if !isSecretName(name) || sep == ":" && strings.HasPrefix(v[m[5]:], "//") {
			pos = m[1]
			if pos == m[0] {
				pos++
			}
			continue
		}
		// NAME=value as the whole word (env DB_PASSWORD=a b) and name:
		// value (a header inside a word) run to the end of the word.
		rest := !conn && (sep == ":" || m[2] == 0 && envName().MatchString(name))
		end := valueEnd(v, start, conn, rest)
		b.WriteString(v[last:start])
		b.WriteString(hide(unquote(v[start:end])))
		last, pos = end, end
	}
	b.WriteString(v[last:])
	return b.String()
}

var (
	envName = lazy.Regexp(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// connString is a connection string: two name=value pairs or more,
	// separated by ; (Server=h;Password=a b;).
	connString = lazy.Regexp(`(?:^|;)\s*[A-Za-z][A-Za-z0-9 _]*=[^;]*;\s*[A-Za-z][A-Za-z0-9 _]*=`)
)

func valueEnd(v string, start int, conn, rest bool) int {
	if start >= len(v) {
		return start
	}
	if c := v[start]; c == '"' || c == '\'' || c == '{' {
		closer := c
		if c == '{' {
			closer = '}'
		}
		if k := strings.IndexByte(v[start+1:], closer); k >= 0 {
			return start + 2 + k
		}
		return len(v)
	}
	if rest {
		return len(v)
	}
	stop := " \t\n;&"
	if conn {
		stop = ";"
	}
	if k := strings.IndexAny(v[start:], stop); k >= 0 {
		return start + k
	}
	return len(v)
}

// unquote is a value without its quotes, to tell a reference ("$NAME").
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}
