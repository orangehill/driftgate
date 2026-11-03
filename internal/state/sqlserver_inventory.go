package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ssInventory is what a database holds, read from its catalog in one
// batch: the tables with their columns, keys and identity, and every other
// object DriftGate fingerprints, each with the text of its definition.
// Restore and Seed use it too.
type ssInventory struct {
	Tables      []*ssTable // by item
	ByItem      map[string]*ssTable
	byID        map[int64]*ssTable
	ForeignKeys []ssForeignKey
	Checks      []ssCheck
	Triggers    []ssTrigger // DML triggers and the database's DDL triggers
	Sequences   []*ssSequence
	// Defs holds the definition text of every item that is not excluded,
	// tables without their rows: columns with every attribute, identity
	// (last_value, NULL too, and IDENT_CURRENT), indexes, constraints with
	// their flags, temporal, graph and memory-optimized settings,
	// permissions and extended properties; sequences with where they
	// stand. It is made by apply, from base and the values.
	Defs map[string]string
	base map[string]string // Defs without the identity and sequence values
}

// ssTable is a table of the database.
type ssTable struct {
	ID           int64
	Schema, Name string
	Item         string // table:schema.name
	Columns      []ssColumn
	Key          []string // primary key columns in key order; nil: no primary key
	KeyClustered bool     // the primary key is the clustered index
	Identity     *ssIdentity
	Rows         int64 // sys.partitions (an estimate under load)
	Heap         bool
	Excluded     bool // Options.Exclude: not fingerprinted or restored

	MemoryOptimized bool
	Durability      string // SCHEMA_AND_DATA, SCHEMA_ONLY (memory-optimized)
	Temporal        int    // 1 history table, 2 system-versioned
	Graph           bool   // node or edge table
	edge            bool   // an edge table
	FileTable       bool
	External        bool
	Ledger          int // ledger_type (2022+): 0 none
	// Security names an enabled row-level security policy with a
	// predicate on the table: a filter predicate hides rows from every
	// read, also DriftGate's, and a block predicate refuses writes, so
	// such a table is refused (fingerprint, Rows); "" for none.
	Security string
	// LegacyLOB: a text, ntext or image column. WRITETEXT and UPDATETEXT
	// change it without moving any of the table's counters, so its rows
	// are read every time.
	LegacyLOB bool
}

// qname is the table's quoted two-part name.
func (t *ssTable) qname() string { return qs2(t.Schema, t.Name) }

// rowversion is the table's rowversion column, or "".
func (t *ssTable) rowversion() string {
	for _, c := range t.Columns {
		if c.rowversion() {
			return c.Name
		}
	}
	return ""
}

// ssIdentity is a table's identity column and where it stands. Last is
// sys.identity_columns.last_value (NULL: never used since created or
// truncated); with Last NULL the next value is Current, IDENT_CURRENT (a
// RESEED of a fresh table shows only there), else Last plus the increment
// (Current is then "").
type ssIdentity struct {
	Column          string
	Seed, Increment string
	Last            sql.NullString
	Current         string
}

// ssForeignKey is a foreign key from Table to Referenced (object ids).
type ssForeignKey struct {
	Name                       string
	Table, Referenced          int64
	Disabled, NotTrusted       bool
	DeleteAction, UpdateAction string // NO_ACTION, CASCADE, SET_NULL, SET_DEFAULT
}

// ssCheck is a CHECK constraint of a table.
type ssCheck struct {
	Name                 string
	Table                int64
	Disabled, NotTrusted bool
}

// ssTrigger is a DML trigger of a table (Table its object id) or, with
// Table 0, a DDL trigger of the database.
type ssTrigger struct {
	ID           int64
	Schema, Name string
	Item         string // trigger:schema.name, trigger:database.name
	Table        int64
	Disabled     bool
}

// ssSequence is a sequence and where it stands. Start is not fingerprinted:
// ALTER SEQUENCE … RESTART WITH n sets it to n, and it only decides where
// a later RESTART without WITH starts.
type ssSequence struct {
	ID                 int64
	Schema, Name, Item string
	Type               string
	Start, Increment   string
	Min, Max           string
	Cycle, Cached      bool
	CacheSize          int64 // -1: the server's default
	Current            string
	LastUsed           sql.NullString // NULL: no value handed out since created or restarted
	Exhausted          bool
}

// def is the sequence's definition text, without where it stands.
func (q *ssSequence) def() string {
	return fmt.Sprintf("sequence %s increment %s min %s max %s cycle %t cached %t cache %d",
		q.Type, q.Increment, q.Min, q.Max, q.Cycle, q.Cached, q.CacheSize)
}

// ssValues are where the identity columns and sequences stand: what they
// hand out next. They change without a change of the catalog, some even
// without a record in the log (a rolled-back INSERT, NEXT VALUE FOR from
// a sequence's cache), so every fingerprint reads them (ssValuesSQL). With
// them the database's options and scoped configurations (ssDatabaseItem):
// ALTER DATABASE moves no modify_date the catalog guard reads.
type ssValues struct {
	ident map[int64][2]string // table: last_value ("NULL"), IDENT_CURRENT when last_value is NULL
	seq   map[int64][3]string // sequence: current_value, last_used_value ("NULL"), is_exhausted
	db    []string            // options and scoped configurations, sorted
	// hash is of the values of the tables and sequences that are not
	// excluded (ident and seq hold those too): the application may write
	// to an excluded table while a fingerprint reads the others.
	hash string
}

