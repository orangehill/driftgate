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
// an argument of its own and inside a shell script (sh -c "…", a command
// ssh runs, $(…)) alike.
//
// DriftGate runs argv as it is, never through a shell: a $ in an argument
// is part of the value. Only in a script that a shell runs (the argument
// after sh -c, bash -lc …) is $NAME or ${NAME} a reference to a variable,
// which holds no value and stays; anything else in a secret's place ($ in
// single quotes or escaped, a value around a reference, $(command)) is ***.
// What is not a secret stays as written; when in doubt, a value is hidden.
// It is for display only: what a command prints is not redacted.
func Line(argv []string) string {
	ws := make([]word, len(argv))
	for i, a := range argv {
		ws[i] = word{raw: a, value: a}
	}
	shown := redactWords(ws, 0)
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
// quotes) and as the program receives it. In a script's word, a reference
// the shell expands is refMark, what it names, and refEnd ($PW is
// "\x00PW\x01", so -p"$PW"x is not taken for one); a command substitution
// is refMark (redacted command) refEnd.
type word struct {
	raw, value string
	op         bool // an operator of a script: ; & | && || ( ) < > or a newline
	space      bool // what separates words in a script
	subst      bool // a command substitution in it showed a secret
}

const (
	refMark = "\x00"
	refEnd  = "\x01"
	// maxDepth bounds scripts in scripts (sh -c "bash -c '…'", $(…)): one
	// deeper is shown as *** whole.
	maxDepth = 16
)

// plain is a value with its references written as the shell does.
func plain(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, refMark, "$"), refEnd, "")
}

// redactWords returns every word as it is shown: as written when it holds
// no secret, its value with the secrets as *** otherwise. What a word means
// depends on the words of its command before it, which a command keeps as
// it goes (linear in the words).
func redactWords(ws []word, depth int) []string {
	out := make([]string, len(ws))
	var c command
	hideNext := false // the word after <<< is standard input
	// Each command's first word and where it starts, for echo … | x
	// --password-stdin.
	var starts []int
	var firsts []string
	var pipes []bool
	newCommand := func(i int, pipe bool) {
		starts, firsts, pipes = append(starts, i), append(firsts, ""), append(pipes, pipe)
	}
	newCommand(0, false)
	for i := range ws {
		w := &ws[i]
		out[i] = w.raw
		if w.space {
			continue
		}
		if w.op {
			switch w.value {
			case "<", ">", ">>", "<<":
				c.prev = ""
			case "<<<":
				c.prev = ""
				hideNext = true
			default:
				if c.stdin && len(starts) > 1 && pipes[len(pipes)-1] {
					hideArguments(ws, out, starts[len(starts)-2], starts[len(starts)-1], firsts[len(firsts)-2])
				}
				c = command{}
				newCommand(i+1, w.value == "|")
			}
			continue
		}
		v := w.value
		var shown string
		switch {
		case hideNext:
			shown = hide(v)
			hideNext = false
		case c.script():
			shown = redactScript(plain(v), depth+1)
		case c.ssh == sshRemote:
			// ssh runs the words after the host in a shell of the other
			// machine, joined: one with a space or an operator is a script,
			// others are words of the command it goes on.
			if strings.ContainsAny(v, " \t\n;&|") {
				shown = redactScript(plain(v), depth+1)
			} else {
				shown = redactWord(&c, v)
			}
		default:
			shown = redactWord(&c, v)
		}
		if shown != v || w.subst {
			out[i] = render(w, shown)
		}
		if firsts[len(firsts)-1] == "" {
			firsts[len(firsts)-1] = base(v)
		}
		c.add(v)
	}
	if c.stdin && len(starts) > 1 && pipes[len(pipes)-1] {
		hideArguments(ws, out, starts[len(starts)-2], starts[len(starts)-1], firsts[len(firsts)-2])
	}
	return out
}

// hideArguments hides the arguments of the command from ws[start:end] when
// it is echo or printf: what it pipes to a command that reads a password
// from standard input (docker login --password-stdin).
func hideArguments(ws []word, out []string, start, end int, first string) {
	if first != "echo" && first != "printf" {
		return
	}
	seen := false
	for k := start; k < end && k < len(ws); k++ {
		if ws[k].space || ws[k].op {
			continue
		}
		if seen {
			out[k] = "***"
		}
		seen = true
	}
}

