package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
)

// A fingerprint of the working database reads what moves with every
// change first, with VIEW SERVER PERFORMANCE STATE (sqlServerInfo.Perf):
//
//  1. The end of the log, after sp_flush_log (a commit under delayed
//     durability is in the log only once it is flushed), and where every
//     identity column and sequence stands (ssValues): a rolled-back INSERT
//     moves an identity and NEXT VALUE FOR a cached sequence without a
//     record in the log. When none of them moved since the last full
//     fingerprint, nothing changed, and it is reused (Reused). Not with
//     memory-optimized tables: their SCHEMA_ONLY writes are not logged.
//  2. Else, per table, the rows (sys.partitions), the write counters of
//     its indexes (sys.dm_db_index_operational_stats: every INSERT,
//     UPDATE, also of a column to its own value, DELETE, MERGE, bulk
//     load, cascade and trigger write moves one), the number and time of
//     the statements that wrote to it (sys.dm_db_index_usage_stats, which
//     an index rebuild does not reset) and its create_date and modify_date
//     (TRUNCATE, SWITCH, ALTER). Only tables whose signature or definition
//     changed are read again; memory-optimized tables and tables with a
//     text, ntext or image column (WRITETEXT moves no counter) always are.
//     After a restart of the server every table is read.
//
// What a fingerprint keeps for the next one is read before any table, so a
// write made while the tables are read shows next time. A transaction that
// wrote and is still open is waited for (up to sqlServerOpenWait): its rows
// are not read under READ COMMITTED, and its commit moves no counter. One
// whose every write lock is on excluded tables (and the system tables and
// extents any insert locks) is not: those tables are not read. When any
// table was read, the table signatures and values are read again after
// the tables: when they moved, or a write transaction was still open, the
// tables are read again (up to sqlServerAttempts times), so no fingerprint
// mixes rows from before and after a commit.
//
// Without the permission every fingerprint reads every table, and checks
// the signatures db_owner can read (rows and index write counters); a
// transaction it cannot see that wrote before the reading and commits
// during it, on a database with READ_COMMITTED_SNAPSHOT, is not noticed.
//
// Either way the catalog is read again only when it changed (ssGuardSQL).

// sqlServerOpenWait is how long a fingerprint waits for open write
// transactions to end.
var sqlServerOpenWait = 10 * time.Second

// sqlServerOpenTell is how long a fingerprint waits for open write
// transactions before it says so.
var sqlServerOpenTell = 2 * time.Second

// sqlServerBeforeScan, when set, runs between the signals and the tables a
// fingerprint reads (tests: a write made meanwhile).
var sqlServerBeforeScan func()

// ssLast is what the next fingerprint may reuse: the last full
// fingerprint, the signals read before it, and per table its definition,
// signature and content.
type ssLast struct {
	f       Fingerprint
	sig     *ssSignals
	memory  bool // memory-optimized tables were fingerprinted
	content map[int64]ssKept
}

type ssKept struct{ item, def, sig, content string }

// ssSignals are what a fingerprint reads first.
type ssSignals struct {
	lsn    string // the end of the log, flushed
	values *ssValues
	guard  string // the catalog (ssGuardSQL); "" when it changed in the last seconds
	open   bool   // a transaction that wrote to the database is open
	lock   string // a table such a transaction holds a lock on, or ""
	start  string // when the server started (sqlServerInfo.Start)
	tables map[int64]string
}

