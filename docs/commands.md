# Commands

Every command and flag of `driftgate`. The descriptions are those of
`driftgate help <command>` (the source is `internal/cli/help.go`); this page
adds the exit codes, notes and an example for each command.

```
driftgate help                 # synopsis of every command
driftgate help <command>       # one command with its flags (also: <command> --help)
```

Contents: [common flags](#common-flags) · [exit codes](#exit-codes) ·
[init](#init) · [setup](#setup) · [seed](#seed) · [run](#run) · [discover](#discover) ·
[generate](#generate) · [coverage](#coverage) · [har import](#har-import) ·
[record](#record) · [capture](#capture) · [compare](#compare) · [report](#report) ·
[allowlist check](#allowlist-check) · [review](#review) · [accept](#accept) · [verify](#verify) ·
[bisect](#bisect) · [data classify](#data-classify) ·
[data anonymize](#data-anonymize) · [data subset](#data-subset) ·
[data verify](#data-verify) ·
[baselines](#baselines) · [key create](#key-create) · [version](#version) ·
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
`accept`, `bisect`) take a lock on it
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
| `1` | drift: responses, side effects, changed routes (`discover`), uncovered routes (`coverage`), a stale catalog (`generate --check`), findings of `allowlist check`, a baseline that disagrees with its manifest or holds personal data or credentials in git (`verify`), personal data in an output (`data verify`), columns without a rule (`data classify` without `--write`) |
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
  others above `state:`. For an SQLite file it also writes the tables
  the framework keeps its sessions, cache and jobs in as `exclude:`
  ([SQLite](configuration.md#sqlite)).

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
   port, user and where it was found; never the password), the address,
   the routes covered, and what to check by hand.

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
  (MySQL, MariaDB, when the root password is known) or its user
  (PostgreSQL, a superuser of the image) can create the pristine copy.
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
  granted. Setup proposes to exclude the framework's own tables in the
  same file (sessions, cache, jobs; default yes, `--yes` says yes), and
  says to run the application's migrations first when the file does not
  exist yet (exit 2). `--driver sqlite` for a setting that names a
  database server, or another driver for one that names an SQLite file,
  stops it (exit 2). In a worktree, the worktree's settings must name a
  file of its own, which must exist there (git does not carry it).
- **A user that may not create a database.** Before the pristine copy it
  says what to grant, as an administrator: `GRANT ALL PRIVILEGES ON
  \`shop_pristine\`.* TO 'shop'@'%';` (MySQL, MariaDB) or `ALTER ROLE
  "shop" CREATEDB;` (PostgreSQL). A seed that fails for want of the
  right (MySQL/MariaDB errors 1044, 1142, 1227; PostgreSQL 42501) repeats
  it; another failure (the server not running) does not.
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
  (the summary lists them; `run` and `coverage` then fail on them); `1`
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
after changing the seed data. Every capture restores the databases to the
pristine copy and checks them against its fingerprint. The fingerprint is
kept in the local store per database server (from the DSN, without its
credentials) and databases, so checkouts of the project (git worktrees)
that share the store but use their own database server never take each
other's; `run` seeds when this server has none yet. An SQLite database is
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
    [--skip-private] [--target <service>]
```

Routes, catalog, capture and compare in one command; the first run makes the
baseline from two equal captures.

| Flag | Meaning |
|---|---|
| `--baseline <name>` | baseline to make or compare with (default `local`) |
| `--allow-mutating-without-restore` | send requests that change data even without a state restore (not uploads of real files) |
| `--skip-private` | accept that private bodies not on this machine are not compared (their status, headers and side effects are); without it the result is incomplete (exit 3) |
| `--target <service>` | work on this service only (repeatable or comma-separated); the check is then reported as partial |

- Steps: `discover` (routes), `generate` (catalog), `coverage`, `seed` when
  needed, `capture`, `compare`. The first run writes the route inventory and
  the catalog it finds. Later runs do not: a changed inventory or catalog is
  reported as drift (adopt it with `discover --write` or `generate --write`),
  and `run` still captures with the existing catalog and compares, so one run
  gives the whole picture.
- Without the baseline, `run` captures twice on the same state and makes the
  baseline only when both captures agree. Otherwise it proposes allowlist
  entries with a format; you add the reason.
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

```sh
driftgate generate --check     # CI: is the committed catalog current?
```

## coverage

```
driftgate coverage [--format text|json] [--target <service>]
```

Routes of the inventory without a catalog request (exit 1 when there are
any).

| Flag | Meaning |
|---|---|
| `--format text\|json` | output format (default text) |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

Each method of a route counts separately (`PUT` and `PATCH` of one route
count as one). A method is covered only by a request that should succeed;
error variants (`not-found`, `invalid-param` …) do not cover it. Exclude a
route on purpose in `.driftgate/exclusions.yaml`, with a reason. It also
reports requests that reach no route, stale exclusions and routes without a
sample body.

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
    [--skip-fingerprint-check] [--calibrate] [--quiet] [--target <service>]
```

Restores the state, sends the catalog and stores the responses under a tag.

| Flag | Meaning |
|---|---|
| `--only <glob>` | only requests whose ID matches |
| `--force` | replace an existing capture with this tag |
| `--restore once\|each` | `once` (default): restore before the read-only requests and once more before the requests that change data, which then run one after another, each on the data the ones before it left (a sequence with `restore: before` and a repeated request get their own restore); `each`: restore before every sequence and every request that changes data |
| `--repeat N` | capture N times on the same state and propose allowlist entries for values that differ (default 1) |
| `--baseline <name>` | the state must match the one this baseline was captured from (default `local`) |
| `--allow-remote` | allow a target outside `allow_hosts` |
| `--allow-remote-mutations` | with `--allow-remote`, also send requests that change data or are repeated to it |
| `--allow-mutating-without-restore` | send requests that change data even without a state restore (not uploads of real files) |
| `--skip-fingerprint-check` | do not check the state fingerprint (diagnostics only; the capture is marked unreliable) |
| `--calibrate` | decide `concurrency: auto`: measure, compare a capture at the chosen setting with a sequential one, record it in the baseline (implies `--repeat 2`; not with `--only`) |
| `--quiet` | no progress output |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- Captures live in the local store (`.driftgate/store/captures/<tag>`; with
  `baselines: git`, `~/.driftgate/<project>/captures/<tag>`), never in the
  repository. A tag is a simple name (letters, digits, `. _ -`; not ending with `.` and not a device name Windows reserves, such as `con` or `nul`).
- Read-only requests run first (in parallel up to `concurrency`); requests
  that change data, sequences, command steps and repeated requests run after
  them, one at a time, after a restore (see `--restore`).
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
  their data. Write `"mutates": false` on those that only read, and
  `"mutates": true` on the others.
- When the restored state differs from the state the baseline was captured
  from, the capture stops with exit 3 and lists the tables that differ.
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

## compare

```
driftgate compare <tag> [--against <baseline|tag>]
    [--level status|shape|types|values] [--only <glob>]
    [--format text|json|junit|markdown] [--max-drifts N]
    [--skip-fingerprint-check] [--show-private] [--skip-private]
    [--target <service>]
```

Compares a capture with a baseline or another capture (exit 1 on drift).

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
`accept` and `bisect` read.

A comparison that compared nothing is incomplete (exit 3), never green: a
typo in `--only`, renamed request IDs or a baseline made for another
catalog would otherwise pass without checking anything. The report says
why. Requests the baseline has no response for are skipped and listed in
every format (`no_baseline` in JSON, a line in text and markdown): a new
request joins the baseline with `accept`. They do not make the result
incomplete while other requests were compared, since every new request
starts without a baseline.

Response times are shown as information, never as a gate: the capture
keeps the time of every request (`timings.json`, also in a baseline made by
`run`), and when both sides have them the report gives the 50th and 95th
percentile of each side over the compared requests and lists the requests
that became much slower: at least twice as slow and at least 200 ms slower
(the ratio alone would flag 3 → 7 ms, the difference alone the jitter of
slow requests). JSON: `timings` with `requests`, `baseline` and `current`
(`p50_ms`, `p95_ms`) and `slower` (`id`, `baseline_ms`, `current_ms`, the
most added time first); text and markdown: one line with up to ten slower
requests. Nothing is shown when a side has no times. The exit code never depends on it: one measurement per request
is too noisy for a gate.

Allowlist rules that applied to no change in the comparison are listed
(`unused_rules` in JSON, a warning in text and markdown; never a
failure): a rule whose requests are all gone or renamed (`no request`),
or one whose values did not change this time (`no change`). A rule listed
comparison after comparison is probably stale: the value it names was
renamed or removed. With `--only` or `--target`, rules of the requests
not compared are not listed.

```
$ driftgate compare before --only 'get-*'

capture:before against baseline:local: 2 compared, 2 passed, 0 drifted, 0 skipped, 0 observed
No drift ✓
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
driftgate review [--group N]
```

Drift groups of the last comparison, with the accept command for each.

| Flag | Meaning |
|---|---|
| `--group N` | show this group with every request it touches |

It asks nothing (it works in CI). A request is accepted whole, so when its
drifts fall into several groups, `review` names them together in one
`accept` command.

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
- Each acceptance is a line in `.driftgate/baselines/<name>/acceptances.jsonl`
  (`driftgate/baselines/…` with `baselines: git`; who, when, why, the
  requests, the drift signatures), chained by the hash of the previous
  line, and `manifest.json` is written again. With `baselines: git`,
  commit both; the message then says so.

```
$ driftgate accept 'get-api-*' post-api-faq --reason "v2 returns numeric ids and a meta object" --ref DEMO-1
accepted 3 requests into baseline local: get-api-news, get-api-news-id, post-api-faq
  header-extra header:x-powered-by
  ...
logged in .driftgate/baselines/local/acceptances.jsonl
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

`verify` finds hand edits, deleted or extra files and edited log lines.
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
    [--format text|json]
```

First commit of each drift group of the last comparison (`bisect:` in
`driftgate.yaml`).

| Flag | Meaning |
|---|---|
| `--group N` | chase this drift group only (repeatable) |
| `--only <glob>` | chase these requests; without a comparison yet, the requests to chase |
| `--baseline <name>` | baseline to compare with when there is no comparison yet (default `local`) |
| `--format text\|json` | output format (default text) |

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
