package data

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/dbx"
)

// TestRetryNote: the note reads as a sentence for every read retryRead
// runs.
func TestRetryNote(t *testing.T) {
	err := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	for _, what := range []string{"source", "target"} {
		want := what + ": the server stopped the read of the " + what + " for a lock (Error 1213: Deadlock), as a Galera node does when another session alters a table; it is read again, and this is not noted again\n"
		if got := retryNote(what, err); got != want {
			t.Errorf("%s:\n%q\nwant\n%q", what, got, want)
		}
	}
}

// TestRetryRead: the fault of a try runs after its read, only when the
// read succeeded, and a read whose fault returns the server's lock error
// is read again.
func TestRetryRead(t *testing.T) {
	defer func(d time.Duration) { lockRetryWait = d }(lockRetryWait)
	lockRetryWait = time.Millisecond
	defer func() { readFault = nil }()
	retryNoted.Clear()
	defer retryNoted.Clear()
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	var calls []string
	readFault = func(what string) error {
		calls = append(calls, "fault "+what)
		if len(calls) < 6 {
			return deadlock
		}
		return nil
	}
	err := retryRead(context.Background(), nil, "source", func() error { calls = append(calls, "read"); return nil })
	want := []string{"read", "fault source", "read", "fault source", "read", "fault source"}
	if err != nil || !reflect.DeepEqual(calls, want) {
		t.Errorf("%v after %q, want %q", err, calls, want)
	}
	calls = nil
	other := &mysql.MySQLError{Number: 1146}
	err = retryRead(context.Background(), nil, "source", func() error { calls = append(calls, "read"); return other })
	if err != other || !reflect.DeepEqual(calls, []string{"read"}) {
		t.Errorf("a failed read: %v after %q", err, calls)
	}
}

// TestMySQLRetrySourceRead: the source phase of anonymize (with a subset
// and an index of originals), of verify (source and target) and of
// classify, stopped for a lock (1213, injected once each try has built its
// state), runs again and then gives exactly what a run without the stop
// gives: the same dump byte for byte, the same reports; so do the reads
// of the target before it is written. Stopped every
// time, anonymize returns the server's error and writes no target.
func TestMySQLRetrySourceRead(t *testing.T) {
	eachMySQL(t, testRetrySourceRead)
}

func testRetrySourceRead(t *testing.T, s *server) {
	defer func(d time.Duration) { lockRetryWait = d }(lockRetryWait)
	lockRetryWait = time.Millisecond
	defer func() { readFault = nil }()
	retryNoted.Clear()
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	ctx := context.Background()
	const src, dst = "dgretry_src", "dgretry_dst"
	t.Cleanup(func() { s.drop(src, dst) })
	var version string
	s.admin.QueryRow("SELECT VERSION()").Scan(&version)
	mariadb := strings.Contains(strings.ToLower(version), "mariadb")
	ps := people(testUsers())
	s.create(t, src)
	s.drop(dst)
	s.exec(t, src, append(seedStatements(s.driver, ps, invoiceSequence(s, mariadb)), subsetSeed(s.driver, ps)...)...)
	rules, err := ParseRules([]byte(testRules + subsetRules))
	if err != nil {
		t.Fatal(err)
	}
	// stop fails the first n tries of what.
	tries := 0
	stop := func(what string, n int) {
		tries = 0
		readFault = func(w string) error {
			if w != what {
				return nil
			}
			tries++
			if tries <= n {
				return deadlock
			}
			return nil
		}
	}
	type run struct {
		dump  []byte
		rep   *Report
		index VerifyIndex
		log   string
	}
	anonymize := func(target *Database) (run, error) {
		var r run
		var dump, log bytes.Buffer
		rep, err := Anonymize(ctx, Options{Source: s.db(src), Target: target, Replace: true, Dump: &dump, Rules: rules, Key: testKey,
			Filter: &Subset{}, Index: &r.index, Log: &log})
		r.dump, r.rep, r.log = dump.Bytes(), rep, log.String()
		return r, err
	}

	readFault = nil
	clean, err := anonymize(nil)
	if err != nil {
		t.Fatal(err)
	}
	stop("source", 2)
	retried, err := anonymize(nil)
	if err != nil || tries != 3 {
		t.Fatalf("anonymize: %v after %d tries", err, tries)
	}
	if !bytes.Equal(clean.dump, retried.dump) {
		t.Fatalf("the dump after a retry differs:\n%s", firstDifference(clean.dump, retried.dump))
	}
	clean.rep.Seconds, retried.rep.Seconds = 0, 0
	if !reflect.DeepEqual(clean.rep.Tables, retried.rep.Tables) {
		t.Errorf("reports differ:\n%+v\n%+v", clean.rep.Tables, retried.rep.Tables)
	}
	if strings.Count(retried.log, "stopped the read of the source for a lock") != 1 ||
		strings.Count(retried.log, "subset:") != strings.Count(clean.log, "subset:") {
		t.Errorf("log after a retry:\n%s\nwithout:\n%s", retried.log, clean.log)
	}
	if clean.index.x.count() != retried.index.x.count() {
		t.Errorf("index of originals: %d values, %d after a retry", clean.index.x.count(), retried.index.x.count())
	}

	// Stopped every time: the server's error, after dbx.MySQLLockRetries
	// more tries, and no target, staging database or dump.
	stop("source", 1000)
	target := s.db(dst)
	failed, err := anonymize(&target)
	if !errors.Is(err, deadlock) || tries != dbx.MySQLLockRetries+1 {
		t.Fatalf("anonymize: %v after %d tries", err, tries)
	}
	if len(failed.dump) != 0 {
		t.Errorf("a dump was written: %d bytes", len(failed.dump))
	}
	if n := s.query(t, "", "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE '"+dst+"%'"); n != "0" {
		t.Errorf("%s databases named like the target", n)
	}

	// The reads of the target before the staging database is made and
	// before it replaces the target (stage, publish).
	stop("target", 2)
	if _, err := anonymize(&target); err != nil || tries != 4 {
		t.Fatalf("anonymize, target stopped: %v after %d tries", err, tries)
	}

	// Verify: its own read of the source, and its scan of the target, with
	// the same log (each kind of read was noted above).
	verify := func() (*VerifyReport, string) {
		t.Helper()
		var log bytes.Buffer
		rep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Rules: rules, Key: testKey, Log: &log})
		if err != nil {
			t.Fatal(err)
		}
		rep.Seconds = 0
		return rep, log.String()
	}
	readFault = nil
	want, wantLog := verify()
	for _, what := range []string{"source", "target"} {
		stop(what, 2)
		if got, log := verify(); tries != 3 || !reflect.DeepEqual(got, want) || log != wantLog {
			t.Errorf("verify, %s stopped: %d tries, %+v, want %+v; log:\n%s\nwant:\n%s", what, tries, got, want, log, wantLog)
		}
	}

	// Classify.
	readFault = nil
	cls, err := Classify(ctx, s.db(src), rules, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	stop("source", 2)
	if got, err := Classify(ctx, s.db(src), rules, nil, 5); err != nil || tries != 3 || !reflect.DeepEqual(got, cls) {
		t.Errorf("classify: %v after %d tries", err, tries)
	}
}