// ssGuardSQL reads, in one result set, what changes with every change of
// the catalog that a definition shows: every object's modify_date (ALTER,
// also of a constraint's or trigger's flags, index changes, renames), the
// database's DDL triggers, permissions, extended properties, schemas,
// types, users and assemblies. ssRecentSQL then says whether an object
// changed in the last two seconds: modify_date counts in steps of about 3
// ms, so a change right after the reading could keep its value, and such
// a reading is not used.
const ssGuardSQL = `
SELECT CONCAT(N'o ', object_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.objects WHERE is_ms_shipped = 0
UNION ALL SELECT CONCAT(N't ', object_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.triggers WHERE parent_class = 0
UNION ALL SELECT CONCAT(N'p ', class, N' ', major_id, N' ', minor_id, N' ', grantee_principal_id, N' ', grantor_principal_id, N' ', type COLLATE DATABASE_DEFAULT, N' ', state COLLATE DATABASE_DEFAULT) COLLATE DATABASE_DEFAULT
  FROM sys.database_permissions
UNION ALL SELECT CONCAT(N'e ', class, N' ', major_id, N' ', minor_id, N' ', name COLLATE DATABASE_DEFAULT, N' ', CAST(SQL_VARIANT_PROPERTY(value, 'BaseType') AS nvarchar(128)) COLLATE DATABASE_DEFAULT,
         N' ', CAST(SQL_VARIANT_PROPERTY(value, 'Collation') AS nvarchar(128)) COLLATE DATABASE_DEFAULT, N' ', CONVERT(varchar(max), CAST(value AS varbinary(8000)), 1)) COLLATE DATABASE_DEFAULT
  FROM sys.extended_properties WHERE NOT (class = 0 AND name = N'driftgate_pristine')
UNION ALL SELECT CONCAT(N's ', schema_id, N' ', name, N' ', principal_id) COLLATE DATABASE_DEFAULT FROM sys.schemas
UNION ALL SELECT CONCAT(N'y ', user_type_id, N' ', schema_id, N' ', name, N' ', rule_object_id, N' ', default_object_id) COLLATE DATABASE_DEFAULT FROM sys.types WHERE is_user_defined = 1
UNION ALL SELECT CONCAT(N'm ', role_principal_id, N' ', member_principal_id) COLLATE DATABASE_DEFAULT FROM sys.database_role_members
UNION ALL SELECT CONCAT(N'f ', function_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.partition_functions
UNION ALL SELECT CONCAT(N'd ', partition_scheme_id, N' ', destination_id, N' ', data_space_id) COLLATE DATABASE_DEFAULT FROM sys.destination_data_spaces
UNION ALL SELECT CONCAT(N'x ', xml_collection_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.xml_schema_collections
UNION ALL SELECT CONCAT(N'u ', principal_id, N' ', name, N' ', type COLLATE DATABASE_DEFAULT, N' ', default_schema_name, N' ', owning_principal_id, N' ', CONVERT(varchar(200), sid, 1)) COLLATE DATABASE_DEFAULT FROM sys.database_principals
UNION ALL SELECT CONCAT(N'a ', assembly_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.assemblies;`

// ssRecentSQL says whether an object changed in the last two seconds
// (ssGuardSQL), but for the modify_date values of DriftGate's own last
// restore in @p1 (ssOwnDDL.values, "|object_id date|…") while the
// server's clock is less than five seconds past the latest of them in @p2
// (parameters, so the batch is compiled once).
const ssRecentSQL = `
DECLARE @dg_own nvarchar(max) = CASE WHEN @p2 <> N'' AND GETDATE() < DATEADD(second, 5, CONVERT(datetime, @p2, 121)) THEN @p1 ELSE N'' END;
SELECT CASE WHEN EXISTS (SELECT 1 FROM sys.objects o WHERE o.is_ms_shipped = 0 AND o.modify_date >= DATEADD(second, -2, GETDATE())
                            AND CHARINDEX(CONCAT(N'|', o.object_id, N' ', CONVERT(nvarchar(30), o.modify_date, 121), N'|'), @dg_own) = 0)
              OR EXISTS (SELECT 1 FROM sys.triggers o WHERE o.parent_class = 0 AND o.modify_date >= DATEADD(second, -2, GETDATE())
                            AND CHARINDEX(CONCAT(N'|', o.object_id, N' ', CONVERT(nvarchar(30), o.modify_date, 121), N'|'), @dg_own) = 0) THEN 1 ELSE 0 END;`

// ssOwnDDL are the objects DriftGate's last item-level restore changed the
// definition of (it turned triggers and constraints off and on again,
// restarted sequences), with the modify_date each had when it committed,
// as "|object_id date|…|"; "" for none. A fingerprint does not take them
// for a change of the last two seconds: the restore read them in its
// transaction, while it held the schema lock that any other change of
// them needs, and committed only once the server's clock had passed them
// (commitOwn), so any later change has a later modify_date. They are
// dropped a few seconds after the latest of them by the server's clock
// (ssRecentSQL), and by DriftGate's, when they are no longer recent
// anyway.
type ssOwnDDL struct {
	values string
	latest string    // the latest of them, by the server's clock
	at     time.Time // when the restore committed
}

// ssLogSQL flushes the log and reads its end (tier 1).
const ssLogSQL = `EXEC sys.sp_flush_log;
SELECT CAST(log_end_lsn AS nvarchar(64)) FROM sys.dm_db_log_stats(DB_ID());`

