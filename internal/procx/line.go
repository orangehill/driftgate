package procx

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Line is a command (argv) as one line, for messages and logs: an argument
// with a space, quote or backslash (or an empty one) quoted, and every
// password, secret, token or key in it shown as *** (Redact), in an
// argument of its own and inside a shell script (sh -c "…") alike. A
// reference to a variable ($NAME, ${NAME}) stays: it holds no value.
func Line(argv []string) string {
	parts := make([]string, len(argv))
	prog := ""
	if len(argv) > 0 {
		prog = filepath.Base(argv[0])
	}
	for i, a := range argv {
		if i > 0 && !isRef(a) {
			switch prev := argv[i-1]; {
			case secretOption(prev) || prev == "-a" && prog == "redis-cli":
				a = "***"
			case prev == "-u" || prev == "--user":
				if user, _, ok := strings.Cut(a, ":"); ok {
					a = user + ":***"
				}
			}
		}
		a = Redact(a)
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			a = strconv.Quote(a)
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// secretWord is in the name of an option or variable whose value is a
// secret.
const secretWord = `(?:pass|pwd|secret|token|key)`

var secretOptionName = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*` + secretWord + `[a-z0-9_-]*$`)

// secretOption reports an option whose next argument is a secret: -p, -P,
// --password, --pass, --token, --db-password, --api-key …
func secretOption(a string) bool {
	return a == "-p" || a == "-P" || secretOptionName.MatchString(a)
}

func isRef(a string) bool { return strings.HasPrefix(a, "$") }

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// scheme://user:password@host, a password with @ or / in it too: up
	// to the last @ of the word.
	{regexp.MustCompile(`(://[^\s:/@]*:)\S*@`), "${1}***@"},
	// NAME=value, name: value, --name=value, with PASS, PWD, SECRET,
	// TOKEN or KEY in the name (PGPASSWORD=, Pwd={…}, password='…',
	// --password=-x, API_KEY=); not a reference to a variable.
	{regexp.MustCompile(`(?i)((?:^|[^\w$])[\w.-]*` + secretWord + `[\w.-]*\s*[=:]\s*)` + `("[^"$]*"|'[^']*'|\{[^}]*\}|[^$\s;&"'{][^\s;&]*)`), "${1}***"},
	// -pvalue, -Pvalue (mysql, sqlcmd)
	{regexp.MustCompile(`((?:^|\s)-[pP])([^\s$=][^\s;&]*)`), "${1}***"},
	// an option and its value in a script: -p x, -P x, --password x,
	// --token x, redis-cli -a x
	{regexp.MustCompile(`(?i)((?:^|\s)(?:-[pP]|--[a-z0-9_-]*` + secretWord + `[a-z0-9_-]*|redis-cli(?:\s+[^\s|;&]+)*?\s+-a)\s+)("[^"$]*"|'[^']*'|[^\s$"'|;&][^\s;&|]*)`), "${1}***"},
	// curl -u user:password, --user user:password
	{regexp.MustCompile(`((?:^|\s)(?:-u|--user)(?:\s+|=)[^\s:]+:)([^\s$]\S*)`), "${1}***"},
	// an Authorization header
	{regexp.MustCompile(`(?i)(authorization:\s*(?:(?:bearer|basic|token|digest)\s+)?)(\S+)`), "${1}***"},
}

// Redact shows the passwords, secrets, tokens and keys in a text (an
// argument, a script, a DSN) as ***.
func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
