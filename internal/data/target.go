package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/dbx"
)

// The target database is written under a staging name and gets its own name
// only after the run, and the check of Options.Verify, have passed
// (no output before it passes). A failed run,
// an error, a cancelled context or a finding drops the staging database and
// leaves an existing target as it was. Only a process that is killed leaves
// a staging database behind, under a name no application uses.
//
// A target is dropped only when it is provably DriftGate's: every database
// DriftGate writes carries a marker (MySQL and MariaDB: the table
// driftgate_target, which the module never reads or copies; PostgreSQL: the
// comment of the database), and a database with objects but without the
// marker is never dropped, not even with Replace. Before anything is
// written, the source and the target are told apart by what their servers
// say they are, not by the text of their DSNs.

// markerTable marks a MySQL or MariaDB database written by DriftGate.
const markerTable = "driftgate_target"

// markerComment starts the comment that marks a PostgreSQL database written
// by DriftGate.
const markerComment = "DriftGate target"

// markerNote is the text of the marker.
const markerNote = markerComment + ": an anonymized copy written by driftgate data, dropped and written again by driftgate data --replace"

// stagingInfix joins the target's name and a random suffix in the name of
// a staging database.
const stagingInfix = "__driftgate_tmp_"

// dbState is what a database holds, as far as replacing it is concerned.
type dbState struct {
	exists  bool
	objects int  // objects of every kind (tables, views, sequences, routines, types…), the marker not counted
	marked  bool // it carries the marker: DriftGate wrote it
}

// check refuses to replace a database that holds objects without the
// marker, and one with the marker without replace.
func (s dbState) check(name string, replace bool) error {
	switch {
	case s.objects == 0:
		return nil
	case !s.marked:
		return fmt.Errorf("%w: database %s holds %d objects and DriftGate did not write it, so it is never dropped; drop it yourself if it is to be replaced", ErrNotTarget, name, s.objects)
	case !replace:
		return fmt.Errorf("%w: target database %s already holds %d objects", ErrTargetExists, name, s.objects)
	}
	return nil
}

// stagingName is a fresh name next to the target's, at most 63 bytes (the
// limit of PostgreSQL; MySQL allows 64 characters).
func stagingName(target string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	suffix := stagingInfix + hex.EncodeToString(b[:])
	base := target
	for len(base)+len(suffix) > 63 {
		_, n := utf8.DecodeLastRuneInString(base)
		base = base[:len(base)-n]
	}
	return base + suffix, nil
}

// checkNotSource refuses a target that is the source database: the same
// server, by its identity, and the same name. When either server cannot
// say who it is, the target is refused too.
func checkNotSource(ctx context.Context, dl dialect, src, dst Database) error {
	who := func(d Database, db string) (string, error) {
		c, err := open(d, db)
		if err != nil {
			return "", err
		}
		defer c.Close()
		return dl.identity(ctx, c)
	}
	srcDB := "" // the server only
	if src.Driver == Postgres {
		srcDB = src.Name // the source DSN may reach no other database
	}
	a, err := who(src, srcDB)
	if err != nil {
		return fmt.Errorf("source: which server it is cannot be told (%w), so the target cannot be told apart from it", err)
	}
	b, err := who(dst, "")
	if err != nil {
		return fmt.Errorf("target: which server it is cannot be told (%w), so it cannot be told apart from the source", err)
	}
	same := src.Name == dst.Name
	if src.Driver != Postgres {
		same = strings.EqualFold(src.Name, dst.Name) // names may not be case sensitive
	}
	if a == b && same {
		return fmt.Errorf("the target is the source database (%s on the same server)", src.Name)
	}
	return nil
}

// cleanupTimeout bounds dropping a staging database after a failed or
// cancelled run.
const cleanupTimeout = time.Minute

// dropDB drops a database on the server of admin.
func dropDB(ctx context.Context, dl dialect, admin *sql.DB, driver, name string) error {
	q := "DROP DATABASE IF EXISTS " + dl.quote(name)
	if driver == Postgres {
		q += " WITH (FORCE)"
	}
	_, err := admin.ExecContext(ctx, q)
	return err
}

// stage checks the target and creates the staging database next to it.
func (e *engine) stage(ctx context.Context, src, dst Database, replace bool) (*Database, error) {
	admin, err := open(dst, "")
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	st, err := e.inspect(ctx, admin, dst)
	if err != nil {
		return nil, err
	}
	if err := st.check(dst.Name, replace); err != nil {
		return nil, err
	}
	name, err := stagingName(dst.Name)
	if err != nil {
		return nil, err
	}
	if err := e.dl.create(ctx, admin, src, name); err != nil {
		// A part may have been made (PostgreSQL: the database, not its marker).
		dropDB(context.Background(), e.dl, admin, dst.Driver, name)
		return nil, err
	}
	staging := dst
	staging.Name = name
	return &staging, nil
}

// galeraNode reports whether the target's server is a Galera cluster node.
func galeraNode(ctx context.Context, dst Database) (bool, error) {
	admin, err := open(dst, "")
	if err != nil {
		return false, err
	}
	defer admin.Close()
	return dbx.GaleraNode(ctx, admin)
}

// galeraSequences refuses a Galera target the source's sequences it would
// refuse to create (1235), naming them, instead of the server's error once
// the target is half written. Copied without its cache or with another
// engine, a sequence would be another object.
func galeraSequences(ctx context.Context, src *sql.Conn, srcName string, s *Schema, dst Database) error {
	names := make([]string, len(s.sequences))
	for i, q := range s.sequences {
		names[i] = q.name
	}
	refused, err := dbx.GaleraRefusedSequences(ctx, src, srcName, names)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("target %s is on a Galera cluster node, which creates no sequence with a cache and an increment other than 0, nor one of another engine than InnoDB (1235), so these sequences of the source cannot be copied: %s; %s in the source, or write to a server without Galera", dst.Name, strings.Join(refused, ", "), dbx.GaleraSequencesHint)
}

// inspect reads what the target holds, again when the server stops the
// read for a lock (retryRead): it reads information_schema, whose ROUTINES
// and EVENTS a Galera node aborts when another session creates a routine
// or an event.
func (e *engine) inspect(ctx context.Context, admin *sql.DB, dst Database) (st dbState, err error) {
	err = retryRead(ctx, nil, "target", func() error {
		st, err = e.dl.inspect(ctx, admin, dst)
		return err
	})
	return st, err
}

// discard drops the staging database, also after the context of the run
// is cancelled.
func (e *engine) discard(staging Database) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	admin, err := open(staging, "")
	if err != nil {
		return err
	}
	defer admin.Close()
	return dropDB(ctx, e.dl, admin, staging.Driver, staging.Name)
}

// publish replaces the target with the staging database: the target is
// checked again (it may have changed during the run), dropped when it
// exists, and the staging database takes its name.
func (e *engine) publish(ctx context.Context, staging, dst Database, replace bool) error {
	admin, err := open(dst, "")
	if err != nil {
		return err
	}
	defer admin.Close()
	st, err := e.inspect(ctx, admin, dst)
	if err != nil {
		return err
	}
	if err := st.check(dst.Name, replace); err != nil {
		return err
	}
	if st.exists {
		if err := dropDB(ctx, e.dl, admin, dst.Driver, dst.Name); err != nil {
			return err
		}
	}
	return e.dl.publish(ctx, admin, staging, dst)
}
