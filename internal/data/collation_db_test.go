package data

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestMySQLDumpKeepsCollation: a column that takes its table's collation
// without naming it (utf8 and utf8_unicode_ci, as a Laravel 5.5 table) is
// made the same way in the target and by the dump: MySQL 8 would print it
// with CHARACTER SET after a CREATE TABLE run as printed, and the state
// fingerprint of the loaded database would differ from one that
// migrations made.
func TestMySQLDumpKeepsCollation(t *testing.T) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	for _, dsn := range strings.Split(v, ",") {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			s := newServer(t, MySQL, dsn)
			const src, dst, loaded = "dgdata_coll_src", "dgdata_coll_dst", "dgdata_coll_loaded"
			t.Cleanup(func() { s.drop(src, dst, loaded) })
			s.create(t, src)
			s.create(t, loaded)
			s.drop(dst)
			s.exec(t, src,
				"CREATE TABLE roles (id INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(255) NOT NULL) DEFAULT CHARACTER SET utf8 COLLATE 'utf8_unicode_ci'",
				"ALTER TABLE roles ADD guard VARCHAR(255) NULL",
				"ALTER TABLE roles ADD scope VARCHAR(255) CHARACTER SET utf8 COLLATE utf8_unicode_ci NULL",
				"INSERT INTO roles (name, guard) VALUES ('admin', 'web')")
			rules, err := ParseRules([]byte("version: 1\ntables:\n  roles:\n    columns:\n      id: keep\n      name: keep\n      guard: keep\n      scope: keep\n"))
			if err != nil {
				t.Fatal(err)
			}
			target := s.db(dst)
			var dump bytes.Buffer
			if _, err := Anonymize(context.Background(), Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: rules, Key: testKey}); err != nil {
				t.Fatal(err)
			}
			s.exec(t, loaded, splitDump(t, MySQL, dump.String())...)
			def := func(db string) string {
				t.Helper()
				var name, def string
				if err := s.admin.QueryRow("SHOW CREATE TABLE "+mysqlDialect{}.quote(db)+".roles").Scan(&name, &def); err != nil {
					t.Fatal(err)
				}
				return def
			}
			want := def(src)
			for _, db := range []string{dst, loaded} {
				if got := def(db); got != want {
					t.Errorf("%s:\n%s\nsource:\n%s", db, got, want)
				}
			}
		})
	}
}
