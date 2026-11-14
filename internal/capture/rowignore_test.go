package capture

import (
	"slices"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
)

// TestIgnoredColumns: a row_tracking table named without a schema is in
// the driver's default schema: its ignore list leaves out columns of
// public.users (PostgreSQL) or dbo.Users (SQL Server, any letter case),
// never of a same-named table in another schema.
func TestIgnoredColumns(t *testing.T) {
	pg := []rowIgnore{{table: rowTable(config.Postgres, "users"), globs: []string{"x"}}}
	if got := ignoredColumns(pg, "public.users"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("public.users: %v", got)
	}
	if got := ignoredColumns(pg, "audit.users"); got != nil {
		t.Errorf("audit.users: %v", got)
	}
	ss := []rowIgnore{{table: rowTable(config.SQLServer, "users"), globs: []string{"x"}}}
	if got := ignoredColumns(ss, "dbo.Users"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("dbo.Users: %v", got)
	}
	if got := ignoredColumns(ss, "sales.users"); got != nil {
		t.Errorf("sales.users: %v", got)
	}
	my := []rowIgnore{{table: rowTable(config.MySQL, "users"), globs: []string{"x"}}}
	if got := ignoredColumns(my, "Users"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("mysql users: %v", got)
	}
}
