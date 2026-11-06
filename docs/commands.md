# Commands

Every command and flag of `driftgate`. The descriptions are those of
`driftgate help <command>` (the source is `internal/cli/help.go`); this page
adds the exit codes, notes and an example for each command.

```
driftgate help                 # synopsis of every command
driftgate help <command>       # one command with its flags (also: <command> --help)
driftgate help catalog         # the fields of catalog.json, and where to read more
driftgate help allowlist       # the rules of allowlist.yaml, then allowlist check
```

An unknown command or help topic names the table of
[what to use for what](configuration.md#what-to-use-for-what).

Contents: [common flags](#common-flags) · [exit codes](#exit-codes) ·
[init](#init) · [setup](#setup) · [seed](#seed) · [run](#run) · [discover](#discover) ·
[generate](#generate) · [coverage](#coverage) · [har import](#har-import) ·
[record](#record) · [capture](#capture) · [compare](#compare) · [report](#report) ·
[allowlist check](#allowlist-check) · [review](#review) · [accept](#accept) · [expect](#expect) · [verify](#verify) ·
[bisect](#bisect) · [data classify](#data-classify) ·
[data anonymize](#data-anonymize) · [data subset](#data-subset) ·
[data verify](#data-verify) ·
[baselines](#baselines) · [baselines rebase](#baselines-rebase) · [key create](#key-create) · [version](#version) ·
[environment variables](#environment-variables)

Paths are those of a new project (`baselines: local`): everything in
`.driftgate/`. With `baselines: git` the same files are in `driftgate/`,
and `driftgate.yaml` is at the project root
([baselines](configuration.md#baselines)).

## Common flags

| Flag | Meaning |
|---|---|
| `--config <path>` | configuration file; every command except `version` takes it. Default: `.driftgate/driftgate.yaml` or `driftgate.yaml` in this folder or the nearest folder above it, up to the top of the git work tree ([baselines](configuration.md#baselines)) |
| `--target <service>` | with several services (`targets:` in `driftgate.yaml`): work on the named services only (repeatable or comma-separated). Without it every command works on all services. `run` reports a check limited this way as partial. |

Flags go before or after the positional arguments: `driftgate compare
before --only 'get-*'` works.

Baselines have names, and the default name is `local`: `setup` and the
first `run` make the baseline `local`, and `run`, `capture`, `compare`,
`allowlist check` and `bisect` (before there is a comparison) use it
unless told otherwise. A baseline
with another name (`run --baseline release`) has to be named in every
command that uses it: `--baseline <name>`, or `--against <name>` for
`compare`. `accept` and `review` work on the last comparison and its
baseline; `verify` checks every baseline unless `--baseline` names one.

## Interrupts and the local store

Ctrl-C or SIGTERM (a cancelled CI job, `docker stop`) stops a long-running
command (`run`, `capture`, `seed`, `bisect`, `discover`, `generate`,
`data …`, `record`) cleanly: a capture that was replacing another puts the
previous one back, `bisect` removes its worktree and runs `bisect.finish`,
and the project's commands still running are stopped with everything they
started (each runs in a process group of its own; a command that reads
from the terminal, such as a `sudo` password prompt, cannot run there). A
second signal ends DriftGate at once (exit 130 or 143). A capture killed
outright (SIGKILL, out of memory) leaves the previous capture of its tag
aside; the next capture of the tag puts it back first. An `accept` or a
first `run` killed while it writes a baseline works on a copy next to it
(`.driftgate/baselines/.tmp-*`): the next `accept` or `run` removes the
copy, the baseline is as it was. Killed in the moment the copy replaces
the baseline, the old baseline is whole in `.tmp-old-*/baseline`; every
command stops (exit 3) until it is put back (or restored from git).

The commands that write the local store (`run`, `capture`, `seed`,
`accept`, `expect`, `baselines rebase`, `bisect`) take a lock on it
(`<store>/.lock`). With `baselines: git` the worktrees of one project
share the store (with `baselines: local` each has its own), so a second
such command waits, saying which process it waits for, until the first one
is done; Ctrl-C stops the wait. The lock goes with the process, also when
it is killed. With the lock, a command removes the temporary files killed
processes left in the store (those older than a few seconds before it
started), before it ends.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | clean |
| `1` | drift: responses and side effects (other than [expected](#expect) ones), changed routes (`discover`), uncovered routes (`coverage`, also routes whose requests failed in the baseline), a stale catalog (`generate --check`), findings of `allowlist check`, a baseline that disagrees with its manifest or holds personal data or credentials in git (`verify`), personal data in an output (`data verify`), columns without a rule (`data classify` without `--write`) |
| `2` | configuration error: the configuration, catalog, allowlist or baseline is not usable, a usage error, a missing `--reason`, a public body with personal data |
| `3` | environment error or an incomplete result: the application or the database cannot be reached, a file or the local store cannot be read or written (a full disk, permissions), the state fingerprint does not match the baseline, a guard failed, no encryption key, a baseline changed outside `accept`, an interrupted capture, private requests whose body is not on this machine (without `--skip-private`), or a comparison that compared nothing (`--only` matched no request, the baseline has none of the requests). The result cannot be trusted; it is not a regression. |

`report` exits `0` also after drift: it only prints the last comparison, and
`compare` and `run` are the gate. With `--exit-code` it exits with the
verdict of that comparison (see [report](#report)).

---

## init

```
driftgate init [--project <name>] [--baselines local|git] [--service <name>]
```

Writes `driftgate.yaml`, a catalog and an allowlist for the project in this
folder, from the frameworks it finds.

| Flag | Meaning |
|---|---|
| `--project <name>` | project name (default: the folder name) |
| `--baselines local\|git` | `local` (default): everything in `.driftgate/`, which it adds to `.gitignore` (and `.dockerignore`, see [baselines](configuration.md#baselines)); `git`: `driftgate.yaml` and `driftgate/` in the repository, the store in `~/.driftgate/<project>`. `--config <path>` decides it too: a path in `.driftgate/` is `local`, any other `git` |
| `--service <name>` | write only this service (repeatable or comma-separated) |

- Recognises Laravel, Symfony, Rails, Django, Spring Boot, Quarkus,
  Micronaut, ASP.NET, FastAPI, NestJS, Magento 2, Next.js and Nuxt, and
  writes the matching `discover:` adapters, framework routes as exclusions
  with reasons (`.driftgate/exclusions.yaml`), and allowlist entries for
  values the framework changes on every response. The `Date` header is
  always allowed. A framework DriftGate has not been checked on is marked
  as such.
- In a monorepo it finds the services in the root, its subfolders,
  `apps/*`, `services/*` and `packages/*`, and reads their ports from
  `docker-compose.yml` (see [several services](guides/multi-service.md)).
- Without a known framework it writes a commented configuration and says to
  add routes under `discover:` or write the catalog by hand.
- The database and the address come from the project's own settings
  (`.env`, `appsettings.json`, `application.properties`, `database.yml`,
  compose files and the others listed under
  [from the project's own settings](configuration.md#state)): `state: db`
  names the setting (`dsn: { dotenv: .env }`), never its value, and
  `base_url` is the address found. `state:` is written **commented out**:
  a `run` would otherwise create the pristine database unasked.
  `driftgate setup` turns it on after asking, or uncomment it. With
  several settings it takes the one `setup --yes` would and lists the
  others above `state:`. It also writes the tables the framework keeps
  its own state in (sessions, cache, jobs; Hangfire, Solid) as
  `exclude:` and `restore_empty:` ([the framework's own
  tables](configuration.md#the-frameworks-own-tables)), for every
  driver.

```sh
driftgate init --project shop
```

## setup

```
driftgate setup [--yes] [--project <name>] [--baselines local|git]
    [--driver mysql|postgres|sqlite|sqlserver] [--database <name>] [--url <address>]
```

Sets up a new project from zero to its first baseline, in this order:

1. recognises the framework, as `init` does;
2. reads the database and the address from the project's own settings;
3. writes `.driftgate/` (or, with `--baselines git`, `driftgate.yaml` and
   `driftgate/`) with its `.gitignore` and `.dockerignore` entries;
4. makes the key: `.driftgate/key` (with `--baselines git` the macOS
   Keychain), unless one is set up already (`DRIFTGATE_KEY`, `key:`);
5. makes the pristine copy of the database (`seed --skip-commands`). This
   creates a database, so it asks first (default no);
6. checks that the application answers, then runs `run`: `discover` and
   `generate` write the routes and the catalog, two captures make the
   baseline `local`;
7. prints a summary: the framework, the database (driver, database, host,
   port, user and where it was found; never the password), that the data
   as it was became the pristine copy (or that the one made before is
   kept), the mode (`freeze`: every drift fails and `accept` is refused;
   `evolve`: a drift may be accepted with a reason), the address, the
   routes covered, and what to check by hand.

| Flag | Meaning |
|---|---|
| `--yes` | ask nothing: take the best database setting and make the pristine copy. Where a value would have to be guessed, it stops and says what to pass |
| `--project <name>` | project name (default: the folder name) |
| `--baselines local\|git` | as for `init`; default `local` |
| `--driver mysql\|postgres\|sqlite\|sqlserver` | the database server, when the settings do not say |
| `--database <name>` | the database the application uses (SQLite: its file, relative to the project root), when the settings do not say |
| `--url <address>` | where the application answers, when the settings do not say it or say it wrongly |

- **Several database settings** (`.env` and `.env.local`, a compose
  service, a variable of this shell): at a terminal it lists them,
  numbered; with `--yes` it takes the application's most specific setting
  (`.env.development.local`, `.env.local`, `.env.development`, `.env`, …),
  and says which it took. A compose service comes first only when the
  application's setting names that server and database: its root user
  (MySQL, MariaDB, when the root password is known), its user
  (PostgreSQL, a superuser of the image) or `sa` (SQL Server) can create
  the pristine copy. A SQL Server image names no database: the service
  takes the database of the application's setting for the same server.
  That is meant for a local, disposable database in a container; on a
  shared server use a limited user ([security](security.md)). A compose
  service of another database comes last. The others are listed in the
  configuration.
- **Settings that disagree**: two kinds of settings (the environment and
  a `.env`, `appsettings.json` and a `.env`) that name different servers
  or databases stop `--yes` (exit 2), naming both; run it at a terminal to
  choose, or make the settings agree.
- **No database name**, or PostgreSQL's own database `postgres` (a compose
  service without `POSTGRES_DB`): the block is written commented out, with
  what to set; `--database` names it.
- **An SQLite file** (Laravel 11 and later by default, Django, Rails,
  Prisma, ASP.NET; see [SQLite](configuration.md#sqlite)): `working:` is
  the file, the pristine copy a file of DriftGate's store, and nothing is
  granted. As for every driver, setup proposes the framework's own tables
  for `exclude` and `restore_empty` ([which](configuration.md#the-frameworks-own-tables);
  default yes, `--yes` says yes), and
  says to run the application's migrations first when the file does not
  exist yet (exit 2). `--driver sqlite` for a setting that names a
  database server, or another driver for one that names an SQLite file,
  stops it (exit 2). In a worktree, the worktree's settings must name a
  file of its own. Git does not carry it: when it is not there and the
  main worktree has a pristine copy of the database (its seed), setup
  offers to make the file a copy of that, the data the main worktree's
  baseline was captured from, without the copy's mark (`--yes`: does);
  else it stops and says to copy the main worktree's file or run the
  migrations (exit 2). A command that finds the file missing in a
  worktree says so.
- **A user that may not create a database.** Before the pristine copy it
  says what to grant, as an administrator: `GRANT ALL PRIVILEGES ON
  \`shop_pristine\`.* TO 'shop'@'%';` (MySQL, MariaDB), `ALTER ROLE
  "shop" CREATEDB;` (PostgreSQL), or for SQL Server the server role
  `dbcreator` and `db_owner` in the working database (the statements are
  under [SQL Server](configuration.md#sql-server); nothing for `sa`).
  `dbcreator` lets the login take any database of the server offline,
  restore over it or drop it, so setup also prints the narrower grant,
  `CREATE ANY DATABASE` and ownership of the working database, which
  changes that database's owner (the login must have no user in it). For
  SQL Server it also proposes `VIEW SERVER PERFORMANCE STATE` (`VIEW
  SERVER STATE` before 2022), which makes fingerprints of unchanged data
  almost free, and says where the copy goes: a database on the same
  server, restored from a `COPY_ONLY` backup, with two backup files in
  the server's backup folder. A seed that fails for want of the right
  (MySQL/MariaDB errors 1044, 1142, 1227; PostgreSQL 42501; SQL Server
  229, 230, 262, 297, 916, 3110, 4060, 4063, 5011, 15247) repeats it;
  another failure (the server not running) does not.
- **A setting only this shell's environment holds** (Spring's
  `SPRING_DATASOURCE_*`): `dsn: { env: DG_DB_DSN }`, with the note of what
  was found; setup stops until `DG_DB_DSN` holds the DSN.
- **Several addresses**: a port a compose service publishes comes before
  the address the application gives itself (`APP_URL`); at a terminal it
  asks. Without an address in the settings it offers the framework's
  default at a terminal, never takes it unasked; with `--yes` it stops:
  pass `--url`. The same holds for each service of `targets:` (a service
  in a subfolder, several services): a port from a compose file stays, a
  framework's default port is asked. `--url` is the address of a single
  service and is refused with several (set each `targets.<name>.base_url`
  after `driftgate init`).
- **What answers**: any HTTP response counts as the application
  answering; setup prints the status and the `Server` header and the
  summary repeats them, so a different server on that port shows.
- **Values with control characters** (a line break in a database name)
  leave the setting out, with a note naming the file; names are quoted in
  `driftgate.yaml`.
- **Questions without a terminal.** When standard input is not a terminal
  (a pipe, a CI job) and `--yes` is not given, a question stops setup with
  the question and what to pass (exit 2).
- **It stops with an exact instruction** and the command that goes on from
  there: an address or driver it cannot tell (exit 2), a DSN that cannot
  be read (exit 2, as in `run`; a 1Password reference exit 3), the
  pristine copy refused (exit 2), an application that does not answer at
  `base_url` (exit 3, with how to start it), a stop signal (Ctrl-C) during
  the seed or the capture (exit 130 or 143: "interrupted").
- **In a worktree** added with `git worktree add`, whose main worktree
  has a `baselines: local` project: setup takes that project
  (configuration, catalog, allowlist, routes, baselines with their
  acceptance logs, the key) instead of starting one, and captures no
  baseline: `run` compares the worktree's code with the main worktree's.
  When the worktree's settings name a database of their own, setup
  offers (`--yes`: does) to make it `working:` and `pristine:`, and the
  address they give `base_url`, comments kept; settings that name the main worktree's database, or none, stop it
  before it takes anything (exit 2), as does a main worktree without a
  baseline. `init` in such a worktree points to setup (exit 2).
- **A project `driftgate init` made.** The `state:` block init wrote
  commented out is turned on in place, its comments kept, after asking
  (`--yes` says yes); refused, setup stops (exit 2). When the project's
  settings name a database and the configuration has no `state:` at all,
  setup stops before routes and captures (exit 2): they would run without
  restoring the database. Add `state:`, or use `driftgate run` to capture
  without one.
- **A second run goes on where the first stopped.** Apart from turning on
  that block, it never changes an existing configuration, key, pristine
  copy or baseline: it uses them and skips their steps. The flags that shape a new project are then ignored
  (edit the configuration instead).
- Exit `0` once the baseline is there, also when routes are left uncovered
  or requests meant to succeed failed in it (the summary lists them; `run`
  and `coverage` then fail on the routes); `1`
  when the two captures differ and no baseline was made (the proposed
  allowlist entries are printed).

```sh
cd shop && driftgate setup
```

## seed

```
driftgate seed [--skip-commands]
```

Runs the `seed:` commands and keeps a pristine copy of every state database.

| Flag | Meaning |
|---|---|
| `--skip-commands` | take the data already loaded as pristine, without the `seed:` commands (a database cache in CI) |

`run` seeds by itself when a pristine copy is missing; run `seed` by hand
after changing the seed data, `restore_only` or `restore_empty`. Run it while the application does not write
(no queue workers, schedulers or requests): MySQL/MariaDB copy the tables
one by one, so a write during the copy can leave a pristine copy that
mixes data from before and after it (a PostgreSQL, SQLite or SQL Server
copy is of one moment). `seed` records the fingerprint of the pristine
copy itself and warns when the working database differs from it right
after the copy (the application wrote during the seed or just after it).
Every capture restores the databases to the pristine copy and checks them
against its fingerprint. The fingerprint is kept in the local store per database server (from the DSN, without its
credentials) and databases, so checkouts of the project (git worktrees)
that share the store but use their own database server never take each
other's; `run` seeds when this server has none yet. A SQL Server pristine
copy is a read-only database on the same server, made from a `COPY_ONLY`
backup of the working database, with a backup of its own in the server's
backup folder ([SQL Server](configuration.md#sql-server)). An SQLite database is
named by the real path of its working file, and its pristine copy is a
file next to that record unless `pristine:` names one
([SQLite](configuration.md#sqlite)); the seed only reads the working
file.

```sh
driftgate seed
```

## run

```
driftgate run [--baseline <name>] [--allow-mutating-without-restore]
    [--skip-private] [--read-only] [--target <service>]
```

Routes, catalog, capture and compare in one command; the first run makes the
baseline from two equal captures.

| Flag | Meaning |
|---|---|
| `--baseline <name>` | baseline to make or compare with (default `local`) |
| `--allow-mutating-without-restore` | send requests that change data even without a state restore (not uploads of real files) |
| `--skip-private` | accept that private bodies not on this machine are not compared (their status, headers and side effects are); without it the result is incomplete (exit 3) |
| `--read-only` | capture only the requests that do not change data, as `capture --read-only` |
| `--target <service>` | work on this service only (repeatable or comma-separated); the check is then reported as partial |

- Steps: `discover` (routes), `generate` (catalog), `coverage`, `seed` when
  needed, `capture`, `compare`. The first run writes the route inventory and
  the catalog it finds. Later runs do not: a changed inventory or catalog is
  reported as drift (adopt it with `discover --write` or `generate --write`),
  and `run` still captures with the existing catalog and compares, so one run
  gives the whole picture.
- Without the baseline, `run` captures twice on the same state and makes the
  baseline only when both captures agree. Otherwise it proposes allowlist
  entries with a format; you add the reason. The second capture runs its
  `prepare:` commands and requests at least a second after the first one
  ended (it waits for what its restore did not take): a timestamp the
  application writes, such as `updated_at`, then differs between the two
  and is found, also when both are shorter than a second. A baseline
  response that came only after a [retry](configuration.md#retries) is
  named in a warning and keeps the mark in its record.
- A baseline it makes is checked as [coverage](#coverage) checks one: requests
  that failed in it are listed, and routes they leave uncovered make the run
  exit 1 (the baseline is kept; a baseline that cannot be read for the check
  is a warning). Later runs check the baseline in their coverage step; a
  baseline that cannot be read for it is a warning there, and the catalog
  alone decides coverage.
- Its capture is named `run-<id>`, where `<id>` is fixed for the folder of
  the project: with `baselines: git` checkouts of the project (git
  worktrees) share the local store but never each other's capture, as each
  keeps its own last comparison. With `baselines: local` each worktree has
  a store of its own.
- An empty catalog is refused. A catalog with requests that change data
  needs a state with a restore (`state:`), or
  `--allow-mutating-without-restore`.
- `run` needs `.driftgate/catalog.json` to exist (`init` writes an empty one).
- When no request of the first capture gets an answer, `run` stops there
  (exit 3) and says how to start the application (its compose file, or
  the framework's own command, such as `php artisan serve`).

```sh
driftgate run
driftgate run --target api            # one service only; reported as partial
driftgate run --skip-private          # CI: private bodies are not on the runner
```

## discover

```
driftgate discover [--write] [--target <service>]
```

Finds the routes of the application and compares them with the route
inventory (exit 1 when they changed).

| Flag | Meaning |
|---|---|
| `--write` | write the routes found as the new inventory (`.driftgate/routes.json`) |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

Routes come from the adapters under `discover:` in `driftgate.yaml`
([configuration](configuration.md#discover)).

```
$ driftgate discover
3 routes (command 3)
routes unchanged ✓
```

## generate

```
driftgate generate [--write] [--check] [--all] [--target <service>]
```

Generates catalog requests for routes the hand-written and imported ones do
not cover.

| Flag | Meaning |
|---|---|
| `--write` | write the generated catalog (not with `--check`) |
| `--check` | exit 1 when the catalog is not what generate would write (not with `--write`) |
| `--all` | generate requests for every route, also the covered ones |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- One request per route method and template (`templates:`; default `ok`).
  Entries written by hand or imported stay untouched; a route method a
  hand-written entry already reaches is skipped unless `--all`.
- Parameter values come from `params:`. For routes it cannot make, it names
  every missing parameter, grouped by reason, with an example.
- The same input always gives a byte-identical catalog.
- The summary says how many generated requests `json-errors` made. A
  project whose `discover:` uses the `laravel`, `symfony`, `rails` or
  `spring` adapter and whose `templates:` lack `json-errors` gets a one-line
  `note:` to add it (JSON variants of the error variants, see
  [templates](configuration.md#templates-generate-params)), once per local
  store (`generate` or `run`; `generate --check` without a store says
  nothing).

```sh
driftgate generate --check     # CI: is the committed catalog current?
```

## coverage

```
driftgate coverage [--format text|json] [--baseline <name>] [--target <service>]
```

Routes of the inventory without a catalog request that succeeds (exit 1
when there are any).

| Flag | Meaning |
|---|---|
| `--format text\|json` | output format (default text) |
| `--baseline <name>` | baseline whose answers decide which requests succeed (default `local`, checked when it exists; a baseline named must exist, exit 2; `none`: the catalog alone) |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

Each method of a route counts separately (`PUT` and `PATCH` of one route
count as one). A method is covered only by a request that should succeed;
error variants (`not-found`, `invalid-param` …) do not cover it. Exclude a
route on purpose in `.driftgate/exclusions.yaml`, with a reason. It also
reports requests that reach no route, stale exclusions and routes without a
sample body. An exclusion `init` wrote for the framework (Debugbar,
Telescope, Horizon …) is not reported stale while it keeps `init`'s
route and reason: the project need not have that part. The JSON output
(`stale_exclusions`) leaves it out too.

With a baseline, a request covers its route only when it did not fail
there, or when the baseline has no answer for it yet. A comparison checks
that answers stay equal, so an error already in the baseline (an endpoint
that answers 500) would otherwise pass on every run. A request fails when:

- it got no answer (a timeout, a connection error, repeated sends cut short);
- it answered a server error (5xx), or one of its repeated sends (`repeat`)
  did;
- it is meant to succeed and answered a client error (4xx) or a redirect to
  a sign-in page (a location whose last path segment is `login`, `signin`,
  `sign-in`, `sign_in`, `log-in` or `logon`). Meant to succeed are the
  requests the `ok` and `json` templates generate, except those for a route
  that takes a body without a sample (they are listed as routes without a
  sample body).

Error variants and command steps never fail here. A request written by hand
or imported may mean a client error (a wrong password, a rate limit): its
4xx covers the route, which is then listed as reached only by requests that
answered an error (never a failure). A validation error that Laravel answers
with a redirect back (to `/` for a request without a `Referer`) cannot be
told from a redirect the route means; the `json` template sends the same
request as an API client, which gets the 422.

The lists:

- routes whose requests meant to succeed all failed in the baseline, with
  those requests and their statuses: not covered (exit 1). Fix the route
  and `accept` the new answer, or acknowledge it as a known failure;
- requests that failed in the baseline, also on routes another request
  covers (a warning);
- known failures (`known_failures:` in
  [exclusions.yaml](configuration.md#exclusionsyaml)): the request stays in
  the catalog and is compared as usual; its route is neither covered nor
  failed (`… with known failures` in the summary). A known failure whose
  requests succeed now, or that matches no request of any service, is
  listed as stale; one whose `status` no longer is the failure leaves the
  request failed, with a note; one that acknowledges failures on more than
  5 routes is listed as broad.

`coverage` reads the answers from the baseline's folder; it needs neither
the local store nor the key. JSON: `baseline_checked`, `failed_in_baseline`
(`route`, `requests` with `id`, `status` and `detail`),
`requests_failed_in_baseline` (`known_status` when a known failure no
longer matches), `known_failures` (with `reason` and `ref`),
`known_failure_routes`, `stale_known_failures`, `broad_known_failures` and
`only_error_answers`.

```
$ driftgate coverage
3 routes: 3 covered, 0 excluded, 0 uncovered

every route is covered or excluded ✓
```

## har import

```
driftgate har import <file.har> [--target <service>] [--write]
```

Adds the requests of a HAR file (recorded in a browser) to the catalog.

| Flag | Meaning |
|---|---|
| `--target <service>` | service the requests belong to (once; needed with several services) |
| `--write` | write the catalog; without it the requests are only listed |

Only requests to the application are taken, without static files, and never
cookies or tokens. Uploaded files are replaced by synthetic files of the
same type. With a route inventory, `/users/1` and `/users/2` become one
request `/users/{id}` with `params: {id: "1"}`; a token that matches a
configured identity gives the request that identity. `har import` and
`record` share this pipeline: the same traffic gives the same entries.

From that shared pipeline: duplicates are dropped by method, route, query
parameter names, identity and body shape (not only identical requests); with `identities:` or `tokens:`
configured, `har import` runs the token command or `op read` to recognise
identities (when that fails, identities stay unrecognised and it says so);
an entry with an identity is private, as is one whose response, any of
them, held personal data or is not in the file whole.

A request that carries a credential or personal data is left out and listed
with the reason, to be added by hand. A credential is found by name (a
query parameter, JSON key or form field named like `password`,
`currentPassword`, `authToken`, `api_key`, `jwt`, `ticket`, `nonce`,
`csrf`, `otp`, `hash`, or `code` alone or with a word like `reset` or
`verification`) or by shape (a JWT, or a run of 20 or more characters that
looks random: hex from 32 characters, or a mix of upper case, lower case
and digits), in the query, the body, the form fields and the path. A
UUID, a 24-character hex ID and a camelCase name with a number at the end
are not taken for credentials. The shape check is a heuristic: it misses
a short or unusual token, so review what `--write` adds.

```sh
driftgate har import ~/Downloads/admin-session.har            # list
driftgate har import ~/Downloads/admin-session.har --write    # add to the catalog
```

## record

```
driftgate record [--listen <addr>] [--target <service>] [--write]
    [--include <glob>] [--exclude <glob>] [--allow-non-loopback]
```

A recording proxy on 127.0.0.1: use the application through it (a browser,
a frontend, an E2E suite) and it proposes catalog requests for what it saw;
responses are never kept.

| Flag | Meaning |
|---|---|
| `--listen <addr>` | address to listen on (default `127.0.0.1:8088`; loopback only) |
| `--target <service>` | service to forward to (once; needed with several services) |
| `--write` | on stop, add the new requests to the catalog; without it they are only listed |
| `--include <glob>` | record only paths that match (repeatable or comma-separated) |
| `--exclude <glob>` | do not record paths that match, besides static files and dev-server noise (repeatable or comma-separated) |
| `--allow-non-loopback` | allow a `--listen` address other than loopback (anyone who reaches it acts as you) |

Every request goes to the service's `target.base_url` (http or https), which
must be in `allow_hosts`. The proxy itself is plain HTTP: point the browser,
the frontend's API URL or the E2E suite's base URL at `http://127.0.0.1:8088`.
Requests go through the same pipeline as `har import`; static files, the
noise of development servers, event streams and websockets pass through
unrecorded. It answers only for its own address (`Host` `127.0.0.1`,
`[::1]` or `localhost` with its port) and refuses requests from other sites;
see [the guide](guides/record.md#https-and-security). It runs until Ctrl-C
(or SIGTERM), then lists the new entries with the routes they reach and
which of them were uncovered before.
Recording only proposes requests: `capture` records their responses on a
restored state. Exit `0`; `2` for a non-loopback `--listen` without the flag
or a target outside `allow_hosts`. See [the guide](guides/record.md).

```sh
driftgate record                                  # list what the session adds
driftgate record --write --exclude '/admin/*'     # add it to the catalog on Ctrl-C
```

## capture

```
driftgate capture <tag> [--only <glob>] [--force] [--restore once|each]
    [--repeat N] [--baseline <name>] [--allow-remote]
    [--allow-remote-mutations] [--allow-mutating-without-restore]
    [--skip-fingerprint-check] [--calibrate] [--read-only] [--quiet]
    [--target <service>]
```

Restores the state, sends the catalog and stores the responses under a tag.

| Flag | Meaning |
|---|---|
| `--only <glob>` | only requests whose ID matches |
| `--force` | replace an existing capture with this tag |
| `--restore once\|each` | `once` (default): restore before the read-only requests and once more before the requests that change data, which then run one after another, each on the data the ones before it left (a sequence with `restore: before` and a repeated request get their own restore); `each`: restore before every sequence and every request that changes data |
| `--repeat N` | capture N times on the same state and propose allowlist entries for values that differ (default 1); each run writes at least a second after the one before ended, as `run` does; the capture keeps the median response time of each request |
| `--baseline <name>` | the state must match the one this baseline was captured from (default `local`) |
| `--allow-remote` | allow a target outside `allow_hosts` |
| `--allow-remote-mutations` | with `--allow-remote`, also send requests that change data or are repeated to it |
| `--allow-mutating-without-restore` | send requests that change data even without a state restore (not uploads of real files) |
| `--skip-fingerprint-check` | do not check the state fingerprint (diagnostics only; the capture is marked unreliable) |
| `--calibrate` | decide `concurrency: auto`: measure, compare a capture at the chosen setting with a sequential one, record it in the baseline (implies `--repeat 2`; not with `--only`) |
| `--read-only` | send only the requests that do not change data; see [below](#read-only-check-of-a-live-server) |
| `--quiet` | no progress output |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- Captures live in the local store (`.driftgate/store/captures/<tag>`; with
  `baselines: git`, `~/.driftgate/<project>/captures/<tag>`), never in the
  repository. A tag is a simple name (letters, digits, `. _ -`; not ending with `.` and not a device name Windows reserves, such as `con` or `nul`).
- Read-only requests run first (in parallel up to `concurrency`); requests
  that change data, sequences, command steps and repeated requests run after
  them, one at a time, after a restore (see `--restore`).
- A read-only `GET`, `HEAD` or `OPTIONS` request whose send times out,
  loses its connection or gets a `502`, `503` or `504` is sent again, once
  by default, after the other read-only requests and one at a time
  ([`retry`](configuration.md#retries)); its record keeps the first send's
  outcome, and the capture lists such requests after its summary. A
  capture at a measured `concurrency: auto` that needed a retry the
  sequential capture did not counts as different from it.
- A service with `concurrency: auto` captures with the setting its baseline
  recorded, and one request at a time while none is recorded. `run` decides
  it when it makes the baseline; `--calibrate` decides it for an existing
  one. After the sequential captures agree, a probe sends up to the first 32
  read-only requests at 1, 2, 4 and 8 at once and takes the smallest
  setting within 10% of the best throughput; a capture at that setting is
  compared with the sequential one of the same restored state. The setting
  is kept only when the two are identical (otherwise it stays 1, naming the
  requests that differed) and is recorded with its evidence in the
  baseline's `meta.json` (commit it with `baselines: git`). When a later
  capture at that setting changes data no single read-only request
  explains, the baseline is put back to 1 and the capture runs again one
  request at a time.
- Requests that change data only because of their method (no `mutates` in
  the catalog, as in a catalog of an earlier release) are listed before the
  capture: a search sent as `POST` among them runs after the writes and sees
  their data. Say `mutates: false` for those that only read, and
  `mutates: true` for the others: for generated entries under
  [`requests`](configuration.md#requests) in `driftgate.yaml`, then
  `driftgate generate --write` (a later `generate` would write the catalog
  entry again); for entries written by hand in the catalog.
- A request that changes data with a fixed id in its path (`PUT
  /api/users/17`, an id neither a `{parameter}` nor from `params_from`) is
  warned of, one line per request, before the capture (and by
  `generate`): on another copy of the data that id may be a real record,
  and the status would not show it. Take the id from a response with
  [`params_from`](configuration.md#templates-generate-params) instead. The exit code stays.
- A read-only request that changed data is recorded with its side
  effects, and the capture prints the `requests:` entries that mark it
  `mutates: true`.
- When the restored state differs from the state the baseline was captured
  from, the capture stops with exit 3 and lists the tables that differ: the
  pristine copy holds other data (a seed since, other seed data or
  migrations). Seed the data the baseline was captured from, or make a new
  baseline of this data: move the baseline's folder away, and the next
  `driftgate run` (`--baseline <name>` for another than `local`) makes
  it; when the data changed on purpose, you can also capture with
  `--skip-fingerprint-check` and make that capture the baseline's content
  with [`driftgate baselines rebase`](#baselines-rebase). When the items that
  differ are tables made `restore_only` since, it says that the
  configuration changed: make the baseline again.
- The [guards](configuration.md#seed-prepare-guards) are checked before
  anything is restored or sent (when requests change data), and again
  right before the first request that changes data: one that fails stops
  the capture at once (exit 3).
- When `prepare` changed the restored state, the capture warns and lists
  what it changed ([seed, prepare,
  guards](configuration.md#seed-prepare-guards)); the exit code stays.
- A public response in which the personal-data detector finds something is
  stored encrypted as private, reported with a masked value, and the capture
  exits 2.

```
$ driftgate capture before --repeat 2
— run 1 of 2 —
restoring api …
200  get-api-news  6ms
...
2 runs on the same state gave the same responses ✓
```

### Read-only check of a live server

`--read-only` sends only the requests that do not change data: `GET`,
`HEAD` and `OPTIONS`, and those marked `mutates: false` (a search sent as
`POST`). It leaves out the requests that change data, command steps (but
those marked `mutates: false`) and every request of a sequence that holds
one of them, as its later requests may need the earlier ones. The capture
says how many it left out and records `read_only` in its `meta.json`; a
comparison of it skips them (`not sent: capture --read-only`) and lists
those that change data under what the result does not cover. A request
missing from it for any other reason still drifts.

With a configuration without `state:` nothing is restored, seeded or
fingerprinted, so this checks a server that must not change (a
production, a new server before the switch) without writing to it:

<!-- check: config-fragment -->
```yaml
version: 1
project: shop
target:
  base_url: https://shop.example
  connect_to: 203.0.113.10        # the new server, whatever DNS says
  allow_hosts: [203.0.113.10]
```

```sh
driftgate capture live-before --read-only
# … the change on the server …
driftgate capture live-after --read-only
driftgate compare live-after --against capture:live-before
```

`allow_hosts` names the server's address, which is outside the local and
private ranges; without it, the captures need `--allow-remote`. Requests
that change data are never sent, so `--allow-remote-mutations` is not
needed. Another developer's traffic on a shared server shows as drift in the data
the responses hold; a side effect is not recorded without `state:`.

## compare

```
driftgate compare <tag> [--against <baseline|tag>]
    [--level status|shape|types|values] [--only <glob>]
    [--format text|json|junit|markdown] [--max-drifts N]
    [--skip-fingerprint-check] [--show-private] [--skip-private]
    [--target <service>]
```

Compares a capture with a baseline or another capture (exit 1 on drift).
`<tag>` `run` is this checkout's capture of `driftgate run` (when no
baseline or capture is named `run`).

| Flag | Meaning |
|---|---|
| `--against <baseline\|tag>` | baseline name or capture tag to compare with (default `local`) |
| `--level status\|shape\|types\|values` | level for this comparison; only stricter than the configured one |
| `--only <glob>` | only requests whose ID matches |
| `--format text\|json\|junit\|markdown` | output format (default text) |
| `--max-drifts N` | list up to N drifts per request, every one (default 20; of a request whose status changed, the change, its location and content type and one line for the rest) |
| `--skip-fingerprint-check` | compare captures made with the same flag |
| `--show-private` | show values from private bodies, on your own machine only (never kept) |
| `--skip-private` | accept that private bodies not on this machine are not compared (their status, headers and side effects are); without it the result is incomplete (exit 3) |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

The comparison is kept as the last comparison, which `report`, `review`,
`accept`, `expect` and `bisect` read.

Against a baseline with [expectations](#expect), a drift an expectation
names exactly is expected, and a request every drift of which is expected
does not fail the comparison. The requests that fail come first, each
with the drifts that are not expected and one line for those that are;
the expected requests follow as one line per ref (text), a collapsed
block after the failing requests that is the first thing left out when
the comment would grow too long (markdown, with an `Expected` column in
the table), or passing test cases with their drifts in `system-out`
(JUnit). Groups all of whose drifts are expected are marked
`(expected)`, and the summary counts `N drifted (M expected)`. JSON has
`expected`, `expected_refs`, the refs per result (`expected`) and per
drift (`expected`, with each drift's `digest`). Expectations whose drifts
the comparison did not have are listed as a warning
(`unmet_expectations`): the request did not drift, drifted otherwise,
drifted in part of what was expected (`partly met`), or is not in the
catalog; in JUnit also in the `system-out` of the request's test case.
The exit code is `1` only while a drift is not expected.

A comparison that compared nothing is incomplete (exit 3), never green: a
typo in `--only`, renamed request IDs or a baseline made for another
catalog would otherwise pass without checking anything. The report says
why. Requests the baseline has no response for are skipped and listed in
every format (`no_baseline` in JSON, a line in text and markdown): a new
request joins the baseline with `accept`. They do not make the result
incomplete while other requests were compared, since every new request
starts without a baseline.

A request the capture or the baseline sent again after a restore ended
the application's database sessions (SQL Server's restore of the whole
database, see [configuration](configuration.md#sql-server)) has a note
with the first answer, and every format counts them in a line of its own,
so the count shows when the notes are cut after ten: `resent` in JSON (the
IDs, and `"resent": true` on each result), a line in text and markdown,
a `resent` property and the line in the suite's `system-out` in JUnit.

Response times are shown as information, never as a gate: the capture
keeps the time of every request (`timings.json`, also in a baseline made by
`run`), and when both sides have them the report gives the 50th and 95th
percentile of each side over the compared requests and lists the requests
that became much slower: at least twice as slow and at least 200 ms slower
(the ratio alone would flag 3 → 7 ms, the difference alone the jitter of
slow requests). JSON: `timings` with `requests`, `baseline` and `current`
(`p50_ms`, `p95_ms`) and `slower` (`id`, `baseline_ms`, `current_ms`, the
most added time first); text and markdown: one line with up to ten slower
requests; JUnit: suite properties (`p50_ms`, `p95_ms`, `slower:<id>`) and
the line in the suite's `system-out`, never a failed test case. Nothing is
shown when a side has no times. The exit code never depends on it: one
measurement per request is too noisy for a gate. A capture with
`--repeat N` (and the two captures `run` makes a baseline from) keeps the
median time of each request over its runs (of an even number of runs the
mean of the two middle times).

A request whose response came only after a retry (its first send failed
under load, see [retries](configuration.md#retries)) and that passed, also
as an [expected](#expect) drift, is never shown as a plain pass: text has
a `RETRY` line with the first outcome and the verdict says
`No drift ✓ (1 passed only after a retry)`; markdown says it in the
headline and in a note; JSON has `retry` on the request and
`passed_after_retry` on the report; JUnit gives the test case a
`system-out` line (an observed or skipped one its `skipped` message) and
the suite's `system-out` the note. A drift that came after a retry says so
among its drifts; one that failed in every send says `failed in all 2
sends` and is never counted as passed after a retry; when it passes
anyway (the baseline failed the same way, or an expected drift), text
has a `RETRY` line (`passed; failed in all 2 sends: …`), the note names
it, and JUnit gives its test case that line. A request that
needed a retry in the baseline and again now is not load, and every
format says so louder (`it needed a retry in the baseline too: not load,
check it`; JSON `retried_in_both`). Requests whose baseline response came
after a retry are listed too (`baseline_retry`, `baseline_retried`). It
counts as passed: the exit code does not change.

Allowlist rules that applied to no change in the comparison are listed
(`unused_rules` in JSON, a warning in text and markdown; never a
failure): a rule whose requests are all gone or renamed (`no request`),
or one whose values did not change this time (`no change`). A rule listed
comparison after comparison is probably stale: the value it names was
renamed or removed. With `--only` or `--target`, rules of the requests
not compared are not listed, and neither is a rule its requests were
not compared far enough for: below `--level values` a header or
value rule never applies, so it is not called stale. A rule `init` wrote for the framework (a
throttle's `x-ratelimit-*`) is not listed, in the text nor in the JSON
(`unused_rules`), while it is as `init` wrote it: the same header or
path, request, format and its options, and reason; one you changed is.

After the verdict, the report says what the result does not cover
(information; the exit code never depends on it):

- one line counting the compared requests by what they do (all of the
  catalog, or `compared (--only)`): `catalog: 622 requests: 572 reads, 50
  mutating, 0 command steps; 0 of them multipart`. A zero is what the gate
  never sends: no `multipart` request, no upload is checked. The line is
  said with gaps, and when the catalog sends no upload or no command step;
- under `Not covered by this result (driftgate coverage lists the
  routes):`, a count and up to five names (`and N more`) for each gap:
  route methods without a request meant to succeed (with a route
  inventory, `routes.json`, and without `--only`); requests meant to
  succeed whose answer in the set compared against is an error, which the
  comparison keeps green (`failed in baseline:local`, or the capture
  compared against); requests observed (measured, not compared);
  requests that change data (and command steps) that were skipped; and
  known failures (`known_failures:` in `exclusions.yaml`). Nothing when
  there is no gap.

`run` uses the coverage its coverage step found; `compare` takes it once
from `routes.json` and the records it read already. JSON: `not_covered`
with `catalog` (`requests`, `reads`, `mutating`, `commands`, and
`multipart` among them), `only`, `against`, `routes_checked`, and
`no_request`, `failed_in_baseline`, `observed`, `mutating_skipped`,
`known_failures` (each `count` and up to ten `names`: request IDs or
routes; left out when empty); markdown: a short list; JUnit: the lines in
the suite's `system-out`.

When one change is in nine of ten requests with an unexpected drift or
more (at least five; expected drifts do not count), a line says so and how
many drift in something else once it is set aside: `one change in 622 of
622 drifted requests: header server changed value (…); set aside, 6 of
622 drift in something else`. Only for a header that names the server
stack (`server`, `x-powered-by`, `via`) whose new value is one value on
every request does it add the rule that allows exactly that value, to
paste under `headers:` in `allowlist.yaml`: `- {name: 'server', format:
'nginx/1\.25', reason: '…'}`. Any other change gets no rule: a change
between builds is fixed, accepted (`driftgate accept group:N`, mode
evolve) or expected (`driftgate expect group:N --ref <ticket>`, mode
freeze); one that changes from run to run is found with `capture
--repeat 2`, which proposes rules for it. Where `accept` and `expect`
refuse the comparison, the line names neither: for a comparison that is
not of a capture against a baseline (two captures, or one baseline
against another) it says to fix the change or name it in the pull request
(`baselines rebase` records a switch of the server stack), and for a
capture made with `--skip-fingerprint-check` to capture again without the
flag first. JSON: `dominant` (`group`, `signature`, `explanation`,
`requests`, `drifted`, `elsewhere`, `rule`, and `captures` or
`unreliable` in those cases).

**What a green result does not prove.** DriftGate compares the responses
of the requests in the catalog, and nothing else. It does not exercise
file uploads unless the catalog has [`multipart`](configuration.md#multipart)
requests with fixtures, nor admin paths that change the schema, nor code
that only background jobs, queue workers or commands reach. The
not-covered summary above and `driftgate coverage` show the gaps;
[command steps](configuration.md#command-steps) with `"output": true` and
state fingerprints bring the rest into the gate
([side checks](guides/side-checks.md)); the table of
[what to use for what](configuration.md#what-to-use-for-what) names the
feature for each.

Groups are numbered by size, the ones the default view leaves out (only
in requests whose status changed) after the ones it lists, so the listed
groups start at [1]; `review --group N` and `accept group:N` use the same
numbers.

```
$ driftgate compare before --only 'get-*'

capture:before against baseline:local: 2 compared, 2 passed, 0 drifted, 0 skipped, 0 observed
No drift ✓
compared (--only): 2 requests: 2 reads, 0 mutating, 0 command steps; 0 of them multipart
```

When a request's status changes, what follows from it (the body, headers
other than `location` and `content-type`) is summarized in one line, and the
groups list the status changes with their requests. The groups that only
follow from a status change are left out (`review` lists them, with the
numbers `accept group:N` takes). `--max-drifts N` lists every drift as usual,
and the JSON report always has them all, with `status_change` and
`collapsed` (how many drifts the summary line stands for) per request and
`status_changes` for the report.

```
DRIFT  home
         status 200 → 302, redirects to /login
         header-extra    header:location   → ["/login"]
         body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
DRIFT  api-news
         status 200 → 500
         header-value    header:content-type  ["application/json"] → ["application/problem+json"]
         body differs: 5 drifts, mostly json; 1 other header differs (x-total); listed with --max-drifts N and in the JSON report

Groups:
  [1] 4 drifts in 4 requests: status changed (for example 200 → 302)
      3 requests: 200 → 302 to /login (home, about, contact)
      1 request: 200 → 500 (api-news)
  [3] 3 drifts in 3 requests: header location is new
  … 9 more groups only in requests whose status changed, summarized there (driftgate review lists them)
```

## report

```
driftgate report [--format text|json|junit|markdown] [--max-drifts N] [--exit-code]
```

The last comparison again, e.g. as a pull-request comment.

By default `report` exits `0` also when the comparison drifted: it only
writes a result that `compare` or `run` already gave, and those two are the
gate. A CI step usually runs `run` and then `report` once per format (the
GitHub Action writes markdown and JUnit under `bash -e`); a `report` that
failed on drift would stop that step before the second file is written.
It exits `2` when there is no comparison yet or a flag is wrong, and `3`
when the local store cannot be read.

`--exit-code` makes `report` exit with the verdict of the comparison it
writes, exactly as `compare` did: `1` on drift, `3` when the result is
incomplete (private requests whose body is not on this machine, without
`--skip-private`, or nothing compared; this wins over drift, as in
`compare`), `0` clean. It is
the comparison's verdict only: when `run` failed for changed routes, a stale
catalog or a capture that stopped, the saved comparison does not hold that,
so gate on `run` itself.

| Flag | Meaning |
|---|---|
| `--format text\|json\|junit\|markdown` | output format (default text) |
| `--max-drifts N` | list up to N drifts per request, every one (default 20; of a request whose status changed, the change, its location and content type and one line for the rest) |
| `--exit-code` | exit with the verdict of the comparison, as `compare` did: `1` on drift, `3` when incomplete, `0` clean (without it, `0`) |

Markdown stays under GitHub's comment limit of 65,536 characters, with a
note on what was left out. No format shows values from private bodies.

```sh
driftgate report --format markdown > report.md
driftgate report --format junit > junit.xml
driftgate report --exit-code > /dev/null   # 1 when the last comparison drifted
```

## allowlist check

```
driftgate allowlist check [--baseline <name>] [--format text|json]
```

Checks the allowlist without capturing anything: against the catalog,
against the responses of a baseline, and by itself. Exit `1` when it finds
something, `0` when not, `2` for an allowlist, catalog or configuration
that cannot be read or a `--baseline` that does not exist, `3` when the
local store cannot be read.

| Flag | Meaning |
|---|---|
| `--baseline <name>` | baseline whose responses the rules are checked against (default `local`; when `local` does not exist yet, only the catalog) |
| `--format text\|json` | output format (default text) |

Every finding names the rule (`body rule 4 ($.entries[*].renamed)`, its
kind — `header`, `body` or `array` — and its position among the rules of
that kind from 1) and one check:

| Check | Meaning |
|---|---|
| `no-request` | the rule applies to no request of the catalog that is compared (its `request` or `target` matches none; observed requests are never compared) |
| `no-place` | none of the baseline's responses to its requests has a place the rule reaches: a header of that name; a value (JSON value, text or attribute of a page) for a rule with a `format`; an array for `any_length`; a cell for a cell rule; a body that is not JSON for `$raw`; an array for an array rule (`arrays:`). Usually a renamed or removed value |
| `any-length-values` | an `any_length` rule without `format` whose path also reaches values (`$.items*` reaches `$.items[0].price`): those values are still compared, so the path is wider than the arrays it is meant for |
| `raw-without-request` | a `$raw` rule without `request`: it applies to every response that is not JSON |
| `broad` | a body rule that reaches far more than a value (below) |
| `shadowed` | an earlier rule always decides first wherever this one applies, so it never decides: the first matching rule with a whole-value `format` decides a value, the first `any_length` rule an array length, the first header rule of a name its header, the first array rule how an array's items pair; a `parts` rule loses to any whole-value rule that matches. Values, cells and `$raw` rules are looked up apart |
| `duplicate` | the same rule as an earlier one (apart from the reason) |

A rule is `broad` when:

- its path starts with a wildcard (`*updated_at`, `?.id`): it reaches
  every kind of body (JSON, pages, the RSC payload) at any depth; start it
  at `$` or `html` and name the place (`$.data[*].updated_at`);
- its path names nothing after its root but wildcards and separators
  (`$*`, `$.*`, `html*`, `html>*`, `xlsx:*`, `xlsx:*!*`, `csv!*`): it
  reaches the whole body;
- its `format` accepts any value (it matches each of `x`, `0`, `-1.5`,
  `true`, `null`, a timestamp, `a b`, `é`, `<p>x</p>` and `{"a":1}`, as
  `.*` and `.+` do) and its path is a glob or `$raw`: every value it
  reaches may change unchecked.

A rule with `parts: true` is never `broad` by its path: only what its
format matches may change, wherever that is (an asset hash anywhere in a
page). Header rules name one header and are never `broad`.

Shadowed rules are found only where that is certain: one glob covers
another when it has no `?` and matches the other's text (`$.data[*].at`
covers `$.data[3].at`); a rule's `request` and `target` must cover the
other's too.

Private bodies are read with the project key when it is at hand and the
body is on this machine, as `compare` reads them; otherwise they are not
read, and nothing is concluded from them: a rule that could reach a place
only in such a body, or whose requests have no response in the baseline
yet, is listed in a note as not checked instead of `no-place`. The note
also lists the bodies not read and why. Without the key the check still
runs (it does not stop with exit 3 as `compare` does): findings that need
no private body are still certain.

```
$ driftgate allowlist check
header rule 2 (x-request-id): no-place: the baseline's responses to its 12 requests have no header of that name it reaches (renamed or removed?)
body rule 5 ($.entries*): any-length-values: any_length lets arrays change length; the path also reaches values such as $.entries[0].id, and those are still compared (no format): narrow the path to the arrays
body rule 7 (*updated_at): broad: the path starts with a wildcard, so it reaches every kind of body (JSON, pages, the RSC payload) at any depth; start it at $ or html and name the place (*.updated_at → $.data[*].updated_at)
note: 2 response bodies not read from the baseline: api-me (a private body, and the project key is not at hand), mut-login (a private body, and the project key is not at hand)
14 rules checked against the catalog and baseline local: 3 findings
```

The JSON output has `baseline`, `rules` (how many), `findings` (each with
`kind`, `rule`, `name` or `path`, `request`, `target`, `check` and
`detail`) and `notes`.

`compare` reports, after a comparison, the rules that applied to no change
(`unused_rules`); `allowlist check` finds the rules that cannot apply at
all, before anything is captured.

## review

```
driftgate review [--group N] [--show-private]
```

Drift groups of the last comparison, with the accept command for each.

| Flag | Meaning |
|---|---|
| `--group N` | show this group with every request it touches |
| `--show-private` | show values from private bodies, compared again from the two sets, on your own machine only (never kept) |

It asks nothing (it works in CI). A request is accepted whole, so when its
drifts fall into several groups, `review` names them together in one
`accept` command. In mode freeze it gives the `expect` command instead.
Expected drifts are listed by ref, apart from the groups to review. It also
lists the requests sent again after a load failure, as `compare` shows them
(passed after a retry, or failed in all their sends); they are not drifts
and have nothing to accept.

```
$ driftgate review
last comparison: capture:run-5729dc0359a3 against baseline:local, 2026-10-29T22:56:13Z (mode evolve, level values)

[1] header-extra header:x-powered-by  (3 requests, 3 drifts)
    example:  post-api-faq  header-extra    header:x-powered-by   → ["demo"]
    requests: post-api-faq, get-api-news, get-api-news-id
    accept:   driftgate accept group:1 group:2 … --reason "<why this change is intended>" --comparison b1b7b001292b
...
```

## accept

```
driftgate accept <request|glob|group:N>... --reason "…" [--ref <ticket>]
    [--comparison <id>]
```

Takes drifts of the last comparison into its baseline (mode evolve), with a
reason in the acceptance log.

| Flag | Meaning |
|---|---|
| `--reason "…"` | why the change is right (required) |
| `--ref <ticket>` | ticket or link kept with the acceptance |
| `--comparison <id>` | the comparison `review` showed; `accept` refuses a newer one |

- Refused with exit 2: `mode: freeze`, a missing `--reason`, a selection
  that takes only some of a request's drifts, a comparison that is stale.
- An accepted answer that is a failure (see [coverage](#coverage)) is
  listed after the acceptance, with the routes of the accepted requests it
  leaves uncovered (exit 0: the reason may explain it, and the acceptance is
  written); `coverage` and `run` then fail on such a route until it is fixed
  or acknowledged as a known failure.
- Each acceptance is a line in `.driftgate/baselines/<name>/acceptances.jsonl`
  (`driftgate/baselines/…` with `baselines: git`; who, when, why, the
  requests, the drift signatures), chained by the hash of the previous
  line, and `manifest.json` is written again. With `baselines: git`,
  commit both; the message then says so.
- [Expectations](#expect) of the accepted requests whose drifts the
  baseline now holds are closed, with a line in the ref's file.

```
$ driftgate accept 'get-api-*' post-api-faq --reason "v2 returns numeric ids and a meta object" --ref DEMO-1
accepted 3 requests into baseline local: get-api-news, get-api-news-id, post-api-faq
  header-extra header:x-powered-by
  ...
logged in .driftgate/baselines/local/acceptances.jsonl
```

## expect

```
driftgate expect <request|glob|group:N>... --ref <ticket> --reason "…"
    [--comparison <id>]
driftgate expect --drop <ref> --reason "…" [--baseline <name>]
driftgate expect --import <file> [--baseline <name>]
```

Records drifts of the last comparison that a change intends, by ticket,
without changing the baseline (also in mode freeze): compare then passes a
request every drift of which is exactly a recorded one, and fails on any
other drift.

| Flag | Meaning |
|---|---|
| `--ref <ticket>` | ticket or link of the change that intends the drifts (required) |
| `--reason "…"` | why the change makes these drifts, or why they are dropped (required) |
| `--comparison <id>` | the comparison `review` showed; `expect` refuses a newer one |
| `--drop <ref>` | remove the expectations of this ref instead (no requests) |
| `--import <file>` | take a ref's expectations file from another checkout (a worktree) instead, after checking it |
| `--baseline <name>` | with `--drop` or `--import`: the baseline (default: the last comparison's, else `local`) |

- For a team in mode freeze whose pull requests carry intended changes:
  each ticket's drifts are recorded once, and the gate stays red for
  everything else. The baseline (its files, manifest and acceptance log)
  does not change.
- Each drift is recorded as a digest of its kind, place and both values.
  Where a drift's description leaves content out (the new key's value,
  the items an array gained, a body that differs only in its bytes, the
  lines after the first changed one, a new element or worksheet and, when
  worksheets come, go or move, the rest of the workbook), the digest
  covers that content too, as the comparison sees it: a value an
  allowlist rule lets change (a `created_at` in a new item) is written as
  the rule, so the expectation holds on the next capture, and a value no
  rule names is exact, with its JSON type. How the body is written
  counts too, wherever the drift is (a header, the status, another part
  of a page): each JSON or CSV document of the response whose bytes
  differ is read for what it writes beyond its values (its layout and
  the whitespace the layout does not say, how its keys and strings are
  escaped, `\/` or `/`, a CSV file's quotes, line ends, blank lines and
  empty fields), and where that changed, it is in the digest of every
  drift of the response: an expectation holds only for a response
  written the same way. A rule covers how the values it lets change are
  written, and an encoder writes the rest the same way for other values
  and lengths, so values a rule lets change and lists that grow keep an
  expectation. Two cases do not: a list an `any_length` rule lets grow
  in a layout the comparison does not predict (Jackson's default
  `[ 1, 2 ]` and `"a" : 1`, Python 2's `", "` before a line end), and a
  CSV cell a rule lets change whose value needs quotes only now and
  then; there an expectation fails when the length or the quoting
  changes. Below the values level only what that level compares
  counts (types, structure, names). A new key, item or element is
  expected with where it stands: when two tickets add keys to one
  object, expect them where both are in. The origins of both sides (and their aliases) are
  written as one neutral origin, so an expectation recorded in a worktree
  that runs on another port holds in the others. A drift is expected only when an
  expectation in force names exactly that drift: never one with another
  value (a header that changes again, a date in another format). A
  request is expected only when every one of its drifts is, by one ref
  or several; a request with one drift that is not expected fails, and
  only that drift is listed as failing.
- Requests named by their ID or a glob are expected with all of their
  drifts; `group:N` takes the drifts of a group, so two tickets that
  change one request in different ways each expect their own drifts. A
  request that was not captured is taken only by its ID.
- A side effect names what a request changed, not the values: it cannot
  be expected. Nor can a request whose private body is not on this
  machine (it was not compared whole). The values of a private response,
  or of a request built from one, are named by a keyed hash (with the
  project key, as private bodies are), so nothing in the file reveals
  them. A value that changes on every capture and no allowlist rule
  covers makes an expectation that the next capture does not meet: allow
  it by format first.
- A drift another ref expects already is left out, and the output says
  so: one ticket does not take another's drifts. Expecting a request
  with the same ref again adds the drifts selected now to that ref's
  expectation of it (a second group of the ticket's drifts); of its
  earlier drifts only those that no longer occur are dropped.
- Refused with exit 2: a missing `--ref` or `--reason`, a comparison
  against a capture, a stale comparison, a selection whose drifts are all
  expected (by another ref) or accepted already.
- Each ref has a file of its own,
  `<baseline>.expectations/<ref>.jsonl` next to the baseline's folder
  (`.driftgate/baselines/local.expectations/API-7.jsonl`; a ref that is
  not a plain file name of at most 60 characters is written with `_` for
  other characters, cut to 60 characters, with 8 bytes of its SHA-256
  after a `~`; a name another ref's file has is refused, never written
  over). Two pull
  requests with different tickets change different files and do not
  conflict. A line is added for each `expect` and `--drop`, and for each
  expectation that [`accept`](#accept) or a
  [rebase](#baselines-rebase) closes because the baseline now holds its
  drifts (who, when, the ref, the reason, the requests with their drift
  digests and signatures). Lines are chained by the hash of the line
  before, a last line seals the file, and every line names the baseline
  and the ref: a file edited by hand, copied from another baseline or
  renamed is refused (exit 3) by `compare`, `run` and `expect`, and fails
  `verify`; restore it, or remove it, which removes the expectations of
  its ref. This shows accidents and hand edits, as the acceptance log's
  chain does; it is not a signature.
- `--drop <ref>` ends every expectation of a ref, for example when the
  ticket is merged and its change is in the baseline, or its expectation
  stays unmet; the line stays in the ref's file. The last comparison is
  marked again after `expect`, `--drop` and `--import`, so `report` and
  `review` show the result at once.
- What a reviewer sees: with `baselines: git` the ref's file is part of
  the pull request (`driftgate/baselines/<name>.expectations/`), with
  each expected drift's signature, the ticket and the reason, next to
  the code that makes the drift; commit it with the change. With
  `baselines: local` the expectations stay on the developer's machine,
  like the baseline: the pull request carries what the report says
  (`report --format markdown`), and the gate is that developer's run.
- Worktrees (`baselines: local`): each worktree has its own
  `.driftgate/`. To carry a ticket's expectations from one to another,
  `driftgate expect --import
  ../shop-api-7/.driftgate/baselines/local.expectations/API-7.jsonl`
  takes the file after checking its chain, seal and baseline; a ref the
  baseline has already is replaced only by a file that continues it.
  When the two files of a ref went apart, delete this baseline's file of
  the ref to take the other (a `--drop` would add a line, and they would
  still differ).

```
$ driftgate expect news me --ref API-7 --reason "IDs are numbers in v2"
expected 9 drifts in 2 requests of baseline local for API-7: me, news
  header-extra header:x-powered-by
  type $.entries[*].id string→int
  ...
recorded in .driftgate/baselines/local.expectations/API-7.jsonl (exactly these drifts, with their values; the baseline is unchanged)
```

## verify

```
driftgate verify [--baseline <name>] [--adopt] [--migrate-private] [--migrate-state]
```

Checks baselines against their manifest and acceptance log (exit 1 when they
disagree).

| Flag | Meaning |
|---|---|
| `--baseline <name>` | check this baseline only (default: all) |
| `--adopt` | write the manifest of a baseline made before manifests (once) |
| `--migrate-private` | keep the personal data of an older baseline out of git (once, with the key; below) |
| `--migrate-state` | record the state fingerprint of a baseline made by an earlier release in the current format, after checking the data is the baseline's (once, after `driftgate seed`; below) |

`verify` finds hand edits, deleted or extra files and edited log lines,
also in a baseline's [expectations](#expect) file.
`capture`, `compare` and `run` refuse such a baseline with exit 3. For private bodies it
reports how many the baseline refers to and how many are on this machine.
A file that differs only in its line endings (CRLF where the manifest has
LF) is reported as such, with the git commands that check it out again.

It also scans what each baseline puts into git as `capture` scans it before
storing it: every public body (personal data, credentials, a body that
cannot be scanned) and every record's headers (a value capture would
mask). Such a record fails verify (exit 1), named with the kind and where,
never the value. `--migrate-private` fixes it the way a capture now stores
it: the body goes into the local store as private (encrypted), the header
values are masked, and the catalog marks the request `storage: private` (a
generated request takes its storage from `storage.public` instead, which
the command names). The values were committed: treat a credential as leaked.

`--migrate-private` also names the private bodies of a baseline made before
keyed refs by a keyed hash, and moves those a baseline kept in the
repository (`private/`) into the local store. `--migrate-state` restores the
data from the pristine copy, fingerprints it, and checks every item whose
hash differs from the baseline's again as the baseline's format hashed it:
only when all match does it write the new fingerprint into the baseline's
`meta.json`. Both keep the acceptance log, add a line to it, rewrite the
manifest and hold the lock of the local store; commit the baseline after.

`run`, `accept` and `verify` put a `.gitattributes` with `* -text` into
the baselines folder when it is missing: baselines are checked byte for byte, and git on
Windows (`core.autocrlf`) would otherwise check their files out with CRLF
line endings, which every check then reports as a change. With
`baselines: git`, commit it. A checkout made before it has the CRLF files already: `git rm -r -q --cached
driftgate/baselines` and `git checkout HEAD -- driftgate/baselines` check
them out again as they were committed.

```
$ driftgate verify
OK     baseline local: 5 files, 1 acceptances
         3 private bodies are outside the repository (encrypted in the local store, checked against their ref when read); 3 of them are on this machine
```

## bisect

```
driftgate bisect <good> <bad> [--group N] [--only <glob>] [--baseline <name>]
    [--format text|json] [--expected]
```

First commit of each drift group of the last comparison (`bisect:` in
`driftgate.yaml`).

| Flag | Meaning |
|---|---|
| `--group N` | chase this drift group only (repeatable) |
| `--only <glob>` | chase these requests; without a comparison yet, the requests to chase |
| `--baseline <name>` | baseline to compare with when there is no comparison yet (default `local`) |
| `--format text\|json` | output format (default text) |
| `--expected` | chase the drifts expectations name too ([`expect`](#expect)); without it they are left out, as accepted ones are |

Needs `bisect.checkout` in `driftgate.yaml`. See the
[bisect guide](guides/bisect.md).

```sh
driftgate run                    # at the bad commit: drift groups
driftgate bisect v2.3.0 HEAD
```

## data classify

```
driftgate data classify [--write] [--sample N]
```

Proposes rules for the columns `classify.yaml` does not cover.

| Flag | Meaning |
|---|---|
| `--write` | add the proposed rules to `classify.yaml` |
| `--sample N` | rows sampled per table (default 200) |

Exit 1 when there are columns or schema findings without a rule (without
`--write`). Proposed rules carry `proposed: true` until a person removes the
mark; sampled values are never written. Needs `data:` in `driftgate.yaml`.

```sh
driftgate data classify --write
```

## data anonymize

```
driftgate data anonymize [--dump <file>] [--no-target] [--replace]
    [--format text|json]
```

Copies `data:` (source) into `data:` (target) and/or a dump, with personal
values replaced by `classify.yaml`; the output is made only if data verify
finds nothing in it.

| Flag | Meaning |
|---|---|
| `--dump <file>` | also write a dump to this file |
| `--no-target` | write only the dump (needs `--dump`) |
| `--replace` | drop an existing target database and write it again |
| `--format text\|json` | output format (default text) |

Stops with exit 2 when a column has no rule, a rule is still proposed or
stale, or a schema object holds unconfirmed personal-looking literals. A
failure leaves an existing target as it was. See
[anonymized test data](guides/test-data.md).

```sh
driftgate data anonymize --dump out/app-anon.sql
```

## data subset

```
driftgate data subset [--dump <file>] [--no-target] [--replace]
    [--format text|json]
```

Data anonymize of a sample with closed references: the rows `subset:` in
`classify.yaml` chooses, and every row they reference.

Flags as for [data anonymize](#data-anonymize). Needs a `subset:` section in
`classify.yaml`.

```sh
driftgate data subset --no-target --dump out/app-sample.sql
```

## data verify

```
driftgate data verify [--target <database>] [--dump <file>]
    [--format text|json]
```

Checks an output of anonymize against the source: the detector over every
value, and every original personal value looked for.

| Flag | Meaning |
|---|---|
| `--target <database>` | database to check (default `data.target`) |
| `--dump <file>` | dump to check |
| `--format text\|json` | output format (default text) |

Exit 1 when it finds personal data. `anonymize` and `subset` run the same
checks before they make their output.

```sh
driftgate data verify --dump out/app-sample.sql
```

## baselines

```
driftgate baselines local|git
```

(For `driftgate baselines rebase`, see [baselines rebase](#baselines-rebase).)

Moves the project to the other [`baselines`](configuration.md#baselines)
mode: the configuration (its comments kept; `baselines: local` set, or for
`git` the key taken out, which keeps the file readable by an earlier release), the
catalog, allowlist, routes, exclusions, classify rules and the baselines
with their acceptance logs and manifests, and the local store. Other files
in the folder (a tokens file kept there) stay where they are; the command
names them. The stores stay locked until the old place is gone, and a
command that was waiting for the lock then stops (exit 2: run it again)
instead of writing into the old place.

- `git`: everything that goes into the repository passes the checks of
  [`verify`](#verify) first (the manifest, private bodies, personal data
  and credentials in public bodies and headers); a finding stops the switch
  before anything is written (exit 2). `.driftgate/key` is never copied
  into the repository: `baselines: git` reads the key from `key:`,
  `DRIFTGATE_KEY` or the macOS Keychain, and the switch stops (exit 3) with
  the command that puts it there when none of them has it, or (exit 2)
  when one has another key. The store moves to `~/.driftgate/<project>`
  (or `store:`); `.driftgate/` is removed. Commit `driftgate.yaml` and
  `driftgate/` afterwards.
- `local`: `.driftgate/` gets the files and the store, `.gitignore` and
  `.dockerignore` the entry (as `init` writes it), and `.driftgate/key` the
  key that `baselines: git` found (unless `key:` names its source).
  `driftgate.yaml` and `driftgate/` are removed from the working tree;
  commit that. The store in `~/.driftgate/<project>` stays, as other
  checkouts of the project may use it; the command prints how to remove it.
- With `DRIFTGATE_STORE` or `store:` the store stays where it is. If the
  project's own store (`.driftgate/store`, or `~/.driftgate/<project>`)
  holds data then too, the switch stops and says to move it or to unset
  the variable, as it would be left behind.
- Nothing in the new place is removed or replaced: the store of
  `baselines: git` is shared by the checkouts of the project. Baselines
  and captures are added by name, private bodies and pristine records by
  file; one of the same name with other content stops the switch before
  anything is written (exit 2), naming it. Only the last comparison of
  this checkout is taken over.
- A folder with both configurations (a branch from before a switch) is
  refused with the commands that settle it.
- The switch writes the new place fully, then the new configuration, then
  removes the old place, with a mark in `.driftgate/switching`. Stopped at
  any point, every other command refuses to run (exit 2) until `driftgate
  baselines <mode>` with the same mode finishes it.

```sh
driftgate baselines git
```

## baselines rebase

```
driftgate baselines rebase <name> --from <tag> --reason "…" [--ref <ticket>]
    [--allow-freeze]
```

Makes a capture the content of a baseline (after a merge), with the checks
of a baseline made by run and one line in the acceptance log; closes the
expectations the new baseline holds.

| Flag | Meaning |
|---|---|
| `--from <tag>` | the capture that becomes the baseline's content (all of the catalog; required) |
| `--reason "…"` | why the baseline takes the capture (required) |
| `--ref <ticket>` | ticket or link kept with the log line |
| `--allow-freeze` | rebase in mode freeze too (for changes reviewed elsewhere, such as a merge) |

- A baseline goes stale when changes reviewed in other pull requests are
  merged: every later comparison shows their drift. Capture the merged
  code (`driftgate capture main`), then `driftgate baselines rebase local
  --from main --reason "…"`. The responses, bodies, `timings.json` and
  the state fingerprint become the capture's; the acceptance log is kept
  and gets one line (`"kind": "rebase"`, the capture, the requests that
  changed, new or are no longer in the catalog, and the drift
  signatures), and the manifest is written again. `concurrency: auto`
  keeps the setting the baseline recorded.
- The capture must be whole and fit for a baseline, as `run` checks one:
  not interrupted; every request of the catalog that the baseline has
  (a capture with `--only` or `--target` is refused); a state fingerprint
  when the baseline has one; private bodies named by a keyed hash and on
  this machine, readable with the key; and nothing in public bodies and
  headers that [`verify`](#verify) would refuse in a baseline (personal
  data, credentials). The new baseline is written next to the old one and
  put in its place as `accept` does; its manifest holds the SHA-256 of the
  bytes as they were copied.
- The new baseline is then checked as `run` checks one: requests meant to
  succeed that failed in it (and the routes they leave uncovered) are
  listed, and responses that came only after a
  [retry](configuration.md#retries) are named in a warning. As with
  `accept`, these are said and never change the exit code: the rebase is
  written.
- When the data changed since the baseline (a new seed or migration), a
  capture checked against the baseline stops (exit 3); capture with
  `--skip-fingerprint-check` instead: it records the fingerprint of the
  data it ran on, which becomes the baseline's. The output says how many
  state items differ from the old baseline's.
- `run` makes a baseline only from two equal captures. A rebase takes
  one capture; when it was made once (not `capture --repeat 2`, which
  records that its runs agreed), the rebase warns: a value that changes
  by itself then shows as a drift on the next run. It is not refused, as
  that shows at once and never passes a drift; capture with `--repeat 2`
  to check first.
- The baseline keeps its name: one named for a date or a release
  (`release-2026-10`) still has that name after a rebase on other code.
  Make a new baseline with `run --baseline <name>` when the name should
  say what it holds.
- Mode freeze means the baseline changes only with intent: a rebase there
  needs `--allow-freeze`, which says that the capture's changes were
  reviewed where they were made. `accept` stays refused.
- [Expectations](#expect) whose drifts the rebase took into the baseline
  (every drift an expectation names is among the request's drifts from
  the old baseline to the capture), and those of requests no longer in
  the catalog, are closed, with a line in the ref's file; the others
  stay, and those of requests that changed otherwise are reported.
- Exit `0`; `2` for a refused rebase (above, or a missing `--reason`);
  `3` for an interrupted capture, when the baseline or its expectations
  changed outside driftgate, or private bodies are not on this machine.

```
$ driftgate baselines rebase local --from main --reason "main merged API-7 and API-9" --allow-freeze
rebased baseline local on capture main: 7 requests changed
  header-extra header:x-powered-by
  ...
logged in .driftgate/baselines/local/acceptances.jsonl
closed the expectations of API-9 the baseline now holds: faq-create, news-item (.driftgate/baselines/local.expectations/API-9.jsonl)
1 expectation kept: their drifts are not all in the baseline (driftgate expect --drop <ref> --reason "…" removes one)
```

## key create

```
driftgate key create
```

Creates the project key: with `baselines: local` in `.driftgate/key` (32
random bytes, base64, open to its owner only (mode 600; not checked on Windows); only once git ignores
`.driftgate/`), with `baselines: git` in the macOS Keychain (service
`driftgate`, account the project name). It refuses to replace an existing
key: private bodies encrypted with it would become unreadable. With
`baselines: git` on Linux and Windows set `DRIFTGATE_KEY` (32 random bytes,
base64) or `key:` in `driftgate.yaml` instead
([configuration](configuration.md#key)).

```sh
driftgate key create
```

## version

```
driftgate version
```

Prints the version.

```
$ driftgate version
driftgate v1.0.2 (commit …, built …)
```

---

## Environment variables

| Variable | Meaning |
|---|---|
| `DRIFTGATE_KEY` | project key (32 bytes, base64), when `key:` names no other source |
| `DRIFTGATE_STORE` | local store folder; takes precedence over `store:` |
| `DRIFTGATE_UPLOADS` | uploads folder; takes precedence over `uploads.dir` |
| `DRIFTGATE_BISECT_DIR`, `DRIFTGATE_BISECT_COMMIT` | set by `bisect` for its hooks: the worktree of the commit under test, and the commit |

Commands DriftGate runs never receive `DRIFTGATE_KEY` or the variable named
by `key.env`.