// command is what the words of a simple command so far say about the next
// one.
type command struct {
	prev string // the word just before, "" after a redirection
	// prog is the nearest program with a -p of its own meaning (portP,
	// flagP, passwordP).
	prog string
	// shell: a shell is one of the words before prev (sh … -c, script);
	// shellNow: one of them all.
	shell, shellNow bool
	// Programs whose options or words name a secret: redis-cli -a and
	// AUTH, curl -b, openssl -k, gh secret set --body, htpasswd -b.
	redis, redisAuth, curl, openssl, gh, ghSecret, htpasswd, htBatch bool
	htArgs                                                           int
	ssh                                                              int  // sshNone, sshHost (looking for it), sshRemote
	sshArg                                                           bool // the next word is an ssh option's argument
	stdin                                                            bool // a word reads a password from standard input
	// hideNext: the next word is the value of a secret's name and its =
	// (password = x, $env:PGPASSWORD = 'x').
	hideNext bool
}

const (
	sshNone = iota
	sshHost
	sshRemote
)

// sshArgs are the options of ssh that take an argument.
const sshArgs = "BbcDEeFIiJLlmOoPpQRSWw"

// base is the name of a program, also of a Windows path (C:\…\cmd.exe).
func base(v string) string {
	if strings.IndexByte(v, '\\') >= 0 {
		v = strings.ReplaceAll(v, `\`, "/")
	}
	b := filepath.Base(v)
	if strings.ContainsAny(b, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		b = strings.ToLower(b)
	}
	return strings.TrimSuffix(b, ".exe")
}

func (c *command) add(v string) {
	b := base(v)
	if portP[b] || flagP[b] || passwordP[b] {
		c.prog = b
	}
	switch {
	case c.ssh == sshNone && b == "ssh":
		c.ssh = sshHost
	case c.ssh == sshHost && c.sshArg:
		c.sshArg = false
	case c.ssh == sshHost && strings.HasPrefix(v, "-"):
		c.sshArg = len(v) == 2 && strings.IndexByte(sshArgs, v[1]) >= 0
	case c.ssh == sshHost:
		c.ssh = sshRemote
	}
	c.redis = c.redis || b == "redis-cli"
	c.redisAuth = c.redisAuth || c.redis && strings.EqualFold(v, "auth")
	c.curl = c.curl || b == "curl"
	c.openssl = c.openssl || b == "openssl"
	c.ghSecret = c.ghSecret || c.gh && v == "secret"
	c.gh = c.gh || b == "gh"
	switch {
	case b == "htpasswd":
		c.htpasswd = true
	case c.htpasswd && len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.IndexByte(v, 'b') >= 0:
		c.htBatch = true
	case c.htBatch && !strings.HasPrefix(v, "-"):
		c.htArgs++
	}
	c.stdin = c.stdin || strings.HasSuffix(v, "-stdin") && stdinOption().MatchString(v)
	c.shell = c.shellNow
	c.shellNow = c.shellNow || shells[b]
	c.prev = v
}

// stdinOption reads a password from standard input.
var stdinOption = lazy.Regexp(`(?i)^--?[a-z0-9-]*(?:pass|secret|token|key)[a-z0-9-]*-stdin$`)

// script reports whether the next word is a script a shell runs: sh -c,
// bash -lc, docker exec x sh -c, pwsh -Command, cmd /c, script -qc,
// nix-shell --run, eval.
func (c *command) script() bool {
	return c.shell && scriptFlag().MatchString(c.prev) || c.prev == "eval"
}

// render shows a word whose value changed: plain in an argument of its own
// (Line quotes it), in double quotes inside a script when it needs them.
func render(w *word, v string) string {
	v = plain(v)
	if w.raw == w.value || !strings.ContainsAny(v, " \t\n\"'\\;&|<>()`") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`")
	return `"` + r.Replace(v) + `"`
}

// shells run the argument after -c (or -Command, /c) as a script. fish,
// PowerShell and cmd are read as a POSIX shell would: enough to find an
// option and its value, an assignment or a connection string in them.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "mksh": true, "su": true,
	"fish": true, "pwsh": true, "powershell": true, "cmd": true, "script": true, "nix-shell": true}

var scriptFlag = lazy.Regexp(`(?i)^(?:-[a-z]*c[a-z]*|-command|/[ck]|--run)$`)

// redactScript shows a shell script with its secrets as ***.
func redactScript(s string, depth int) string {
	if depth > maxDepth {
		return "***"
	}
	ws := lex(sqlPasswords(s), depth)
	var b strings.Builder
	for _, a := range redactWords(ws, depth) {
		b.WriteString(a)
	}
	return b.String()
}