// ssValuesSQL reads the values, in three result sets (ssValueSets).
const ssValuesSQL = `
SELECT c.object_id, OBJECT_SCHEMA_NAME(c.object_id), OBJECT_NAME(c.object_id), ISNULL(CONVERT(nvarchar(40), c.last_value), N'NULL'),
       ISNULL(CASE WHEN c.last_value IS NULL THEN CONVERT(nvarchar(40), IDENT_CURRENT(QUOTENAME(OBJECT_SCHEMA_NAME(c.object_id)) + N'.' + QUOTENAME(OBJECT_NAME(c.object_id)))) END, N'')
  FROM sys.identity_columns c WHERE OBJECTPROPERTY(c.object_id, 'IsUserTable') = 1;
SELECT object_id, SCHEMA_NAME(schema_id), name, CONVERT(nvarchar(40), current_value), ISNULL(CONVERT(nvarchar(40), last_used_value), N'NULL'), CAST(is_exhausted AS nvarchar(1))
  FROM sys.sequences WHERE is_ms_shipped = 0;
SELECT CONCAT(N'option compatibility ', compatibility_level, N' collation ', collation_name, N' read_committed_snapshot ', is_read_committed_snapshot_on,
       N' snapshot_isolation ', CASE WHEN is_read_only = 1 THEN -1 ELSE snapshot_isolation_state END, N' ansi_null_default ', is_ansi_null_default_on, N' ansi_nulls ', is_ansi_nulls_on,
       N' ansi_padding ', is_ansi_padding_on, N' ansi_warnings ', is_ansi_warnings_on, N' arithabort ', is_arithabort_on,
       N' concat_null_yields_null ', is_concat_null_yields_null_on, N' numeric_roundabort ', is_numeric_roundabort_on,
       N' quoted_identifier ', is_quoted_identifier_on, N' recursive_triggers ', is_recursive_triggers_on, N' cursor_close_on_commit ', is_cursor_close_on_commit_on,
       N' local_cursor_default ', is_local_cursor_default, N' parameterization_forced ', is_parameterization_forced, N' date_correlation ', is_date_correlation_on,
       N' elevate_to_snapshot ', is_memory_optimized_elevate_to_snapshot_on, N' containment ', containment, N' language ', default_language_lcid,
       N' fulltext_language ', default_fulltext_language_lcid, N' nested_triggers ', is_nested_triggers_on, N' transform_noise_words ', is_transform_noise_words_on,
       N' two_digit_year_cutoff ', two_digit_year_cutoff) COLLATE DATABASE_DEFAULT
  FROM sys.databases WHERE database_id = DB_ID()
UNION ALL SELECT CONCAT(N'scoped configuration ', name, N' ', CONVERT(nvarchar(4000), value), N' ', CONVERT(nvarchar(4000), value_for_secondary)) COLLATE DATABASE_DEFAULT
  FROM sys.database_scoped_configurations;`

