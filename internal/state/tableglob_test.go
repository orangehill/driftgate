package state

import (
	"slices"
	"strings"
	"testing"
)

// TestMatchGlobs: a glob matches the name; no glob matches nothing, an
// empty name only a glob that takes it. Brackets match themselves.
func TestMatchGlobs(t *testing.T) {
	for _, c := range []struct {
		globs []string
		name  string
		want  bool
	}{
		{nil, "users", false},
		{[]string{"*"}, "", true},
		{[]string{"?"}, "", false},
		{[]string{"users"}, "users", true},
		{[]string{"users"}, "Users", false},
		{[]string{"sess*", "job?"}, "jobs", true},
		{[]string{"sess*", "job?"}, "public.jobs", false},
		{[]string{"app.*"}, "app.users", true},
		{[]string{"[a]"}, "a", false},
		{[]string{"[a]"}, "[a]", true},
		{[]string{"a[b"}, "a[b", true},
	} {
		if got := matchGlobs(c.globs, c.name); got != c.want {
			t.Errorf("matchGlobs(%q, %q) = %v, want %v", c.globs, c.name, got, c.want)
		}
	}
}

// TestNewTableGlobs: the globs are folded copies, restore_empty's among
// restore_only's; the Options keep theirs as written. A driver whose
// constructor did not make its globs stops at its first match.
func TestNewTableGlobs(t *testing.T) {
	o := Options{Exclude: []string{"USERS", "Été"}, RestoreOnly: []string{"Jobs"}, RestoreEmpty: []string{"Cache"}}
	for _, c := range []struct {
		fold                 func(string) string
		exclude, only, empty []string
	}{
		{nil, []string{"USERS", "Été"}, []string{"Jobs", "Cache"}, []string{"Cache"}},
		{asciiLower, []string{"users", "Été"}, []string{"jobs", "cache"}, []string{"cache"}},
		{strings.ToLower, []string{"users", "été"}, []string{"jobs", "cache"}, []string{"cache"}},
	} {
		g := newTableGlobs(o, c.fold)
		if !slices.Equal(g.exclude, c.exclude) || !slices.Equal(g.only, c.only) || !slices.Equal(g.empty, c.empty) {
			t.Errorf("newTableGlobs = %+v, want %q, %q, %q", g, c.exclude, c.only, c.empty)
		}
	}
	if o.Exclude[0] != "USERS" || o.RestoreOnly[0] != "Jobs" || o.RestoreEmpty[0] != "Cache" {
		t.Errorf("Options changed: %+v", o)
	}
	if g := newTableGlobs(Options{}, strings.ToLower); g.excluded("x") || g.restoreOnly("x") || g.restoreEmpty("x") {
		t.Error("no globs match a name")
	}
	defer func() {
		if recover() == nil {
			t.Error("a driver without its globs matched")
		}
	}()
	(&MySQL{}).g.excluded("x")
}

// FuzzLowerParts: lowering schema and name apart and joining them with a
// dot, as SQL Server's matchers do, is lowering schema.name whole, in any
// bytes.
func FuzzLowerParts(f *testing.F) {
	for _, s := range [][2]string{{"dbo", "Users"}, {"\xc3", "\xa9"}, {"\xff", "x"}, {"\u212a", "\u212a"}, {"ΣΑΣ", "ΣΑΣ"}, {"İ", "ẞ"}, {"", ""}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, schema, name string) {
		if parts, whole := strings.ToLower(schema)+"."+strings.ToLower(name), strings.ToLower(schema+"."+name); parts != whole {
			t.Errorf("%q.%q: %q apart, %q whole", schema, name, parts, whole)
		}
	})
}