// ssTablesSQL reads the signature of every table in three result sets,
// joined by object id (signals; as one query the optimizer reads the
// counters once per table): the table's name and dates, rows and index
// write counters; a db_owner reads them. ssUsageSQL adds the statements
// that wrote to every table, then the transactions that wrote to the
// database and are open, with the objects they hold a write lock on (a
// row per transaction and object; NULL: a lock on no object, or none):
// after them, so one that wrote before the signatures shows. It needs VIEW
// SERVER PERFORMANCE STATE.
const (
	ssTablesSQL = `
SELECT object_id, SCHEMA_NAME(schema_id), name, CONCAT(CONVERT(varchar(30), create_date, 121), ' ', CONVERT(varchar(30), modify_date, 121)) FROM sys.objects WHERE type = 'U' AND is_ms_shipped = 0;
SELECT object_id, CAST(SUM(rows) AS varchar(20)) FROM sys.partitions WHERE index_id IN (0, 1) GROUP BY object_id;
SELECT object_id, CONCAT(SUM(leaf_insert_count), ' ', SUM(leaf_delete_count), ' ', SUM(leaf_update_count), ' ', SUM(leaf_ghost_count),
       ' ', SUM(nonleaf_insert_count + nonleaf_delete_count + nonleaf_update_count), ' ', SUM(leaf_allocation_count + nonleaf_allocation_count), ' ', COUNT(*))
  FROM sys.dm_db_index_operational_stats(DB_ID(), NULL, NULL, NULL) GROUP BY object_id;`
	ssUsageSQL = `
SELECT object_id, CONCAT(SUM(user_updates), ' ', CONVERT(varchar(30), MAX(last_user_update), 121))
  FROM sys.dm_db_index_usage_stats WHERE database_id = DB_ID() GROUP BY object_id;
SELECT DISTINCT t.transaction_id, o.type, OBJECT_SCHEMA_NAME(o.object_id), o.name, l.resource_type
  FROM sys.dm_tran_database_transactions t
  LEFT JOIN (SELECT k.request_owner_id, k.resource_type,
                    CASE WHEN k.resource_type = N'OBJECT' THEN k.resource_associated_entity_id
                         WHEN k.resource_type IN (N'PAGE', N'KEY', N'RID', N'HOBT') THEN (SELECT p.object_id FROM sys.partitions p WHERE p.hobt_id = k.resource_associated_entity_id)
                         WHEN k.resource_type = N'ALLOCATION_UNIT' THEN (SELECT p.object_id FROM sys.allocation_units a JOIN sys.partitions p ON p.partition_id = a.container_id
                                                                        WHERE a.allocation_unit_id = k.resource_associated_entity_id) END AS object_id
               FROM sys.dm_tran_locks k
              WHERE k.resource_database_id = DB_ID() AND k.request_owner_type = N'TRANSACTION'
                AND k.request_mode NOT IN (N'S', N'IS', N'Sch-S', N'RangeS-S', N'NULL')) l ON l.request_owner_id = t.transaction_id
  LEFT JOIN sys.objects o ON o.object_id = l.object_id
 WHERE t.database_id = DB_ID() AND t.database_transaction_type = 1 AND t.database_transaction_log_record_count > 0;`
)

// ssDMLSystemTables are the system tables any INSERT, UPDATE, DELETE or
// TRUNCATE (of an excluded table too) takes a write lock on: where an
// identity stands, a sequence's value, the allocation of rows.
var ssDMLSystemTables = map[string]bool{"syscolpars": true, "sysobjvalues": true, "sysallocunits": true, "sysrowsets": true, "sysrscols": true}

// ssOpenLock is a write lock an open transaction holds (ssUsageSQL).
type ssOpenLock struct {
	kind, schema, name, resource sql.NullString
}

// openWrites reports whether open transactions (their write locks, by
// transaction) may have written to what a fingerprint reads, and names a
// table one of them holds a lock on. A transaction that is passed over
// holds a write lock on an excluded table or sequence, and otherwise only
// on the system tables and extents such a write locks (DDL locks more).
func (s *SQLServer) openWrites(locks map[int64][]ssOpenLock) (open bool, table string) {
	for _, ls := range locks {
		excluded, other := false, ""
		for _, l := range ls {
			switch kind := strings.TrimSpace(l.kind.String); {
			case (kind == "U" || kind == "SO") && s.excluded(l.schema.String, l.name.String):
				excluded = true
			case kind == "S" && ssDMLSystemTables[l.name.String]:
			case !l.kind.Valid && l.resource.String == "EXTENT":
			case l.name.Valid:
				other = l.schema.String + "." + l.name.String
			default:
				other = "?"
			}
		}
		if excluded && other == "" {
			continue
		}
		open = true
		if table == "" || table == "?" {
			table = other
		}
	}
	if table == "?" {
		table = ""
	}
	return open, table
}

