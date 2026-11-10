package state

import (
	"strings"
	"testing"
)

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