// TestTableGlobs: each driver, as its constructor makes it, matches
// exclude, restore_only and restore_empty as its database compares names.
// MySQL and PostgreSQL take them as written (PostgreSQL with or without
// the schema), SQLite without ASCII case (and a shadow table as its
// virtual table), SQL Server without any case and with or without the
// schema. Brackets match themselves.
func TestTableGlobs(t *testing.T) {
	type want struct{ excluded, restoreOnly, restoreEmpty bool }
	match := func(t *testing.T, o Options) func(driver, table string) want {
		my, err := NewMySQL(withDSN(o, "mysql://u:p@127.0.0.1:1/"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { my.Close() })
		pg, err := NewPostgres(withDSN(o, "postgres://u:p@127.0.0.1:1/postgres"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.Close() })
		ss, err := NewSQLServer(withDSN(o, "sqlserver://u:p@127.0.0.1:1"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ss.Close() })
		sl := newSQLite(o) // NewSQLite's, without a working file to open
		return func(driver, table string) want {
			switch driver {
			case "mysql":
				return want{my.g.excluded(table), my.g.restoreOnly(table), my.g.restoreEmpty(table)}
			case "postgres":
				return want{pg.excludedQualified(table), pg.restoreOnlyQualified(table), pg.restoreEmptyQualified(table)}
			case "sqlite":
				return want{sl.excludedTable(table, ""), sl.restoreOnlyTable(table, ""), sl.restoreEmptyTable(table)}
			}
			schema, name, _ := strings.Cut(table, ".")
			tb := &ssTable{Schema: schema, Name: name}
			ss.globFlags(tb)
			if ss.excluded(schema, name) != tb.Excluded {
				t.Errorf("sqlserver %s: excluded and globFlags differ", table)
			}
			return want{tb.Excluded, tb.RestoreOnly, tb.RestoreEmpty}
		}
	}
	o := Options{Exclude: []string{"sess*", "Cache", "[a]", "Ünit"}, RestoreOnly: []string{"job?"}, RestoreEmpty: []string{"Été*"}}
	got := match(t, o)
	for _, c := range []struct {
		driver, table string
		want
	}{
		{"mysql", "sessions", want{true, false, false}},
		{"mysql", "Sessions", want{}},
		{"mysql", "Cache", want{true, false, false}},
		{"mysql", "cache", want{}},
		{"mysql", "[a]", want{true, false, false}},
		{"mysql", "a", want{}},
		{"mysql", "jobs", want{false, true, false}},
		{"mysql", "Jobs", want{}},
		{"mysql", "job", want{}},
		{"mysql", "Étés", want{false, true, true}},
		{"mysql", "étés", want{}},

		{"postgres", "public.sessions", want{true, false, false}},
		{"postgres", "public.Sessions", want{}},
		{"postgres", "sess.users", want{true, false, false}},
		{"postgres", "public.[a]", want{true, false, false}},
		{"postgres", "public.jobs", want{false, true, false}},
		{"postgres", "app.Jobs", want{}},
		{"postgres", "public.Étés", want{false, true, true}},
		{"postgres", "public.étés", want{}},

		{"sqlite", "Sessions", want{true, false, false}},
		{"sqlite", "CACHE", want{true, false, false}},
		{"sqlite", "[A]", want{true, false, false}},
		{"sqlite", "Ünit", want{true, false, false}},
		{"sqlite", "ÜNIT", want{true, false, false}},
		{"sqlite", "ünit", want{}},
		{"sqlite", "JOBS", want{false, true, false}},
		{"sqlite", "Étés", want{false, true, true}},
		{"sqlite", "ÉTÉS", want{}},
		{"sqlite", "étés", want{}},

		{"sqlserver", "dbo.Sessions", want{true, false, false}},
		{"sqlserver", "dbo.CACHE", want{true, false, false}},
		{"sqlserver", "Sess.Users", want{true, false, false}},
		{"sqlserver", "dbo.[A]", want{true, false, false}},
		{"sqlserver", "dbo.ünit", want{true, false, false}},
		{"sqlserver", "app.JOBS", want{false, true, false}},
		{"sqlserver", "dbo.étés", want{false, true, true}},
		{"sqlserver", "dbo.ÉTÉS", want{false, true, true}},
		{"sqlserver", "dbo.users", want{}},
	} {
		if g := got(c.driver, c.table); g != c.want {
			t.Errorf("%s %s: excluded, restore_only, restore_empty %v, want %v", c.driver, c.table, g, c.want)
		}
	}
	// A shadow table of SQLite is as its virtual table is, whatever its name.
	sl := newSQLite(o)
	if !sl.excludedTable("x_data", "Sessions") || !sl.restoreOnlyTable("x_data", "Jobs") || sl.restoreOnlyTable("jobs", "x") {
		t.Error("sqlite: a shadow table is not matched by its virtual table's name")
	}

	// A glob with a schema matches the table in that schema only; one
	// without, SQL Server's in any schema.
	for _, c := range []struct {
		o             Options
		driver, table string
		want
	}{
		{Options{RestoreOnly: []string{"app.*"}}, "postgres", "app.x", want{false, true, false}},
		{Options{RestoreOnly: []string{"app.*"}}, "postgres", "public.x", want{}},
		{Options{RestoreEmpty: []string{"app.*"}}, "postgres", "app.x", want{false, true, true}},
		{Options{RestoreEmpty: []string{"app.*"}}, "postgres", "public.x", want{}},
		{Options{Exclude: []string{"*"}}, "postgres", "x", want{true, false, false}},
		{Options{Exclude: []string{"App.*"}}, "sqlserver", "app.x", want{true, false, false}},
		{Options{Exclude: []string{"App.*"}}, "sqlserver", "dbo.x", want{}},
		{Options{Exclude: []string{"X"}}, "sqlserver", "app.x", want{true, false, false}},
	} {
		if g := match(t, c.o)(c.driver, c.table); g != c.want {
			t.Errorf("%+v %s %s: excluded, restore_only, restore_empty %v, want %v", c.o, c.driver, c.table, g, c.want)
		}
	}
}

func withDSN(o Options, dsn string) Options {
	o.DSN, o.Working, o.Pristine = dsn, "shop", "shop_pristine"
	return o
}
