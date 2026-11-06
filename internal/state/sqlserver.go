package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/orangehill/driftgate/internal/glob"
)

var _ interface {
	Component
	Querier
} = (*SQLServer)(nil)

// SQLServer is a SQL Server database (2017 or later; not Azure SQL Database
// or Managed Instance). The pristine copy is a second database on the same
// server, read-only, made by BACKUP and RESTORE. A restore copies back the
// tables and sequences that differ; anything it cannot restore item by
// item brings the whole database back from the pristine copy's backup.
//
// Fingerprints read the server's log end and identity and sequence values
// first: when none moved since the last one, nothing changed (Reused). Else
// the per-table write counters name the tables to hash again. Both need
// VIEW SERVER PERFORMANCE STATE (2019 and older: VIEW SERVER STATE);
// without it every fingerprint hashes every table.
type SQLServer struct {
	o   Options
	cfg msdsn.Config

	mu    sync.Mutex
	pools map[string]*sql.DB // by database; "master" for the server

	// ended are the application's sessions in the working database the
	// last Restore ended (a restore of the whole database; EndedSessions),
	// each once across the runs of a retried Restore.
	ended map[ssSession]bool

	info    *sqlServerInfo // read once (server)
	checked bool           // the working database passed checkWorking
	multi   bool           // the working database was not SINGLE_USER at the first fingerprint (checkShared)

	// What the next fingerprint may reuse (sqlserver_reuse.go): the last
	// full fingerprint, the inventory of the working database with the
	// guard read before it, and the connection fingerprints read on; the
	// mode fingerprints are taken in, and whether the last one was reused.
	last     *ssLast
	inv      *ssInventory
	invGuard string
	own      ssOwnDDL // the catalog changes of DriftGate's own last restore
	work     *sql.Conn
	reused   bool
	mode     string
	// rowversion: the largest value of every table after DriftGate's own
	// last restore, and the value multiset of the pristine copy's tables
	// (sqlserver_fingerprint.go, rowversionMoved).
	rvMax      map[string][]byte
	rvPristine map[string]string
	// The pristine copy's catalog, read once per copy: it is read-only,
	// and a new copy has a new mark (sqlserver_restore.go).
	pcat *ssPristineCatalog
	// seen is the backup set GUID of the pristine copy this component
	// fingerprinted, seeded or restored from first: another one means
	// another seed replaced the copy meanwhile (pristineMark).
	seen string
}

