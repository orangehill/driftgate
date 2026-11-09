# MySQL and MariaDB

How DriftGate seeds, restores and fingerprints a MySQL or MariaDB
database. What every driver shares is in [State](../state.md).

## How seed and restore work

- A restore copies only the tables that changed, with their counters, triggers and views.
- `seed` marks the pristine database it makes with a table `driftgate_pristine`, which is never fingerprinted or restored ([how seed and restore work](../state.md#how-seed-and-restore-work)).
- `seed` needs `CREATE` and `DROP` on the pristine database ([what the database user needs](../state.md#what-the-database-user-needs)).
- Triggers and views are fingerprinted with the collation they were made with, which a restore keeps.
- A Galera node cannot create a MariaDB sequence that has a cache with an increment other than 0, or one that is not InnoDB. On such a node, a seed or restore stops before it changes anything. Make the sequence `NOCACHE` or `INCREMENT BY 0`, store it in InnoDB, or exclude it.
- A fingerprint, seed or restore that the server stops for a lock (1213 or 1205) runs again from the start, up to three times. A Galera node stops a read this way when another session alters a table.
- Triggers and views are made again with their `DEFINER`, which needs rights of its own ([below](#triggers-and-views-with-a-definer)).

## Triggers and views with a DEFINER

A seed and a restore make triggers and views again with their
`DEFINER`, which DriftGate never drops or changes. A trigger, and a view
with `SQL SECURITY DEFINER`, run with their definer's rights, and the
`DEFINER` is part of the fingerprint.

A login may name another account as `DEFINER` only with one of:

- `SET_ANY_DEFINER` (MySQL 8.2 and later; `SET_USER_ID` before), plus `SYSTEM_USER` when the definer account has it, such as root;
- `SET USER` (MariaDB);
- `SUPER`.

With the binary log on, a login without `SUPER` may create a trigger
only when `log_bin_trust_function_creators` is on. A Galera node checks
as if the binary log were on, also with `log_bin=0`.

Each `DEFINER` is first tried on a short-lived probe object.

- A restore the login may not create a trigger or view for stops before it drops anything, and names the object.
- A seed stops before it drops the old pristine copy.

A probe cannot be tried in two cases. There the refusal comes from the
real `CREATE`, after the drops, and the message names what is missing.

- A seed tries the probe in the working database, where the login may not create the view at all.
- A trigger's probe needs a table to sit on. A login with `TRIGGER` on some tables only does not have one in a database without those tables.

The server may still refuse a drop: on MySQL, an object whose `DEFINER`
has `SYSTEM_USER`, such as root. The objects dropped before it are then
made again from the pristine copy. The message names any that could not
be, and the next successful restore makes them again.

Best: create the triggers and views with the application's login as
their `DEFINER` (run the migrations as it), and seed again.

The prefix `dgt_probe_` is reserved for those probes. An object named
`dgt_probe_` and 16 lowercase hexadecimal digits is never fingerprinted,
copied or restored, and a seed or restore drops it.

## The binary log and replicas

Restores and seeds stay out of the binary log: DriftGate's sessions set
`sql_log_bin = 0`. MySQL 8 logs by default, and a restore would
otherwise log every row it copies.

- The user needs `SESSION_VARIABLES_ADMIN` (MySQL) or `BINLOG ADMIN` (MariaDB) for it.
- Without it the copies are logged as before, with one note in the log.

When something reads the server's binary log, restores and seeds are
logged, so a replica gets them too. An application that reads from a
replica would otherwise see old rows, and later replication may break.

DriftGate checks once per command for:

- a replica (`SHOW REPLICAS`, or `SHOW SLAVE HOSTS`);
- another client reading the log: a `Binlog Dump` thread, such as a change-data-capture tool;
- a Galera node, or group replication.

When the user may not list replicas, DriftGate cannot tell, and the
copies are logged; the log says why. Listing replicas needs
`REPLICATION SLAVE` on MySQL, and `REPLICATION MASTER ADMIN` on MariaDB
(`REPLICATION SLAVE` before 10.5).

A replica that is disconnected at that moment is not seen. Stop it from
replicating the state databases (`replicate-ignore-db`), or give the user
no rights to list replicas.

## The optional PROCESS privilege

A fingerprint reads the server's write counters first. When none moved
since the last fingerprint, no table is read again. When only rows were
written, per-table counters name the tables to checksum again; anything
else reads every table.

The per-table counters need settings DriftGate reads but never changes,
and some grants:

- MySQL: each table's `UPDATE_TIME`, and the write counts of `performance_schema`. That needs `performance_schema` on, table I/O instrumented for the working database and every connection, and `SELECT ON performance_schema.*`.
- MariaDB: `information_schema.TABLE_STATISTICS`, which needs `userstat=ON`.
- Both: the global `PROCESS` privilege. Through `SHOW ENGINE INNODB STATUS`, it tells whether another connection still holds uncommitted writes.

Without them the result is the same, and fingerprints still work. But
every fingerprint after any write then checksums every table, which is
slower on a large database. The log says once how fingerprints are taken,
and why the per-table counters are off, naming the missing grant.

## Column collations on MySQL 8

A column that takes its table's collation without naming it keeps it
that way through a seed and a restore. That is any such column of a
table whose collation is not its character set's default, as
`utf8mb3_unicode_ci` in a Laravel 5.5 table. A pristine copy seeded by
an earlier release needs a step of its own:
[column collations after v1.0.2 on MySQL 8](../upgrading-driftgate.md#column-collations-after-v102-on-mysql-8).

## Run the server in a container

Start the server with `--skip-name-resolve` (compose:
`command: --skip-name-resolve`). Without it the server looks up the name
of every client that connects, and a slow lookup makes each connection
slow. Measured: fingerprints of 622 requests took 29 minutes, and 45
seconds with it.

- The official images set it in a file that a project's own mounted configuration can replace: `/etc/my.cnf` for `mysql`, `/etc/mysql/mariadb.conf.d/05-skipcache.cnf` for `mariadb`.
- Accounts are then matched by address (`'app'@'%'`), not by host name.

## Connections DriftGate keeps open

DriftGate keeps up to 13 connections per database component open for
the whole command, so fingerprints and restores do not connect again.
Three components take 39, against the default `max_connections` of 151
(MariaDB 10.3: 100).

- Twelve serve fingerprints and restores. Each is prepared once, when it is made, with one `SET` statement for all its session settings.
- One more reads `SHOW CREATE` for many objects in one query of many statements. It sends only quoted names, no parameters, and needs no extra grant.
- When a server or proxy refuses a query of several statements, the statements are read one at a time, spread over several connections, with one line in the log. So it is when the connection's character set cannot quote a name safely.
- Results and hashes are the same either way, and nothing needs a new seed.

## Generated and virtual columns on MariaDB

- A table with a generated column is fingerprinted from its rows: MariaDB's own table checksum of such a table changes without a change of data.
- On MariaDB 10.3 and 10.4, a table with a `VIRTUAL` column is restored whole, not by chunks. The chunked restore made those versions crash now and then.

---
**Related:** [State: restoring the data](../state.md) · [Database connection settings](../database-settings.md) · [driftgate seed](../commands/state.md#driftgate-seed)
