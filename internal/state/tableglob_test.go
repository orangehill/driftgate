package state

import (
	"strings"
	"testing"
)

// TestMatchAny: a glob matches one of the names, after the fold when there
// is one; no glob matches nothing, an empty name only a glob that takes it.
func TestMatchAny(t *testing.T) {
	for _, c := range []struct {
		globs []string
		fold  func(string) string
		names []string
		want  bool
	}{
		{nil, nil, []string{"users"}, false},
		{nil, strings.ToLower, []string{""}, false},
		{[]string{"*"}, nil, []string{""}, true},
		{[]string{"?"}, nil, []string{""}, false},
		{[]string{"users"}, nil, nil, false},
		{[]string{"users"}, nil, []string{"users"}, true},
		{[]string{"users"}, nil, []string{"Users"}, false},
		{[]string{"Users"}, strings.ToLower, []string{"USERS"}, true},
		{[]string{"Été"}, strings.ToLower, []string{"ÉTÉ"}, true},
		{[]string{"Été"}, asciiLower, []string{"ÉTÉ"}, false},
		{[]string{"Été"}, asciiLower, []string{"ÉtÉ"}, false},
		{[]string{"Été"}, asciiLower, []string{"ÉTé"}, true},
		{[]string{"sess*", "job?"}, nil, []string{"public.jobs", "jobs"}, true},
		{[]string{"sess*", "job?"}, nil, []string{"public.job", "job"}, false},
		{[]string{"app.*"}, strings.ToLower, []string{"APP.users", "users"}, true},
		{[]string{"[a]"}, nil, []string{"a"}, false},
		{[]string{"[a]"}, asciiLower, []string{"[A]"}, true},
		{[]string{"a[b"}, nil, []string{"a[b"}, true},
	} {
		if got := matchAny(c.globs, c.fold, c.names...); got != c.want {
			t.Errorf("matchAny(%q, %q) = %v, want %v", c.globs, c.names, got, c.want)
		}
	}
	// The fold leaves the globs and the names as they were.
	globs, names := []string{"USERS"}, []string{"Users", "dbo.Users", "Users"}
	matchAny(globs, strings.ToLower, names...)
	if globs[0] != "USERS" || names[0] != "Users" || names[1] != "dbo.Users" {
		t.Errorf("after matchAny: globs %q, names %q", globs, names)
	}
}

// TestTableGlobs: each driver matches exclude, restore_only and
// restore_empty as its database compares names. MySQL and PostgreSQL take
// them as written (PostgreSQL with or without the schema), SQLite without
// ASCII case (and a shadow table as its virtual table), SQL Server without
// any case and with or without the schema. Brackets match themselves.
func TestTableGlobs(t *testing.T) {
	o := Options{Exclude: []string{"sess*", "Cache", "[a]"}, RestoreOnly: []string{"job?"}, RestoreEmpty: []string{"Été*"}}
	pg, ss := &Postgres{o: o}, &SQLServer{o: o}
	type want struct{ excluded, restoreOnly, restoreEmpty bool }
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
		{"sqlite", "JOBS", want{false, true, false}},
		{"sqlite", "Étés", want{false, true, true}},
		{"sqlite", "ÉTÉS", want{}},
		{"sqlite", "étés", want{}},

		{"sqlserver", "dbo.Sessions", want{true, false, false}},
		{"sqlserver", "dbo.CACHE", want{true, false, false}},
		{"sqlserver", "Sess.Users", want{true, false, false}},
		{"sqlserver", "dbo.[A]", want{true, false, false}},
		{"sqlserver", "app.JOBS", want{false, true, false}},
		{"sqlserver", "dbo.étés", want{false, true, true}},
		{"sqlserver", "dbo.ÉTÉS", want{false, true, true}},
		{"sqlserver", "dbo.users", want{}},
	} {
		var got want
		switch c.driver {
		case "mysql":
			got = want{o.excluded(c.table), o.restoreOnly(c.table), o.restoreEmpty(c.table)}
		case "postgres":
			got = want{pg.excludedQualified(c.table), pg.restoreOnlyQualified(c.table), pg.restoreEmptyQualified(c.table)}
		case "sqlite":
			got = want{o.excludedTable(c.table, ""), o.restoreOnlyTable(c.table, ""), o.restoreEmptyTable(c.table)}
		case "sqlserver":
			schema, name, _ := strings.Cut(c.table, ".")
			got = want{ss.excluded(schema, name), ss.restoreOnly(schema, name), ss.restoreEmpty(schema, name)}
		}
		if got != c.want {
			t.Errorf("%s %s: excluded, restore_only, restore_empty %v, want %v", c.driver, c.table, got, c.want)
		}
	}
	// A shadow table of SQLite is as its virtual table is, whatever its name.
	if !o.excludedTable("x_data", "Sessions") || !o.restoreOnlyTable("x_data", "Jobs") || o.restoreOnlyTable("jobs", "x") {
		t.Error("sqlite: a shadow table is not matched by its virtual table's name")
	}
}