// TestMySQLGaleraTargetSequences (MariaDB Galera): a target on a Galera
// node gets no sequence the node refuses to create (a cache with an
// increment other than 0, another engine than InnoDB): anonymize names
// them before anything is written, and writes no target or staging
// database. A dump, which may be loaded anywhere, is still made.
func TestMySQLGaleraTargetSequences(t *testing.T) {
	eachMySQL(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		var wsrep int
		if s.admin.QueryRow("SELECT @@wsrep_on").Scan(&wsrep) != nil || wsrep == 0 {
			t.Skip("not a Galera node")
		}
		const src, dst = "dggalera_src", "dggalera_dst"
		t.Cleanup(func() { s.drop(src, dst) })
		s.create(t, src)
		s.drop(dst)
		s.exec(t, src, "CREATE TABLE t (id INT PRIMARY KEY)", "INSERT INTO t VALUES (1)", "CREATE SEQUENCE plain NOCACHE")
		// Only a session without wsrep makes what the node refuses.
		c, err := s.admin.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"SET SESSION wsrep_on = 0", "CREATE SEQUENCE " + src + ".cached CACHE 10",
			"CREATE SEQUENCE " + src + ".aria NOCACHE ENGINE=Aria", "SET SESSION wsrep_on = 1"} {
			if _, err := c.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		c.Close()
		rules, err := ParseRules([]byte("version: 1\ntables:\n  t:\n    columns: {id: keep}\n"))
		if err != nil {
			t.Fatal(err)
		}
		target := s.db(dst)
		var dump bytes.Buffer
		_, err = Anonymize(ctx, Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: rules, Key: testKey})
		if err == nil || !strings.Contains(err.Error(), "cached (CACHE 10, INCREMENT BY 1)") || !strings.Contains(err.Error(), "aria (ENGINE=Aria)") ||
			strings.Contains(err.Error(), "plain") || strings.Contains(err.Error(), "1235") && !strings.Contains(err.Error(), "Galera cluster node") {
			t.Fatalf("anonymize: %v", err)
		}
		if dump.Len() != 0 {
			t.Errorf("a dump was written: %d bytes", dump.Len())
		}
		if n := s.query(t, "", "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE '"+dst+"%'"); n != "0" {
			t.Errorf("%s databases named like the target", n)
		}
		dump.Reset()
		if _, err := Anonymize(ctx, Options{Source: s.db(src), Dump: &dump, Rules: rules, Key: testKey}); err != nil || !strings.Contains(dump.String(), "cached") {
			t.Errorf("dump only: %v", err)
		}
	})
}
