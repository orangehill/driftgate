package appconf

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The settings of an SQLite database: the file the application opens, as
// each framework names it (a path relative to the folder it takes it from,
// a sqlite: or file: URL, a connection string's Data Source). The setting
// is read into parts.file, relative to the project root with /, inside the
// project past any symbolic links. The file itself is never opened here:
// only its path is looked at (a file SQLite has open in this process loses
// its locks when another handle of it is closed).

// sqliteDefault is Laravel's database file when DB_DATABASE is not set:
// database_path('database.sqlite') of config/database.php.
const sqliteDefault = "database/database.sqlite"

// isSQLiteValue reports a DSN that is the name of an SQLite file: one
// ending in .db, .sqlite or .sqlite3 (fromValue, after the other forms),
// with no scheme before it (a drive letter is none).
func isSQLiteValue(v string) bool {
	if scheme, _, ok := strings.Cut(v, ":"); ok && isScheme(scheme) {
		return false
	}
	lv := strings.ToLower(v)
	return strings.HasSuffix(lv, ".db") || strings.HasSuffix(lv, ".sqlite") || strings.HasSuffix(lv, ".sqlite3")
}

// sqliteValue reads a DSN that starts with a scheme only SQLite takes:
// sqlite: (also sqlite+pysqlite:), sqlite3:, spatialite: or file:. It is
// tried before the other forms, as such a URL may hold an = in its query.
// A file of another engine (h2:file:app.db, derby:app.db, the jdbc: cut)
// is that engine's, not an SQLite file for its .db. ok is false for any
// other DSN.
func sqliteValue(v string) (p parts, ok bool, err error) {
	scheme, _, found := strings.Cut(v, ":")
	if !found || !isScheme(scheme) || mysqlNetwork(v) {
		return parts{}, false, nil
	}
	if !sqliteScheme(scheme) {
		if _, other := driverName(scheme); other != "" && !strings.Contains(v, "://") {
			return parts{other: other}, true, nil
		}
		return parts{}, false, nil
	}
	if !strings.Contains(v, "://") && atNetwork(v) {
		// user:pass@net(…)/db whose network mysqlNetwork does not know (one
		// registered with RegisterDialContext) or whose last / is in its
		// parameters: not a file, and its password must not become a path.
		return parts{}, true, errNotDSN
	}
	p, err = fromSQLiteValue(v)
	return p, true, err
}

// errNotDSN refuses a value read as none of the forms, without showing it.
const errNotDSN = constError("not a database URL, a MySQL DSN or a connection string (the value is not shown)")

// atNetwork reports an @ followed by a name and a (, as the MySQL driver
// writes its network and address (@tcp(…), @cloudsql(…)), anywhere in v.
func atNetwork(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] != '@' {
			continue
		}
		j := i + 1
		for j < len(v) && (isLetter(v[j]) || ('0' <= v[j] && v[j] <= '9') || v[j] == '_' || v[j] == '-' || v[j] == '.') {
			j++
		}
		if j > i+1 && j < len(v) && v[j] == '(' {
			return true
		}
	}
	return false
}

// mysqlNetwork reports the MySQL driver's own form, read as the driver
// reads it: a user (which may read like a scheme, sqlite:… or oauth2:…)
// before the last @ ahead of the last /, then a network: none (the
// default), tcp, tcp4, tcp6 or unix, with or without an address. Such a
// value is never an SQLite file: its user's password would be written as
// a path. (In a DSN or URL, an SQLite file whose folder ends in @ must be
// written as file:///…; a plain path does in DB_DATABASE, working: and
// --database.)
func mysqlNetwork(v string) bool {
	slash := strings.LastIndexByte(v, '/')
	if slash < 0 {
		return false
	}
	at := strings.LastIndexByte(v[:slash], '@')
	if at < 0 {
		return false
	}
	net := v[at+1 : slash]
	if net == "" {
		return true
	}
	if i := strings.IndexByte(net, '('); i >= 0 {
		if !strings.HasSuffix(net, ")") {
			return false
		}
		net = net[:i]
	}
	switch net {
	case "tcp", "tcp4", "tcp6", "unix":
		return true
	}
	return false
}

