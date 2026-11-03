package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
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
// are not read under READ COMMITTED, and its commit moves no counter. The
// table signatures and values are read again after the tables: when they
// moved, or a write transaction was still open, the tables are read again
// (up to sqlServerAttempts times), so no fingerprint mixes rows from before
// and after a commit.
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
	start  string // when the server started (sqlServerInfo.Start)
	tables map[int64]string
}

// ssGuardSQL reads, in one result set, what changes with every change of
// the catalog that a definition shows: every object's modify_date (ALTER,
// also of a constraint's or trigger's flags, index changes, renames), the
// database's DDL triggers, permissions, extended properties, schemas,
// types, users and assemblies. The second result set says whether an
// object changed in the last two seconds: modify_date counts in steps of
// about 3 ms, so a change right after the reading could keep its value,
// and such a reading is not used.
const ssGuardSQL = `
SELECT CONCAT(N'o ', object_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.objects WHERE is_ms_shipped = 0
UNION ALL SELECT CONCAT(N't ', object_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.triggers WHERE parent_class = 0
UNION ALL SELECT CONCAT(N'p ', class, N' ', major_id, N' ', minor_id, N' ', grantee_principal_id, N' ', grantor_principal_id, N' ', type COLLATE DATABASE_DEFAULT, N' ', state COLLATE DATABASE_DEFAULT) COLLATE DATABASE_DEFAULT
  FROM sys.database_permissions WHERE class IN (1, 3, 6)
UNION ALL SELECT CONCAT(N'e ', class, N' ', major_id, N' ', minor_id, N' ', name COLLATE DATABASE_DEFAULT, N' ', CAST(SQL_VARIANT_PROPERTY(value, 'BaseType') AS nvarchar(128)) COLLATE DATABASE_DEFAULT,
         N' ', CAST(SQL_VARIANT_PROPERTY(value, 'Collation') AS nvarchar(128)) COLLATE DATABASE_DEFAULT, N' ', CONVERT(varchar(max), CAST(value AS varbinary(8000)), 1)) COLLATE DATABASE_DEFAULT
  FROM sys.extended_properties WHERE class IN (1, 3, 6)
UNION ALL SELECT CONCAT(N's ', schema_id, N' ', name, N' ', principal_id) COLLATE DATABASE_DEFAULT FROM sys.schemas
UNION ALL SELECT CONCAT(N'y ', user_type_id, N' ', schema_id, N' ', name) COLLATE DATABASE_DEFAULT FROM sys.types WHERE is_user_defined = 1
UNION ALL SELECT CONCAT(N'u ', principal_id, N' ', name) COLLATE DATABASE_DEFAULT FROM sys.database_principals
UNION ALL SELECT CONCAT(N'a ', assembly_id, N' ', CONVERT(varchar(30), modify_date, 121)) COLLATE DATABASE_DEFAULT FROM sys.assemblies;
SELECT CASE WHEN EXISTS (SELECT 1 FROM sys.objects WHERE is_ms_shipped = 0 AND modify_date >= DATEADD(second, -2, GETDATE()))
              OR EXISTS (SELECT 1 FROM sys.triggers WHERE parent_class = 0 AND modify_date >= DATEADD(second, -2, GETDATE())) THEN 1 ELSE 0 END;`

// ssLogSQL flushes the log and reads its end (tier 1).
const ssLogSQL = `EXEC sys.sp_flush_log;
SELECT CAST(log_end_lsn AS nvarchar(64)) FROM sys.dm_db_log_stats(DB_ID());`

// ssTablesSQL reads the signature of every table in three result sets,
// joined by object id (signals; as one query the optimizer reads the
// counters once per table): the table's dates, rows and index write
// counters; a db_owner reads them. ssUsageSQL adds the statements that
// wrote to every table, then whether a transaction that wrote to the
// database is open: after them, so one that wrote before the signatures
// shows. It needs VIEW SERVER PERFORMANCE STATE.
const (
	ssTablesSQL = `
SELECT object_id, CONCAT(CONVERT(varchar(30), create_date, 121), ' ', CONVERT(varchar(30), modify_date, 121)) FROM sys.objects WHERE type = 'U' AND is_ms_shipped = 0;
SELECT object_id, CAST(SUM(rows) AS varchar(20)) FROM sys.partitions WHERE index_id IN (0, 1) GROUP BY object_id;
SELECT object_id, CONCAT(SUM(leaf_insert_count), ' ', SUM(leaf_delete_count), ' ', SUM(leaf_update_count), ' ', SUM(leaf_ghost_count),
       ' ', SUM(nonleaf_insert_count + nonleaf_delete_count + nonleaf_update_count), ' ', SUM(leaf_allocation_count + nonleaf_allocation_count), ' ', COUNT(*))
  FROM sys.dm_db_index_operational_stats(DB_ID(), NULL, NULL, NULL) GROUP BY object_id;`
	ssUsageSQL = `
SELECT object_id, CONCAT(SUM(user_updates), ' ', CONVERT(varchar(30), MAX(last_user_update), 121))
  FROM sys.dm_db_index_usage_stats WHERE database_id = DB_ID() GROUP BY object_id;
SELECT COUNT(*) FROM sys.dm_tran_database_transactions
 WHERE database_id = DB_ID() AND database_transaction_type = 1 AND database_transaction_log_record_count > 0;`
)

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
	valueSets, done := ssValueSets(sig.values)
	batch += ssValuesSQL
	sets = append(sets, valueSets...)
	var lines []string
	var recent, open int
	if guard {
		batch += ssGuardSQL
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
		for i := range n {
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
			sets = append(sets, func(r *sql.Rows) error { return r.Scan(&open) })
		}
	}
	if err := readSets(ctx, c, batch, sets...); err != nil {
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
	sig.open = open > 0
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
	deadline := time.Now().Add(sqlServerOpenWait)
	for {
		sig, err := s.signals(ctx, c, s.info.Perf, true, true)
		if err != nil || !sig.open || time.Now().After(deadline) {
			return sig, err
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
		if d, derr := s.database(ctx, s.o.Working); derr == nil && d == nil {
			return Fingerprint{}, nil
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
		fp, err := s.fingerprint(ctx, c, s.o.Working, items, s.cached(sig), sig.values, reuse)
		if err != nil {
			return nil, err
		}
		after, err := s.signals(ctx, c, false, false, true)
		if err != nil {
			return nil, err
		}
		if !sig.open && after.values.hash == sig.values.hash && maps.Equal(after.tables, sig.tables) {
			s.cache(sig, fp.inv)
			if info.Perf && items == nil {
				s.keep(fp, sig)
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

// forget drops what the next fingerprint could reuse: it reads the
// catalog and every table again. The rowversion maxima of DriftGate's
// last restore go too: after a restore of the whole database they are
// another copy's, and a value the application gives a row later could
// reach them (rowversionMoved then compares with the pristine copy).
func (s *SQLServer) forget() { s.last, s.inv, s.rvMax = nil, nil, nil }

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
