package compare

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/orangehill/driftgate/internal/lazy"
)

// An application writes the folder it runs from into some responses: the
// stack trace of a debug error page or JSON (Laravel with APP_DEBUG, a
// framework's development error page), a path in a message. Two checkouts
// of one project (the main worktree and another git worktree) are in
// different folders. When the two sides were captured in different project
// folders, or either recorded one in a home folder (~/…, which may be two
// users' homes), each side's own folders (all it recorded: accept adds a
// capture's) are replaced by one neutral folder in its body and headers,
// in memory only, before anything is compared: as written, with its
// slashes escaped as JSON escapes them (\/, which PHP's json_encode
// writes) and with its backslashes doubled (a Windows path in JSON). Only
// its own: nested folders of the two sides (/var/www and /var/www/html)
// do not make another path of one side the project's. Two equal bodies
// are not read for them, so the shortcut that skips identical bodies
// agrees with the comparison. A folder is replaced only where a path
// starts (after a quote, white space, a bracket, = , or >) and its name
// ends (another folder whose name starts with the project's still
// differs), and only a folder of two names or more: /app is also a path
// of a URL.
//
// A folder of two names or more in a home folder of the forms the
// comparison knows (HomeRelative: /Users/<name>, /home/<name>, /root,
// /var/root, WSL's /mnt/<drive>/Users/<name>, C:\Users\<name>, a name
// without spaces) is recorded as ~/… (meta.json goes into the repository
// with baselines: git, and names no user that way): it stands for that
// path under any home folder of those forms, so a baseline made on one
// machine compares with a capture of another user, or of CI. Any other
// folder is recorded as it is; the home folder itself not at all (~, as a
// release before recorded it, is left out): it would take every path of
// any home folder for the project's.
const neutralRoot = "/driftgate-project"

// rootForm is one way a folder is written, with the neutral folder written
// the same way. home, for a folder in the home folder, matches the home
// folder written before from (from is then the rest, from its separator).
type rootForm struct {
	from, to []byte
	home     *regexp.Regexp
	last     []byte // the folder's last name, in every form: a body without it has none of them
}

// The home folders a folder recorded as ~/… stands under: as written,
// with / escaped as \/ (JSON), and with \ doubled (a Windows path in
// JSON); without letter case, as macOS and Windows open them (cd
// /users/alice works there, and the folder keeps that case). homeName is
// one name of a path.
const (
	homeName    = `[^/\\\s"'<>]+`
	homePlain   = `(?i:/(?:Users|home)/` + homeName + `|/root|/var/root|/mnt/[a-z]/Users/` + homeName + `|[a-z]:\\Users\\` + homeName + `)`
	homeEscaped = `(?i:\\/(?:Users|home)\\/` + homeName + `|\\/root|\\/var\\/root|\\/mnt\\/[a-z]\\/Users\\/` + homeName + `)`
	homeDoubled = `(?i:[a-z]:\\\\Users\\\\` + homeName + `)`
)

var homeStart = lazy.Regexp(`^` + homePlain + `([/\\].*)?$`)

// localHomes are this machine's home folder ($HOME) and its real path
// (a home linked to /nfs/home/<name>), each when the forms of homePlain
// do not know it (a service account's /var/lib/jenkins, /srv/app): a
// folder in it is recorded as ~/… too, and the home folder before a ~/…
// folder may be one of them. A home with white space is left out: the
// comparison reads a home folder's path up to white space.
func localHomes() []string {
	h, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	list := []string{h}
	if real, err := filepath.EvalSymlinks(h); err == nil {
		list = append(list, real)
	}
	var out []string
	for _, x := range list {
		x = strings.TrimRight(x, `/\\`)
		if len(restNames(x)) == 0 || strings.ContainsAny(x, " \t") || homeStart().MatchString(x) || slices.Contains(out, x) {
			continue
		}
		out = append(out, x)
	}
	return out
}

// InLocalHome reports a folder in this machine's home folder or its real
// path, of any form, white space too: its absolute path names the user.
func InLocalHome(folder string) bool {
	h, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	list := []string{h}
	if real, err := filepath.EvalSymlinks(h); err == nil {
		list = append(list, real)
	}
	for _, x := range list {
		x = strings.TrimRight(x, `/\\`)
		if len(restNames(x)) == 0 {
			continue
		}
		if _, ok := inFolder(folder, x); ok {
			return true
		}
	}
	return false
}