// sqlServerAttempts is how often a fingerprint reads the tables before it
// gives up on a database that changes while it is read.
var sqlServerAttempts = 3

// signals reads, on a connection to the working database, the values;
// with log the end of the log (tier 1); with guard the catalog guard; with
// tables the table signatures (tier 2) and, with VIEW SERVER PERFORMANCE
// STATE, the statements per table and the open transactions.
func (s *SQLServer) signals(ctx context.Context, c *sql.Conn, log, guard, tables bool) (*ssSignals, error) {
	sig := &ssSignals{values: &ssValues{}, start: s.info.Start}
	var batch string
	var sets []func(*sql.Rows) error
	if log {
		batch = ssLogSQL
		sets = append(sets, func(r *sql.Rows) error { return r.Scan(&sig.lsn) })
	}
	valueSets, done := ssValueSets(sig.values, s.excluded)
	batch += ssValuesSQL
	sets = append(sets, valueSets...)
	var lines []string
	var recent int
	locks := map[int64][]ssOpenLock{}
	var args []any
	if guard {
		own, latest := "", ""
		if time.Since(s.own.at) < 5*time.Second {
			own, latest = s.own.values, s.own.latest
		}
		batch += ssGuardSQL + ssRecentSQL
		args = append(args, own, latest)
		sets = append(sets, func(r *sql.Rows) error {
			var line string
			if err := r.Scan(&line); err != nil {
				return err
			}
			lines = append(lines, line)
			return nil
		}, func(r *sql.Rows) error { return r.Scan(&recent) })
	}
	parts := [4]map[int64]string{{}, {}, {}, {}} // dates, rows, writes, statements
	if tables {
		sig.tables = map[int64]string{}
		batch += ssTablesSQL
		n := 3
		if s.info.Perf {
			batch += ssUsageSQL
			n = 4
		}
		sets = append(sets, func(r *sql.Rows) error {
			var id int64
			var schema, name, v string
			if err := r.Scan(&id, &schema, &name, &v); err != nil {
				return err
			}
			if !s.excluded(schema, name) { // what changes there is never read
				parts[0][id] = v
			}
			return nil
		})
		for i := 1; i < n; i++ {
			sets = append(sets, func(r *sql.Rows) error {
				var id int64
				var v string
				if err := r.Scan(&id, &v); err != nil {
					return err
				}
				parts[i][id] = v
				return nil
			})
		}
		if s.info.Perf {
			sets = append(sets, func(r *sql.Rows) error {
				var tx int64
				var l ssOpenLock
				if err := r.Scan(&tx, &l.kind, &l.schema, &l.name, &l.resource); err != nil {
					return err
				}
				locks[tx] = append(locks[tx], l)
				return nil
			})
		}
	}
	if err := readSetsWith(ctx, c, batch, args, sets...); err != nil {
		return nil, err
	}
	done()
	for id, dates := range parts[0] {
		sig.tables[id] = dates + " rows " + parts[1][id] + " writes " + parts[2][id] + " statements " + parts[3][id]
	}
	if guard && recent == 0 {
		sort.Strings(lines)
		h := sha256.New()
		for _, l := range lines {
			h.Write([]byte(l + "\n"))
		}
		sig.guard = hex.EncodeToString(h.Sum(nil))
	}
	sig.open, sig.lock = s.openWrites(locks)
	return sig, nil
}

// same reports whether nothing changed between the last full fingerprint
// and these signals (tier 1).
func (l *ssLast) same(sig *ssSignals) bool {
	return !l.memory && l.sig.lsn != "" && l.sig.lsn == sig.lsn && l.sig.values.hash == sig.values.hash
}

