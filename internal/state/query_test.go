package state

import (
	"context"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

// TestCheckQuery: one read-only SELECT; anything that could write, reach
// the server's files or programs, or hide a second statement is refused.
func TestCheckQuery(t *testing.T) {
	for _, q := range []string{
		"SELECT id FROM news ORDER BY id",
		"select id from news order by id;",
		"WITH t AS (SELECT id FROM news) SELECT id FROM t ORDER BY id",
		"(SELECT id FROM a) UNION (SELECT id FROM b) ORDER BY id",
		"SELECT `id` FROM `news` WHERE slug = 'it''s' AND \"x\" = 1 ORDER BY id",
		"SELECT replace(slug, 'a', 'b'), coalesce(x, 0)::text, col$1 FROM pg_class ORDER BY 1",
		"SELECT CAST(x AS CHAR CHARACTER SET utf8mb4) FROM t ORDER BY 1e3",
	} {
		if err := CheckQuery(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		"",
		"DROP TABLE news",
		"DELETE FROM news ORDER BY id",
		"CREATE TABLE x AS SELECT 1 ORDER BY 1",
		"SELECT 1 ORDER BY 1; DROP TABLE news",
		"SELECT 1 /*!50000 , (SELECT 1) */ ORDER BY 1",
		"SELECT 1 -- ; DROP TABLE news",
		"SELECT 1 # comment",
		"SELECT id FROM news ORDER BY id INTO OUTFILE '/tmp/x'",
		"SELECT id INTO DUMPFILE '/tmp/x' FROM news ORDER BY id",
		"SELECT id INTO newtable FROM news ORDER BY id",
		"WITH d AS (DELETE FROM news RETURNING id) SELECT id FROM d ORDER BY id",
		"WITH d AS (UPDATE news SET a = 1 RETURNING id) SELECT id FROM d ORDER BY id",
		"COPY (SELECT 1) TO PROGRAM 'id'",
		"SELECT query_to_xml('copy (select 1) to program ''id''', true, true, '') ORDER BY 1",
		"SELECT pg_read_file('/etc/passwd') ORDER BY 1",
		"SELECT pg_catalog.pg_ls_dir ('.') ORDER BY 1",
		"SELECT \"pg_read_file\"('/etc/passwd') ORDER BY 1",
		"SELECT lo_import('/etc/passwd') ORDER BY 1",
		"SELECT * FROM dblink('host=x', 'select 1') AS t(a int) ORDER BY 1",
		"SELECT set_config('x', 'y', false) ORDER BY 1",
		"SELECT nextval('s') ORDER BY 1",
		"SELECT LOAD_FILE('/etc/passwd') ORDER BY 1",
		"SELECT SLEEP(10) ORDER BY 1",
		"SELECT $$a$$ ORDER BY 1",
		"SELECT $1 ORDER BY 1",
		"SELECT 'a\\' ; DROP TABLE news; --' ORDER BY 1",
		"SELECT E'\\'' ORDER BY 1",
		"SELECT 'unterminated ORDER BY 1",
		"VALUES (1) ORDER BY 1",
		"SHOW TABLES",
		"CALL p()",
		"SELECT id FROM t FOR UPDATE",
		"SELECT * FROM t PROCEDURE ANALYSE()",
	} {
		if err := CheckQuery(q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}

// TestCommandsDoNotSeeTheKey: hooks and state commands come from the
// repository; the variables that hold the project key are not passed on,
// the ones given to the command (bisect: the commit) are.
func TestCommandsDoNotSeeTheKey(t *testing.T) {
	t.Setenv("DRIFTGATE_KEY", "c2VjcmV0")
	t.Setenv("DG_TEST_PROJECT_KEY", "c2VjcmV0")
	t.Setenv("DG_TEST_OTHER", "kept")
	secret.HideEnv("DG_TEST_PROJECT_KEY")
	check := []string{"sh", "-c", `test -z "$DRIFTGATE_KEY" && test -z "$DG_TEST_PROJECT_KEY" && test "$DG_TEST_OTHER" = kept && test "$DG_TEST_ADDED" = yes`}
	env := []string{"DG_TEST_ADDED=yes"}
	if err := Run(context.Background(), t.TempDir(), check, env, nil); err != nil {
		t.Errorf("restore command: %v", err)
	}
	c := NewCommand("files", t.TempDir(), nil, check, env, nil)
	if _, err := c.Fingerprint(context.Background(), nil); err != nil {
		t.Errorf("fingerprint command: %v", err)
	}
}
