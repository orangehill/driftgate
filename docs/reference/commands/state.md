# Commands: the pristine copy

The commands that make and remove the pristine copy that every capture
restores. What a restore does is under
[state](../../configuration.md#state).

## driftgate seed

```
driftgate seed [--skip-commands]
```

Runs the `seed:` commands and keeps a pristine copy of every state
database.

| Flag | Meaning |
|---|---|
| `--skip-commands` | take the data already loaded as pristine, without the `seed:` commands (a database cache in CI) |

- `seed` records the fingerprint of the pristine copy itself. Every capture restores the databases to the pristine copy and checks them against that fingerprint.
- The fingerprint is kept in the local store per database server (from the DSN, without its credentials) and per database. Checkouts that share the store but use their own database server never take each other's.
- A SQL Server pristine copy is a read-only database on the same server, made from a `COPY_ONLY` backup of the working database. Its own backup stays in the server's backup folder ([SQL Server](../../configuration.md#sql-server)).
- An SQLite database is named by the real path of its working file. Its pristine copy is a file next to that record, unless `pristine:` names one; the seed only reads the working file ([SQLite](../../configuration.md#sqlite)).

```sh
driftgate seed
```

### When to seed by hand

`run` seeds by itself when a pristine copy is missing, also when this
database server has none yet. Run `seed` by hand after you change the
seed data, `restore_only` or `restore_empty`.

### Seed while the application is quiet

Run `seed` while the application does not write: no queue workers,
schedulers or requests. MySQL and MariaDB copy the tables one by one, so
a write during the copy can leave a pristine copy that mixes data from
before and after it. A PostgreSQL, SQLite or SQL Server copy is of one
moment.

`seed` warns when the working database differs from the copy right after
it was made: the application wrote during the seed, or just after it.

## driftgate clean

```
driftgate clean [<component>…] [--yes]
```

Removes what `seed` made: the pristine copy of every database component
under `state:`, or of the components named, and its record in the local
store.

| Flag | Meaning |
|---|---|
| `--yes` | remove without asking (needed without a terminal) |

- It lists what it would remove, such as `state db: database shop_pristine (mysql, tcp(127.0.0.1:3306))` or an SQLite pristine file, and asks. Without a terminal to ask on, it stops (exit 2) unless `--yes`.
- The project's own commands (test users, a cache) are not run. The next capture needs `driftgate seed` again; `run` seeds by itself.

```sh
driftgate clean            # asks first
driftgate clean db --yes
```

### Which copies clean removes

- Only the pristine copies the configuration names, on the server its DSN names now; never a working database.
- A name that is not a database component under `state:` is refused (exit 2).
- A copy is removed only when `seed` made it as the copy of this project's working database. The mark that says so names the working database: the note of the `driftgate_pristine` table (MySQL, MariaDB), the database's comment (PostgreSQL), its mark (SQL Server), or the `driftgate_pristine` table (SQLite).
- Any other database or file of that name, such as another project's copy, is left as it is with its record (exit 3).
- A MySQL or MariaDB copy made by an earlier release has a mark that names no working database, and is refused too. Remove it yourself, or run `seed` once to mark it.
- SQL Server: the backup of the pristine copy in the server's backup folder is not removed.
- SQLite: the working file need not be there, as in a worktree that git did not give it. The copy is found by its record in the local store. Without the file and without a record, clean stops (exit 2) and names the folder of the store's copies.

---
**Related:** [state](../../configuration.md#state) · [seed, prepare, guards](../../configuration.md#seed-prepare-guards) · [Command line basics](../cli.md)
