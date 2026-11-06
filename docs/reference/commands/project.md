# Commands: set up a project

The commands that create a project, its key and its layout. Paths are
those of the local layout: everything in `.driftgate/`. With
`baselines: git` the same files are in `driftgate/`, and
`driftgate.yaml` is at the project root
([baselines](../../configuration.md#baselines)).

## driftgate init

```
driftgate init [--project <name>] [--baselines local|git] [--service <name>]
```

Writes `driftgate.yaml`, a catalog and an allowlist for the project in
this folder, from the frameworks it finds.

| Flag | Meaning |
|---|---|
| `--project <name>` | the project name (default: the folder name) |
| `--baselines local\|git` | `local` (default): everything in `.driftgate/`, which git ignores; `git`: `driftgate.yaml` and `driftgate/` in the repository, the store in `~/.driftgate/<project>` |
| `--service <name>` | write only this service (repeatable or comma-separated) |

- `local` adds `.driftgate/` to `.gitignore`, and to `.dockerignore` where Docker would copy it ([baselines](../../configuration.md#baselines)).
- `--config <path>` decides the layout too: a path in `.driftgate/` is `local`, any other `git`.
- In a worktree whose main worktree has a `baselines: local` project, `init` points to [setup](#setup-in-a-git-worktree) (exit 2).

```sh
driftgate init --project shop
```

### What init finds in a project

- It recognises Laravel, Symfony, Rails, Django, Spring Boot, Quarkus, Micronaut, ASP.NET, FastAPI, NestJS, Magento 2, Next.js and Nuxt.
- For each it writes the matching `discover:` adapters, the framework's own routes as exclusions with reasons (`.driftgate/exclusions.yaml`), and allowlist entries for values the framework changes on every response.
- The `Date` header is always allowed.
- A framework DriftGate has not been checked on is marked as such.
- In a monorepo it finds the services in the root, its subfolders, `apps/*`, `services/*` and `packages/*`. It reads their ports from `docker-compose.yml` ([several services](../../guides/multi-service.md)).
- Without a known framework it writes a commented configuration, and says to add routes under `discover:` or to write the catalog by hand.

### How init writes the database settings

The database and the address come from the project's own settings:
`.env`, `appsettings.json`, `application.properties`, `database.yml`,
compose files and the others under
[database DSNs](../../configuration.md#database-dsns).

- `state: db` names the setting (`dsn: { dotenv: .env }`), never its value. `base_url` is the address found.
- `state:` is written **commented out**: a `run` would otherwise create the pristine database unasked. `driftgate setup` turns it on after asking, or you uncomment it.
- With several settings, it takes the one `setup --yes` would, and lists the others above `state:`.
- For every driver, it writes the tables the framework keeps its own state in (sessions, cache, jobs; Hangfire, Solid) as `exclude:` and `restore_empty:` ([the framework's own tables](../../configuration.md#the-frameworks-own-tables)).

## driftgate setup

```
driftgate setup [--yes] [--project <name>] [--baselines local|git]
    [--driver mysql|postgres|sqlite|sqlserver] [--database <name>] [--url <address>]
```

Sets up a new project from zero to its first baseline.

| Flag | Meaning |
|---|---|
| `--yes` | ask nothing: take the best database setting and make the pristine copy |
| `--project <name>` | the project name (default: the folder name) |
| `--baselines local\|git` | as for `init`; default `local` |
| `--driver mysql\|postgres\|sqlite\|sqlserver` | the database server, when the settings do not say |
| `--database <name>` | the database the application uses (SQLite: its file, relative to the project root), when the settings do not say |
| `--url <address>` | where the application answers, when the settings do not say it or say it wrongly |

- `--yes` stops and says what to pass where a value would have to be guessed.

Exit `0` once the baseline is there. That holds also when routes are
left uncovered, or requests meant to succeed failed in it: the summary
lists them, and `run` and `coverage` then fail on those routes. Exit `1`
when the two captures differ and no baseline was made; the proposed
allowlist entries are printed. Other stops are under
[When setup stops](#when-setup-stops).

```sh
cd shop && driftgate setup
```

### The steps setup takes

1. Recognises the framework, as `init` does.
2. Reads the database and the address from the project's own settings.
3. Writes `.driftgate/` (with `--baselines git`, `driftgate.yaml` and `driftgate/`), with its `.gitignore` and `.dockerignore` entries.
4. Makes the key: `.driftgate/key`, or the macOS Keychain with `--baselines git`. It skips this when a key is set up already (`DRIFTGATE_KEY`, `key:`).
5. Makes the pristine copy of the database (`seed --skip-commands`). This creates a database, so it asks first (default no).
6. Checks that the application answers, then runs `run`: `discover` and `generate` write the routes and the catalog, and two captures make the baseline `local`.
7. Prints a summary (below).

Any HTTP response counts as the application answering. Setup prints the
status and the `Server` header, and the summary repeats them, so a
different server on that port shows.

The summary names:

- the framework;
- the database: driver, database, host, port, user and where it was found, never the password;
- that the data as it was became the pristine copy, or that the copy made before is kept;
- the mode: `freeze` (every drift fails, and `accept` is refused) or `evolve` (a drift may be accepted with a reason);
- the address, the routes covered, and what to check by hand.

A second run goes on where the first stopped. Apart from turning on the
`state:` block, it never changes an existing configuration, key,
pristine copy or baseline: it uses them and skips their steps. The flags
that shape a new project are then ignored; edit the configuration
instead.

### When the project has several database settings

Several settings are, for example, `.env` and `.env.local`, a compose
service, or a variable of this shell.

- At a terminal, setup lists them, numbered.
- With `--yes` it takes the application's most specific setting (`.env.development.local`, `.env.local`, `.env.development`, `.env`, …), and says which it took.
- A compose service comes first only when the application's setting names that server and database. Its root user (MySQL, MariaDB, when the root password is known), its user (PostgreSQL, a superuser of the image) or `sa` (SQL Server) can then create the pristine copy.
- A SQL Server image names no database: the service takes the database of the application's setting for the same server.
- This is meant for a local, disposable database in a container. On a shared server, use a limited user ([security](../../security.md)).
- A compose service of another database comes last. The others are listed in the configuration.
- Two kinds of settings that name different servers or databases stop `--yes` (exit 2), naming both. Examples are the environment and a `.env`, or `appsettings.json` and a `.env`. Run setup at a terminal to choose, or make the settings agree.
- With no database name, or PostgreSQL's own database `postgres` (a compose service without `POSTGRES_DB`), the block is written commented out, with what to set. `--database` names it.
- A setting only this shell's environment holds (Spring's `SPRING_DATASOURCE_*`) becomes `dsn: { env: DG_DB_DSN }`, with a note of what was found. Setup stops until `DG_DB_DSN` holds the DSN.

### When the database user may not create a database

Before it makes the pristine copy, setup says what an administrator must
grant. The statements are under [state](../../configuration.md#state) for
MySQL, MariaDB and PostgreSQL, and under
[SQL Server](../../configuration.md#sql-server); `sa` needs nothing.

- For SQL Server it asks for the server role `dbcreator` and `db_owner` in the working database. `dbcreator` lets the login take any database of the server offline, restore over it or drop it.
- So it also prints the narrower grant: `CREATE ANY DATABASE` and ownership of the working database. That changes the database's owner, and the login must have no user in it.
- It proposes `VIEW SERVER PERFORMANCE STATE` (`VIEW SERVER STATE` before 2022), which makes fingerprints of unchanged data almost free.
- It says where the SQL Server copy goes: a database on the same server, restored from a `COPY_ONLY` backup, with two backup files in the server's backup folder.

A seed that fails for want of the right repeats what to grant. Another
failure, such as the server not running, does not. The errors that mean
a missing right are:

- MySQL and MariaDB: 1044, 1142, 1227;
- PostgreSQL: 42501;
- SQL Server: 229, 230, 262, 297, 916, 3110, 4060, 4063, 5011, 15247.

### When setup finds an SQLite file

SQLite is the default of Laravel 11 and later, and is used by Django,
Rails, Prisma and ASP.NET ([SQLite](../../configuration.md#sqlite)).

- `working:` is the file, the pristine copy is a file of DriftGate's store, and nothing is granted.
- As for every driver, setup proposes the framework's own tables for `exclude` and `restore_empty` ([which](../../configuration.md#the-frameworks-own-tables)). The default is yes, and `--yes` says yes.
- When the file does not exist yet, setup says to run the application's migrations first (exit 2).
- `--driver sqlite` for a setting that names a database server stops setup (exit 2). So does another driver for a setting that names an SQLite file.

In a worktree, the worktree's settings must name a file of its own. Git
does not carry the file.

- When it is missing and the main worktree has a pristine copy (its seed), setup offers to make the file a copy of that, without the copy's mark (`--yes`: it does). That is the data the main worktree's baseline was captured from.
- Otherwise it stops and says to copy the main worktree's file, or to run the migrations (exit 2).
- A command that finds the file missing in a worktree says so.

### Setup in a git worktree

This applies to a worktree added with `git worktree add` whose main
worktree has a `baselines: local` project.

- Setup takes that project instead of starting one: the configuration, catalog, allowlist, routes, baselines with their acceptance logs, and the key.
- It captures no baseline: `run` compares the worktree's code with the main worktree's baseline.
- When the worktree's settings name a database of their own, setup offers to make it `working:` and `pristine:` (`--yes`: it does). The address they give becomes `base_url`; comments are kept.
- Settings that name the main worktree's database, or none, stop setup before it takes anything (exit 2). So does a main worktree without a baseline.

What else a worktree takes, and why, is under
[baselines](../../configuration.md#baselines).

### Setup in a project that init made

- The `state:` block that init wrote commented out is turned on in place, comments kept, after asking (`--yes` says yes). Refused, setup stops (exit 2).
- When the project's settings name a database and the configuration has no `state:` at all, setup stops before routes and captures (exit 2): they would run without restoring the database. Add `state:`, or capture without one with `driftgate run`.

### When setup stops

Setup stops with an exact instruction and the command that goes on from
there:

- an address or driver it cannot tell (exit 2);
- a DSN that cannot be read (exit 2, as in `run`; a 1Password reference: exit 3);
- the pristine copy refused (exit 2);
- an application that does not answer at `base_url` (exit 3, with how to start it);
- a stop signal (Ctrl-C) during the seed or the capture (exit 130 or 143: "interrupted");
- a question when standard input is not a terminal (a pipe, a CI job) and `--yes` is not given (exit 2, with the question and what to pass).

Addresses:

- A port a compose service publishes comes before the address the application gives itself (`APP_URL`). At a terminal it asks.
- Without an address in the settings, it offers the framework's default at a terminal, never unasked. With `--yes` it stops: pass `--url`.
- The same holds for each service of `targets:` (a service in a subfolder, several services): a port from a compose file stays, and a framework's default port is asked.
- `--url` is the address of a single service and is refused with several. Set each `targets.<name>.base_url` after `driftgate init`.

A value with a control character (a line break in a database name) is
left out, with a note naming the file. Names are quoted in
`driftgate.yaml`.

## driftgate key create

```
driftgate key create
```

Creates the project key (32 random bytes, base64).

- With `baselines: local`: in `.driftgate/key`, open to its owner only (mode 600; not checked on Windows). Only once git ignores `.driftgate/`.
- With `baselines: git`: in the macOS Keychain (service `driftgate`, account the project name). On Linux and Windows, set `DRIFTGATE_KEY` or `key:` instead ([key](../../configuration.md#key)).
- It refuses to replace an existing key: private bodies encrypted with it would become unreadable.

```sh
driftgate key create
```

## driftgate baselines

```
driftgate baselines local|git
```

Moves the project to the other [layout](../../configuration.md#baselines).
For `driftgate baselines rebase`, see
[review](review.md#driftgate-baselines-rebase).

It moves the configuration, the catalog, allowlist, routes, exclusions,
classify rules, the baselines with their acceptance logs and manifests,
and the local store.

- The configuration keeps its comments. For `local` it gets `baselines: local`; for `git` the key is taken out, so an earlier release can still read the file.
- Other files in the folder, such as a tokens file, stay where they are; the command names them.
- The stores stay locked until the old place is gone. A command that was waiting for the lock then stops (exit 2: run it again) instead of writing into the old place.
- With `DRIFTGATE_STORE` or `store:`, the store stays where it is. If the project's own store (`.driftgate/store`, or `~/.driftgate/<project>`) holds data then too, the switch stops: that data would be left behind. It says to move it, or to unset the variable.
- Nothing in the new place is removed or replaced, as the store of `baselines: git` is shared by the checkouts of the project. Baselines and captures are added by name, private bodies and pristine records by file.
- One of the same name with other content stops the switch before anything is written (exit 2), naming it.
- Only the last comparison of this checkout is taken over.
- A folder with both configurations (a branch from before a switch) is refused, with the commands that settle it.

```sh
driftgate baselines git
```

### Switch to the git layout

- Everything that goes into the repository first passes the checks of [`verify`](review.md#driftgate-verify): the manifest, private bodies, and personal data and credentials in public bodies and headers. A finding stops the switch before anything is written (exit 2).
- `.driftgate/key` is never copied into the repository. `baselines: git` reads the key from `key:`, `DRIFTGATE_KEY` or the macOS Keychain.
- When none of them has the key, the switch stops (exit 3) with the command that puts it there. When one has another key, it stops with exit 2.
- The store moves to `~/.driftgate/<project>` (or `store:`), and `.driftgate/` is removed.
- Commit `driftgate.yaml` and `driftgate/` afterwards.

### Switch to the local layout

- `.driftgate/` gets the files and the store. `.gitignore` and `.dockerignore` get the entry, as `init` writes it.
- `.driftgate/key` gets the key that `baselines: git` found, unless `key:` names its source.
- `driftgate.yaml` and `driftgate/` are removed from the working tree; commit that.
- The store in `~/.driftgate/<project>` stays, as other checkouts of the project may use it. The command prints how to remove it.

### A switch that stopped halfway

The switch writes the new place fully, then the new configuration, then
removes the old place, with a mark in `.driftgate/switching`. Stopped at
any point, every other command refuses to run (exit 2) until
`driftgate baselines <mode>` with the same mode finishes it.

## driftgate version

```
driftgate version
```

Prints the version.

```console
$ driftgate version
driftgate v1.0.2 (commit …, built …)
```

---
**Related:** [Command line basics](../cli.md) · [driftgate.yaml](../../configuration.md#driftgateyaml) · [Security model](../../security.md)