// inFolder reports a path in folder h or h itself, without letter case
// where the system opens paths so (macOS, Windows), and the rest from its
// separator.
func inFolder(path, h string) (string, bool) {
	p, hh := path, h
	if runtime.GOOS != "linux" {
		p, hh = strings.ToLower(p), strings.ToLower(hh)
	}
	if !strings.HasPrefix(p, hh) || len(p) > len(hh) && p[len(hh)] != '/' && p[len(hh)] != '\\' {
		return "", false
	}
	return path[len(h):], true
}

// homeRegexps match the home folder before a ~/… folder, as written, with
// / escaped and with \ doubled: the forms of homePlain and this
// machine's localHomes. They are made once per set of homes.
func homeRegexps() [3]*regexp.Regexp {
	homes := localHomes()
	key := strings.Join(homes, "\x00")
	homeCache.Lock()
	defer homeCache.Unlock()
	if r, ok := homeCache.m[key]; ok {
		return r
	}
	plain, escaped, doubled := homePlain, homeEscaped, homeDoubled
	for _, h := range homes {
		q := func(s string) string {
			s = regexp.QuoteMeta(s)
			if runtime.GOOS != "linux" {
				s = "(?i:" + s + ")"
			}
			return s
		}
		plain += "|" + q(h)
		if strings.Contains(h, "/") {
			escaped += "|" + q(strings.ReplaceAll(h, "/", `\/`))
		}
		if strings.Contains(h, `\`) {
			doubled += "|" + q(strings.ReplaceAll(h, `\`, `\\`))
		}
	}
	r := [3]*regexp.Regexp{regexp.MustCompile(`^(?:` + plain + `)$`), regexp.MustCompile(`^(?:` + escaped + `)$`), regexp.MustCompile(`^(?:` + doubled + `)$`)}
	if homeCache.m == nil {
		homeCache.m = map[string][3]*regexp.Regexp{}
	}
	homeCache.m[key] = r
	return r
}

var homeCache struct {
	sync.Mutex
	m map[string][3]*regexp.Regexp
}

// HomeRelative tells a folder in a home folder of a form the comparison
// knows (inHome): rest is the rest from its separator (/Sites/shop of
// /Users/alice/Sites/shop) when it has two names or more, and the folder
// is recorded as ~ and the rest; else rest is "", and the folder (the
// home folder itself, one name in it: /app is also a path of a URL) is
// not recorded, as its absolute path names the user. A folder in this
// machine's own home of another form (localHomes) is one too. A folder
// elsewhere is recorded as it is.
func HomeRelative(folder string) (rest string, inHome bool) {
	if m := homeStart().FindStringSubmatch(folder); m != nil {
		rest = m[1]
	} else {
		in := false
		for _, h := range localHomes() {
			if rest, in = inFolder(folder, h); in {
				break
			}
		}
		if !in {
			return "", false
		}
	}
	if len(restNames(rest)) < 2 {
		return "", true
	}
	return rest, true
}

// homeLook is how far before the rest of a ~/… folder its home folder is
// looked for.
const homeLook = 256

// restNames are the names of a path.
func restNames(p string) []string {
	return strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' })
}

// Folders are the forms of the two sides' folders (Side.Folders), made
// once for every request of two sets; nil when there is nothing to
// replace.
type Folders struct{ base, cur []rootForm }

// NewFolders makes the Folders of two sides that recorded base and cur.
func NewFolders(base, cur []string) *Folders { return newRoots(base, cur) }

func newRoots(base, cur []string) *Folders {
	// Two records of one folder in the home folder may be of two homes.
	home := slices.ContainsFunc(append(append([]string{}, base...), cur...), func(r string) bool { return strings.HasPrefix(r, "~") })
	if len(base) == 0 || len(cur) == 0 || slices.Equal(base, cur) && !home {
		return nil
	}
	f := &Folders{base: rootForms(base), cur: rootForms(cur)}
	if len(f.base)+len(f.cur) == 0 {
		return nil
	}
	return f
}

// rootForms are the forms of the folders, the longest first: a folder that
// ends another (its real path /private/tmp/x and /tmp/x) is replaced in it
// whole.
func rootForms(list []string) []rootForm {
	var out []rootForm
	for _, r := range list {
		r = strings.TrimRight(r, `/\`)
		names := restNames(r)
		var plain, escaped, doubled *regexp.Regexp
		if rest, ok := strings.CutPrefix(r, "~"); ok {
			// The home folder itself (~) and one name in it are left out.
			if rest == "" || rest[0] != '/' && rest[0] != '\\' || len(restNames(rest)) < 2 {
				continue
			}
			h := homeRegexps()
			r, plain, escaped, doubled = rest, h[0], h[1], h[2]
		} else if len(names) < 2 {
			continue
		}
		last := []byte(names[len(names)-1])
		out = append(out, rootForm{[]byte(r), []byte(neutralRoot), plain, last})
		if strings.Contains(r, "/") {
			out = append(out, rootForm{[]byte(strings.ReplaceAll(r, "/", `\/`)), []byte(`\/driftgate-project`), escaped, last})
		}
		if strings.Contains(r, `\`) {
			out = append(out, rootForm{[]byte(strings.ReplaceAll(r, `\`, `\\`)), []byte(`\\driftgate-project`), doubled, last})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].from) > len(out[j].from) })
	return out
}

// pathByte reports a byte that may continue a file or folder name.
func pathByte(c byte) bool {
	return c == '.' || c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// delimiter reports a byte a path starts after: a quote, white space, a
// bracket, =, , or > (HTML). Any other byte may be part of a path before
// it (/data+/, user@host:/…), where the folder is another one.
func delimiter(c byte) bool {
	switch c {
	case '"', '\'', '`', ' ', '\t', '\n', '\r', '(', '[', '{', '<', '>', '=', ',':
		return true
	}
	return false
}

// replaceRoots returns b with the folders replaced; b itself is never
// changed, and b is returned as it is when it holds none of them.
func replaceRoots(b []byte, forms []rootForm) []byte {
	// One search for the last name of every folder instead of one for
	// each of its forms: most bodies name none.
	var checked [8][]byte // the last names searched for, and whether b has them
	var has [8]bool
	n := 0
	for _, f := range forms {
		found, known := false, false
		for k := range n {
			if bytes.Equal(checked[k], f.last) {
				found, known = has[k], true
				break
			}
		}
		if !known {
			found = bytes.Contains(b, f.last)
			if n < len(checked) {
				checked[n], has[n] = f.last, found
				n++
			}
		}
		if !found || !bytes.Contains(b, f.from) {
			continue
		}
		var out []byte
		at := 0 // b[:at] is in out
		for at < len(b) {
			i := bytes.Index(b[at:], f.from)
			if i < 0 {
				break
			}
			i += at
			end := i + len(f.from)
			if f.home != nil {
				// The home folder before it, any user's: the path that
				// ends there, from where it starts (a byte no path has
				// before it), must be one (homeBefore); a home folder is
				// short, so at most homeLook bytes are looked at.
				j := i
				for j > at && i-j < homeLook && (pathByte(b[j-1]) || b[j-1] == '/' || b[j-1] == '\\' || b[j-1] == ':') {
					j--
				}
				// No start of the path within homeLook bytes: nothing
				// is taken for a home folder.
				if j > 0 && i-j >= homeLook || !f.home.Match(b[j:i]) {
					out = append(out, b[at:end]...)
					at = end
					continue
				}
				i = j
			}
			starts := i == 0 || delimiter(b[i-1])
			ends := end == len(b) || !pathByte(b[end])
			out = append(out, b[at:i]...)
			if starts && ends {
				out = append(out, f.to...)
			} else {
				out = append(out, b[i:end]...) // inside another path
			}
			at = end
		}
		b = append(out, b[at:]...)
	}
	return b
}

// side returns s with forms, its own folders, replaced in its body
// (unless body is false: two equal bodies stay equal), headers and error.
func (r *Folders) side(s Side, forms []rootForm, body bool) Side {
	if s.Record == nil {
		return s
	}
	// The record is copied only when its error or a header names a
	// folder: most do not.
	errText := string(replaceRoots([]byte(s.Record.Error), forms))
	var hs [][2]string
	for i, h := range s.Record.Headers {
		if v := string(replaceRoots([]byte(h[1]), forms)); v != h[1] {
			if hs == nil {
				hs = append([][2]string{}, s.Record.Headers...)
			}
			hs[i][1] = v
		}
	}
	if hs != nil || errText != s.Record.Error {
		rec := *s.Record
		rec.Error = errText
		if hs != nil {
			rec.Headers = hs
		}
		s.Record = &rec
	}
	if body {
		// An XLSX file's bytes stay as they are; it is told only of a
		// body that names a folder (sheetKind reads the archive).
		if nb := replaceRoots(s.Body, forms); len(nb) > 0 && (len(nb) != len(s.Body) || &nb[0] != &s.Body[0]) && sheetKind(s) != "xlsx" {
			s.Body = nb
		}
	}
	return s
}