// lex splits a shell script into words, the spaces between them and its
// operators, enough to find secrets: quotes and escapes are removed from a
// word's value, a reference is marked (word) and the command of a
// substitution, $(…) or `…`, is redacted.
func lex(s string, depth int) []word {
	out := make([]word, 0, len(s)/2+1)
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
				if c == '<' && j < len(s) && s[j] == '<' {
					j++ // <<<
				}
			}
			out = append(out, word{raw: s[i:j], value: s[i:j], op: true})
			i = j
			continue
		}
		// A word without quotes, escapes or expansions is its value.
		j := i
		for j < len(s) && strings.IndexByte(" \t\n;()&|<>\\'\"$`", s[j]) < 0 {
			j++
		}
		if j == len(s) || strings.IndexByte(" \t\n;()&|<>", s[j]) >= 0 {
			out = append(out, word{raw: s[i:j], value: s[i:j]})
			i = j
			continue
		}
		var v strings.Builder
		v.WriteString(s[i:j])
		subst := false
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
						var changed bool
						j, changed = expansion(s, j, &v, depth)
						subst = subst || changed
					default:
						v.WriteByte(s[j])
						j++
					}
				}
				j = min(j+1, len(s)) // the closing quote, if there is one
			case '$', '`':
				var changed bool
				j, changed = expansion(s, j, &v, depth)
				subst = subst || changed
			default:
				v.WriteByte(c)
				j++
			}
		}
		out = append(out, word{raw: s[i:j], value: v.String(), subst: subst})
		i = j
	}
	return out
}

// expansion writes the expansion at s[j] ($NAME, ${…}, $(…), `…`) into v
// and returns where it ends, and whether the command of a substitution
// showed a secret.
func expansion(s string, j int, v *strings.Builder, depth int) (int, bool) {
	if s[j] == '`' {
		k := strings.IndexByte(s[j+1:], '`')
		if k < 0 {
			v.WriteString(refMark + "(" + s[j+1:] + ")" + refEnd)
			return len(s), false
		}
		return j + k + 2, substitution(s[j+1:j+1+k], v, depth)
	}
	j++ // $
	switch {
	case j < len(s) && s[j] == '(':
		// $(command): to its closing parenthesis.
		open := 0
		for k := j; k < len(s); k++ {
			switch s[k] {
			case '(':
				open++
			case ')':
				if open--; open == 0 {
					return k + 1, substitution(s[j+1:k], v, depth)
				}
			}
		}
		v.WriteString(refMark + s[j:] + refEnd)
		return len(s), false
	case j < len(s) && s[j] == '{':
		k := strings.IndexByte(s[j:], '}')
		if k < 0 {
			v.WriteString(refMark + s[j:] + refEnd)
			return len(s), false
		}
		v.WriteString(refMark + s[j:j+k+1] + refEnd)
		return j + k + 1, false
	case j < len(s) && (s[j] == '_' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= 'a' && s[j] <= 'z'):
		k := j
		for k < len(s) && (s[k] == '_' || s[k] >= 'A' && s[k] <= 'Z' || s[k] >= 'a' && s[k] <= 'z' || s[k] >= '0' && s[k] <= '9') {
			k++
		}
		v.WriteString(refMark + s[j:k] + refEnd)
		return k, false
	case j < len(s) && strings.IndexByte("0123456789?$!#@*-", s[j]) >= 0:
		v.WriteString(refMark + s[j:j+1] + refEnd)
		return j + 1, false
	}
	v.WriteByte('$')
	return j, false
}

// substitution writes a command substitution with its command redacted.
func substitution(cmd string, v *strings.Builder, depth int) bool {
	shown := redactScript(cmd, depth+1)
	v.WriteString(refMark + "(" + shown + ")" + refEnd)
	return shown != cmd
}

// ref is a value that is only a reference to a variable: $NAME, ${NAME}.
var ref = lazy.Regexp(`^` + refMark + `(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\}|[0-9])` + refEnd + `$`)

// hide is *** for a value that is not empty and not a reference.
func hide(v string) string {
	if v == "" || ref().MatchString(v) {
		return v
	}
	return "***"
}

// sqlPassword is a password SQL sets: IDENTIFIED BY '…' (MySQL), PASSWORD
// '…', E'…' or $$…$$ (PostgreSQL), PASSWORD = '…' or N'…' (SQL Server),
// SET PASSWORD [FOR user] = '…' or PASSWORD('…').
var sqlPassword = lazy.Regexp(`(\bidentified(?:\s+with\s+\S+)?\s+by|\bpassword(?:\s+for\s+\S+)?)((?:\s*=\s*|\s+)(?:password\s*\(\s*)?[ne]?)('(?:[^']|'')*'|"(?:[^"\\]|\\.)*"|\$\$(?:[^$]|\$[^$])*\$\$)`)

