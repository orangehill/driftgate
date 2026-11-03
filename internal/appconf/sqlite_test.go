package appconf

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// sqliteProject is a project folder with files.
func sqliteProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestSQLiteValues: the forms an SQLite file is named in, the file left
// for resolveSQLite; an in-memory database and a host are refused.
func TestSQLiteValues(t *testing.T) {
	for v, want := range map[string]string{
		"file:./dev.db":                         "./dev.db",
		"file:./dev.db?connection_limit=1":      "./dev.db",
		"file:data%20base.db#x":                 "data base.db",
		"file:///var/app.db":                    "/var/app.db",
		"file://localhost/var/app.db":           "/var/app.db",
		"file:///C:/data/app.db":                "C:/data/app.db",
		"sqlite:///database/app.sqlite":         "database/app.sqlite",
		"sqlite:////var/app.sqlite":             "/var/app.sqlite",
		"sqlite+pysqlite:///db.sqlite3?mode=ro": "db.sqlite3",
		"file:app.db?nolock=0&vfs=unix":         "app.db",
		"sqlite:db.sqlite3":                     "db.sqlite3",
		"sqlite3:storage/development.sqlite3":   "storage/development.sqlite3",
		"sqlite3:///srv/app.sqlite3":            "/srv/app.sqlite3",
		"jdbc:sqlite:app.db":                    "app.db",
		"storage/app.sqlite3":                   "storage/app.sqlite3",
		"Data Source=app.db;Cache=Shared":       "app.db",
		"Filename=data/app":                     "data/app",
	} {
		p, err := fromValue(v)
		if err != nil || p.driver != SQLite || p.name != want {
			t.Errorf("%s: %q %q, %v; want %q", v, p.driver, p.name, err, want)
		}
	}
	for v, want := range map[string]string{
		"sqlite://":                          ":memory:",
		"sqlite:///:memory:":                 ":memory:",
		"file::memory:?cache=shared":         ":memory:",
		"file:x.db?mode=memory":              ":memory:",
		"sqlite3::memory:":                   ":memory:",
		"Data Source=:memory:":               ":memory:",
		"Data Source=|DataDirectory|\\x.db":  "|DataDirectory|",
		"file://db.example.com/var/app.db":   "names a host",
		"file:bad%zz.db":                     "two hex digits",
		"sqlite3:":                           "names no file",
		"Data Source=x.db;Mode=Memory;X=1":   ":memory:",
		"Filename=;Mode=ReadWriteCreate;X=1": "names no SQLite file",
		"file:app.db?nolock=1":               "sets nolock=1",
		"file:app.db?mode=rw&immutable=1":    "sets immutable=1",
		"file:app.db?VFS=unix-none":          "sets vfs=unix-none",
		"file:///C:/app.db?vfs=win32-none":   "sets vfs=win32-none",
		"sqlite:///app.db?vfs=unix-dotfile":  "sets vfs=unix-dotfile",
	} {
		_, err := fromValue(v)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %s", v, err, want)
		} else if strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "bad%") {
			t.Errorf("%s: the error quotes the value: %v", v, err)
		}
	}
	// A URL or a file of another database ending in .db is not an SQLite
	// file.
	if p, err := fromValue("postgres://u:p@db/app.db"); err != nil || p.driver != Postgres {
		t.Errorf("postgres URL: %q %v", p.driver, err)
	}
	for _, v := range []string{"jdbc:h2:file:./data/app.db", "jdbc:hsqldb:file:app.db", "jdbc:derby:app.db"} {
		if p, err := fromValue(v); err != nil || p.driver != "" || p.other == "" {
			t.Errorf("%s: %q %q %v", v, p.driver, p.other, err)
		}
	}
	for _, v := range []string{"sqlite://localhost/app.db", "sqlite3://localhost/app.db", "duckdb:///app.db"} {
		if p, err := fromValue(v); err == nil {
			t.Errorf("%s: %q %q, want an error", v, p.driver, p.name)
		}
	}
	if p, err := fromValue("sqlite3:storage/my%20app.sqlite3"); err != nil || p.name != "storage/my app.sqlite3" {
		t.Errorf("sqlite3: %q %v", p.name, err)
	}
}