// NewSQLServer connects to the server. o.DSN is a sqlserver:// URL of the
// go-mssqldb driver; o.Working and o.Pristine are databases on that server.
// The database named in the URL is not used. Nothing is sent to the server
// before the first fingerprint, restore, seed or query.
func NewSQLServer(o Options) (*SQLServer, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	allowNegativeSerial() // before the first TLS handshake
	cfg, err := sqlServerConfig(o.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.AppName == "go-mssqldb" { // the driver's default: the DSN names none
		// DriftGate's sessions are told from the application's by it
		// (countSessions), and a DBA sees whose they are.
		cfg.AppName = sqlServerAppName
	}
	if strings.EqualFold(o.Working, o.Pristine) {
		return nil, fmt.Errorf("state %s: the pristine database must be another one than %s", o.Name, o.Working)
	}
	return &SQLServer{o: o, cfg: cfg, pools: map[string]*sql.DB{}}, nil
}

func (s *SQLServer) Name() string  { return s.o.Name }
func (s *SQLServer) Partial() bool { return true }

func (s *SQLServer) Close() error {
	s.unpin()
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for db, p := range s.pools {
		if e := p.Close(); err == nil {
			err = e
		}
		delete(s.pools, db)
	}
	return err
}

// Reused reports whether the last fingerprint came from the log end check.
func (s *SQLServer) Reused() bool { return s.reused }

// Mode says how the last full fingerprint knew which tables to read.
func (s *SQLServer) Mode() string { return s.mode }

// setMode records how fingerprints are taken and says so when it changes.
func (s *SQLServer) setMode(mode string) {
	if mode != s.mode {
		s.mode = mode
		fmt.Fprintf(s.o.Log, "state %s: %s\n", s.o.Name, mode)
	}
}

// excluded reports whether a table (schema.name) is excluded: a glob
// matches it with or without its schema. SQL Server compares names
// without case (in the usual collations), so the globs do too.
func (s *SQLServer) excluded(schema, name string) bool {
	full, bare := strings.ToLower(schema+"."+name), strings.ToLower(name)
	for _, g := range s.o.Exclude {
		g = strings.ToLower(g)
		if glob.Match(g, full) || glob.Match(g, bare) {
			return true
		}
	}
	return false
}

// restoreOnly is excluded for restore_only (Options.RestoreOnly).
func (s *SQLServer) restoreOnly(schema, name string) bool {
	full, bare := strings.ToLower(schema+"."+name), strings.ToLower(name)
	for _, g := range s.o.restoreOnlyGlobs() {
		g = strings.ToLower(g)
		if glob.Match(g, full) || glob.Match(g, bare) {
			return true
		}
	}
	return false
}

// restoreEmpty is restoreOnly for Options.RestoreEmpty.
func (s *SQLServer) restoreEmpty(schema, name string) bool {
	full, bare := strings.ToLower(schema+"."+name), strings.ToLower(name)
	for _, g := range s.o.RestoreEmpty {
		g = strings.ToLower(g)
		if glob.Match(g, full) || glob.Match(g, bare) {
			return true
		}
	}
	return false
}

// tracked reports whether a table (schema.name) tracks its rows: listed
// with its schema, or without it when it is in dbo.
func (s *SQLServer) tracked(schema, name string) bool {
	for _, t := range s.o.RowTracking {
		if strings.EqualFold(t, schema+"."+name) || !strings.Contains(t, ".") && strings.EqualFold(schema, "dbo") && strings.EqualFold(t, name) {
			return true
		}
	}
	return false
}

// sqlServerInfo is what DriftGate needs to know of the server.
type sqlServerInfo struct {
	Major   int    // ProductMajorVersion: 14 is 2017, 15 2019, 16 2022, 17 2025
	Engine  int    // EngineEdition: 2 Standard, 3 Enterprise or Developer, 4 Express, 5 Azure SQL Database, 8 Managed Instance
	Version string // ProductVersion, e.g. 16.0.4305.5
	Name    string // @@SERVERNAME
	// Perf: the login may read the server's performance state (log end,
	// open transactions, index usage): VIEW SERVER PERFORMANCE STATE on
	// 2022 and later, VIEW SERVER STATE before. Fingerprints are reused
	// and counted per table only with it.
	Perf     bool
	Sysadmin bool
	Start    string // sqlserver_start_time (with Perf): counters start again with the server
}

// Express reports whether the server is SQL Server Express (no backup
// compression).
func (i *sqlServerInfo) Express() bool { return i.Engine == 4 }

// perfGrant is the statement that gives a login what Perf needs.
func (i *sqlServerInfo) perfGrant(login string) string {
	if i.Major >= 16 {
		return "GRANT VIEW SERVER PERFORMANCE STATE TO " + qms(login)
	}
	return "GRANT VIEW SERVER STATE TO " + qms(login)
}

// server reads the server's version, edition and the login's permissions
// once, and refuses a server DriftGate does not support: older than 2017,
// or Azure SQL Database, Managed Instance or Synapse, whose databases
// cannot be backed up to and restored from a file.
func (s *SQLServer) server(ctx context.Context) (*sqlServerInfo, error) {
	if s.info != nil {
		return s.info, nil
	}
	c, err := s.conn(ctx, "")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	i := &sqlServerInfo{}
	var perf, sysadmin sql.NullInt64
	err = c.QueryRowContext(ctx, `SELECT ISNULL(CAST(SERVERPROPERTY('ProductMajorVersion') AS int), 0), ISNULL(CAST(SERVERPROPERTY('EngineEdition') AS int), 0),
		ISNULL(CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)), N''), ISNULL(@@SERVERNAME, N''),
		HAS_PERMS_BY_NAME(NULL, NULL, CASE WHEN CAST(SERVERPROPERTY('ProductMajorVersion') AS int) >= 16 THEN 'VIEW SERVER PERFORMANCE STATE' ELSE 'VIEW SERVER STATE' END),
		IS_SRVROLEMEMBER('sysadmin')`).Scan(&i.Major, &i.Engine, &i.Version, &i.Name, &perf, &sysadmin)
	if err != nil {
		return nil, err
	}
	switch i.Engine {
	case 5, 6, 8, 11:
		return nil, fmt.Errorf("state %s: the server is Azure SQL Database, Managed Instance or Synapse (engine edition %d), which DriftGate does not support; it needs SQL Server 2017 or later", s.o.Name, i.Engine)
	}
	if i.Major < 14 {
		return nil, fmt.Errorf("state %s: the server is SQL Server %s; DriftGate needs SQL Server 2017 (14.0) or later", s.o.Name, i.Version)
	}
	i.Sysadmin = sysadmin.Int64 == 1
	if perf.Int64 == 1 {
		// The permission is there; the views it opens must answer too.
		var start string
		if err := c.QueryRowContext(ctx, `SELECT CONVERT(varchar(30), sqlserver_start_time, 126) FROM sys.dm_os_sys_info`).Scan(&start); err == nil {
			i.Perf, i.Start = true, start
		}
	}
	s.info = i
	return i, nil
}

// sqlServerDatabase is what DriftGate reads of a database in sys.databases.
type sqlServerDatabase struct {
	Name, State, UserAccess, Recovery string
	ID                                int
	ReadOnly                          bool
	CDC, Replicated, AG, Ledger       bool
	ChangeTracking                    bool
	DelayedDurability                 string // DISABLED, ALLOWED or FORCED
	Broker, Trustworthy, Chaining     bool
	Collation                         string
}

// database reads a database's properties; nil when there is none of that
// name.
func (s *SQLServer) database(ctx context.Context, name string) (*sqlServerDatabase, error) {
	dbs, _, err := s.databases(ctx, []string{name}, "")
	if err != nil {
		return nil, err
	}
	return dbs[0], nil
}

// databases reads the properties of the databases named (nil for one
// there is none of) and, with marked, the mark of that database
// (readMark: nil when it has none or is not online), in one batch.
func (s *SQLServer) databases(ctx context.Context, names []string, marked string) ([]*sqlServerDatabase, *sqlServerMark, error) {
	info, err := s.server(ctx)
	if err != nil {
		return nil, nil, err
	}
	ledger := "0"
	if info.Major >= 16 {
		ledger = "ISNULL(CAST(d.is_ledger_on AS int), 0)"
	}
	var list []string
	var args []any
	for i, name := range names {
		list = append(list, fmt.Sprintf("(%d, @p%d)", i, i+1))
		args = append(args, name)
	}
	q := `SELECT n.i, d.name, d.database_id, d.state_desc, d.user_access_desc, d.recovery_model_desc, d.is_read_only,
		CAST(d.is_cdc_enabled AS int) | CAST(d.is_published AS int) * 2 | CAST(d.is_subscribed AS int) * 4 | CAST(d.is_merge_published AS int) * 8 | CAST(d.is_distributor AS int) * 16,
		CASE WHEN d.replica_id IS NULL THEN 0 ELSE 1 END, ` + ledger + `,
		CASE WHEN EXISTS (SELECT 1 FROM sys.change_tracking_databases t WHERE t.database_id = d.database_id) THEN 1 ELSE 0 END,
		ISNULL(d.delayed_durability_desc, N'DISABLED'), d.is_broker_enabled, d.is_trustworthy_on, d.is_db_chaining_on, ISNULL(d.collation_name, N'')
		FROM (VALUES ` + strings.Join(list, ", ") + `) n (i, name) JOIN sys.databases d ON d.name = n.name;`
	if marked != "" {
		// The mark of an online database, by its quoted name (a
		// database's name fits QUOTENAME's 128 characters).
		q += fmt.Sprintf(`
DECLARE @dg_n int = 0, @dg_m nvarchar(4000);
IF EXISTS (SELECT 1 FROM sys.databases WHERE name = @p%d AND state = 0)
	BEGIN
	DECLARE @dg_q nvarchar(max) = N'SELECT @n = COUNT(*), @m = MAX(CAST(value AS nvarchar(4000))) FROM ' + QUOTENAME(@p%d) + N'.sys.extended_properties WHERE class = 0 AND name = @k';
	EXEC sys.sp_executesql @dg_q, N'@n int OUTPUT, @m nvarchar(4000) OUTPUT, @k sysname', @n = @dg_n OUTPUT, @m = @dg_m OUTPUT, @k = @p%d;
	END
SELECT @dg_n, @dg_m;`, len(names)+1, len(names)+1, len(names)+2)
		args = append(args, marked, sqlServerMarkName)
	}
	c, err := s.conn(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	rows, err := c.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := make([]*sqlServerDatabase, len(names))
	for rows.Next() {
		d := &sqlServerDatabase{}
		var i int
		var flags int // change data capture, then the four kinds of replication
		if err := rows.Scan(&i, &d.Name, &d.ID, &d.State, &d.UserAccess, &d.Recovery, &d.ReadOnly, &flags, &d.AG, &d.Ledger, &d.ChangeTracking,
			&d.DelayedDurability, &d.Broker, &d.Trustworthy, &d.Chaining, &d.Collation); err != nil {
			return nil, nil, err
		}
		d.CDC, d.Replicated = flags&1 != 0, flags&^1 != 0
		out[i] = d
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if marked == "" {
		return out, nil, nil
	}
	if !rows.NextResultSet() {
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("SQL Server returned no mark")
	}
	var n int
	var raw sql.NullString
	for rows.Next() {
		if err := rows.Scan(&n, &raw); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil || n == 0 {
		return out, nil, err
	}
	m, err := s.parseMark(marked, raw)
	return out, m, err
}

// refuse says why DriftGate does not restore a database, or nil: change
// data capture, replication, an availability group (a restore of a
// database in one fails or breaks it) and ledger databases.
func (d *sqlServerDatabase) refuse(component string) error {
	why := ""
	switch {
	case d.AG:
		why = "is in an availability group"
	case d.CDC:
		why = "has change data capture on (sys.sp_cdc_disable_db turns it off)"
	case d.Replicated:
		why = "is published, subscribed or a distributor of replication"
	case d.Ledger:
		why = "is a ledger database"
	default:
		return nil
	}
	return fmt.Errorf("state %s: database %s %s, which DriftGate does not restore; use a database without it", component, d.Name, why)
}

// sqlServerAppName is the program name of DriftGate's sessions when the
// DSN names none.
const sqlServerAppName = "driftgate"

// systemDatabases are never a working or pristine database.
var systemDatabases = map[string]bool{"master": true, "model": true, "msdb": true, "tempdb": true, "distribution": true, "resource": true}

// checkWorking refuses a working database DriftGate does not restore
// (sqlServerDatabase.refuse), once; it notes change tracking, which the
// restore leaves inconsistent for the application's sync. A working
// database that does not exist yet is checked again next time.
func (s *SQLServer) checkWorking(ctx context.Context) error {
	if s.checked {
		return nil
	}
	if systemDatabases[strings.ToLower(s.o.Working)] || systemDatabases[strings.ToLower(s.o.Pristine)] {
		return fmt.Errorf("state %s: a system database (master, model, msdb, tempdb) cannot be the working or pristine database", s.o.Name)
	}
	d, err := s.database(ctx, s.o.Working)
	if err != nil || d == nil {
		return err
	}
	if err := d.refuse(s.o.Name); err != nil {
		return err
	}
	if d.ChangeTracking {
		fmt.Fprintf(s.o.Log, "state %s: database %s has change tracking on; a restore resets the rows but not the change tracking versions the application syncs from\n", s.o.Name, d.Name)
	}
	s.checked = true
	return nil
}

// sqlServerMarkName names the extended property of the database that marks
// a pristine copy Seed made (sqlServerMark); a database without it is
// never dropped or overwritten unless it is empty or adopted
// (checkPristine).
const sqlServerMarkName = "driftgate_pristine"

// sqlServerMark is the value of the mark, as JSON: the working database
// the copy was made of, the server (@@SERVERNAME), the backup set GUID of
// the fallback backup made of the copy (RESTORE HEADERONLY BackupSetGUID),
// when it was made (RFC 3339), and whether the working database had
// Service Broker on (a restore of the whole database that stopped
// halfway no longer shows it).
type sqlServerMark struct {
	Working  string `json:"working"`
	Server   string `json:"server"`
	Fallback string `json:"fallback,omitempty"`
	Created  string `json:"created"`
	Broker   bool   `json:"broker,omitempty"`
	// Snapshot is the working database's ALLOW_SNAPSHOT_ISOLATION
	// (snapshot_isolation_state) when the copy was made: a read-only
	// database reports it on, whatever it was set to (readOnlyValues).
	Snapshot *int `json:"snapshot_isolation,omitempty"`
}

// readMark reads the mark of a database; nil when it has none. The
// database must be online.
func (s *SQLServer) readMark(ctx context.Context, db string) (*sqlServerMark, error) {
	c, err := s.conn(ctx, "")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var raw sql.NullString
	err = c.QueryRowContext(ctx, `SELECT CAST(value AS nvarchar(4000)) FROM `+qms(db)+`.sys.extended_properties WHERE class = 0 AND name = @p1`, sqlServerMarkName).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.parseMark(db, raw)
}

// parseMark reads the value of the mark of database db.
func (s *SQLServer) parseMark(db string, raw sql.NullString) (*sqlServerMark, error) {
	m := &sqlServerMark{}
	if err := json.Unmarshal([]byte(raw.String), m); err != nil || m.Working == "" {
		return nil, fmt.Errorf("state %s: database %s has a DriftGate mark that cannot be read; drop the database yourself if it is not needed", s.o.Name, db)
	}
	return m, nil
}

// markValue is the mark Seed writes into a new pristine copy.
func (s *SQLServer) markValue(ctx context.Context, fallback, created string) (string, error) {
	info, err := s.server(ctx)
	if err != nil {
		return "", err
	}
	m := sqlServerMark{Working: s.o.Working, Server: info.Name, Fallback: fallback, Created: created}
	c, err := s.conn(ctx, "")
	if err != nil {
		return "", err
	}
	defer c.Close()
	var snapshot int
	if err := c.QueryRowContext(ctx, "SELECT snapshot_isolation_state FROM sys.databases WHERE name = @p1 AND is_read_only = 0", s.o.Working).Scan(&snapshot); err == nil {
		m.Snapshot = &snapshot
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// denied adds what the login lacks to an error of a seed or restore the
// server refused for want of a right: to create a database (262), or to
// open one it has no user in (916: a pristine copy another login made).
func (s *SQLServer) denied(err error) error {
	switch sqlServerNumber(err) {
	case 262:
		return fmt.Errorf("%w (the login needs the server role dbcreator to make the pristine copy; driftgate setup shows the statements)", err)
	case 916:
		return fmt.Errorf("%w (the login needs the server role dbcreator and db_owner in the working database, and a seed made with it; driftgate setup shows the statements)", err)
	}
	return err
}

// checkPristine refuses to drop or overwrite a database (the pristine copy
// or a database Seed and Restore make next to it) that DriftGate did not
// make for this working database: one with user objects, schemas or users
// and no mark for it, unless it is the pristine database and adopted
// (Options.AdoptPristine). A database that does not exist is fine. It
// runs right before every RESTORE, DROP or rename: RESTORE without
// REPLACE only checks that the backup is of the same database family,
// and every copy of the working database is.
func (s *SQLServer) checkPristine(ctx context.Context, db string) error {
	switch {
	case strings.EqualFold(db, s.o.Working):
		return fmt.Errorf("state %s: %s is the working database; it is never replaced by the pristine copy", s.o.Name, db)
	case systemDatabases[strings.ToLower(db)]:
		return fmt.Errorf("state %s: %s is a system database", s.o.Name, db)
	}
	d, err := s.database(ctx, db)
	if err != nil || d == nil {
		return err
	}
	if d.AG {
		return d.refuse(s.o.Name)
	}
	if d.State == "RESTORING" {
		return fmt.Errorf("state %s: database %s is RESTORING, so DriftGate cannot see whether it made it; drop it yourself (DROP DATABASE %s)", s.o.Name, db, qms(db))
	}
	if d.State != "ONLINE" {
		return fmt.Errorf("state %s: database %s is %s, so DriftGate cannot see whether it made it; bring it online or drop it yourself", s.o.Name, db, d.State)
	}
	m, err := s.readMark(ctx, db)
	if err != nil {
		return err
	}
	if m != nil {
		if !strings.EqualFold(m.Working, s.o.Working) {
			return fmt.Errorf("state %s: database %s is the pristine copy of %s, not of %s, so it is not replaced; name another pristine database", s.o.Name, db, m.Working, s.o.Working)
		}
		return nil
	}
	if s.o.AdoptPristine && strings.EqualFold(db, s.o.Pristine) {
		return nil
	}
	c, err := s.conn(ctx, "")
	if err != nil {
		return err
	}
	defer c.Close()
	var objects int
	if err := c.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM `+qms(db)+`.sys.objects WHERE is_ms_shipped = 0)
		+ (SELECT COUNT(*) FROM `+qms(db)+`.sys.schemas WHERE schema_id > 4 AND schema_id < 16384)
		+ (SELECT COUNT(*) FROM `+qms(db)+`.sys.database_principals WHERE principal_id > 4 AND is_fixed_role = 0 AND name <> N'public')
		+ (SELECT COUNT(*) FROM `+qms(db)+`.sys.types WHERE is_user_defined = 1)`).Scan(&objects); err != nil {
		return err
	}
	if objects == 0 {
		return nil
	}
	return fmt.Errorf("state %s: database %s, named as the pristine copy, was not made by driftgate seed and holds %d objects, schemas or users, so it is not dropped; drop it yourself or name another pristine database", s.o.Name, db, objects)
}