// sqlPasswords hides the passwords SQL in a text sets (mysql -e, psql -c,
// a here-document, tinker's --execute). sqlPassword is matched in the text
// in lower case (ASCII only, so every byte stays in its place): a pattern
// without (?i) is several times faster.
func sqlPasswords(s string) string {
	if !strings.ContainsAny(s, "'\"$") {
		return s
	}
	l := asciiLower(s)
	if !strings.Contains(l, "password") && !strings.Contains(l, "identified") {
		return s
	}
	ms := sqlPassword().FindAllStringSubmatchIndex(l, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		b.WriteString(s[last:m[6]])
		b.WriteString("***")
		last = m[7]
	}
	b.WriteString(s[last:])
	return b.String()
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

var (
	// secretName is in the name of an option, variable, key or header whose
	// value is a secret.
	secretName = lazy.Regexp(`(?i)pass|pw|secret|token|key|auth|cookie|session|credential|signature|^-*sig$|[_-]sig$`)
	// notSecretKey are names with key that name a column or an order, and
	// with auth that name no secret (MongoDB's authSource, --author).
	notSecretKey = lazy.Regexp(`(?i)(?:primary|foreign|sort|partition|unique|cache)[_-]?keys?$|(?:^|[^a-z])auth(?:source|mechanism|or|ors|ority)$`)
)

func isSecretName(name string) bool {
	return secretName().MatchString(name) && !notSecretKey().MatchString(name)
}

var (
	optionName = lazy.Regexp(`^--?[A-Za-z0-9_-]+$`)
	// passOption are options of one dash that start like -p with a
	// password glued to it, whose next argument is the secret.
	passOption = lazy.Regexp(`(?i)^-pass(?:in|out|word)?$`)
)

// programs whose -p takes a port (or a port mapping) or nothing; any other
// program's -p takes a password.
var (
	portP = map[string]bool{"psql": true, "pg_dump": true, "pg_dumpall": true, "pg_restore": true, "pg_isready": true, "createdb": true, "dropdb": true, "redis-cli": true, "docker": true, "podman": true, "ssh": true, "nc": true}
	flagP = map[string]bool{"mkdir": true, "cp": true, "rsync": true, "install": true, "tar": true}
	// passwordP are programs whose -p takes a password, found before the
	// ones above (docker exec db mysql -p …).
	passwordP = map[string]bool{"mysql": true, "mysqldump": true, "mysqladmin": true, "mariadb": true, "mariadb-dump": true, "sshpass": true, "sqlcmd": true}
	port      = lazy.Regexp(`^(?:[0-9.]+:)?[0-9]+(?:-[0-9]+)?(?::[0-9]+(?:-[0-9]+)?)?(?:/(?:tcp|udp|sctp))?$`)
)

// consoleCommand is a framework console's command (key:generate,
// secrets:list), after artisan or console.
var consoleCommand = lazy.Regexp(`^[a-z][a-z0-9-]*(?::[a-z][a-z0-9-]*)+$`)

// redactWord shows a word's value with its secrets as ***, after the words
// of its command so far (c).
func redactWord(c *command, v string) string {
	prev, prog := c.prev, c.prog
	if p := base(prev); (p == "artisan" || p == "console") && consoleCommand().MatchString(v) {
		return v
	}
	if strings.HasSuffix(prev, "=") && optionName().MatchString(prev[:len(prev)-1]) {
		prev = prev[:len(prev)-1] // --password= x
	}
	if c.hideNext {
		c.hideNext = false
		return hide(v)
	}
	switch {
	case (v == "=" || v == "=>") && assignsSecret(prev):
		// password = x in a configuration a here-document writes,
		// $env:PGPASSWORD = 'x' in PowerShell: the value is the next word.
		c.hideNext = true
		return v
	case strings.HasSuffix(prev, ":") && len(prev) > 1 && assignsSecret(prev[:len(prev)-1]):
		// password: x (YAML), "password": "x" (JSON) over several words
		return hide(v)
	case c.redisAuth: // redis-cli AUTH [user] password
		return hide(v)
	case c.htBatch && c.htArgs >= 1 && !strings.HasPrefix(v, "-"): // htpasswd -b file user password
		return hide(v)
	case prev == "-p" && flagP[prog]:
	case prev == "-p" && portP[prog] && port().MatchString(v):
	case prev == "-p" || prev == "-P":
		return hide(v)
	case prev == "-a" && c.redis, prev == "-b" && c.curl, prev == "-k" && c.openssl,
		(prev == "--body" || prev == "-b") && c.ghSecret:
		return hide(v)
	case optionName().MatchString(prev) && isSecretName(prev) && !flagOption().MatchString(prev):
		return hide(v)
	case bareName().MatchString(prev) && isSecretName(prev), // aws configure set aws_secret_access_key x
		len(prev) >= 10 && strings.EqualFold(prev[len(prev)-10:], "_authtoken"),              // npm config set //registry/:_authToken x
		strings.HasPrefix(prev, "github-oauth.") || strings.HasPrefix(prev, "gitlab-token."): // composer config
		return hide(v)
	case (prev == "-u" || prev == "--user" || prev == "-U") && !strings.Contains(v, "://"):
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
	if strings.HasPrefix(v, "-u") || strings.HasPrefix(v, "--user=") {
		if m := userOption().FindStringSubmatch(v); m != nil {
			return m[1] + hide(m[2])
		}
	}
	// --name=value with a secret's name: all of the rest
	if name, val, ok := strings.Cut(v, "="); ok && optionName().MatchString(name) && isSecretName(name) {
		return name + "=" + hide(val)
	}
	// Name: value, a header with a secret's name: all of the rest but an
	// authorization's scheme.
	if strings.IndexByte(v, ':') >= 0 {
		if m := header().FindStringSubmatch(v); m != nil && isSecretName(m[1]) {
			return m[1] + ":" + m[2] + m[3] + hide(m[4])
		}
	}
	return sqlPasswords(assignments(url(v)))
}

// assignsSecret reports a word that is the name of a secret being set:
// password, PGPASSWORD, $env:PGPASSWORD, $Password, a JSON key.
func assignsSecret(w string) bool {
	if i := strings.LastIndexAny(w, ":"+refEnd); i >= 0 {
		w = w[i+1:]
	}
	w = strings.Trim(w, refMark+"${}\"'[ ")
	return w != "" && envName().MatchString(w) && isSecretName(w)
}

// flagOption is an option with a secret's name that takes no value.
var flagOption = lazy.Regexp(`^--?(?:no|skip)-|^--askpass$|-stdin$`)

// bareName is a word that only names a setting (aws_secret_access_key,
// password), not a program (sshpass), an option, a command of a console
// (key:generate) or a path.
var bareName = lazy.Regexp(`(?i)^(?:[a-z][a-z0-9]*_[a-z0-9_]*|password|passwd)$`)

var (
	userOption = lazy.Regexp(`^((?:-u|--user=)[^:\s]*:)(.*)$`)
	header     = lazy.Regexp(`(?s)^([A-Za-z][A-Za-z0-9_-]*):(\s*)((?i:(?:bearer|basic|token|digest|negotiate)\s+)?)(.*)$`)
	urlPass    = lazy.Regexp(`(://[^\s:/@]*:)\S*@`)
	// urlToken is a URL's user without a password, often a token
	// (https://ghp_x@github.com); a user name is hidden too.
	urlToken = lazy.Regexp(`(://)[^\s:/@]+@`)
)

// url shows a URL's password (scheme://user:password@host) as ***, up to
// the last @ of the word: a password may hold @ or /.
func url(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	v = urlToken().ReplaceAllString(v, "${1}***@") // https://token@host
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
	if !strings.ContainsAny(v, "=:") {
		return v
	}
	conn := strings.IndexByte(v, ';') >= 0 && connString().MatchString(v)
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
		// Not a URL's scheme (https:) or user (://user:password@, url).
		if !isSecretName(name) || sep == ":" && (strings.HasPrefix(v[m[5]:], "//") || strings.HasSuffix(v[:m[2]], "://")) {
			pos = m[1]
			if pos == m[0] {
				pos++
			}
			continue
		}
		// NAME=value as the whole word (env DB_PASSWORD=a b) and name:
		// value (a header inside a word) run to the end of the word.
		rest := !conn && (sep == ":" || m[2] == 0 && envName().MatchString(name))
		if sep == "=" && start < len(v) && v[start] == '>' {
			// 'password' => 'x' (PHP, Ruby)
			for start++; start < len(v) && (v[start] == ' ' || v[start] == '\t'); start++ {
			}
		}
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
	if c := v[start]; c == '"' {
		// to the closing quote; a backslash escapes one (JSON)
		for k := start + 1; k < len(v); k++ {
			switch v[k] {
			case '\\':
				k++
			case '"':
				return k + 1
			}
		}
		return len(v)
	}
	if c := v[start]; c == '\'' || c == '{' {
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