// settled reads the signals a fingerprint starts from; with VIEW SERVER
// PERFORMANCE STATE it waits while a transaction that wrote to the
// database is open, up to sqlServerOpenWait. With one still open, the
// signals say so (open).
func (s *SQLServer) settled(ctx context.Context, c *sql.Conn) (*ssSignals, error) {
	start := time.Now()
	deadline, told := start.Add(sqlServerOpenWait), false
	for {
		sig, err := s.signals(ctx, c, s.info.Perf, true, true)
		if err != nil || !sig.open || time.Now().After(deadline) {
			return sig, err
		}
		if !told && time.Since(start) >= sqlServerOpenTell {
			told = true
			on := ""
			if sig.lock != "" {
				on = " holding a lock on " + sig.lock
			}
			fmt.Fprintf(s.o.Log, "state %s: waiting for a transaction%s to end (up to %s): it wrote to %s and is still open\n", s.o.Name, on, sqlServerOpenWait, s.o.Working)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Fingerprint hashes the working database (see above for what is read
// again). A working database that does not exist has no items, so a
// restore brings back every item of the pristine copy.
func (s *SQLServer) Fingerprint(ctx context.Context, items []string) (Fingerprint, error) {
	s.reused = false
	pinned := s.work != nil
	f, err := s.fingerprintWorking(ctx, items)
	if err != nil {
		s.forget()
		s.unpin()
		// The pinned connection may have been ended since the last
		// fingerprint (ALTER DATABASE … WITH ROLLBACK IMMEDIATE, a restart
		// of the server): once more on a new one.
		if pinned && ctx.Err() == nil {
			if f, err = s.fingerprintWorking(ctx, items); err != nil {
				s.forget()
				s.unpin()
			}
		}
	}
	return f, err
}

func (s *SQLServer) fingerprintWorking(ctx context.Context, items []string) (Fingerprint, error) {
	info, err := s.server(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkWorking(ctx); err != nil {
		return nil, err
	}
	c, err := s.pinned(ctx)
	if err != nil {
		d, derr := s.database(ctx, s.o.Working)
		switch {
		case derr != nil:
		case d == nil:
			return Fingerprint{}, nil
		case d.State != "ONLINE":
			// OFFLINE or RESTORING after a restore that was stopped: its
			// items cannot be read, and it is not the pristine copy. The
			// database's item says so (no real definition hashes alike),
			// so the restore that follows brings the database back.
			s.forget()
			return Fingerprint{ssDatabaseItem: hash([]byte("\x00the working database is " + d.State))}, nil
		}
		return nil, err
	}
	if !info.Perf {
		s.setMode("fingerprint of every table every time (the login cannot read the server's performance state; " + info.perfGrant("<login>") + " makes fingerprints of unchanged data almost free)")
	} else {
		s.setMode("fingerprint per table (log end, identity and sequence values, and SQL Server's per-table write counters)")
		if s.last != nil {
			sig, err := s.signals(ctx, c, true, false, false)
			if err != nil {
				return nil, err
			}
			if s.last.same(sig) {
				s.reused = true
				f := maps.Clone(s.last.f)
				if items != nil {
					f = f.Subset(items)
				}
				return f, nil
			}
		}
	}
	// The tables are read on several connections, each at its own moment:
	// a transaction that commits meanwhile could leave some tables read
	// before it and some after, a state the database never had. The
	// signatures read again afterwards must be those read before (and no
	// write transaction open then), else the tables are read again.
	for attempt := 1; ; attempt++ {
		sig, err := s.settled(ctx, c)
		if err != nil {
			return nil, err
		}
		if sqlServerBeforeScan != nil {
			sqlServerBeforeScan()
		}
		last := s.last
		var reuse func(t *ssTable, def string) (string, bool)
		if info.Perf && last != nil && last.sig.tables != nil && !last.sig.open && last.sig.start == sig.start {
			reuse = func(t *ssTable, def string) (string, bool) {
				k, ok := last.content[t.ID]
				now, counted := sig.tables[t.ID]
				if !ok || !counted || t.MemoryOptimized || t.LegacyLOB || k.def != def || k.sig != now {
					return "", false
				}
				return k.content, true
			}
		}
		inv := s.cached(sig)
		fp, err := s.fingerprint(ctx, c, s.o.Working, items, inv, sig.values, reuse)
		if err != nil {
			return nil, err
		}
		// With no table read and the catalog kept, the fingerprint is made
		// of the signals alone, which read the values before the tables: a
		// commit between the two moved the counters of the tables it wrote
		// to, which are then read, and no write was open. Nothing can mix.
		// A catalog read after the signals can: it may show a commit (a
		// table dropped) whose writes to other tables the reused contents
		// do not.
		after := sig
		if sig.open || fp.read > 0 || inv == nil {
			if after, err = s.signals(ctx, c, false, false, true); err != nil {
				return nil, err
			}
		}
		if !sig.open && after.values.hash == sig.values.hash && maps.Equal(after.tables, sig.tables) {
			s.cache(sig, fp.inv)
			switch {
			case info.Perf && items == nil:
				s.keep(fp, sig)
			case info.Perf:
				s.keepTables(fp, sig)
			}
			return fp.f, nil
		}
		if attempt == sqlServerAttempts {
			why := "it was written to while it was read"
			if sig.open {
				why = fmt.Sprintf("a transaction that wrote to it stayed open for more than %s", sqlServerOpenWait)
			}
			return nil, fmt.Errorf("state %s: no consistent fingerprint of %s after %d reads: %s", s.o.Name, s.o.Working, attempt, why)
		}
	}
}

// cached is the inventory of the working database read last, when the
// catalog did not change since (the guards are the same), else nil.
func (s *SQLServer) cached(sig *ssSignals) *ssInventory {
	if s.inv != nil && sig.guard != "" && sig.guard == s.invGuard {
		return s.inv
	}
	return nil
}

// cache keeps the inventory for the next fingerprint, with the guard read
// before it.
func (s *SQLServer) cache(sig *ssSignals, inv *ssInventory) {
	s.inv, s.invGuard = inv, sig.guard
	if sig.guard == "" {
		s.inv = nil
	}
}

// keep makes a full fingerprint the one the next starts from, unless a
// write transaction was open when its signals were read.
func (s *SQLServer) keep(fp *ssFingerprint, sig *ssSignals) {
	s.last = nil
	if sig.open {
		return
	}
	last := &ssLast{f: maps.Clone(fp.f), sig: sig, content: map[int64]ssKept{}}
	for id, content := range fp.content {
		t := fp.inv.byID[id]
		last.memory = last.memory || t.MemoryOptimized
		last.content[id] = ssKept{item: t.Item, def: fp.inv.Defs[t.Item], sig: sig.tables[id], content: content}
	}
	s.last = last
}

// keepTables adds the tables a fingerprint of some items read (the
// tracker's after a restore) to what the next fingerprint may reuse, each
// with the signature read before it: one written to since shows in its
// signature as after a full fingerprint. Only when the last full
// fingerprint's signals allow reuse at all, and of the same run of the
// server.
func (s *SQLServer) keepTables(fp *ssFingerprint, sig *ssSignals) {
	last := s.last
	if last == nil || sig.open || last.sig.tables == nil || last.sig.open || last.sig.start != sig.start {
		return
	}
	for id, content := range fp.content {
		t := fp.inv.byID[id]
		now, counted := sig.tables[id]
		if !counted {
			delete(last.content, id)
			continue
		}
		last.content[id] = ssKept{item: t.Item, def: fp.inv.Defs[t.Item], sig: now, content: content}
	}
}

// forget drops what the next fingerprint could reuse: it reads the
// catalog and every table again. The rowversion maxima of DriftGate's
// last restore go too: after a restore of the whole database they are
// another copy's, and a value the application gives a row later could
// reach them (rowversionMoved then compares with the pristine copy).
func (s *SQLServer) forget() { s.last, s.inv, s.rvMax, s.own = nil, nil, nil, ssOwnDDL{} }

// forgetPristine drops what DriftGate knows of the pristine copy too (a
// seed made a new one).
func (s *SQLServer) forgetPristine() {
	s.forget()
	s.rvPristine = nil
}

// restored tells the next fingerprint which items DriftGate's own restore
// rewrote: those tables are read again, and the log end check is skipped
// once; every other table keeps what its counters say.
func (s *SQLServer) restored(items []string) {
	if s.last == nil {
		return
	}
	s.last.sig.lsn = ""
	for id, k := range s.last.content {
		if contains(items, k.item) {
			delete(s.last.content, id)
		}
	}
}

// pinned is the connection to the working database fingerprints read the
// signals and the catalog on. It stays out of the pool, so it is not reset
// (one round trip less: sqlServerSessionSQL runs once), and only reads.
func (s *SQLServer) pinned(ctx context.Context) (*sql.Conn, error) {
	if s.work != nil {
		return s.work, nil
	}
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return nil, err
	}
	s.work = c
	return c, nil
}

// unpin ends the pinned connection (after an error, or before the working
// database is taken offline: closePool).
func (s *SQLServer) unpin() {
	if s.work != nil {
		discard(s.work)
		s.work = nil
	}
}