// ssValueSets are the scanners of ssValuesSQL's result sets; done hashes
// the values once they are read, without those of the tables and
// sequences excluded says are (nil: none).
func ssValueSets(v *ssValues, excluded func(schema, name string) bool) (sets []func(*sql.Rows) error, done func()) {
	v.ident, v.seq, v.db = map[int64][2]string{}, map[int64][3]string{}, nil
	skip := map[int64]bool{}
	note := func(id int64, schema, name sql.NullString) {
		if excluded != nil && schema.Valid && name.Valid && excluded(schema.String, name.String) {
			skip[id] = true
		}
	}
	sets = []func(*sql.Rows) error{
		func(r *sql.Rows) error {
			var id int64
			var schema, n sql.NullString
			var x [2]string
			if err := r.Scan(&id, &schema, &n, &x[0], &x[1]); err != nil {
				return err
			}
			note(id, schema, n)
			v.ident[id] = x
			return nil
		},
		func(r *sql.Rows) error {
			var id int64
			var schema, n sql.NullString
			var x [3]string
			if err := r.Scan(&id, &schema, &n, &x[0], &x[1], &x[2]); err != nil {
				return err
			}
			note(id, schema, n)
			v.seq[id] = x
			return nil
		},
		func(r *sql.Rows) error {
			var line string
			if err := r.Scan(&line); err != nil {
				return err
			}
			v.db = append(v.db, line)
			return nil
		},
	}
	done = func() {
		h := sha256.New()
		ids := make([]int64, 0, len(v.ident))
		for id := range v.ident {
			if !skip[id] {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			fmt.Fprintf(h, "i %d %s\n", id, v.ident[id])
		}
		ids = ids[:0]
		for id := range v.seq {
			if !skip[id] {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			fmt.Fprintf(h, "s %d %s\n", id, v.seq[id])
		}
		sort.Strings(v.db)
		for _, l := range v.db {
			fmt.Fprintf(h, "d %s\n", l)
		}
		v.hash = hex.EncodeToString(h.Sum(nil))
	}
	return sets, done
}

// values reads the values on c (hash: of all of them).
func values(ctx context.Context, c *sql.Conn) (*ssValues, error) {
	v := &ssValues{}
	sets, done := ssValueSets(v, nil)
	if err := readSets(ctx, c, ssValuesSQL, sets...); err != nil {
		return nil, err
	}
	done()
	return v, nil
}

// apply sets where the identity columns and sequences stand and makes
// Defs.
func (inv *ssInventory) apply(v *ssValues) {
	inv.Defs = make(map[string]string, len(inv.base))
	for item, def := range inv.base {
		inv.Defs[item] = def
	}
	for _, t := range inv.Tables {
		if t.Identity == nil {
			continue
		}
		x, ok := v.ident[t.ID]
		if !ok {
			x = [2]string{"?", "?"} // a table made after the values were read: read again next time
		}
		t.Identity.Last = sql.NullString{String: x[0], Valid: x[0] != "NULL"}
		t.Identity.Current = x[1]
		if def, ok := inv.Defs[t.Item]; ok {
			inv.Defs[t.Item] = def + "\nidentity last " + x[0] + " current " + x[1]
		}
	}
	for _, q := range inv.Sequences {
		x, ok := v.seq[q.ID]
		if !ok {
			x = [3]string{"?", "?", "?"}
		}
		q.Current, q.LastUsed, q.Exhausted = x[0], sql.NullString{String: x[1], Valid: x[1] != "NULL"}, x[2] == "1"
		inv.Defs[q.Item] += "\ncurrent " + x[0] + " last " + x[1] + " exhausted " + x[2]
	}
	if len(v.db) > 0 {
		inv.Defs[ssDatabaseItem] += "\n" + strings.Join(v.db, "\n")
	}
}

// ssObjectKinds names the item kind of an object type of sys.objects.
var ssObjectKinds = map[string]string{
	"V": "view", "P": "procedure", "PC": "procedure",
	"FN": "function", "IF": "function", "TF": "function", "FS": "function", "FT": "function",
	"TR": "trigger", "R": "rule", "D": "default", // a default of CREATE DEFAULT (bound with sp_bindefault)
}

// ssDatabaseItem is the item of the database itself: its users and roles
// with their members, the permissions and extended properties that belong
// to no other item (on the database, on dbo and the other built-in
// schemas…), and the options that change what a query does (ssValuesSQL:
// compatibility level, collation, snapshot isolation, the ANSI defaults…)
// with the database scoped configurations. A restore restores it only
// with the whole database. Statistics are left out on purpose: they change
// how a query runs, not what it returns.
const ssDatabaseItem = "database:"

// ssCatalog is the batch inventory reads, one result set per query, in
// the order of the scanners in inventory. %[1]s is the ledger type of a
// table (SQL Server 2022 and later), else 0.
const ssCatalog = `
SELECT t.object_id, s.name, t.name, t.is_memory_optimized, ISNULL(t.durability_desc, N''), t.temporal_type,
       ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(t.history_table_id)) + N'.' + QUOTENAME(OBJECT_NAME(t.history_table_id)), N''),
       CAST(t.is_node AS int) + 2 * CAST(t.is_edge AS int), t.is_filetable, t.is_external, %[1]s, t.lock_escalation_desc,
       ISNULL(p.rows, 0), CASE WHEN p.heap > 0 THEN 1 ELSE 0 END
  FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id
  LEFT JOIN (SELECT object_id, SUM(rows) AS rows, COUNT(CASE WHEN index_id = 0 THEN 1 END) AS heap FROM sys.partitions WHERE index_id IN (0, 1) GROUP BY object_id) p
    ON p.object_id = t.object_id
 WHERE t.is_ms_shipped = 0;
SELECT c.object_id, c.column_id, c.name,
       CASE WHEN ut.is_assembly_type = 1 THEN ut.name ELSE ISNULL(st.name, ut.name) END, ut.is_assembly_type,
       CASE WHEN ut.is_user_defined = 1 THEN QUOTENAME(SCHEMA_NAME(ut.schema_id)) + N'.' + QUOTENAME(ut.name) ELSE ut.name END,
       c.max_length, c.precision, c.scale, ISNULL(c.collation_name, N''), c.is_nullable, c.is_identity, c.is_computed, c.is_rowguidcol,
       c.is_sparse, c.is_column_set, c.generated_always_type, c.is_hidden, c.is_masked, CASE WHEN c.encryption_type IS NULL THEN 0 ELSE 1 END,
       c.is_filestream, ISNULL(c.graph_type, 0), ISNULL(QUOTENAME(SCHEMA_NAME(xc.schema_id)) + N'.' + QUOTENAME(xc.name), N''),
       ISNULL(cc.definition, N''), ISNULL(CAST(cc.is_persisted AS int), 0), ISNULL(dc.name, N''), ISNULL(dc.definition, N''),
       ISNULL(mc.masking_function, N''),
       ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(NULLIF(c.rule_object_id, 0))) + N'.' + QUOTENAME(OBJECT_NAME(NULLIF(c.rule_object_id, 0))), N''),
       CASE WHEN c.default_object_id <> 0 AND dc.object_id IS NULL
            THEN ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(c.default_object_id)) + N'.' + QUOTENAME(OBJECT_NAME(c.default_object_id)), N'?') ELSE N'' END,
       ISNULL(CONVERT(nvarchar(40), ic.seed_value), N''), ISNULL(CONVERT(nvarchar(40), ic.increment_value), N''),
       ISNULL(CAST(ic.is_not_for_replication AS int), 0)
  FROM sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
  JOIN sys.types ut ON ut.user_type_id = c.user_type_id
  LEFT JOIN sys.types st ON st.user_type_id = c.system_type_id
  LEFT JOIN sys.xml_schema_collections xc ON xc.xml_collection_id = c.xml_collection_id AND c.xml_collection_id <> 0
  LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
  LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
  LEFT JOIN sys.masked_columns mc ON mc.object_id = c.object_id AND mc.column_id = c.column_id
  LEFT JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
 WHERE t.is_ms_shipped = 0;
SELECT i.object_id, i.index_id, ISNULL(i.name, N''), i.type_desc, i.is_unique, i.is_primary_key, i.is_unique_constraint, i.ignore_dup_key,
       i.is_disabled, ISNULL(i.filter_definition, N''), ISNULL(ds.name, N''),
       ISNULL((SELECT STRING_AGG(CONVERT(nvarchar(max), QUOTENAME(c.name) + CASE WHEN ic.is_included_column = 1 THEN N' include' WHEN ic.is_descending_key = 1 THEN N' desc' ELSE N'' END
                        + CASE WHEN ic.partition_ordinal > 0 THEN N' partition' ELSE N'' END), N', ') WITHIN GROUP (ORDER BY ic.is_included_column, ic.key_ordinal, ic.column_id)
                 FROM sys.index_columns ic JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
                WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id), N''),
       ISNULL((SELECT STRING_AGG(CONVERT(nvarchar(max), p.data_compression_desc), N',') WITHIN GROUP (ORDER BY p.partition_number)
                 FROM sys.partitions p WHERE p.object_id = i.object_id AND p.index_id = i.index_id), N'')
  FROM sys.indexes i JOIN sys.objects o ON o.object_id = i.object_id LEFT JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
 WHERE o.is_ms_shipped = 0 AND o.type IN ('U', 'V') AND i.type > 0;
SELECT ic.object_id, c.name, i.index_id
  FROM sys.indexes i JOIN sys.tables t ON t.object_id = i.object_id
  JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
  JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
 WHERE i.is_primary_key = 1 AND t.is_ms_shipped = 0 AND ic.key_ordinal > 0
 ORDER BY ic.object_id, ic.key_ordinal;
SELECT fk.parent_object_id, fk.referenced_object_id, fk.name, fk.delete_referential_action_desc, fk.update_referential_action_desc,
       fk.is_disabled, fk.is_not_trusted, fk.is_not_for_replication,
       ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(fk.referenced_object_id)) + N'.' + QUOTENAME(OBJECT_NAME(fk.referenced_object_id)), N''),
       ISNULL((SELECT STRING_AGG(CONVERT(nvarchar(max), QUOTENAME(pc.name) + N' ' + QUOTENAME(rc.name)), N', ') WITHIN GROUP (ORDER BY k.constraint_column_id)
                 FROM sys.foreign_key_columns k
                 JOIN sys.columns pc ON pc.object_id = k.parent_object_id AND pc.column_id = k.parent_column_id
                 JOIN sys.columns rc ON rc.object_id = k.referenced_object_id AND rc.column_id = k.referenced_column_id
                WHERE k.constraint_object_id = fk.object_id), N'')
  FROM sys.foreign_keys fk WHERE fk.is_ms_shipped = 0;
SELECT cc.parent_object_id, cc.name, ISNULL(cc.definition, N''), cc.is_disabled, cc.is_not_trusted, cc.is_not_for_replication,
       ISNULL(COL_NAME(cc.parent_object_id, NULLIF(cc.parent_column_id, 0)), N'')
  FROM sys.check_constraints cc WHERE cc.is_ms_shipped = 0;
SELECT o.object_id, RTRIM(o.type), s.name, o.name, o.parent_object_id, ISNULL(m.definition, N''),
       ISNULL(CAST(m.uses_ansi_nulls AS int), -1), ISNULL(CAST(m.uses_quoted_identifier AS int), -1), ISNULL(CAST(m.is_schema_bound AS int), -1),
       ISNULL(QUOTENAME(USER_NAME(m.execute_as_principal_id)), N''),
       ISNULL((SELECT a.name COLLATE DATABASE_DEFAULT + N' ' + ISNULL(am.assembly_class COLLATE DATABASE_DEFAULT, N'') + N' ' + ISNULL(am.assembly_method COLLATE DATABASE_DEFAULT, N'')
                 FROM sys.assembly_modules am JOIN sys.assemblies a ON a.assembly_id = am.assembly_id WHERE am.object_id = o.object_id), N''),
       ISNULL(CAST(tr.is_disabled AS int), 0), ISNULL(CAST(tr.is_instead_of_trigger AS int), 0), ISNULL(CAST(tr.is_not_for_replication AS int), 0),
       ISNULL((SELECT STRING_AGG(CONVERT(nvarchar(max), e.type_desc COLLATE DATABASE_DEFAULT + CASE WHEN e.is_first = 1 THEN N' first' WHEN e.is_last = 1 THEN N' last' ELSE N'' END), N', ')
                        WITHIN GROUP (ORDER BY e.type) FROM sys.trigger_events e WHERE e.object_id = o.object_id), N'')
  FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id
  LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id LEFT JOIN sys.triggers tr ON tr.object_id = o.object_id
 WHERE o.is_ms_shipped = 0 AND o.type IN ('V', 'P', 'PC', 'FN', 'IF', 'TF', 'FS', 'FT', 'TR', 'R', 'D') AND (o.type <> 'D' OR o.parent_object_id = 0);
SELECT t.object_id, t.name, t.is_disabled, ISNULL(m.definition, N''),
       ISNULL((SELECT STRING_AGG(CONVERT(nvarchar(max), e.type_desc COLLATE DATABASE_DEFAULT + CASE WHEN e.is_first = 1 THEN N' first' WHEN e.is_last = 1 THEN N' last' ELSE N'' END), N', ')
                        WITHIN GROUP (ORDER BY e.type) FROM sys.trigger_events e WHERE e.object_id = t.object_id), N'')
  FROM sys.triggers t LEFT JOIN sys.sql_modules m ON m.object_id = t.object_id
 WHERE t.parent_class = 0 AND t.is_ms_shipped = 0;
SELECT q.object_id, SCHEMA_NAME(q.schema_id), q.name, TYPE_NAME(q.user_type_id) + N'(' + CAST(q.precision AS nvarchar(5)) + N',' + CAST(q.scale AS nvarchar(5)) + N')',
       CONVERT(nvarchar(40), q.start_value), CONVERT(nvarchar(40), q.increment), CONVERT(nvarchar(40), q.minimum_value), CONVERT(nvarchar(40), q.maximum_value),
       q.is_cycling, q.is_cached, ISNULL(q.cache_size, -1)
  FROM sys.sequences q WHERE q.is_ms_shipped = 0;
SELECT t.user_type_id, SCHEMA_NAME(t.schema_id), t.name, ISNULL(TYPE_NAME(t.system_type_id), N''), t.max_length, t.precision, t.scale,
       ISNULL(t.collation_name, N''), t.is_nullable, t.is_table_type,
       ISNULL(CASE WHEN t.is_table_type = 1 THEN (SELECT STRING_AGG(CONVERT(nvarchar(max), QUOTENAME(c.name) + N' ' + TYPE_NAME(c.user_type_id) + N' '
                        + CAST(c.max_length AS nvarchar(10)) + N' ' + CAST(c.precision AS nvarchar(5)) + N' ' + CAST(c.scale AS nvarchar(5)) + N' ' + ISNULL(c.collation_name COLLATE DATABASE_DEFAULT, N'')
                        + CASE WHEN c.is_nullable = 1 THEN N' null' ELSE N' not null' END + CASE WHEN c.is_identity = 1 THEN N' identity' ELSE N'' END
                        + ISNULL(N' as ' + cc.definition COLLATE DATABASE_DEFAULT, N'')), N', ') WITHIN GROUP (ORDER BY c.column_id)
                 FROM sys.table_types tt JOIN sys.columns c ON c.object_id = tt.type_table_object_id
                 LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
                WHERE tt.user_type_id = t.user_type_id) END, N''),
       ISNULL(CASE WHEN t.is_assembly_type = 1 THEN (SELECT a.name COLLATE DATABASE_DEFAULT + N' ' + at.assembly_class COLLATE DATABASE_DEFAULT FROM sys.assembly_types at JOIN sys.assemblies a ON a.assembly_id = at.assembly_id
                WHERE at.user_type_id = t.user_type_id) END, N''),
       ISNULL(N' rule ' + QUOTENAME(OBJECT_SCHEMA_NAME(NULLIF(t.rule_object_id, 0))) + N'.' + QUOTENAME(OBJECT_NAME(NULLIF(t.rule_object_id, 0))), N'')
       + ISNULL(N' bound default ' + QUOTENAME(OBJECT_SCHEMA_NAME(NULLIF(t.default_object_id, 0))) + N'.' + QUOTENAME(OBJECT_NAME(NULLIF(t.default_object_id, 0))), N'')
  FROM sys.types t WHERE t.is_user_defined = 1;
SELECT object_id, SCHEMA_NAME(schema_id), name, ISNULL(base_object_name, N'') FROM sys.synonyms WHERE is_ms_shipped = 0;
SELECT schema_id, name, ISNULL(QUOTENAME(USER_NAME(principal_id)), N'') FROM sys.schemas WHERE schema_id > 4 AND schema_id < 16384;
SELECT p.class, p.major_id, N'grant ' + p.state_desc COLLATE DATABASE_DEFAULT + N' ' + p.permission_name COLLATE DATABASE_DEFAULT + N' to ' + ISNULL(QUOTENAME(USER_NAME(p.grantee_principal_id)), N'?')
       + CASE WHEN p.class = 1 AND p.minor_id > 0 THEN N' column ' + ISNULL(QUOTENAME(COL_NAME(p.major_id, p.minor_id)), N'?') ELSE N'' END + N' on ' + CASE p.class WHEN 0 THEN N'database'
         WHEN 1 THEN ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(p.major_id)) + N'.' + QUOTENAME(OBJECT_NAME(p.major_id)), N'?')
         WHEN 3 THEN N'schema ' + ISNULL(QUOTENAME(SCHEMA_NAME(p.major_id)), N'?')
         WHEN 4 THEN N'principal ' + ISNULL(QUOTENAME(USER_NAME(p.major_id)), N'?')
         WHEN 6 THEN N'type ' + ISNULL(QUOTENAME(TYPE_NAME(p.major_id)), N'?')
         WHEN 10 THEN N'xml schema collection ' + ISNULL((SELECT QUOTENAME(x.name) FROM sys.xml_schema_collections x WHERE x.xml_collection_id = p.major_id), N'?')
         ELSE CONCAT(p.class_desc COLLATE DATABASE_DEFAULT, N' ', p.major_id) END
  FROM sys.database_permissions p;
SELECT e.class, e.major_id, N'property ' + QUOTENAME(e.name)
       + CASE WHEN e.class = 1 AND e.minor_id > 0 THEN N' column ' + ISNULL(QUOTENAME(COL_NAME(e.major_id, e.minor_id)), N'?')
              WHEN e.minor_id > 0 THEN CONCAT(N' minor ', e.minor_id) ELSE N'' END + N' on ' + CASE e.class WHEN 0 THEN N'database'
         WHEN 1 THEN ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(e.major_id)) + N'.' + QUOTENAME(OBJECT_NAME(e.major_id)), N'?')
         WHEN 3 THEN N'schema ' + ISNULL(QUOTENAME(SCHEMA_NAME(e.major_id)), N'?')
         WHEN 4 THEN N'principal ' + ISNULL(QUOTENAME(USER_NAME(e.major_id)), N'?')
         WHEN 6 THEN N'type ' + ISNULL(QUOTENAME(TYPE_NAME(e.major_id)), N'?')
         WHEN 10 THEN N'xml schema collection ' + ISNULL((SELECT QUOTENAME(x.name) FROM sys.xml_schema_collections x WHERE x.xml_collection_id = e.major_id), N'?')
         ELSE CONCAT(e.class_desc COLLATE DATABASE_DEFAULT, N' ', e.major_id) END
       + N' ' + ISNULL(CAST(SQL_VARIANT_PROPERTY(e.value, 'BaseType') AS nvarchar(128)) COLLATE DATABASE_DEFAULT
         + N' ' + ISNULL(CAST(SQL_VARIANT_PROPERTY(e.value, 'Collation') AS nvarchar(128)) COLLATE DATABASE_DEFAULT, N'')
         + N' ' + CONVERT(varchar(max), CAST(e.value AS varbinary(8000)), 1), N'NULL')
  FROM sys.extended_properties e WHERE NOT (e.class = 0 AND e.name = N'driftgate_pristine');
SELECT pr.target_object_id, QUOTENAME(SCHEMA_NAME(p.schema_id)) + N'.' + QUOTENAME(p.name), p.is_enabled, p.is_schema_bound, pr.predicate_type,
       ISNULL(pr.operation_desc COLLATE DATABASE_DEFAULT, N''), ISNULL(pr.predicate_definition, N'')
  FROM sys.security_predicates pr JOIN sys.security_policies p ON p.object_id = pr.object_id;
SELECT CONCAT(N'principal ', QUOTENAME(name), N' ', type COLLATE DATABASE_DEFAULT, N' ', authentication_type_desc COLLATE DATABASE_DEFAULT,
              N' schema ', ISNULL(QUOTENAME(default_schema_name), N''), N' owner ', ISNULL(QUOTENAME(USER_NAME(owning_principal_id)), N''),
              N' sid ', CONVERT(varchar(200), sid, 1)) COLLATE DATABASE_DEFAULT
  FROM sys.database_principals WHERE principal_id > 4 AND is_fixed_role = 0
UNION ALL SELECT CONCAT(N'member ', QUOTENAME(USER_NAME(role_principal_id)), N' ', QUOTENAME(USER_NAME(member_principal_id))) COLLATE DATABASE_DEFAULT
  FROM sys.database_role_members;
SELECT 0, N'partition_function:' + pf.name, CONCAT(pf.type_desc COLLATE DATABASE_DEFAULT, N' right ', pf.boundary_value_on_right, N' parameters ',
       (SELECT STRING_AGG(CONVERT(nvarchar(max), CONCAT(TYPE_NAME(pp.user_type_id), N'(', pp.max_length, N',', pp.precision, N',', pp.scale, N') ', pp.collation_name COLLATE DATABASE_DEFAULT)), N', ')
               WITHIN GROUP (ORDER BY pp.parameter_id) FROM sys.partition_parameters pp WHERE pp.function_id = pf.function_id),
       N' values ', (SELECT STRING_AGG(CONVERT(varchar(max), CONCAT(CAST(SQL_VARIANT_PROPERTY(rv.value, 'BaseType') AS varchar(128)), ' ', CONVERT(varchar(max), CAST(rv.value AS varbinary(8000)), 1))), ', ')
               WITHIN GROUP (ORDER BY rv.boundary_id) FROM sys.partition_range_values rv WHERE rv.function_id = pf.function_id)) COLLATE DATABASE_DEFAULT
  FROM sys.partition_functions pf
UNION ALL SELECT 0, N'partition_scheme:' + ps.name, CONCAT(N'function ', QUOTENAME(pf.name), N' to ',
       (SELECT STRING_AGG(CONVERT(nvarchar(max), QUOTENAME(ds.name)), N', ') WITHIN GROUP (ORDER BY dd.destination_id)
          FROM sys.destination_data_spaces dd JOIN sys.data_spaces ds ON ds.data_space_id = dd.data_space_id WHERE dd.partition_scheme_id = ps.data_space_id)) COLLATE DATABASE_DEFAULT
  FROM sys.partition_schemes ps JOIN sys.partition_functions pf ON pf.function_id = ps.function_id
UNION ALL SELECT xml_collection_id, N'xml_schema:' + SCHEMA_NAME(schema_id) + N'.' + name, CONVERT(nvarchar(max), XML_SCHEMA_NAMESPACE(SCHEMA_NAME(schema_id), name)) COLLATE DATABASE_DEFAULT
  FROM sys.xml_schema_collections WHERE xml_collection_id > 1;`

// inventory reads the catalog of the database c is connected to, without
// the values (apply). A table of a type DriftGate cannot fingerprint is
// not refused here: the fingerprint says so, and only if the table is not
// excluded.
func (s *SQLServer) inventory(ctx context.Context, c *sql.Conn) (*ssInventory, error) {
	info, err := s.server(ctx)
	if err != nil {
		return nil, err
	}
	ledger := "0"
	if info.Major >= 16 {
		ledger = "t.ledger_type"
	}
	inv := &ssInventory{ByItem: map[string]*ssTable{}, byID: map[int64]*ssTable{}, base: map[string]string{}}
	lines := map[string][]string{} // item → lines of its definition, sorted
	columnLines := map[int64][]string{}
	add := func(item, line string) { lines[item] = append(lines[item], line) }
	objects := map[int64]string{} // object id → item, for permissions and properties
	schemas := map[int64]string{}
	types := map[int64]string{}
	xmlSchemas := map[int64]string{}
	table := func(id int64) *ssTable { return inv.byID[id] }
	// Item names are schema.name without quoting: two objects ([a.b].c and
	// a.[b.c]) may come out alike, and are refused rather than mixed up.
	owners := map[string]string{}
	claim := func(item, owner string) error {
		if o, ok := owners[item]; ok {
			return fmt.Errorf("state %s: %s and %s both have the item name %s, so DriftGate cannot tell their changes apart; rename one of them", s.o.Name, o, owner, item)
		}
		owners[item] = owner
		return nil
	}
	b := func(v bool) string { return strconv.FormatBool(v) }

	sets := []func(*sql.Rows) error{
		func(r *sql.Rows) error { // tables
			t := &ssTable{}
			var graph int
			var history, lockEscalation string
			if err := r.Scan(&t.ID, &t.Schema, &t.Name, &t.MemoryOptimized, &t.Durability, &t.Temporal, &history, &graph,
				&t.FileTable, &t.External, &t.Ledger, &lockEscalation, &t.Rows, &t.Heap); err != nil {
				return err
			}
			t.Graph, t.edge = graph != 0, graph == 2
			t.Item = "table:" + t.Schema + "." + t.Name
			if err := claim(t.Item, "table "+t.qname()); err != nil {
				return err
			}
			t.Excluded = s.excluded(t.Schema, t.Name)
			inv.Tables = append(inv.Tables, t)
			inv.byID[t.ID] = t
			inv.ByItem[t.Item] = t
			objects[t.ID] = t.Item
			add(t.Item, fmt.Sprintf("table memory %s %s temporal %d %s graph %d filetable %s external %s ledger %d lock %s",
				b(t.MemoryOptimized), t.Durability, t.Temporal, history, graph, b(t.FileTable), b(t.External), t.Ledger, lockEscalation))
			return nil
		},
		func(r *sql.Rows) error { // columns, in column order
			var id int64
			var col ssColumn
			var declared, collation, xmlSchema, computedDef, defaultName, defaultDef, mask, rule, bound, seed, increment string
			var precision, scale, persisted, notForRepl int
			var rowguid, sparse, hidden, masked, filestream bool
			if err := r.Scan(&id, &col.ID, &col.Name, &col.Type, &col.CLR, &declared, &col.MaxLength, &precision, &scale, &collation,
				&col.Nullable, &col.Identity, &col.Computed, &rowguid, &sparse, &col.ColumnSet, &col.Generated, &hidden, &masked, &col.Encrypted,
				&filestream, &col.Graph, &xmlSchema, &computedDef, &persisted, &defaultName, &defaultDef, &mask, &rule, &bound, &seed, &increment, &notForRepl); err != nil {
				return err
			}
			t := table(id)
			if t == nil {
				return nil
			}
			col.Edge = t.Graph && t.edge
			t.Columns = append(t.Columns, col)
			t.LegacyLOB = t.LegacyLOB || col.Type == "text" || col.Type == "ntext" || col.Type == "image"
			line := fmt.Sprintf("column %d %s %s %d %d %d %s null %s", col.ID, qms(col.Name), declared, col.MaxLength, precision, scale, collation, b(col.Nullable))
			if col.Computed {
				line += fmt.Sprintf(" as %s persisted %d", computedDef, persisted)
			}
			if col.Identity {
				t.Identity = &ssIdentity{Column: col.Name, Seed: seed, Increment: increment}
				line += fmt.Sprintf(" identity %s %s not-for-replication %d", seed, increment, notForRepl)
			}
			if defaultName != "" {
				line += " default " + qms(defaultName) + " " + defaultDef
			}
			if rule != "" {
				line += " rule " + rule
			}
			if bound != "" {
				line += " bound default " + bound
			}
			for _, f := range []struct {
				on   bool
				text string
			}{{rowguid, " rowguidcol"}, {sparse, " sparse"}, {col.ColumnSet, " column_set"}, {hidden, " hidden"},
				{masked, " masked " + mask}, {col.Encrypted, " encrypted"}, {filestream, " filestream"}, {xmlSchema != "", " xml " + xmlSchema},
				{col.Generated != 0, " generated " + strconv.Itoa(col.Generated)}, {col.Graph != 0, " graph " + strconv.Itoa(col.Graph)}} {
				if f.on {
					line += f.text
				}
			}
			columnLines[id] = append(columnLines[id], line)
			return nil
		},
		func(r *sql.Rows) error { // indexes of tables and views
			var id int64
			var indexID int
			var name, kind, filter, space, cols, compression string
			var unique, primary, uniqueConstraint, ignoreDup, disabled bool
			if err := r.Scan(&id, &indexID, &name, &kind, &unique, &primary, &uniqueConstraint, &ignoreDup, &disabled, &filter, &space, &cols, &compression); err != nil {
				return err
			}
			if t := table(id); t != nil && primary {
				t.KeyClustered = indexID == 1
			}
			add(strconv.FormatInt(id, 10), fmt.Sprintf("index %s %s unique %s primary %s constraint %s ignore_dup_key %s disabled %s on %s (%s) where %s compression %s",
				qms(name), kind, b(unique), b(primary), b(uniqueConstraint), b(ignoreDup), b(disabled), space, cols, filter, compression))
			return nil
		},
		func(r *sql.Rows) error { // primary key columns, in key order
			var id int64
			var col string
			var indexID int
			if err := r.Scan(&id, &col, &indexID); err != nil {
				return err
			}
			if t := table(id); t != nil {
				t.Key = append(t.Key, col)
			}
			return nil
		},
		func(r *sql.Rows) error { // foreign keys
			var fk ssForeignKey
			var notForRepl bool
			var refName, cols string
			if err := r.Scan(&fk.Table, &fk.Referenced, &fk.Name, &fk.DeleteAction, &fk.UpdateAction, &fk.Disabled, &fk.NotTrusted, &notForRepl, &refName, &cols); err != nil {
				return err
			}
			inv.ForeignKeys = append(inv.ForeignKeys, fk)
			add(strconv.FormatInt(fk.Table, 10), fmt.Sprintf("foreign key %s (%s) references %s on delete %s on update %s disabled %s not trusted %s not for replication %s",
				qms(fk.Name), cols, refName, fk.DeleteAction, fk.UpdateAction, b(fk.Disabled), b(fk.NotTrusted), b(notForRepl)))
			return nil
		},
		func(r *sql.Rows) error { // CHECK constraints
			var ck ssCheck
			var def, col string
			var notForRepl bool
			if err := r.Scan(&ck.Table, &ck.Name, &def, &ck.Disabled, &ck.NotTrusted, &notForRepl, &col); err != nil {
				return err
			}
			inv.Checks = append(inv.Checks, ck)
			add(strconv.FormatInt(ck.Table, 10), fmt.Sprintf("check %s %s %s disabled %s not trusted %s not for replication %s",
				qms(ck.Name), col, def, b(ck.Disabled), b(ck.NotTrusted), b(notForRepl)))
			return nil
		},
		func(r *sql.Rows) error { // views, procedures, functions, DML triggers
			var id, parent int64
			var kind, schema, name, def, executeAs, assembly, events string
			var ansiNulls, quotedID, schemaBound, disabled, insteadOf, notForRepl int
			if err := r.Scan(&id, &kind, &schema, &name, &parent, &def, &ansiNulls, &quotedID, &schemaBound, &executeAs, &assembly,
				&disabled, &insteadOf, &notForRepl, &events); err != nil {
				return err
			}
			item := ssObjectKinds[kind] + ":" + schema + "." + name
			if err := claim(item, ssObjectKinds[kind]+" "+qs2(schema, name)); err != nil {
				return err
			}
			objects[id] = item
			text := fmt.Sprintf("%s ansi_nulls %d quoted_identifier %d schemabinding %d execute as %s %s\n%s", ssObjectKinds[kind], ansiNulls, quotedID, schemaBound, executeAs, assembly, def)
			if kind == "TR" {
				inv.Triggers = append(inv.Triggers, ssTrigger{ID: id, Schema: schema, Name: name, Item: item, Table: parent, Disabled: disabled != 0})
				text = fmt.Sprintf("on %s disabled %d instead of %d not for replication %d events %s\n", tableName(table(parent)), disabled, insteadOf, notForRepl, events) + text
			}
			add(item, text)
			return nil
		},
		func(r *sql.Rows) error { // DDL triggers of the database
			var id int64
			var name, def, events string
			var disabled bool
			if err := r.Scan(&id, &name, &disabled, &def, &events); err != nil {
				return err
			}
			item := "trigger:database." + name
			if err := claim(item, "database trigger "+qms(name)); err != nil {
				return err
			}
			inv.Triggers = append(inv.Triggers, ssTrigger{ID: id, Name: name, Item: item, Disabled: disabled})
			add(item, fmt.Sprintf("on database disabled %s events %s\n%s", b(disabled), events, def))
			return nil
		},
		func(r *sql.Rows) error { // sequences
			q := &ssSequence{}
			if err := r.Scan(&q.ID, &q.Schema, &q.Name, &q.Type, &q.Start, &q.Increment, &q.Min, &q.Max, &q.Cycle, &q.Cached, &q.CacheSize); err != nil {
				return err
			}
			q.Item = "sequence:" + q.Schema + "." + q.Name
			if err := claim(q.Item, "sequence "+qs2(q.Schema, q.Name)); err != nil {
				return err
			}
			objects[q.ID] = q.Item
			if s.excluded(q.Schema, q.Name) {
				return nil
			}
			inv.Sequences = append(inv.Sequences, q)
			add(q.Item, q.def())
			return nil
		},
		func(r *sql.Rows) error { // user-defined types
			var id int64
			var schema, name, base, collation, columns, assembly, bindings string
			var maxLength, precision, scale int
			var nullable, tableType bool
			if err := r.Scan(&id, &schema, &name, &base, &maxLength, &precision, &scale, &collation, &nullable, &tableType, &columns, &assembly, &bindings); err != nil {
				return err
			}
			item := "type:" + schema + "." + name
			if err := claim(item, "type "+qs2(schema, name)); err != nil {
				return err
			}
			types[id] = item
			add(item, fmt.Sprintf("type %s %d %d %d %s null %s table %s (%s) %s%s", base, maxLength, precision, scale, collation, b(nullable), b(tableType), columns, assembly, bindings))
			return nil
		},
		func(r *sql.Rows) error { // synonyms
			var id int64
			var schema, name, base string
			if err := r.Scan(&id, &schema, &name, &base); err != nil {
				return err
			}
			item := "synonym:" + schema + "." + name
			if err := claim(item, "synonym "+qs2(schema, name)); err != nil {
				return err
			}
			objects[id] = item
			add(item, "synonym for "+base)
			return nil
		},
		func(r *sql.Rows) error { // schemas
			var id int64
			var name, owner string
			if err := r.Scan(&id, &name, &owner); err != nil {
				return err
			}
			schemas[id] = "schema:" + name
			add("schema:"+name, "schema owner "+owner)
			return nil
		},
		func(r *sql.Rows) error { // permissions
			var class int
			var id int64
			var text string
			if err := r.Scan(&class, &id, &text); err != nil {
				return err
			}
			add(classItem(class, id), text)
			return nil
		},
		func(r *sql.Rows) error { // extended properties
			var class int
			var id int64
			var text string
			if err := r.Scan(&class, &id, &text); err != nil {
				return err
			}
			add(classItem(class, id), text)
			return nil
		},
		func(r *sql.Rows) error { // row-level security predicates
			var id int64
			var policy, operation, def string
			var enabled, bound bool
			var kind int
			if err := r.Scan(&id, &policy, &enabled, &bound, &kind, &operation, &def); err != nil {
				return err
			}
			predicate := "filter"
			if kind == 1 {
				predicate = "block"
			}
			if t := table(id); t != nil && enabled && t.Security == "" {
				t.Security = "policy " + policy + " (" + predicate + " predicate)"
			}
			add(strconv.FormatInt(id, 10), fmt.Sprintf("security policy %s enabled %s schemabinding %s %s %s %s", policy, b(enabled), b(bound), predicate, operation, def))
			return nil
		},
		func(r *sql.Rows) error { // users, roles and role members
			var line string
			if err := r.Scan(&line); err != nil {
				return err
			}
			add(ssDatabaseItem, line)
			return nil
		},
		func(r *sql.Rows) error { // partition functions and schemes, XML schema collections
			var id int64
			var item, def string
			if err := r.Scan(&id, &item, &def); err != nil {
				return err
			}
			if err := claim(item, item); err != nil {
				return err
			}
			if strings.HasPrefix(item, "xml_schema:") {
				xmlSchemas[id] = item
			}
			add(item, def)
			return nil
		},
	}
	if err := readSets(ctx, c, fmt.Sprintf(ssCatalog, ledger), sets...); err != nil {
		return nil, err
	}
	// Permissions and properties were filed by class and id: now by item.
	byClass := func(key string) string {
		class, id, _ := strings.Cut(key, "/")
		n, _ := strconv.ParseInt(id, 10, 64)
		switch class {
		case "1":
			return objects[n]
		case "3":
			return schemas[n]
		case "6":
			return types[n]
		case "10":
			return xmlSchemas[n]
		}
		return ""
	}
	byItem := map[string][]string{}
	for key, ls := range lines {
		item := key
		switch {
		case strings.Contains(key, "/"):
			// What belongs to no item (the database, dbo and the other
			// built-in schemas, users…) belongs to the database's.
			if item = byClass(key); item == "" {
				item = ssDatabaseItem
			}
		case !strings.Contains(key, ":"): // an object id: indexes and constraints of a table or view
			n, _ := strconv.ParseInt(key, 10, 64)
			item = objects[n]
		}
		if item != "" {
			byItem[item] = append(byItem[item], ls...)
		}
	}
	lines = byItem
	if lines[ssDatabaseItem] == nil {
		lines[ssDatabaseItem] = []string{} // always an item: the options are added (apply)
	}
	sort.Slice(inv.Tables, func(i, j int) bool { return inv.Tables[i].Item < inv.Tables[j].Item })
	for _, t := range inv.Tables {
		if t.Excluded {
			delete(lines, t.Item)
			continue
		}
		ls := lines[t.Item]
		sort.Strings(ls)
		// Columns keep their order; the other lines are sorted.
		inv.base[t.Item] = strings.Join(append(columnLines[t.ID], ls...), "\n")
		delete(lines, t.Item)
	}
	for item, ls := range lines {
		kind, name, _ := strings.Cut(item, ":")
		if kind == "view" || kind == "sequence" {
			schema, bare, _ := strings.Cut(name, ".")
			if s.excluded(schema, bare) {
				continue
			}
		}
		if kind == "sequence" && !hasSequence(inv, item) {
			continue // excluded
		}
		var main, rest []string
		for _, l := range ls {
			if strings.HasPrefix(l, "grant ") || strings.HasPrefix(l, "property ") || strings.HasPrefix(l, "index ") {
				rest = append(rest, l)
			} else {
				main = append(main, l)
			}
		}
		sort.Strings(rest)
		inv.base[item] = strings.Join(append(main, rest...), "\n")
	}
	return inv, nil
}

// classItem files a permission or property by its class and id until the
// items are known (inventory).
func classItem(class int, id int64) string { return fmt.Sprintf("%d/%d", class, id) }

func tableName(t *ssTable) string {
	if t == nil {
		return "?"
	}
	return t.qname()
}

func hasSequence(inv *ssInventory, item string) bool {
	for _, q := range inv.Sequences {
		if q.Item == item {
			return true
		}
	}
	return false
}

// readSets runs a batch and hands every row of its n-th result set to the
// n-th function.
func readSets(ctx context.Context, c *sql.Conn, batch string, sets ...func(*sql.Rows) error) error {
	rows, err := c.QueryContext(ctx, batch)
	if err != nil {
		return err
	}
	defer rows.Close()
	for i, f := range sets {
		if i > 0 && !rows.NextResultSet() {
			if err := rows.Err(); err != nil {
				return err
			}
			return fmt.Errorf("SQL Server returned %d result sets, not %d", i, len(sets))
		}
		for rows.Next() {
			if err := f(rows); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}
