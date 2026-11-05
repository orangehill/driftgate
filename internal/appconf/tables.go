package appconf

import (
	"path"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
)

// Tables are the tables a framework keeps its own state in, in the
// application's database, proposed for state: of it. Exclude: never
// compared or restored, as a reset would disturb what uses them while
// DriftGate runs (the developer's own session, a running queue worker,
// monitoring) and no response depends on them. RestoreEmpty: restored to
// no rows, never compared: a cache, whose entries (rate limit counters
// too) would otherwise carry over from one capture to the next and change
// its responses; empty, not the rows the seed found (the developer's own
// cache, entries that never expire). Why says what the proposal comes
// from.
type Tables struct {
	Exclude, RestoreEmpty []string
	Why                   string
}

// FrameworkTables proposes the tables of the database of setting d, for
// any driver: Laravel's sessions, cache, jobs, job batches, failed jobs,
// Telescope and Pulse (laravelTables); Django's sessions; Rails' Solid
// Cache, Solid Queue and Solid Cable in the primary database; Hangfire's
// and the ASP.NET Data Protection keys of an ASP.NET project.
func FrameworkTables(root string, d Database, env Lookup) Tables {
	switch d.Ref.Source {
	case Dotenv:
		return laravelTables(root, d, env)
	case Python:
		return djangoTables(root, d)
	case YAML:
		return railsTables(root, d)
	case AppSettings:
		return dotnetTables(root, d)
	}
	return Tables{}
}

// djangoCache finds the table of Django's database cache: the LOCATION
// after a DatabaseCache backend.
var djangoCache = lazy.Regexp(`django\.core\.cache\.backends\.db\.DatabaseCache['"][^}]*?['"]LOCATION['"]\s*:\s*['"]([A-Za-z0-9_]+)['"]`)

// djangoTables are django_session with database sessions, and the table
// of a database cache (restore_empty).
func djangoTables(root string, d Database) Tables {
	raw, err := readFile(root, d.Ref.File)
	if err != nil {
		return Tables{}
	}
	s := string(raw)
	var t Tables
	var why []string
	sessions := strings.Contains(s, "django.contrib.sessions")
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "SESSION_ENGINE") && !strings.Contains(line, "db") {
			sessions = false
		}
	}
	if sessions {
		t.Exclude, why = []string{"django_session"}, append(why, "sessions")
	}
	if m := djangoCache().FindStringSubmatch(s); m != nil {
		t.RestoreEmpty, why = []string{m[1]}, append(why, "a database cache")
	}
	if len(why) > 0 {
		t.Why = "Django keeps " + strings.Join(why, " and ") + " in the database (" + d.Ref.File + ")"
	}
	return t
}

// railsTables are the Solid Cache, Solid Queue and Solid Cable tables the
// schema of the primary database (db/schema.rb or db/structure.sql) has:
// an application that keeps them there and not in databases of their own
// (as a new Rails 8 application does in production).
func railsTables(root string, d Database) Tables {
	if path.Base(d.Ref.File) != "database.yml" || (d.Ref.Key != "" && d.Ref.Key != "development" && d.Ref.Key != "development.primary") {
		return Tables{}
	}
	var schema, file string
	for _, f := range []string{"db/schema.rb", "db/structure.sql"} {
		if raw, err := readFile(root, f); err == nil {
			schema, file = string(raw), f
			break
		}
	}
	var t Tables
	if strings.Contains(schema, "solid_cache_entries") {
		t.RestoreEmpty = append(t.RestoreEmpty, "solid_cache_entries")
	}
	if strings.Contains(schema, "solid_queue_jobs") {
		t.Exclude = append(t.Exclude, "solid_queue_*")
	}
	if strings.Contains(schema, "solid_cable_messages") {
		t.Exclude = append(t.Exclude, "solid_cable_messages")
	}
	if len(t.Exclude)+len(t.RestoreEmpty) > 0 {
		t.Why = "Solid Cache, Queue or Cable in the primary database (" + file + ")"
	}
	// activerecord-session_store keeps the sessions in a table.
	if gems, err := readFile(root, "Gemfile"); err == nil && strings.Contains(string(gems), "activerecord-session_store") {
		t.Exclude = append(t.Exclude, "sessions")
		t.Why = strings.TrimPrefix(t.Why+"; activerecord-session_store (Gemfile)", "; ")
	}
	return t
}

// dotnetTables are, for an ASP.NET project whose .csproj references them,
// Hangfire's tables (its schema: HangFire on SQL Server, hangfire on
// PostgreSQL) and the Data Protection keys of
// PersistKeysToDbContext (DataProtectionKeys).
//
// The packages may be referenced by another project of the solution (a
// worker, an infrastructure library): every .csproj in the root and up
// to three folders below it is read. Hangfire, the metapackage, names its
// storage with it.
func dotnetTables(root string, d Database) Tables {
	projects := map[string]string{} // file → its text in lower case
	for _, f := range (&finder{root: root}).glob("*.csproj", "*/*.csproj", "*/*/*.csproj", "*/*/*/*.csproj") {
		if raw, err := readFile(root, f); err == nil {
			projects[f] = strings.ToLower(string(raw))
		}
	}
	var files []string // those that reference a package proposed for
	has := func(pkg string) bool {
		found := false
		for _, f := range sortedKeys(projects) {
			if strings.Contains(projects[f], `include="`+pkg+`"`) {
				found = true
				if !slices.Contains(files, f) {
					files = append(files, f)
				}
			}
		}
		return found
	}
	var t Tables
	var why []string
	switch {
	case d.Driver == SQLServer && (has("hangfire.sqlserver") || has("hangfire")):
		t.Exclude, why = append(t.Exclude, "HangFire.*"), append(why, "Hangfire")
	case d.Driver == Postgres && has("hangfire.postgresql"):
		t.Exclude, why = append(t.Exclude, "hangfire.*"), append(why, "Hangfire.PostgreSql")
	}
	if has("microsoft.aspnetcore.dataprotection.entityframeworkcore") {
		t.Exclude, why = append(t.Exclude, "DataProtectionKeys"), append(why, "Data Protection keys in the database")
	}
	if len(why) > 0 {
		t.Why = strings.Join(why, ", ") + " (" + strings.Join(files, ", ") + ")"
	}
	return t
}