// isScheme reports a URL scheme: a letter, then letters, digits, + - and
// ., two characters at least (C: is a drive).
func isScheme(s string) bool {
	if len(s) < 2 || !isLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isLetter(c) && !('0' <= c && c <= '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// sqliteScheme reports a URL scheme that names SQLite.
func sqliteScheme(s string) bool {
	s, _, _ = strings.Cut(strings.ToLower(s), "+")
	switch s {
	case "sqlite", "sqlite3", "spatialite", "file":
		return true
	}
	return false
}

// errMemory refuses an in-memory database: it lives as long as the
// connection that made it, so there is no file to restore.
const errMemory = constError("an in-memory SQLite database (:memory:) lives only as long as its connection: there is no file to restore")

// fromSQLiteValue reads an SQLite DSN: a file name, or a URL.
//
//   - file:NAME, file:///ABS, file://localhost/ABS: SQLite's URI filename,
//     its ?query and #fragment left out, %HH decoded;
//   - sqlite:///NAME (relative), sqlite:////ABS, as SQLAlchemy, Laravel's
//     DB_URL and dj-database-url write them; sqlite:NAME;
//   - sqlite3:NAME, sqlite3:///ABS, as Rails writes them, %HH decoded.
//
// Only file: takes a host, localhost.
//
// The name is left in p.name, for resolveSQLite.
func fromSQLiteValue(v string) (parts, error) {
	p := parts{driver: SQLite}
	scheme, rest, found := strings.Cut(v, ":")
	if !found || !sqliteScheme(scheme) {
		if v == ":memory:" {
			return parts{}, errMemory
		}
		p.name = v
		return p, nil
	}
	scheme, _, _ = strings.Cut(strings.ToLower(scheme), "+")
	rest, query, _ := strings.Cut(rest, "?")
	rest, _, _ = strings.Cut(rest, "#")
	for _, kv := range strings.Split(query, "&") {
		if strings.EqualFold(kv, "mode=memory") {
			return parts{}, errMemory
		}
		if err := sqliteUnlocked(kv); err != nil {
			return parts{}, err
		}
	}
	if auth, ok := strings.CutPrefix(rest, "//"); ok {
		host, name, _ := strings.Cut(auth, "/")
		if host != "" && !(scheme == "file" && strings.EqualFold(host, "localhost")) {
			return parts{}, errors.New("the SQLite URL names a host; an SQLite database is a file of this machine (the value is not shown)")
		}
		switch {
		case name == "" && scheme == "sqlite":
			return parts{}, errMemory // sqlite:// is SQLAlchemy's in-memory database
		case scheme == "sqlite":
			rest = name // SQLAlchemy: three slashes and a relative name, four for an absolute one
		default:
			rest = "/" + name
		}
	}
	if scheme == "file" || scheme == "sqlite3" {
		dec, err := percentDecode(rest)
		if err != nil {
			return parts{}, err
		}
		rest = dec
	}
	if len(rest) >= 3 && rest[0] == '/' && rest[2] == ':' && isLetter(rest[1]) { // file:///C:/data/app.db
		rest = rest[1:]
	}
	switch rest {
	case "":
		return parts{}, errors.New("the SQLite URL names no file")
	case ":memory:":
		return parts{}, errMemory
	}
	p.name = rest
	return p, nil
}

// sqliteUnlocked refuses a parameter of an SQLite URI that leaves the
// application's connections without the locks DriftGate's restores and
// seeds take (nolock, immutable, a VFS without POSIX or Windows locks):
// the application would read and write the file while DriftGate rewrites
// it.
func sqliteUnlocked(kv string) error {
	k, v, _ := strings.Cut(strings.ToLower(kv), "=")
	on := v == "1" || v == "true" || v == "yes" || v == "on"
	var why string
	switch {
	case k == "nolock" && on:
		why = "nolock=" + v
	case k == "immutable" && on:
		why = "immutable=" + v
	case k == "vfs" && (strings.HasSuffix(v, "-none") || v == "unix-dotfile" || v == "unix-excl" || v == "unix-namedsem"):
		why = "vfs=" + v
	default:
		return nil
	}
	return fmt.Errorf("the SQLite URI sets %s: the application's connections then do not take the locks DriftGate's restores take, and would read and write the file while it is restored; remove it from the setting", why)
}

// percentDecode decodes the %HH of a file: URI, as SQLite does (and of a
// sqlite3: URL, as Rails does).
func percentDecode(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || unhex(s[i+1]) < 0 || unhex(s[i+2]) < 0 {
			return "", errors.New("the file: URI has a % that is not followed by two hex digits (the value is not shown)")
		}
		b.WriteByte(byte(unhex(s[i+1])<<4 | unhex(s[i+2])))
		i += 2
	}
	return b.String(), nil
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// fromSQLiteURL reads a URL whose scheme names SQLite (sqlite://, file://),
// when fromValue reaches it as a URL.
func fromSQLiteURL(v string) (parts, error) { return fromSQLiteValue(v) }

// sqliteConnString reads an ADO.NET connection string of
// Microsoft.Data.Sqlite (Data Source=app.db, Filename=…); ok is false for
// one that is not.
func sqliteConnString(kv fields) (p parts, ok bool, err error) {
	// Keys of a database server's connection string, none of
	// Microsoft.Data.Sqlite's: a server named app.db is not a file.
	for _, k := range []string{"initialcatalog", "database", "userid", "uid", "user", "username", "server", "host", "trustedconnection", "integratedsecurity", "port"} {
		if _, ok := kv[k]; ok {
			return parts{}, false, nil
		}
	}
	name, ok := kv["filename"]
	if !ok {
		name, _ = kv.get(hostKeys)
		switch ls := strings.ToLower(name); {
		case ls == ":memory:", isSQLiteValue(ls):
		default:
			return parts{}, false, nil
		}
	}
	if _, ok := kv["password"]; ok {
		return parts{}, true, errors.New("the SQLite file of the connection string is encrypted (Password=); DriftGate cannot read it")
	}
	if strings.EqualFold(name, ":memory:") || strings.EqualFold(kv["mode"], "memory") {
		return parts{}, true, errMemory
	}
	if name == "" {
		return parts{}, true, errors.New("the connection string names no SQLite file (Data Source=)")
	}
	// |DataDirectory| is set by the application's code.
	if strings.Contains(name, "|") {
		return parts{}, true, errors.New("the SQLite file of the connection string uses |DataDirectory|, which the application's code sets; DriftGate does not run it")
	}
	return parts{driver: SQLite, name: name}, true, nil
}

// laravelSQLite reads Laravel's SQLite setting of a .env file:
// DB_CONNECTION=sqlite with DB_DATABASE (default database/database.sqlite,
// which config/database.php makes absolute), or a database URL of
// DB_URL or DATABASE_URL, whose file wins.
func laravelSQLite(file string, env map[string]string) (parts, error) {
	p := parts{driver: SQLite, name: env[dbDatabase]}
	from := dbDatabase
	for _, k := range laravelURLs {
		if u := strings.TrimSpace(env[k]); u != "" {
			up, err := fromValue(u)
			if err != nil {
				return parts{}, fmt.Errorf("%s: %s: %w", file, k, err)
			}
			if up.driver != SQLite || up.other != "" {
				return parts{}, fmt.Errorf("%s: DB_CONNECTION is sqlite, and %s names another database", file, k)
			}
			if up.name != "" {
				p.name, from = up.name, k
			}
			break
		}
	}
	switch {
	case p.name == "":
		p.file = sqliteDefault
	case p.name == ":memory:", strings.Contains(strings.ToLower(p.name), "mode=memory"):
		return parts{}, fmt.Errorf("%s: %s: %w", file, from, errMemory)
	case !sqliteAbs(p.name):
		// Laravel 11 and later: realpath($path) ?: realpath(base_path($path)),
		// the folder PHP runs in first; Laravel 10 and before that folder
		// only (public/ under php artisan serve and PHP-FPM).
		p.note = from + " is a relative path: Laravel 11 and later open it from the folder PHP runs in when it is there, else from the project root; Laravel 10 and before only from the folder PHP runs in (public/ under php artisan serve and PHP-FPM); DriftGate takes it relative to the project root: make it absolute, or leave DB_DATABASE unset for database/database.sqlite"
	}
	return p, nil
}

// sqliteAbs reports an absolute file name, of this system or another: /…,
// C:\…, \\server\share.
func sqliteAbs(name string) bool {
	n := strings.ReplaceAll(name, `\`, "/")
	return strings.HasPrefix(n, "/") || len(n) >= 2 && isLetter(n[0]) && n[1] == ':'
}

// resolveSQLite turns the database file p names, as the framework of r's
// file takes it (relative to the project root, manage.py's folder,
// schema.prisma's, the .csproj's), into p.file: relative to the project
// root with /, inside it. A relative name the framework takes from the
// folder the application runs in is taken from the root's, with a note.
func resolveSQLite(root string, r Ref, p parts) (parts, error) {
	if p.file != "" { // relative to the root already (Laravel's default)
		name := p.file
		p.file = ""
		return sqliteResolved(root, r, p, "", name)
	}
	name := p.name
	if r.Source == Python {
		return resolveDjango(root, r, p)
	}
	if name == "" {
		return parts{}, fmt.Errorf("%s: names no SQLite file", r.where())
	}
	if sqliteAbs(name) {
		return sqliteResolved(root, r, p, "", name)
	}
	if rest, ok := strings.CutPrefix(name, "%kernel.project_dir%/"); ok {
		// Symfony's DATABASE_URL (sqlite:///%kernel.project_dir%/var/data.db):
		// the parameter is the Symfony project's folder, which need not be
		// DriftGate's root (symfonyDir). /proj//etc/x.db is /proj/etc/x.db.
		return sqliteResolved(root, r, p, symfonyDir(r), strings.TrimLeft(rest, "/"))
	}
	base, cwd := "", true
	switch r.Source {
	case Dotenv, Env:
		// Prisma takes a relative file: URL from schema.prisma's folder.
		if pr := (&finder{root: root}).prisma(); pr.name != "" && pr.name == r.Key && pr.driver == SQLite {
			base, cwd = "prisma", false
		}
		if r.Source == Dotenv && r.Key == "" { // Laravel: laravelSQLite notes it
			cwd = false
		}
	case AppSettings:
		base = path.Dir(r.File) // dotnet run runs in the project's folder
	case YAML:
		if railsDatabaseYML(r.File) { // Rails: relative to Rails.root
			base, cwd = path.Dir(path.Dir(r.File)), false
		}
	}
	if cwd {
		p.note = "the SQLite file is a relative path, which the application opens from the folder it runs in; DriftGate takes it relative to the project root: check it is the file the application uses"
		if base != "" {
			p.note = "the SQLite file is a relative path, which the application opens from the folder it runs in; DriftGate takes it relative to " + base + "/: check it is the file the application uses"
		}
	}
	return sqliteResolved(root, r, p, base, name)
}

// sqliteResolved sets p.file to the file name names relative to base (a
// folder of the project, relative to its root with /).
func sqliteResolved(root string, r Ref, p parts, base, name string) (parts, error) {
	file, err := sqliteFile(root, base, name)
	if err != nil {
		return parts{}, fmt.Errorf("%s: %w", r.where(), err)
	}
	p.file, p.name = file, ""
	return p, nil
}

// sqliteFile is the database file name names, relative to base (a folder
// relative to root with /; name may be absolute), as a path relative to
// root with /, resolved as sqliteReal resolves it. No error quotes the
// name.
func sqliteFile(root, base, name string) (string, error) {
	r, err := sqliteReal(root, base, name)
	if err != nil {
		return "", err
	}
	// The path as the setting writes it, when it is the same file (a link
	// inside the project kept); else the one the system opens.
	for _, dir := range []string{r.absRoot, r.realRoot} {
		if lex, ok := within(dir, filepath.Clean(r.raw)); ok {
			if again, err := physicalPath(filepath.Join(r.realRoot, lex)); err == nil && again == r.real {
				return filepath.ToSlash(lex), nil
			}
		}
	}
	return filepath.ToSlash(r.rel), nil
}

// SQLitePath is the absolute path the system opens for file, an SQLite
// file of the project as driftgate.yaml names it (working:, pristine:):
// relative to root (or absolute), resolved as a setting of the application
// is (sqliteReal), so link/../app.db is the file the system opens, not
// app.db. It must be inside the project past any symbolic links; it, and
// folders above it, need not exist yet. The file is looked at, never
// opened. No error quotes the name.
func SQLitePath(root, file string) (string, error) {
	r, err := sqliteReal(root, "", file)
	if err != nil {
		return "", err
	}
	return r.real, nil
}

// sqlitePaths is a file of the project as sqliteReal resolves it.
type sqlitePaths struct {
	absRoot, realRoot string // the project folder, absolute and past links
	raw               string // the name, absolute, not cleaned
	real              string // the file the system opens
	rel               string // real relative to realRoot
}

// sqliteReal resolves the database file name names, relative to base (a
// folder relative to root with /; name may be absolute), as the system
// opens it: component by component, each symbolic link followed before a
// .. after it is taken, so storage/../app.db with storage a link to
// var/storage is var/app.db. The file must be inside root, also when it
// does not exist yet. No error quotes the name.
func sqliteReal(root, base, name string) (sqlitePaths, error) {
	n := strings.ReplaceAll(name, `\`, "/") // as CheckFile takes it, so ..\ cannot escape on Windows
	switch {
	case n == "":
		return sqlitePaths{}, errors.New("names no SQLite file")
	case strings.ContainsRune(n, 0):
		return sqlitePaths{}, errors.New("the SQLite file name holds a NUL")
	case strings.HasPrefix(n, "//"):
		return sqlitePaths{}, errors.New("the SQLite file is on a network share; DriftGate restores a file of the project only")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return sqlitePaths{}, fmt.Errorf("the project folder: %w", unwrapPath(err))
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return sqlitePaths{}, fmt.Errorf("the project folder: %w", unwrapPath(err))
	}
	sep := string(filepath.Separator)
	var raw string // not cleaned: a .. is taken where the system takes it
	switch drive := len(n) >= 2 && isLetter(n[0]) && n[1] == ':'; {
	case drive && runtime.GOOS != "windows":
		return sqlitePaths{}, errors.New("the SQLite file is a Windows path, outside the project on this system")
	case drive && (len(n) < 3 || n[2] != '/'):
		return sqlitePaths{}, errors.New("the SQLite file is relative to a drive (C:name); write it relative to the project or in full")
	case drive, strings.HasPrefix(n, "/"):
		if !drive && runtime.GOOS == "windows" {
			return sqlitePaths{}, errors.New("the SQLite file is a path without a drive; write it relative to the project or in full")
		}
		raw = filepath.FromSlash(n)
	default:
		raw = absRoot + sep + filepath.FromSlash(base) + sep + filepath.FromSlash(n)
	}
	real, err := physicalPath(raw)
	if err != nil {
		return sqlitePaths{}, err
	}
	rel, ok := within(realRoot, real)
	if !ok {
		if _, lexical := within(absRoot, filepath.Clean(raw)); lexical {
			return sqlitePaths{}, errors.New("the SQLite file: a symbolic link leads outside the project")
		}
		return sqlitePaths{}, errors.New("the SQLite file is outside the project; DriftGate restores a file of the project only")
	}
	if rel == "." {
		return sqlitePaths{}, errors.New("the SQLite file is the project folder")
	}
	if fi, err := os.Stat(real); err == nil && fi.IsDir() {
		return sqlitePaths{}, errors.New("the SQLite file is a folder")
	}
	return sqlitePaths{absRoot: absRoot, realRoot: realRoot, raw: raw, real: real, rel: rel}, nil
}

// physicalPath is the absolute path p as the system resolves it when it
// opens it: from the root of its volume, component by component, a
// symbolic link followed before a .. after it goes up. A part that does
// not exist yet is kept as written; a link that leads nowhere is refused.
func physicalPath(p string) (string, error) {
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	missing := false
	for _, c := range strings.Split(filepath.ToSlash(p[len(vol):]), "/") {
		switch c {
		case "", ".":
			continue
		case "..": // after a part that does not exist, which is no link, it goes back up that part
			cur = filepath.Dir(cur)
			if missing {
				_, err := os.Lstat(cur)
				missing = err != nil
			}
			continue
		}
		next := filepath.Join(cur, c)
		if missing {
			cur = next
			continue
		}
		r, err := filepath.EvalSymlinks(next)
		switch {
		case err == nil:
			cur = r
		case errors.Is(err, os.ErrNotExist):
			if _, err := os.Lstat(next); err == nil {
				return "", errors.New("the SQLite file: a symbolic link leads nowhere, or outside the project")
			}
			missing, cur = true, next
		default:
			return "", fmt.Errorf("the SQLite file: %w", unwrapPath(err))
		}
	}
	return cur, nil
}

// within is p relative to dir when p is dir or inside it.
func within(dir, p string) (string, bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// SQLiteFile is the database file an SQLite DSN of the environment or
// 1Password names, relative to the project root with /, read as Find and
// Ref.Read read a setting (the one resolver of SQLite paths): variable is
// the environment variable that holds it ("" for none), which says
// whether Prisma takes it from schema.prisma's folder. No error quotes
// the value.
func SQLiteFile(root, variable, value string) (string, error) {
	v := strings.TrimSpace(value)
	p, err := fromValue(v)
	if err != nil && v != "" && !strings.ContainsAny(v, ":=;") { // a path without a known ending
		p, err = parts{driver: SQLite, name: v}, nil
	}
	if err != nil {
		return "", err
	}
	if p.driver != SQLite || p.other != "" {
		return "", errors.New("not the path of an SQLite file, a file: URI or an sqlite: URL (the value is not shown)")
	}
	p, err = resolveSQLite(root, Ref{Source: Env, Key: variable}, p)
	if err != nil {
		if inner := errors.Unwrap(err); inner != nil {
			return "", inner // without the variable, which the caller names
		}
		return "", err
	}
	return p.file, nil
}

// sqliteDSN is the DSN of an SQLite setting (parts.dsn): none, as the
// database is its file (Ref.Read returns it), which state: names as
// working.
func (p parts) sqliteDSN(where string) (string, error) { return "", nil }

// sqliteEngine reports a Django setting of an SQLite database, whose NAME
// may be a path computed from BASE_DIR (resolveDjango reads it).
func sqliteEngine(f fields) bool {
	d, _ := driverName(f["engine"])
	return d == SQLite
}

// resolveDjango reads NAME of a Django SQLite setting: a string, a path
// relative to the folder Django runs in (manage.py's), or BASE_DIR / 'name'
// and os.path.join(BASE_DIR, 'name') with BASE_DIR as startproject writes
// it (Path(__file__).resolve().parent.parent, or os.path.dirname taken of
// __file__).
func resolveDjango(root string, r Ref, p parts) (parts, error) {
	raw, err := readFile(root, r.File)
	if err != nil {
		return parts{}, err
	}
	key := r.Key
	if key == "" {
		key = "default"
	}
	v, err := djangoDatabases(string(raw))
	if err != nil {
		return parts{}, fmt.Errorf("%s: %w", r.File, err)
	}
	node, _ := at(v, key)
	m, _ := node.(map[string]any)
	var name any
	for k, x := range m {
		if fieldKey(k) == "name" {
			name = x
		}
	}
	settingsDir := path.Dir(r.File)
	switch t := name.(type) {
	case string:
		if t == "" {
			return parts{}, fmt.Errorf("%s: %s.NAME names no SQLite file", r.File, key)
		}
		if t == ":memory:" {
			return parts{}, fmt.Errorf("%s: %s.NAME: %w", r.File, key, errMemory)
		}
		if sqliteAbs(t) {
			return sqliteResolved(root, r, p, "", t)
		}
		base := managePy(root, settingsDir)
		p.note = "NAME is a relative path, which Django opens from the folder it runs in; DriftGate takes it relative to manage.py's folder: check it is the file the application uses"
		return sqliteResolved(root, r, p, base, t)
	case code:
		elems, ok := baseDirJoin(t.src)
		if !ok {
			break
		}
		up, ok := baseDirLevels(string(raw))
		if !ok {
			return parts{}, fmt.Errorf("%s: BASE_DIR is not the one django-admin startproject writes; DriftGate does not run the code that computes it (set working: to the file)", r.File)
		}
		base := settingsDir
		for range up {
			if base == "." || base == "" {
				return parts{}, fmt.Errorf("%s: BASE_DIR is outside the project", r.File)
			}
			base = path.Dir(base)
		}
		return sqliteResolved(root, r, p, base, path.Join(elems...))
	}
	return parts{}, fmt.Errorf("%s: %s.NAME is computed by Python code; DriftGate does not run it (set working: to the database file)", r.File, key)
}

// managePy is the folder of the manage.py nearest above dir (a folder
// relative to root), "" (the root) when there is none.
func managePy(root, dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if _, err := localPath(root, path.Join(d, "manage.py")); err == nil {
			if d == "." {
				return ""
			}
			return d
		}
		if d == "." || d == "/" || d == "" {
			return ""
		}
	}
}

// baseDirJoin reads BASE_DIR / 'a' / 'b', str(BASE_DIR / 'a') and
// os.path.join(BASE_DIR, 'a', 'b'): the names joined to BASE_DIR.
func baseDirJoin(src string) ([]string, bool) {
	s := strings.TrimSpace(src)
	if in, ok := strings.CutPrefix(s, "str("); ok && strings.HasSuffix(in, ")") {
		s = strings.TrimSpace(in[:len(in)-1])
	}
	var items []string
	if in, ok := strings.CutPrefix(s, "os.path.join("); ok && strings.HasSuffix(in, ")") {
		items = strings.Split(in[:len(in)-1], ",")
	} else {
		items = strings.Split(s, "/")
	}
	if len(items) < 2 || strings.TrimSpace(items[0]) != "BASE_DIR" {
		return nil, false
	}
	var out []string
	for _, it := range items[1:] {
		it = strings.TrimSpace(it)
		if len(it) < 2 || (it[0] != '\'' && it[0] != '"') || it[len(it)-1] != it[0] {
			return nil, false
		}
		v := it[1 : len(it)-1]
		if v == "" || strings.ContainsAny(v, `'"\`) {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// baseDirLevels reads BASE_DIR of a settings file: the number of folders
// it is above the file's own (Path(__file__).resolve().parent.parent is
// 1), when it is written as startproject writes it; the last assignment
// counts, and one inside a block makes it unknown.
func baseDirLevels(src string) (int, bool) {
	var def string
	for _, line := range strings.Split(src, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimLeft(line, " \t"), "BASE_DIR"); ok && len(rest) < len(line)-len("BASE_DIR") {
			// Set again in a block (if, try): which one counts, only running
			// the code tells.
			if rest = strings.TrimLeft(rest, " \t"); strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==") {
				return 0, false
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "BASE_DIR"); ok {
			if rest = strings.TrimLeft(rest, " \t"); strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==") {
				def = strings.TrimSpace(rest[1:])
				if i := strings.Index(def, "#"); i >= 0 {
					def = strings.TrimSpace(def[:i])
				}
			}
		}
	}
	if rest, ok := strings.CutPrefix(def, "Path(__file__)"); ok {
		for _, r := range []string{".resolve(strict=True)", ".resolve()", ".absolute()"} {
			rest = strings.TrimPrefix(rest, r)
		}
		n := 0
		for {
			after, ok := strings.CutPrefix(rest, ".parent")
			if !ok || strings.HasPrefix(after, "s") {
				break
			}
			n, rest = n+1, after
		}
		if rest == "" && n >= 1 {
			return n - 1, true
		}
		if strings.HasPrefix(rest, ".parents[") && strings.HasSuffix(rest, "]") && n == 0 {
			i := strings.TrimSuffix(strings.TrimPrefix(rest, ".parents["), "]")
			if len(i) == 1 && '0' <= i[0] && i[0] <= '9' {
				return int(i[0] - '0'), true
			}
		}
		return 0, false
	}
	n := 0
	for {
		after, ok := strings.CutPrefix(def, "os.path.dirname(")
		if !ok {
			break
		}
		if !strings.HasSuffix(after, ")") {
			return 0, false
		}
		n, def = n+1, after[:len(after)-1]
	}
	switch def {
	case "__file__", "os.path.abspath(__file__)", "os.path.realpath(__file__)":
		if n >= 1 {
			return n - 1, true
		}
	}
	return 0, false
}

// railsSQLite adds the other databases of a Rails environment that has
// several (primary, cache, queue, cable) when they are SQLite files: each
// is a file of its own, so a state: component of its own. Those of Solid
// Cache, Queue and Cable change without a request changing data, which the
// note says.
func (f *finder) railsSQLite(file, env string) {
	raw, err := readFile(f.root, file)
	if err != nil {
		return
	}
	var doc any
	if yaml.Unmarshal([]byte(erb(string(raw), f.env)), &doc) != nil {
		return
	}
	node, _ := at(yamlMap(doc), env)
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	for _, k := range sortedKeys(m) {
		if _, ok := m[k].(map[string]any); !ok || k == "primary" || printableName(k) != k {
			continue
		}
		r := Ref{Source: YAML, File: file, Key: env + "." + k}
		if p, err := r.read(f.root, f.env); err != nil || p.driver != SQLite {
			continue
		}
		n := len(f.out.Databases)
		f.add(r, "", "")
		if len(f.out.Databases) > n && slices.Contains([]string{"cache", "queue", "cable"}, k) {
			d := &f.out.Databases[n]
			d.Note = join(d.Note, "Rails' "+k+" database (Solid "+strings.ToUpper(k[:1])+k[1:]+") changes without a request changing data: leave it out of state:, or exclude its tables")
		}
	}
}

// sqliteNote is what to check about the SQLite setting p found in the
// project, "" for nothing: p's own note, and a compose service that mounts
// the folder of the file into a container.
func (f *finder) sqliteNote(p parts) string {
	note := p.note
	if svc := f.composeMount(p.file); svc != "" {
		note = join(note, "compose service "+svc+" mounts the folder of the database file into its container: when the application runs there, on Docker Desktop (macOS, Windows), SQLite's locks may not hold across its file sharing, and a restore while the application writes could corrupt the file; run the application on this machine, or on Linux")
	}
	return note
}

// composeMount is the compose service that bind-mounts a folder holding
// file (relative to the root) into its container, "" for none.
func (f *finder) composeMount(file string) string {
	var cf string
	for _, n := range composeFiles {
		if f.exists(n) {
			cf = n
			break
		}
	}
	if cf == "" || file == "" {
		return ""
	}
	cp, err := readComposeProject(f.root, cf, f.env)
	if err != nil {
		return ""
	}
	absRoot, err := filepath.Abs(f.root)
	if err != nil {
		return ""
	}
	db := filepath.Join(absRoot, filepath.FromSlash(file))
	files := append([]string{cf}, composeOverrides(cf)...)
	for _, name := range files {
		raw, err := readFile(f.root, name)
		if err != nil {
			continue
		}
		var y struct {
			Services map[string]struct {
				Volumes []yaml.Node `yaml:"volumes"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(raw, &y) != nil {
			continue
		}
		for _, svc := range sortedKeys(y.Services) {
			for _, v := range y.Services[svc].Volumes {
				src := ""
				switch v.Kind {
				case yaml.ScalarNode:
					src, _, _ = strings.Cut(v.Value, ":")
					if len(v.Value) >= 2 && isLetter(v.Value[0]) && v.Value[1] == ':' { // C:\data:/data
						src = v.Value[:2] + strings.SplitN(v.Value[2:], ":", 2)[0]
					}
				case yaml.MappingNode:
					var long struct {
						Type   string `yaml:"type"`
						Source string `yaml:"source"`
					}
					if v.Decode(&long) == nil && long.Type == "bind" {
						src = long.Source
					}
				}
				if src, err = interpolate(src, cp.vars); err != nil || src == "" {
					continue
				}
				if !strings.HasPrefix(src, ".") && !sqliteAbs(src) {
					continue // a named volume
				}
				var mount string
				if sqliteAbs(src) {
					mount = filepath.Clean(filepath.FromSlash(strings.ReplaceAll(src, `\`, "/")))
				} else {
					mount = filepath.Join(absRoot, filepath.FromSlash(cp.dir), filepath.FromSlash(strings.ReplaceAll(src, `\`, "/")))
				}
				if _, ok := within(mount, db); ok {
					return svc
				}
			}
		}
	}
	return ""
}

// laravelTables is FrameworkTables of a Laravel .env setting of the
// default connection. A store is proposed when its driver is database on that
// connection: the .env's SESSION_DRIVER, CACHE_STORE (CACHE_DRIVER before
// Laravel 11) and QUEUE_CONNECTION, else the default of config/session.php,
// cache.php and queue.php (env('X', 'database')), else, without that file
// (Laravel 11 and later publish none), the framework's: database.
// job_batches and failed_jobs are on the default connection whatever the
// queue (failed_jobs unless QUEUE_FAILED_DRIVER is null, file or
// dynamodb); Telescope's and Pulse's tables when composer.json requires
// them. The cache and its locks are restore_empty, the others excluded;
// with Sanctum, personal_access_tokens is restore_only (its seeded tokens
// kept, last_used_at written on every authenticated request).
func laravelTables(root string, d Database, env Lookup) Tables {
	if _, err := localPath(root, "artisan"); err != nil {
		return Tables{}
	}
	if d.Ref.Key != "" && !slices.Contains(laravelURLs, d.Ref.Key) {
		return Tables{} // not the default connection
	}
	raw, err := readFile(root, d.Ref.File)
	if err != nil {
		return Tables{}
	}
	vals, err := parseDotenv(raw, env)
	if err != nil {
		return Tables{}
	}
	table := func(key, def string) string {
		if t := vals[key]; t != "" && printableName(t) == t {
			return t
		}
		return def
	}
	var tables, cache, only, why []string
	// uses reports a store whose driver is database on the default
	// connection, and says where that comes from.
	uses := func(config, connKey string, keys ...string) bool {
		if c := vals[connKey]; c != "" && c != vals[dbConnection] {
			return false
		}
		for _, k := range keys {
			if v, ok := vals[k]; ok {
				if v != "database" {
					return false
				}
				why = append(why, k+"=database in "+d.Ref.File)
				return true
			}
		}
		file := "config/" + config + ".php"
		src, err := readFile(root, file)
		if err != nil {
			why = append(why, keys[0]+" unset: Laravel's default database")
			return true
		}
		for _, k := range keys {
			if def, ok := phpEnvDefault(string(src), k); ok {
				if def != "database" {
					return false
				}
				why = append(why, keys[0]+" unset: database by "+file)
				return true
			}
		}
		return false
	}
	if uses("session", "SESSION_CONNECTION", "SESSION_DRIVER") {
		tables = append(tables, table("SESSION_TABLE", "sessions"))
	}
	if uses("cache", "DB_CACHE_CONNECTION", "CACHE_STORE", "CACHE_DRIVER") {
		cache = append(cache, table("DB_CACHE_TABLE", "cache"), table("DB_CACHE_LOCK_TABLE", "cache_locks"))
	}
	if uses("queue", "DB_QUEUE_CONNECTION", "QUEUE_CONNECTION", "QUEUE_DRIVER") { // QUEUE_DRIVER: Laravel 5.5 to 5.6
		tables = append(tables, table("DB_QUEUE_TABLE", "jobs"))
	}
	tables = append(tables, "job_batches")
	switch vals["QUEUE_FAILED_DRIVER"] {
	case "null", "file", "dynamodb":
	default:
		tables = append(tables, "failed_jobs")
	}
	why = append(why, "job_batches and failed_jobs are on the default connection")
	if composer, err := readFile(root, "composer.json"); err == nil {
		if strings.Contains(string(composer), `"laravel/telescope"`) {
			tables = append(tables, "telescope_entries", "telescope_entries_tags", "telescope_monitoring")
			why = append(why, "Telescope")
		}
		if strings.Contains(string(composer), `"laravel/pulse"`) {
			tables = append(tables, "pulse_values", "pulse_entries", "pulse_aggregates")
			why = append(why, "Pulse")
		}
		if strings.Contains(string(composer), `"laravel/sanctum"`) {
			only = append(only, "personal_access_tokens")
			why = append(why, "Sanctum (last_used_at on every authenticated request)")
		}
	}
	return Tables{Exclude: tables, RestoreEmpty: cache, RestoreOnly: only, Why: strings.Join(why, ", ")}
}

// phpEnvDefault reads the default of env('KEY', 'default') in a PHP
// configuration file.
func phpEnvDefault(src, key string) (string, bool) {
	for _, q := range []string{"'", `"`} {
		i := strings.Index(src, "env("+q+key+q)
		if i < 0 {
			continue
		}
		rest := strings.TrimLeft(src[i+len("env("+q+key+q):], " \t\r\n")
		rest, ok := strings.CutPrefix(rest, ",")
		if !ok {
			return "", false
		}
		v, after, ok := rubyString(rest) // a quoted PHP string reads the same
		if !ok || !strings.HasPrefix(strings.TrimSpace(after), ")") {
			return "", false
		}
		return v, true
	}
	return "", false
}

// symfonyDir is the folder of the Symfony project whose setting r is,
// relative to the root with /: the folder of its .env file, the folder
// above config/ for a file under it (config/packages/doctrine.yaml), the
// root for the environment.
func symfonyDir(r Ref) string {
	if r.File == "" || r.Source == Env {
		return ""
	}
	dir := path.Dir(r.File)
	for d := dir; d != "." && d != "/"; d = path.Dir(d) {
		if path.Base(d) == "config" {
			dir = path.Dir(d)
			break
		}
	}
	if dir == "." {
		return ""
	}
	return dir
}