// TestSQLiteFile: a file inside the project, also through a link inside
// it and when it does not exist yet; never one outside it, by .., an
// absolute path, a link, a network share or another system's path.
func TestSQLiteFile(t *testing.T) {
	root := sqliteProject(t, map[string]string{"database/app.sqlite": "", "shared/real.db": ""})
	outside := t.TempDir()
	links := runtime.GOOS != "windows"
	if links {
		for link, target := range map[string]string{"in": filepath.Join(root, "shared"), "out": outside, "dangling": filepath.Join(outside, "nothing")} {
			if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
				t.Fatal(err)
			}
		}
	}
	absRoot, _ := filepath.Abs(root)
	realRoot, _ := filepath.EvalSymlinks(absRoot)
	for _, c := range []struct{ base, name, want string }{
		{"", "database/app.sqlite", "database/app.sqlite"},
		{"", "./database/../database/app.sqlite", "database/app.sqlite"},
		{"", `database\app.sqlite`, "database/app.sqlite"},
		{"prisma", "../database/app.sqlite", "database/app.sqlite"},
		{"prisma", "./dev.db", "prisma/dev.db"}, // not there yet
		{"", filepath.Join(absRoot, "database", "app.sqlite"), "database/app.sqlite"},
		{"", filepath.Join(realRoot, "database", "app.sqlite"), "database/app.sqlite"},
	} {
		if got, err := sqliteFile(root, c.base, c.name); err != nil || got != c.want {
			t.Errorf("%s + %s: %q, %v; want %s", c.base, c.name, got, err, c.want)
		}
	}
	bad := []struct{ base, name string }{
		{"", "../x.db"}, {"prisma", "../../x.db"}, {"", filepath.Join(outside, "x.db")}, {"", "//server/share/x.db"},
		{"", `\\server\share\x.db`}, {"", "database"}, {"", "."}, {"", "a\x00.db"}, {"", ""},
	}
	if runtime.GOOS != "windows" {
		bad = append(bad, struct{ base, name string }{"", `C:\data\x.db`})
	} else {
		bad = append(bad, struct{ base, name string }{"", "C:x.db"}, struct{ base, name string }{"", "/x.db"})
	}
	if links {
		// .. after a link goes up from where the link leads, as the system
		// takes it: storage/../app.sqlite is shared/app.sqlite, not app.sqlite.
		if err := os.Symlink(filepath.Join(root, "shared", "storage"), filepath.Join(root, "storage")); err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(root, "shared", "storage"), 0o755)
		if got, err := sqliteFile(root, "", "storage/../app.sqlite"); err != nil || got != "shared/app.sqlite" {
			t.Errorf("storage/../app.sqlite: %q %v, want shared/app.sqlite", got, err)
		}
		os.Symlink(filepath.Join(outside, "sub"), filepath.Join(root, "ext"))
		os.MkdirAll(filepath.Join(outside, "sub"), 0o755)
		bad = append(bad, struct{ base, name string }{"", "ext/../app.sqlite"})
		bad = append(bad, struct{ base, name string }{"", "out/x.db"}, struct{ base, name string }{"", "dangling"}, struct{ base, name string }{"", "dangling/x.db"})
		if got, err := sqliteFile(root, "", "in/real.db"); err != nil || got != "in/real.db" {
			t.Errorf("a link inside the project: %q %v", got, err)
		}
	}
	for _, c := range bad {
		if got, err := sqliteFile(root, c.base, c.name); err == nil {
			t.Errorf("%s + %q: %q, want an error", c.base, c.name, got)
		} else if strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), "server") {
			t.Errorf("%q: the error quotes the path: %v", c.name, err)
		}
	}
}

