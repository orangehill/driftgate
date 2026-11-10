package dbx

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestPostgresAlone, as a role that is not a superuser (as an application's
// often is): stmt is tried again only while it fails with 55006, at most
// pgAloneTries times; a cancel in the wait returns at once. pgTerminate's
// filter keeps a superuser's session, whose 42501 is returned with the
// 55006 when the session stays (after one wait of the server, 5 s), and
// does not matter when the session ends within that wait.
func TestPostgresAlone(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	su, err := OpenPostgres(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	const role, home, db = "dgx_alone_app", "dgx_alone", "dgx_alone_db"
	renamed := db + "_renamed"
	drop := func() {
		for _, n := range []string{home, db, renamed} {
			su.Exec("DROP DATABASE IF EXISTS " + QuotePostgres(n) + " WITH (FORCE)")
		}
		su.Exec("DROP ROLE IF EXISTS " + role)
	}
	drop()
	t.Cleanup(func() {
		drop()
		su.Close()
	})
	for _, q := range []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD 'app' CREATEDB",
		"CREATE DATABASE " + home + " OWNER " + role,
		"CREATE DATABASE " + db + " OWNER " + role,
	} {
		if _, err := su.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "app")
	admin, err := OpenPostgres(u.String(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	// The seam: a stmt that counts its tries in a sequence (nextval is not
	// undone by the error) and fails with code.
	if _, err := admin.Exec("CREATE SEQUENCE dgx_alone_tries"); err != nil {
		t.Fatal(err)
	}
	counting := func(code string) string {
		return "DO $$BEGIN PERFORM nextval('dgx_alone_tries'); RAISE EXCEPTION USING ERRCODE = '" + code + "'; END$$"
	}
	tries := func() int {
		var n int
		admin.QueryRow("SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM dgx_alone_tries").Scan(&n)
		return n
	}
	restart := func(t *testing.T) {
		t.Helper()
		if _, err := admin.Exec("ALTER SEQUENCE dgx_alone_tries RESTART"); err != nil {
			t.Fatal(err)
		}
	}
	code := func(err error) string {
		pe := (*pgconn.PgError)(nil)
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}

	t.Run("in use", func(t *testing.T) {
		restart(t)
		err := PostgresAlone(ctx, admin, db, counting("55006"))
		if n := tries(); code(err) != "55006" || n != pgAloneTries {
			t.Fatalf("%v after %d tries, want 55006 after %d", err, n, pgAloneTries)
		}
	})
	t.Run("other error", func(t *testing.T) {
		restart(t)
		err := PostgresAlone(ctx, admin, db, counting("22012"))
		if n := tries(); code(err) != "22012" || n != 1 {
			t.Fatalf("%v after %d tries, want 22012 after 1", err, n)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		restart(t)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		at := make(chan time.Time, 1)
		go func() {
			// Cancel once the first try failed: in the wait before the next.
			for tries() < 1 && cctx.Err() == nil {
				time.Sleep(5 * time.Millisecond)
			}
			at <- time.Now()
			cancel()
		}()
		err := PostgresAlone(cctx, admin, db, counting("55006"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled: %v", err)
		}
		if took := time.Since(<-at); took > 150*time.Millisecond {
			t.Fatalf("returned %v after the cancel", took)
		}
		if n := tries(); n >= pgAloneTries {
			t.Fatalf("%d tries after a cancel", n)
		}
	})

	// hold opens a superuser session on db that runs q; done gets its error.
	hold := func(t *testing.T, q string) (done chan error, release func()) {
		t.Helper()
		d, err := OpenPostgres(dsn, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		go func() {
			_, err := d.Exec(q)
			d.Close()
			done <- err
		}()
		for i := 0; ; i++ {
			var n int
			su.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND query = $2", db, q).Scan(&n)
			if n > 0 {
				break
			}
			if i == 200 {
				t.Fatalf("no session on %s", db)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return done, func() {
			su.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", db)
			<-done
		}
	}
	rename := "ALTER DATABASE " + QuotePostgres(db) + " RENAME TO " + QuotePostgres(renamed)

	t.Run("session that stays", func(t *testing.T) {
		_, release := hold(t, "SELECT pg_sleep(60)")
		defer release()
		// The filter (no autovacuum workers) keeps it, also for a role
		// that may not see the superuser's session.
		var n int
		if err := admin.QueryRow(strings.Replace(pgTerminate, "pg_catalog.pg_terminate_backend(pid)", "count(*)", 1), db).Scan(&n); err != nil || n != 1 {
			t.Fatalf("sessions pgTerminate ends: %d, %v", n, err)
		}
		start := time.Now()
		err := PostgresAlone(ctx, admin, db, rename)
		if took := time.Since(start); took > 8*time.Second {
			t.Fatalf("the error took %v", took)
		}
		if code(err) != "55006" || !strings.Contains(err.Error(), "SQLSTATE 42501") {
			t.Fatalf("rename over a superuser's session: %v", err)
		}
	})
	t.Run("session that ends", func(t *testing.T) {
		done, _ := hold(t, "SELECT pg_sleep(1.5)")
		if err := PostgresAlone(ctx, admin, db, rename); err != nil {
			t.Fatalf("rename over a session that ends: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("the session was ended: %v", err)
		}
		if _, err := admin.Exec("ALTER DATABASE " + QuotePostgres(renamed) + " RENAME TO " + QuotePostgres(db)); err != nil {
			t.Fatal(err)
		}
	})
}