// resolvePhysical resolves p as the system does when it opens it, with
// Lstat and Readlink only (independent of physicalPath): each link is
// replaced by its target before the components after it are taken.
func resolvePhysical(p string) string {
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	cur := vol + sep
	todo := strings.Split(filepath.ToSlash(p[len(vol):]), "/")
	for hops := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		if fi, err := os.Lstat(next); err == nil && fi.Mode()&os.ModeSymlink != 0 && hops < 40 {
			hops++
			target, _ := os.Readlink(next)
			if filepath.IsAbs(target) {
				v := filepath.VolumeName(target)
				cur, target = v+sep, target[len(v):]
			}
			todo = append(strings.Split(filepath.ToSlash(target), "/"), todo...)
			continue
		}
		cur = next
	}
	return cur
}

// FuzzSQLitePath: whatever a setting names, the file the system would open
// for it is inside the project, and is the file the path read names; the
// path is relative with /.
func FuzzSQLitePath(f *testing.F) {
	for _, s := range []string{"db.sqlite", "../x.db", "..\\..\\x.db", "out/x.db", "C:\\x.db", "\\\\srv\\s\\x.db", "//srv/s/x.db",
		"file:../x.db", "file:%2e%2e/x.db", "file:x.db?mode=ro#f", "file:///etc/x.db", "sqlite:////etc/x.db", "sqlite:///../x.db",
		"sqlite3:../x.db", "Data Source=../x.db", "in/../../x.db", "in/x.db", "/", "a/./b/../../../x", "storage/../x.db",
		"storage/../../x.db", "in/up/x.db", "nope/../storage/../x.db"} {
		f.Add(s)
	}
	root := f.TempDir()
	outside := f.TempDir()
	os.MkdirAll(filepath.Join(root, "in", "deep"), 0o755)
	if runtime.GOOS != "windows" {
		os.Symlink(outside, filepath.Join(root, "out"))
		os.Symlink("../..", filepath.Join(root, "in", "up"))
		os.Symlink(filepath.Join("in", "deep"), filepath.Join(root, "storage"))
	}
	absRoot, _ := filepath.Abs(root)
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, v string) {
		p, err := fromValue(v)
		if err != nil || p.driver != SQLite {
			p = parts{driver: SQLite, name: v}
		}
		n := strings.ReplaceAll(p.name, `\`, "/")
		for _, base := range []string{"", "in", "in/deep"} {
			r, err := sqliteFile(root, base, p.name)
			if err != nil {
				continue
			}
			if r == "" || filepath.IsAbs(r) || strings.Contains(r, `\`) || r == ".." || strings.HasPrefix(r, "../") || strings.Contains(r, "/../") {
				t.Fatalf("%q in %s: %q is not a path inside the project", v, base, r)
			}
			// From the root past links: on Windows also its long name, as
			// TEMP may hold the short one (C:\Users\RUNNER~1).
			opened := realRoot + "/" + base + "/" + n
			if strings.HasPrefix(n, "/") || len(n) >= 2 && isLetter(n[0]) && n[1] == ':' {
				opened = n
			}
			want := resolvePhysical(filepath.FromSlash(opened))
			got := resolvePhysical(filepath.Join(realRoot, filepath.FromSlash(r)))
			if got != want {
				t.Fatalf("%q in %s: %q is %s, the system opens %s", v, base, r, got, want)
			}
			if rel, err := filepath.Rel(realRoot, got); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("%q in %s: %q leads outside the project (%s)", v, base, r, got)
			}
		}
	})
}

// TestSQLiteFind: the SQLite file of each framework's setting, relative to
// the folder the framework takes it from, with what to check.
func TestSQLiteFind(t *testing.T) {
	djangoSettings := "from pathlib import Path\n\nBASE_DIR = Path(__file__).resolve().parent.parent\n\nINSTALLED_APPS = ['django.contrib.sessions']\n\nDATABASES = {\n    'default': {\n        'ENGINE': 'django.db.backends.sqlite3',\n        'NAME': BASE_DIR / 'db.sqlite3',\n    }\n}\n"
	elsewhere := "/var/lib/app/database.sqlite" // an absolute path of this system, outside the project
	if runtime.GOOS == "windows" {
		elsewhere = `C:\ProgramData\app\database.sqlite`
	}
	tests := []struct {
		name  string
		files map[string]string
		env   map[string]string
		paths []string // path <- ref
		notes []string // a part of each database's note, "" for none
		other []string // parts of Found.Notes
	}{
		{
			name:  "laravel default",
			files: map[string]string{"artisan": "", ".env": "DB_CONNECTION=sqlite\n"},
			paths: []string{"database/database.sqlite <- dotenv .env "},
			notes: []string{""},
		},
		{
			name:  "laravel relative",
			files: map[string]string{"artisan": "", ".env": "DB_CONNECTION=sqlite\nDB_DATABASE=storage/app.sqlite\n"},
			paths: []string{"storage/app.sqlite <- dotenv .env "},
			notes: []string{"DB_DATABASE is a relative path: Laravel 11 and later open it from the folder PHP runs in"},
		},
		{
			name:  "laravel outside",
			files: map[string]string{"artisan": "", ".env": "DB_CONNECTION=sqlite\nDB_DATABASE=" + elsewhere + "\n"},
			other: []string{".env: the SQLite file is outside the project"},
		},
		{
			name:  "laravel memory",
			files: map[string]string{"artisan": "", ".env": "DB_CONNECTION=sqlite\nDB_DATABASE=:memory:\n"},
			other: []string{".env: DB_DATABASE: an in-memory SQLite database"},
		},
		{
			name:  "django",
			files: map[string]string{"manage.py": "", "shop/settings.py": djangoSettings},
			paths: []string{"db.sqlite3 <- python shop/settings.py default"},
			notes: []string{""},
		},
		{
			name:  "django os.path",
			files: map[string]string{"manage.py": "", "shop/settings.py": strings.NewReplacer("Path(__file__).resolve().parent.parent", "os.path.dirname(os.path.dirname(os.path.abspath(__file__)))", "BASE_DIR / 'db.sqlite3'", "os.path.join(BASE_DIR, 'var', 'db.sqlite3')").Replace(djangoSettings)},
			paths: []string{"var/db.sqlite3 <- python shop/settings.py default"},
			notes: []string{""},
		},
		{
			name:  "django literal",
			files: map[string]string{"manage.py": "", "shop/settings.py": strings.Replace(djangoSettings, "BASE_DIR / 'db.sqlite3'", "'var/db.sqlite3'", 1)},
			paths: []string{"var/db.sqlite3 <- python shop/settings.py default"},
			notes: []string{"Django opens from the folder it runs in; DriftGate takes it relative to manage.py's folder"},
		},
		{
			name:  "django computed",
			files: map[string]string{"manage.py": "", "shop/settings.py": strings.Replace(djangoSettings, "BASE_DIR / 'db.sqlite3'", "env('DB_FILE')", 1)},
			other: []string{"shop/settings.py: default.NAME is computed by Python code"},
		},
		{
			name: "prisma",
			files: map[string]string{".env": "DATABASE_URL=\"file:./dev.db\"\n",
				"prisma/schema.prisma": "datasource db {\n  provider = \"sqlite\"\n  url      = env(\"DATABASE_URL\")\n}\n"},
			paths: []string{"prisma/dev.db <- dotenv .env DATABASE_URL"},
			notes: []string{""},
		},
		{
			name:  "node",
			files: map[string]string{".env": "DATABASE_URL=sqlite:data/app.db\n"},
			paths: []string{"data/app.db <- dotenv .env DATABASE_URL"},
			notes: []string{"DriftGate takes it relative to the project root"},
		},
		{
			name:  "rails",
			files: map[string]string{"config/database.yml": "default: &default\n  adapter: sqlite3\n  timeout: 5000\n\ndevelopment:\n  primary:\n    <<: *default\n    database: storage/development.sqlite3\n  cache:\n    <<: *default\n    database: storage/development_cache.sqlite3\n  queue:\n    <<: *default\n    database: storage/development_queue.sqlite3\n"},
			paths: []string{
				"storage/development.sqlite3 <- yaml config/database.yml development.primary",
				"storage/development_cache.sqlite3 <- yaml config/database.yml development.cache",
				"storage/development_queue.sqlite3 <- yaml config/database.yml development.queue",
			},
			notes: []string{"", "Rails' cache database (Solid Cache)", "Rails' queue database (Solid Queue)"},
		},
		{
			name: "aspnet",
			files: map[string]string{"src/Api/Api.csproj": `<PackageReference Include="Microsoft.EntityFrameworkCore.Sqlite" />`,
				"src/Api/appsettings.json": `{"ConnectionStrings": {"Default": "Data Source=app.db"}}`},
			paths: []string{"src/Api/app.db <- appsettings src/Api/Api.csproj ConnectionStrings:Default"},
			notes: []string{"DriftGate takes it relative to src/Api/"},
		},
		{
			name: "compose mount",
			files: map[string]string{"artisan": "", ".env": "DB_CONNECTION=sqlite\n",
				"compose.yaml": "services:\n  app:\n    image: php\n    volumes:\n      - ./:/var/www/html\n      - data:/data\nvolumes:\n  data: {}\n"},
			paths: []string{"database/database.sqlite <- dotenv .env "},
			notes: []string{"compose service app mounts the folder of the database file"},
		},
		{
			name: "compose other folder",
			files: map[string]string{"artisan": "", ".env": "DB_CONNECTION=sqlite\n",
				"compose.yaml": "services:\n  app:\n    image: php\n    volumes:\n      - type: bind\n        source: ./public\n        target: /var/www/html/public\n"},
			paths: []string{"database/database.sqlite <- dotenv .env "},
			notes: []string{""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := sqliteProject(t, tt.files)
			found := Find(root, lookup(tt.env))
			var paths []string
			for _, d := range found.Databases {
				paths = append(paths, d.Path+" <- "+d.Ref.Source+" "+d.Ref.File+" "+d.Ref.Key)
				if d.Driver != SQLite || d.Name != "" {
					t.Errorf("%s: driver %q, name %q", d.Path, d.Driver, d.Name)
				}
				if _, db, err := d.Ref.Read(root, SQLite, lookup(tt.env)); err != nil || db != d.Path {
					t.Errorf("Read %s: %q, %v", d.Path, db, err)
				}
			}
			if !reflect.DeepEqual(paths, tt.paths) {
				t.Errorf("databases:\n got %q\nwant %q\nnotes %v", paths, tt.paths, found.Notes)
			}
			for i, n := range tt.notes {
				if i >= len(found.Databases) {
					break
				}
				got := found.Databases[i].Note
				if n == "" && got != "" || !strings.Contains(got, n) {
					t.Errorf("%s: note %q, want %q", found.Databases[i].Path, got, n)
				}
			}
			for _, want := range tt.other {
				ok := false
				for _, n := range found.Notes {
					ok = ok || strings.Contains(n.File+": "+n.Text, want)
				}
				if !ok {
					t.Errorf("notes %v, want %q", found.Notes, want)
				}
			}
		})
	}
}

// TestSQLiteExclude: the tables the framework keeps its own state in, in
// the same file, proposed for exclude: Laravel's for a store whose driver
// is database on the default connection, by .env, config/*.php or the
// framework's default; job_batches and failed_jobs always (unless failed
// jobs go elsewhere); Telescope's; Django's sessions.
func TestSQLiteExclude(t *testing.T) {
	all := []string{"sessions", "cache", "cache_locks", "jobs", "job_batches", "failed_jobs"}
	l10 := map[string]string{
		"config/session.php": "<?php return ['driver' => env('SESSION_DRIVER', 'file')];",
		"config/cache.php":   "<?php return ['default' => env(\"CACHE_DRIVER\", \"file\")];",
		"config/queue.php":   "<?php return ['default' => env('QUEUE_CONNECTION', 'database')];",
	}
	for _, c := range []struct {
		env    string
		config map[string]string
		want   []string
		why    string
	}{
		{"DB_CONNECTION=sqlite\nSESSION_DRIVER=database\nCACHE_STORE=database\nQUEUE_CONNECTION=database\n", nil, all, "SESSION_DRIVER=database in .env"},
		{"DB_CONNECTION=sqlite\n", nil, all, "SESSION_DRIVER unset: Laravel's default database"}, // Laravel 11: no config/*.php
		{"DB_CONNECTION=sqlite\n", l10, []string{"jobs", "job_batches", "failed_jobs"}, "QUEUE_CONNECTION unset: database by config/queue.php"},
		{"DB_CONNECTION=sqlite\nSESSION_DRIVER=file\nCACHE_DRIVER=database\nDB_CACHE_TABLE=app_cache\nQUEUE_CONNECTION=redis\nQUEUE_FAILED_DRIVER=null\n", nil,
			[]string{"app_cache", "cache_locks", "job_batches"}, "CACHE_DRIVER=database in .env"},
		{"DB_CONNECTION=sqlite\nSESSION_CONNECTION=other\nDB_CACHE_CONNECTION=other\nDB_QUEUE_CONNECTION=other\n", nil, []string{"job_batches", "failed_jobs"}, "on the default connection"},
		{"DB_CONNECTION=sqlite\nSESSION_DRIVER=file\nCACHE_STORE=file\nQUEUE_CONNECTION=sync\n", map[string]string{"composer.json": `{"require": {"laravel/telescope": "^5.0"}}`},
			[]string{"job_batches", "failed_jobs", "telescope_entries", "telescope_entries_tags", "telescope_monitoring"}, "Telescope"},
	} {
		files := map[string]string{"artisan": "", ".env": c.env}
		for k, v := range c.config {
			files[k] = v
		}
		root := sqliteProject(t, files)
		d := Find(root, nil).Databases[0]
		got, why := SQLiteExclude(root, d, nil)
		if !reflect.DeepEqual(got, c.want) || !strings.Contains(why, c.why) {
			t.Errorf("%q: %q (%s), want %q (%s)", c.env, got, why, c.want, c.why)
		}
	}
	settings := "INSTALLED_APPS = ['django.contrib.sessions']\nDATABASES = {'default': {'ENGINE': 'django.db.backends.sqlite3', 'NAME': 'db.sqlite3'}}\n"
	root := sqliteProject(t, map[string]string{"manage.py": "", "settings.py": settings})
	if got, _ := SQLiteExclude(root, Find(root, nil).Databases[0], nil); !reflect.DeepEqual(got, []string{"django_session"}) {
		t.Errorf("django: %q", got)
	}
	root = sqliteProject(t, map[string]string{"manage.py": "", "settings.py": settings + "SESSION_ENGINE = 'django.contrib.sessions.backends.cache'\n"})
	if got, _ := SQLiteExclude(root, Find(root, nil).Databases[0], nil); got != nil {
		t.Errorf("django with cache sessions: %q", got)
	}
}

// TestSQLiteBaseDir: BASE_DIR as startproject writes it, and nothing else;
// a relative NAME is taken from manage.py's folder.
func TestSQLiteBaseDir(t *testing.T) {
	root := sqliteProject(t, map[string]string{"src/manage.py": "", "src/shop/settings.py": "DATABASES = {'default': {'ENGINE': 'django.db.backends.sqlite3', 'NAME': 'db.sqlite3'}}\n"})
	if _, db, err := (Ref{Source: Python, File: "src/shop/settings.py"}).Read(root, SQLite, nil); err != nil || db != "src/db.sqlite3" {
		t.Errorf("relative NAME: %q, %v", db, err)
	}
	for src, want := range map[string]int{
		"BASE_DIR = Path(__file__).resolve().parent.parent":                                         1,
		"BASE_DIR = Path(__file__).resolve(strict=True).parent.parent.parent":                       2,
		"BASE_DIR = Path(__file__).resolve().parents[1]  # root":                                    1,
		"BASE_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))":                    1,
		"BASE_DIR = Path(env('ROOT'))":                                                              -1,
		"BASE_DIR = Path(__file__).parent.parents":                                                  -1,
		"BASE_DIR = Path(__file__).resolve().parent.parent\nif DEBUG:\n    BASE_DIR = Path('/srv')": -1,
		"x = 1": -1,
	} {
		n, ok := baseDirLevels(src + "\n")
		if !ok {
			n = -1
		}
		if n != want {
			t.Errorf("%s: %d, want %d", src, n, want)
		}
	}
}
