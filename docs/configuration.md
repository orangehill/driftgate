# Configuration

## What to use for what

| Task or symptom | What to use |
|---|---|
| a value that may change on every request (a time, a token) | a body or header rule in [`allowlist.yaml`](#allowlistyaml), with a format and a reason |
| the same header differs on every request (another server stack: `server`, `x-powered-by`, `via`) | a header rule: `name`, `format` (or `ignore_case`) and `reason` ([header rules](#allowlistyaml)); for those three headers the [line on the one change almost every request has](commands.md#compare) proposes it with the exact new value. Any other header that changed is fixed, accepted or expected |
| rows in another order (`item-order`; another database server or ORM) | an array rule: `key` pairs the items by an id, `any_order` allows the order ([arrays](#allowlistyaml)) |
| uploading a file | a [`multipart`](#multipart) catalog entry with a fixture from the uploads folder |
| a queue worker, an artisan or other command, an import or export, an admin path that changes the schema | a [command step](#command-steps) (`"output": true` compares what it prints), or a catalog request for the path; [side checks](guides/side-checks.md) |
| measured, not compared (a value no rule can describe) | [`observe`](#observe) |
| what the gate does not check | `driftgate coverage` and the [not-covered summary](commands.md#compare) after every comparison |
| two server stacks (database, PHP, web server) | [upgrading the server](guides/framework-upgrade.md#upgrading-the-server-database-php-web-server) |

`driftgate help catalog` and `driftgate help allowlist` list the fields of
those files.

DriftGate reads one configuration file and a few files beside it. Where
they are is set by [`baselines`](#baselines). A new project (`driftgate
init`) keeps all of them in the project's own `.driftgate/` folder, which
git ignores (`baselines: local`):

```
.driftgate/
  driftgate.yaml               the configuration (this page)
  routes.json                  route inventory, written by discover --write
  catalog.json                 request catalog, written by generate, edited by hand
  allowlist.yaml               values that may change, each with a format and a reason
  exclusions.yaml              routes deliberately not checked, each with a reason
  classify.yaml                column rules of the data module
  baselines/<name>/            baselines; change only through driftgate accept
  store/                       the local store: captures, private bodies, pristine metadata
  key                          the project key (driftgate key create), open to its owner only
```

With `baselines: git` (the layout of an earlier release, and of every
`driftgate.yaml` at the project root without a `baselines:` key) the
configuration and the files are committed to the repository, and the local
store is outside it (`~/.driftgate/<project>/`, see [`store`](#store)):

```
driftgate.yaml                 the configuration
driftgate/
  routes.json, catalog.json, allowlist.yaml, exclusions.yaml, classify.yaml
  baselines/<name>/
```

Commands find the project from any folder inside it: the nearest folder
(up to the top of the git work tree) that has `.driftgate/driftgate.yaml`
or `driftgate.yaml`. A folder with both (as after checking out a branch
from before a switch of `baselines`) is refused with the commands that
settle it; `.driftgate/` holds the store and the key, so it is moved aside,
never deleted. `--config <path>` names the file instead.

Every file has a `version` field (now `1`); a newer version is refused. In
the YAML files an unknown key is an error, not a warning, and so is a second
document after `---` in `driftgate.yaml` and `exclusions.yaml` (it would be
ignored).

Contents: [what to use for what](#what-to-use-for-what) · [driftgate.yaml](#driftgateyaml) ·
[allowlist.yaml](#allowlistyaml) · [exclusions.yaml](#exclusionsyaml) ·
[catalog.json](#catalogjson) · [classify.yaml](#classifyyaml)

---

## driftgate.yaml

A complete example of a Laravel API on MySQL with Redis:

<!-- check: config -->
```yaml
version: 1
project: shop
mode: freeze
level: values

target:
  base_url: http://localhost:8000
  concurrency: 4
  rate_limits:
    - match: "/api/search*"
      per_minute: 50

identities:
  admin: { env: DG_TOKEN_ADMIN }
  user: { op: "op://Shop/driftgate-user/token" }

state:
  db:
    driver: mysql
    dsn: { env: DG_DB_DSN }
    working: shop
    exclude: [sessions, cache, jobs, failed_jobs]
    row_tracking: [orders]
  redis:
    restore: ["redis-cli", "FLUSHALL"]
    fingerprint: ["redis-cli", "DBSIZE"]
seed:
  - ["php", "artisan", "migrate:fresh", "--seed", "--force"]
prepare:
  - ["php", "artisan", "cache:clear"]
guards:
  - name: mail goes to the log
    command: ["sh", "-c", "grep -q '^MAIL_MAILER=log' .env"]

discover:
  - adapter: laravel
    command: ["php", "artisan", "route:list", "--json"]
    php: ["php"]
templates: [ok, not-found, anonymous, json]
generate:
  identity: admin
params:
  - name: product
    sql: "SELECT id FROM products WHERE active = 1 ORDER BY id LIMIT 3"
    foreach: true
  - name: order
    from: { request: post-api-orders, path: $.data.id }
storage:
  public: ["get-api-products*"]
observe:
  - match: "GET /api/stats"
    reason: Counts requests of the last minute.
```

### Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | required | `1` |
| `project` | string | required | a simple name (letters, digits, `. _ -`); names the local store and the Keychain entry |
| `mode` | `freeze` \| `evolve` | `freeze` | `freeze`: every drift fails unless a ticket expects exactly that drift ([`expect`](commands.md#expect)), and `accept` is refused; `evolve`: drifts may be accepted with a reason |
| `level` | `status` \| `shape` \| `types` \| `values` | `values` | how far comparison goes: status only; + keys, order and nesting; + the type of every value; + exact values. A catalog entry can set its own level; `compare --level` can only make it stricter |
| [`baselines`](#baselines) | `local` \| `git` | where `driftgate.yaml` is | where the project's files, the store and the key are |
| [`store`](#store) | path | `.driftgate/store` (local), `~/.driftgate/<project>` (git) | local store |
| [`key`](#key) | `{env}` \| `{op}` | `DRIFTGATE_KEY`, then `.driftgate/key` (local) or the Keychain (git) | source of the project key |
| [`target`](#target-and-targets) | mapping | | the application (one service) |
| [`targets`](#target-and-targets) | map of name → target | | several services; not together with `target` |
| [`identities`](#identities-and-tokens) | map of name → `{env}` \| `{op}` | | users and where their tokens come from |
| [`tokens`](#identities-and-tokens) | `{command}` \| `{file}` | | a source of all tokens as one JSON object |
| [`state`](#state) | map of name → component | | what is restored before captures |
| [`seed`](#seed-prepare-guards) | list of commands | | load the data; `driftgate seed` then keeps the pristine copy |
| [`prepare`](#seed-prepare-guards) | list of commands | | run after every restore |
| [`guards`](#seed-prepare-guards) | list | | must succeed before the first request and before any request that changes data |
| [`discover`](#discover) | list of adapters | | where the routes come from |
| [`templates`](#templates-generate-params) | list | `[ok]` | request variants `generate` makes per route |
| [`generate`](#templates-generate-params) | `{identity}` | | identity for routes that need authentication |
| [`params`](#templates-generate-params) | list | | values of route parameters |
| [`storage`](#storage-and-pii) | `{default, public}` | private | whether generated requests store bodies as public or private |
| [`pii`](#storage-and-pii) | `{allow}` | | values the personal-data detector lets through in public bodies |
| [`observe`](#observe) | list | | generated requests that are measured but never compared |
| [`requests`](#requests) | list | | whether generated requests change data, and their bodies |
| [`uploads`](#uploads) | `{dir}` | | folder of real files that catalog requests upload |
| [`bisect`](#bisect) | mapping | | hooks of `driftgate bisect` |
| [`data`](#data) | mapping | | the data module (`driftgate data …`) |

Commands (`seed`, `prepare`, guards, restore and fingerprint, `tokens.command`,
adapters, bisect hooks) are lists: the program and its arguments, run
without a shell, from the project folder (`seed`, `prepare` and guards may
also name a folder and a standard input, see
[seed, prepare, guards](#seed-prepare-guards)). A shell is written explicitly
(`["sh", "-c", "…"]`) and shows in the diff. Commands never receive
`DRIFTGATE_KEY` or the variable named by `key.env`.

### baselines

Where the project's files are: `local` or `git`.

| | `local` | `git` |
|---|---|---|
| `driftgate.yaml` | `.driftgate/driftgate.yaml` | `driftgate.yaml` at the project root |
| catalog, allowlist, routes, exclusions, baselines | `.driftgate/` | `driftgate/`, committed |
| local store | `.driftgate/store/` | `~/.driftgate/<project>/` |
| key without `key:` | `DRIFTGATE_KEY`, then `.driftgate/key` | `DRIFTGATE_KEY`, then the macOS Keychain |
| for | checks on this machine before a pull request | baselines shared through git, CI |

The place of `driftgate.yaml` is the mode: without the key, a file in
`.driftgate/` is `local` and one at the project root is `git`, so a
configuration of an earlier release stays as it was. A `baselines:` value that
disagrees with the place of the file is refused (exit 2); [`driftgate
baselines local|git`](commands.md#baselines) moves the files. `driftgate init` makes a `local`
project (`--baselines git` for the other).

With `local` nothing DriftGate writes leaves the project folder, and
nothing of it enters git:

- `init` adds `.driftgate/` to `.gitignore` (made when missing) and to
  `.dockerignore` when there is one, or a `Dockerfile` or compose file that
  would copy the folder into an image; in a monorepo also every folder up
  to the top of the work tree with Docker files, by its path from there
  (`api/.driftgate/`). A file that has the entry already (as git, or
  Docker, reads its lines: git keeps leading spaces) is left as it is; its
  line endings are kept.
- Before a command writes the store (captures, private bodies, the last
  comparison), DriftGate makes sure git ignores `.driftgate/` when the
  project is in a git work tree: the project's `.gitignore` (or, at the top
  of the work tree, `.git/info/exclude`) has the entry and no negation
  (`!…`) that could take any of it back; otherwise it asks `git
  check-ignore` about the folder, the key, the store and the baselines,
  each. `key create` and `driftgate baselines` always ask git, which also
  finds files of the folder that git tracks. If git does not ignore one of
  them, the command stops (exit 2) and says what to add, take out or
  untrack.
- Every worktree of the repository has its own `.driftgate/`: its own
  baselines, store and key. Git does not carry the folder, so a worktree
  added with `git worktree add` (one per change) starts without one. Its
  commands then name the main worktree's project, and `driftgate setup`
  there takes the configuration, catalog, allowlist, routes, exclusions,
  baselines with their acceptance logs, the key, and the private bodies
  those baselines name from the main worktree. Not its captures or
  pristine records: the worktree's runs compare its changed code with the
  main worktree's baseline, never with a baseline made from that code.
  `state:` is copied as it is: a worktree that runs on another database
  must name it there. When the worktree's own settings (its `.env`) name
  another database than `working`, setup offers (`--yes`: does) to make
  it `working:` and `pristine:` in the copied file (and the worktree's
  `APP_URL` its `base_url`), comments kept, and makes that database's
  pristine copy. A worktree whose settings name the
  main worktree's database, or none, stops setup before it takes
  anything: its runs would restore the main worktree's database.

Scanning for personal data, encryption of private bodies and the state
fingerprint work the same in both modes.

### store

The local store holds captures, encrypted private bodies and pristine
metadata. Precedence: `DRIFTGATE_STORE`, then `store:`, then
`.driftgate/store` (`baselines: local`) or `~/.driftgate/<project>`
(`baselines: git`). A relative `store:` is relative to the project folder
(not to where driftgate runs); `~/` is the home folder.
`store:` may not point anywhere inside the git repository of the project,
also when `driftgate.yaml` is in a subfolder (it is refused, exit 2):
private bodies never enter git, not even encrypted. Folders DriftGate
creates are open to their owner only.

### key

The project key (32 bytes, base64) encrypts private bodies (AES-256-GCM) and
names them by a keyed hash (HMAC).

| Form | Source |
|---|---|
| no `key:`, `baselines: local` | `DRIFTGATE_KEY`, then `.driftgate/key` (`driftgate key create`) |
| no `key:`, `baselines: git` | `DRIFTGATE_KEY`, then the macOS Keychain (`driftgate key create`) |
| `key: { env: NAME }` | the environment variable `NAME` |
| `key: { op: "op://vault/item/field" }` | 1Password, through `op read` |

<!-- check: config-fragment -->
```yaml
key:
  op: "op://Shop/driftgate-key/password"
```

The key file `.driftgate/key` holds the key in base64 and is open to its
owner only (mode `600`); a file others can read is refused. Windows has no
such mode: there the check is not made, and the folder's access list is
what protects the file. Without a key,
capturing or comparing private bodies stops with exit 3.
Changing the key is not supported: bodies encrypted with the old key become
unreadable.

#### Handing a capture to another developer

To let someone else compare a capture (or check a result) on their
machine, copy, in the same project:

- the capture's folder, `captures/<tag>/` of the local store, into theirs;
- the baseline, `baselines/<name>/` (with `baselines: git` it is in the
  repository already);
- the store's `private/` folder, which holds the private bodies of the
  captures and baselines, encrypted with the project key; without it their
  requests are compared without the body (exit 3, or `--skip-private`).

The private bodies can be read only with the project key: give it as
`DRIFTGATE_KEY` (or `key:`) on the other machine. Send the key through a
secure channel (a password manager's shared vault, `key: { op: … }`),
never with the files and never in the same message, chat or archive.
Private bodies hold personal data: copy them only to someone who may see
that data, keep them out of shared drives and tickets, and remove the
copy when it is no longer needed. Public bodies were scanned for personal
data, but a scan is not a proof.

### target and targets

`target:` describes the application. With several services (an API, a
frontend, microservices) use `targets:` with one entry per service name
instead; see [several services](guides/multi-service.md).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `base_url` | URL | | where requests go; the request path is appended |
| `allow_hosts` | list of host names, IPs or CIDR ranges | `localhost` and local and private addresses (`127.0.0.0/8`, `::1`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`) | hosts requests may go to. A list replaces the default. A host name in the list may resolve anywhere; any other name must resolve only to allowed addresses. Redirects are checked too |
| `connect_to` | host or IP, optional `:port` | | send the requests to this address instead of the one DNS gives for `base_url`'s host, as `curl --connect-to`; see [below](#connect_to) |
| `user_agent` | string | `driftgate/1` | the `User-Agent` header of every request |
| `timeout` | duration (`30s`) | `60s` | per request; `0` is the default, a negative one is refused |
| `concurrency` | int, or `auto` | `1` | read-only requests sent in parallel; `auto`: measured once and recorded in the baseline (see [capture](commands.md#capture), `--calibrate`) |
| `follow_redirects` | bool | `false` | follow redirects instead of recording them (at most 10). A redirect from `https` to `http` is refused (the request fails); one to another host or port keeps only `User-Agent`, `Accept`, `Accept-Encoding` and `Accept-Language`, not the identity's token nor the catalog's headers |
| `rate_limits` | list of `{match, per_minute}` | | spaces requests whose path (or `METHOD path`) matches the glob; `per_minute` must be positive |
| `aliases` | list of origins | | other origins under which the application writes its own URLs (`APP_URL`, a second host name) |
| `retry` | int, `0` to `3` | `1` | how many times a read-only `GET`, `HEAD` or `OPTIONS` request that failed under load (a timeout, a reset connection, a `502`, `503` or `504` status) is sent again; `0`: never. See [retries](#retries) |

<!-- check: config-fragment -->
```yaml
targets:
  api:
    base_url: http://localhost:8001
    concurrency: 4
  web:
    base_url: http://localhost:3001
    allow_hosts: [localhost, web.internal.example]
```

A capture records the origin of every service's `base_url` (`scheme://host[:port]`,
lower case, without a default port) in its `meta.json`. When the two compared
sets were captured from different origins, `compare` (also `run` and `bisect`)
takes both origins and the `aliases` as one: before comparing, they are
replaced by a neutral origin on both sides, in memory only, and a drift shows
each side's real URL. This lets a copy of the stack on another port, or a CI
runner, be compared with a baseline without a drift for every absolute URL
the application builds from the request's `Host`. It applies to

- `<origin>` followed by anything that does not continue the host or port
  (`/`, `?`, `#`, a quote, the end…), also escaped as in JSON (`http:\/\/…`);
- `//host[:port]` (a scheme-relative URL, or another scheme such as `ws://`:
  the scheme is kept, so `http` → `https` is still a drift);
- a bare `host:port`, only when the origin has a port: a bare host name is too
  common a word (an e-mail domain, a database host).

`localhost:80801`, `a.localhost:8080` and `localhost:8080@evil.test` are not
the origin `http://localhost:8080`; a URL of any other host, port, scheme or
path is compared as it is. Bodies are rewritten for the comparison only:
stored captures, baselines and private bodies are never changed. Nothing is
replaced when the origins are the same or a service was captured on one
side only. A set that records no origin at all is not compared with one that does: `compare` stops (exit 3) and names the
command that captures it again. The report says when the origins were
taken as the same:

```text
origins: http://localhost:8080 ≙ http://localhost:8081 (own URLs compared as the same)
```

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:8081   # a second copy of the application
  aliases: [http://app.test]
```

**The project's folder.** A capture also records the folder of the project
it was made in (and its real path, when a symbolic link leads there) in
`meta.json`; with `baselines: git` it goes into the repository. A folder
of two names or more in a home folder of one of the forms
`/Users/<name>`, `/home/<name>`, `/root`, `/var/root`,
`/mnt/<drive>/Users/<name>` (WSL, also when `$HOME` is elsewhere) and
`C:\Users\<name>` (a name without spaces; `Users` and `home` in any
letter case, as macOS and Windows open them) is recorded as `~/…`, which
names no user, and stands for that path in any home folder of those
forms: a baseline made on one machine compares with another user's
capture or CI's (`/home/runner/work/…` is in the runner's home). The home
folder itself and one name in it are not recorded (their path names the
user, and a record of them would take any path of any home folder, or a
URL's, for the project's): their traces are compared as they are. The
home folder of the machine that captures (`$HOME`) is a person's when it
is outside the system's folders, or in their `home` folder, and of two
names or more (`/nfs/home/<name>`, `/data/home/<name>`, `/var/home/<name>`),
and so is the real path of a home of those forms linked elsewhere
(`/home/<name>` to `/nfs/home/<name>`): a folder there is recorded as
`~/…` too, and `compare` on that machine takes that home for one (on
another machine its traces may then differ, never hide a difference). A
home whose name has a space is left out: its folders are not recorded. A
folder elsewhere (a container's `/srv/…` or a home of one name such as
`/app`, a service account's home under `/var`, `/srv`, `/opt` or `/usr`,
such as `/var/lib/jenkins/…`) is
recorded as it is, absolute, and compares on any machine. A debug
error page or JSON writes the application's folder into its stack trace
(Laravel with `APP_DEBUG`): two checkouts of the project (the main
worktree and a git worktree, a developer's machine and CI) are in
different folders. When the two sides were captured in different folders,
or either recorded one as `~/…` (two homes may hold it), `compare`
replaces each side's own folders (a baseline's include those `accept`
added) by one neutral folder (`/driftgate-project`), in bodies and
headers, in memory only: as written, with its `/` escaped as `\/`
(PHP's `json_encode`) and with its `\` doubled (a Windows path in JSON);
a `Content-Length` that differs only by them is no drift. Only where a
path starts (after a quote, white space, a bracket, `=`, `,` or `>`) and
a name ends: a folder whose name starts with the project's (`shop2` for
`shop`), the folder inside another path or URL, another file or line
still differ. A folder of one name (`/app`, as in a container) is not
replaced: it is also a URL's path. Limits: a relative URL path of the
form `/home/<name>/` followed by the project's names, and a body that
holds `/driftgate-project` itself, are taken for the project's folder. A set made by an earlier release
records no folder and is compared as it is. `accept` adds the folders of
the capture it takes bodies from to the baseline's.

A target outside `allow_hosts` needs `capture --allow-remote`; requests that
change data need `--allow-remote-mutations` as well.

#### connect_to

When DNS names another server for the target's host than the one under
test (the host name still points to the old production while the new
server is being prepared), `connect_to` sends every request to a fixed
address, as `curl --connect-to` or `--resolve` does:

<!-- check: config-fragment -->
```yaml
target:
  base_url: https://shop.example
  connect_to: 203.0.113.10        # or 203.0.113.10:8443, [2001:db8::1]:443
  allow_hosts: [203.0.113.10]
```

The requests keep `base_url`'s host name in the `Host` header, the TLS
server name and the certificate check; only the connection goes to
`connect_to`. The host name never goes where DNS says: without a port in
`connect_to`, every port of it goes to that host on the same port (a
redirect from `http` to `https` of the name goes to `203.0.113.10:443`);
with a port, `base_url`'s port goes to it and any other port of the name
is refused (`connect_to covers shop.example:443 only`), so a redirect
there fails rather than reach the server DNS names. A redirect to another
host is not changed.
`allow_hosts` applies to the address connected to, not to the host name:
naming `shop.example` there does not allow `connect_to` to reach an
address outside the list, and such a target needs `--allow-remote` (and
`--allow-remote-mutations` for requests that change data) like any
remote one. `record`, `discover` and `setup` connect the same way; the
project's own commands (`prepare`, `tokens.command`, `seed`, guards)
do not: they reach whatever their own settings and DNS name. A
capture records the setting per service in its `meta.json`
(`connect_to`).

#### Retries

A development server under the load of a capture now and then times out,
drops a connection or answers `502`, `503` or `504` once. A read-only
request of a safe method (`GET`, `HEAD`, `OPTIONS`) whose send fails so
is sent again, up to `retry` times (default once), within `rate_limits`.
The resends wait until the other read-only requests are done and go one
at a time: without the load, a resend that passes is evidence, not luck.
No retry is spent on what cannot recover; these keep their first
response:

- a request whose baseline response failed the same way: the same
  status, a failed request on both sides, or a retry in the baseline that
  did not recover;
- the timeouts of a service (and its `504`s that took as long as
  `timeout`) once one of its resends, sent alone, timed out again;
- the `502`, `503` and `504` answers of a service once two of its
  resends got one again (an application in maintenance mode answers
  `503` to everything).

Not retried: a `500` (an exception of the application, such as the first
call after a restore on a cold cache, stays a drift), any other method
(also with `"mutates": false`), requests that change data (also a `GET`
the baseline knows writes: its response there has side effects), sequences
(and so no request that takes a parameter from another response),
command steps, repeated requests and a refused connection (the
application is not running). A request already sent again after a
restore that ended the application's database sessions (SQL Server, see
below) is retried only when that second answer is a load failure; its
record then keeps both (`resent` with the first answer, `retry` with the
second). A resend that fails in another way than load (a refused
connection, an interrupt) did not recover.

A retry never makes a failure pass unseen. The record of the response
keeps the first send's outcome and whether a later send recovered
(`"retry": {"sends": 2, "status": 503, "ms": 41, "recovered": true}`, or
`"error": "timeout"`), never compared but shown everywhere: the capture
names the requests answered only after a retry, `run` warns when it makes
a baseline from such a response (and so does `accept`), and `compare` and
`report` show a request that passed only after a retry as a class of its
own in every format (see [compare](commands.md#compare)). It counts as
passed: the exit code does not change. A request that failed in every
send is never counted as passed after a retry: it shows as failed in all
its sends, and when it passes anyway (the baseline failed the same way,
or an expected drift) every format names it as such.
A request that is retried in every run fails now and then for a reason of
its own; set `retry: 0` to see each failure as a drift.

### identities and tokens

A request with `"identity": "admin"` is sent with `Authorization: Bearer
<token>`. Tokens come from one of:

| Key | Meaning |
|---|---|
| `identities.<name>: { env: NAME }` | the environment variable `NAME` |
| `identities.<name>: { op: "op://…" }` | 1Password |
| `tokens: { command: [...] }` | a command that prints a JSON object `{"<identity>": "<token>"}` (e.g. an artisan command that logs the test users in) |
| `tokens: { file: path }` | a file with that JSON object; `~/` is the home folder, a relative path is relative to the project. Keep it out of the repository |

`tokens` takes `command` or `file`, not both. An identity's own `env` or `op`
takes precedence over `tokens`. A catalog request whose identity has no
token stops the capture.

<!-- check: config-fragment -->
```yaml
tokens:
  command: ["php", "artisan", "driftgate:tokens"]
identities:
  admin: {}
  editor: {}
  auditor: { env: DG_TOKEN_AUDITOR }
```

### state

Every capture starts from the same data. Each entry under `state:` is a
component, named by a simple name: a database with a built-in driver, or
anything else through commands.

**A database** (MySQL, MariaDB, PostgreSQL or SQL Server; an SQLite file
takes the keys of [SQLite](#sqlite); SQL Server's own notes are under
[SQL Server](#sql-server)):

| Key | Type | Meaning |
|---|---|---|
| `driver` | `mysql` (also MariaDB; `mariadb` is accepted) \| `postgres` \| `sqlite` \| `sqlserver` (`mssql` is accepted) | |
| `dsn` | `{env}` \| `{op}` \| a file of the project | the server, never written in the file: `mysql://user:pass@host:3306/` (or the driver's `user:pass@tcp(host:3306)/`), `postgres://user:pass@host:5432/`, `sqlserver://user:pass@host:1433?encrypt=true`, or where the project keeps it (`{ dotenv: .env }`); see [database DSNs](#database-dsns) |
| `working` | string, required | the database the application uses |
| `pristine` | string | its pristine copy; default `<working>_pristine`; must differ from `working` (MySQL/MariaDB and SQL Server: also in letter case) |
| `exclude` | list of globs | tables never fingerprinted or restored (sessions, caches, logs) |
| `restore_only` | list of globs | tables restored to the pristine copy's rows, but never fingerprinted: no side effect, no part of the state a baseline is checked against ([below](#restore_only)); not an excluded table |
| `restore_empty` | list of globs | `restore_only` tables restored to no rows: the pristine copy leaves their rows out, as an excluded table's (a cache; [below](#restore_only)) |
| `row_tracking` | list of tables | also record which rows and columns each request changes (without values); not an excluded or restore_only table. A capture records which tables it tracked: rows of a table the baseline did not track (row_tracking came after it) are not compared, and a note says to make the baseline again |
| `allow_remote` | bool | allow a database host that is neither local nor named in some target's `allow_hosts` |

- `driftgate seed` (or the first `run`) copies `working` to `pristine` and
  records the fingerprint of the pristine copy (not of `working`, which the
  application may have changed since the copy). Seed while the application
  does not write; seed warns when `working` differs from the copy right
  after it was made. A capture restores `working` from `pristine`
  (MySQL/MariaDB: only the tables that changed, with their counters,
  triggers and views; PostgreSQL: the whole database from the pristine one
  as a template; SQL Server: the tables and sequences that changed, else
  the whole database from the pristine copy's backup), checks it against
  the fingerprint, and stops with exit 3 when the data differ from the
  data the baseline was captured from.
- `seed` replaces only a pristine database it made itself: it marks the one
  it makes (MySQL/MariaDB: a table `driftgate_pristine`, never fingerprinted
  or restored; PostgreSQL: the database's comment; SQL Server: an
  extended property of the database). A database under the
  `pristine` name that has objects and no mark is not dropped; seed stops
  and names it. (A pristine database made by an earlier release has no
  mark; it is replaced when DriftGate's record of it names it.) The record
  of the pristine fingerprint is removed right before the old pristine
  database is dropped or swapped out, so a seed that fails or is
  interrupted after that leaves no half-made copy that a capture would
  trust: the capture asks for `driftgate seed` (`run` seeds by itself). A
  seed that fails before (no working database, a copy that cannot be made)
  leaves the old copy and its record as they were.
- PostgreSQL: a restore or seed makes the new database under the name
  `<name>__driftgate_new` and swaps it in only when the copy is complete,
  so a failed or interrupted restore leaves the working database as it
  was. The old database is renamed `<name>__driftgate_old` and dropped
  while the capture goes on (from PostgreSQL 15 a drop forces a
  checkpoint); for that moment the server holds a third copy, so keep room
  for it. A working database that is missing is restored in full.
- PostgreSQL: the copy keeps what a copy of a template does not: the
  working database's owner, its settings (`ALTER DATABASE … SET`, `ALTER
  ROLE … IN DATABASE … SET`), privileges (`GRANT … ON DATABASE`), comment
  and connection limit, read from the working database before every
  restore and at seed (the pristine copy gets the working database's
  settings, not those of the pristine database it replaces). The
  application's role thus keeps its rights (from PostgreSQL 15
  the schema `public` belongs to the database's owner) and its sessions
  their `timezone` or `search_path`. The DSN's user needs the rights to
  give them (a superuser, or a member of the owner's role).
- MySQL/MariaDB: restores and seeds stay out of the binary log
  (`sql_log_bin = 0` for DriftGate's sessions; MySQL 8 logs by default and a
  restore would otherwise log every row it copies). The user needs
  `SESSION_VARIABLES_ADMIN` (MySQL) or `BINLOG ADMIN` (MariaDB) for
  it; without it the copies are logged as before, with one note in the log.
- Replicas: when something reads the server's binary log, restores and
  seeds are logged, so a replica gets them too (an application that reads
  from a replica would otherwise see old rows, and later replication may
  break). DriftGate checks once per command: a replica (`SHOW REPLICAS`, or
  `SHOW SLAVE HOSTS`), another client reading the log (a `Binlog Dump`
  thread, such as a change-data-capture tool), a Galera node or group
  replication. When the user may not list replicas (MySQL: `REPLICATION
  SLAVE`; MariaDB: `REPLICATION MASTER ADMIN`, before 10.5 `REPLICATION
  SLAVE`), it cannot tell, and the copies are logged; the log says why. A
  replica that is disconnected at that moment is not seen: stop it from
  replicating the state databases (`replicate-ignore-db`) or give the user
  no rights to list replicas.
- Versions tested in CI: MariaDB 10.3, 10.6 and 11.4, MySQL 8.0 and 8.4,
  PostgreSQL 13, 16 and 17, SQL Server 2017, 2019, 2022 and 2025. On MariaDB a
  table with a generated column is fingerprinted from its rows (MariaDB's own table checksum of such a table
  changes without a change of data); on MariaDB 10.3 and 10.4 a table with
  a `VIRTUAL` column is restored whole, not by chunks (the chunked restore
  made those versions crash now and then). PostgreSQL before 15 has no file copy of a
  database; restores use the default copy.
- The database must be on this machine (a local address or a socket) or on a
  host some target's `allow_hosts` names; a private address is not enough.
  Every host the driver may connect to is checked (PostgreSQL `host=`,
  several hosts, `PG*` variables), and every connection on the address it
  goes to. Otherwise set `allow_remote: true`.
- `seed` creates the pristine database, which an application's own login
  often may not: MySQL/MariaDB need `CREATE` and `DROP` on the pristine
  database (`GRANT ALL ON shop_pristine.* TO 'shop'@'%'`, or `CREATE` on
  `*.*`), PostgreSQL `CREATEDB` (`ALTER ROLE shop CREATEDB`), SQL Server
  the grants [below](#sql-server). Without them `seed` stops with the
  server's refusal.
- MySQL/MariaDB: a seed and a restore make triggers and views again with
  their `DEFINER`, which DriftGate never drops or changes: a trigger, and a
  view with `SQL SECURITY DEFINER`, run with their definer's rights, and
  the `DEFINER` is part of the fingerprint. A login may name another
  account as `DEFINER` only with `SET_ANY_DEFINER` (MySQL 8.2 and later;
  `SET_USER_ID` before, and `SYSTEM_USER` for an account that has it, such
  as root), `SET USER` (MariaDB) or `SUPER`, and with the binary log on, a
  login without `SUPER` may create a trigger only when
  `log_bin_trust_function_creators` is on. Each `DEFINER` is first tried
  on a short-lived probe object (`dgt_probe_…`, in no fingerprint), so a
  seed or restore the login may not make stops before it drops anything,
  and names the object. Best: create the triggers and views with
  the application's login as their `DEFINER` (run the migrations as it)
  and seed again.
- Give the DSN a user limited to the working and pristine databases (for
  PostgreSQL with `CREATEDB`), not a superuser and not one with MySQL's
  `FILE` privilege. The one exception is a disposable database in a local
  container (a compose service on this machine), whose root or
  `POSTGRES_USER` may be used, as `driftgate setup` does when the
  application's setting names that service; on a shared server, always a
  limited user. PostgreSQL restore ends the application's sessions on the
  working database. SQL Server: a login with `dbcreator` and `db_owner` in
  the working database ([SQL Server](#sql-server)), not `sysadmin`, except
  for `sa` of a local container.
- MySQL/MariaDB: the global `PROCESS` privilege is optional. With it, a
  fingerprint after a request re-reads only the tables written to (per-table
  counters: MariaDB `userstat`, MySQL `performance_schema`), as it can see
  through `SHOW ENGINE INNODB STATUS` whether a transaction is still open.
  Without it the result is the same, but every fingerprint checksums all
  tables, which is slower on a large database.
- Fingerprints are comparable only on the same database server version.
  They cover tables (definition and rows), views, triggers and sequences;
  triggers and views with the collation they were made with, which a
  restore keeps (MySQL/MariaDB). PostgreSQL also: functions and procedures,
  enum types with their values in order, domains, schemas, materialized
  views with their rows, row security and policies (objects of extensions
  are left out); SQL Server: see [SQL Server](#sql-server). A request that
  changes one of them is a side effect, and the next restore brings it
  back.
- Triggers and views (MySQL/MariaDB) and the PostgreSQL objects above are
  fingerprinted in the current format. A pristine copy or a baseline whose
  fingerprint has an older format keeps working while none of them differs;
  otherwise the capture stops with exit 3 and says so: run `driftgate
  seed`, and when the message names the baseline, `driftgate verify
  --baseline <name> --migrate-state` (see [verify](commands.md#verify)).
- MySQL 8: a column that takes its table's collation without naming it
  (any such column of a table whose collation is not its character set's
  default, as `utf8mb3_unicode_ci` in a Laravel 5.5 table) keeps it that
  way through a seed and a restore. Up to v1.0.2 the copy named it, so the
  copy printed `CHARACTER SET … COLLATE …` and a restore stopped with "left
  1 items different from the pristine copy"; the message then adds that the
  pristine copy no longer matches its record. That restore has already
  rewritten the working tables from the copy, so re-create the working
  database (migrations or your dump), then run `driftgate seed`.
- MySQL/MariaDB in a container: start the server with
  `--skip-name-resolve` (compose: `command: --skip-name-resolve`). Without it
  the server looks up the name of every client that connects, and a slow
  lookup makes each connection slow (measured: fingerprints of 622 requests
  took 29 minutes, 45 seconds with it). The official images set it in a
  file a project's own mounted configuration can replace: `mysql` in
  `/etc/my.cnf`, `mariadb` in `/etc/mysql/mariadb.conf.d/05-skipcache.cnf`. Accounts are then matched by address
  (`'app'@'%'`), not by host name.
- MySQL/MariaDB: DriftGate keeps up to 12 connections per database
  component open for the whole command, so fingerprints and restores do not
  connect again (3 components: 36, against the default `max_connections`
  of 151, MariaDB 10.3: 100).

#### restore_only

What the application writes on every request or outside requests (a
session or cache table, a job queue) has two other choices: `exclude`
(never compared, never restored: sessions and queued jobs carry over from
one capture to the next) or tracking (restored, but a side effect of
every request). `restore_only` restores such tables like the others and
compares them never:

- `driftgate seed` keeps their rows in the pristine copy (an excluded
  table's are left out) and records their fingerprint apart from the
  others'.
- A restore reads them every time and restores those that differ from
  the pristine copy's record, with the items that changed. They are no
  item of a fingerprint, so a write to them is no side effect and no
  difference from the baseline's state, and the check after the restore
  leaves them out (the application may write them at any moment). A
  write to them never hides a change to another table: the other tables
  are fingerprinted as without them.
- A table is excluded or restore_only, not both, and not under
  `row_tracking`; names follow `exclude` (globs; SQLite and SQL Server
  without letter case, PostgreSQL and SQL Server with or without the
  schema). PostgreSQL: the sequences of their `serial` and identity
  columns are restore_only too. SQLite: the shadow tables of a
  restore_only virtual table are; those of a fingerprinted one never are,
  whatever their names. A trigger of a restore_only table stays an item.
- SQL Server: a restore of the whole database takes them from the
  pristine copy's backup; they are not saved and copied back as excluded
  tables are.
- `restore_empty` implies `restore_only`: everything above holds for its
  tables, whose pristine content is no rows (a cache). The seed leaves their rows out of the pristine
  copy, as an excluded table's (the cache the developer's application
  filled, entries that never expire, do not become the data every
  capture starts from); every restore empties them when they have rows.
  A table that refers to their rows by a foreign key stops the seed on
  SQL Server and PostgreSQL.
- Changing `restore_only` or `restore_empty` needs `driftgate seed` (a
  capture says so): the pristine copy holds the rows of the tables
  restore_only names, not of excluded or restore_empty ones. A table
  moved from tracking to them is no longer in the state a baseline was
  captured from: make the baseline again (a capture says so).

The check costs one read of those tables per restore; restoring them all
every time instead was measured slower (sessions of 200 rows, a cache of
1,000 and jobs of 50, next to twenty tables of 100 rows; check and
restore of all three: SQLite 0.58 ms and 0.88 ms, MySQL 5.1 ms and 73 ms,
PostgreSQL 17 ms and 53 ms).

**PostgreSQL** restores by copying the whole database, so a write to a
`restore_only` table alone makes the next restore copy it all, where an
excluded table is left as it is: about 53 ms for a small database, 0.2 s
for 20 MB, seconds for gigabytes, at every restore after such a write
(with `--restore each`, before every unit). `restore_empty` tables alone
are emptied in place instead (`TRUNCATE … RESTART IDENTITY`, waiting at
most 2 s for a lock, then checked against the pristine copy). The whole
database is copied instead when a session holds a lock on them longer
(a transaction left open after it read the cache; the copy ends it), a
table has a `TRUNCATE` trigger, or the check fails. A seed refuses a
restore_empty table with a `TRUNCATE` trigger, and one that a table
which is neither restore_empty nor excluded inherits from: 54 ms against
244 ms for 22 MB, independent of the database's size. Use `restore_only`
there for tables that change rarely.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { dotenv: .env }
    working: shop
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
```

#### The framework's own tables

`init` and `setup` propose, for every driver, the tables the framework
keeps its own state in, from the project's settings. `setup` asks
(default yes; without a terminal it needs `--yes`, or stops). Excluded:
what a reset would disturb while DriftGate runs and no response depends
on (the developer's own session in the browser, a running queue worker,
monitoring). `restore_empty`: a cache, whose entries (the counters of a
rate limit too) would otherwise carry over from one capture to the next
and change its responses.

| Framework | `exclude` | `restore_empty` |
|---|---|---|
| Laravel (`artisan`), the setting of the default connection | `sessions` and `jobs` when `SESSION_DRIVER` and `QUEUE_CONNECTION` (`QUEUE_DRIVER` before Laravel 5.7) are `database` on that connection, or unset with `database` as the default (`config/session.php`, `queue.php`, else the framework's since Laravel 11), or the tables `SESSION_TABLE` and `DB_QUEUE_TABLE` name; `job_batches`, and `failed_jobs` unless `QUEUE_FAILED_DRIVER` is `null`, `file` or `dynamodb`; Telescope's (`telescope_entries`, `telescope_entries_tags`, `telescope_monitoring`) and Pulse's (`pulse_values`, `pulse_entries`, `pulse_aggregates`) when `composer.json` requires them | `cache` and `cache_locks` when `CACHE_STORE` (`CACHE_DRIVER` before Laravel 11) is `database` in the same way, or the tables `DB_CACHE_TABLE` and `DB_CACHE_LOCK_TABLE` name |
| Django | `django_session`, with database sessions | the `LOCATION` of a `DatabaseCache` |
| Rails, Solid tables in the primary database (`db/schema.rb` or `db/structure.sql`; a new Rails 8 application keeps them in databases of their own in production) | `solid_queue_*`, `solid_cable_messages`; `sessions` with `activerecord-session_store` in the `Gemfile` | `solid_cache_entries` |
| ASP.NET (every `.csproj` of the solution, in the root and up to three folders below it) | Hangfire's schema (`HangFire.*` on SQL Server with `Hangfire` or `Hangfire.SqlServer`, `hangfire.*` on PostgreSQL with `Hangfire.PostgreSql`); `DataProtectionKeys` with `Microsoft.AspNetCore.DataProtection.EntityFrameworkCore` | |

#### Database DSNs

The DSNs of `state:` and `data:` hold the password, so they come from the
environment, 1Password or the project's own settings file, and no error
message shows them. A DSN is read only by a command that works with the
database (`seed`, `capture`, `run`, `setup`, and the commands that run
or query it); `compare`, `report`, `review`, `accept` and `expect`
read captures only, so on a machine where the variable is not set or the
settings file is not there they still run (the YAML itself is checked).
Percent-encode
special characters (`@ : / ? # %`) in the user name and password. The
database named in the URL is ignored (`working`, `pristine` and
`database` name it). A `mysql://` (or `mariadb://`) URL takes these
parameters; any other stops with an error instead of being ignored:

| Parameter | Meaning |
|---|---|
| `tls` | `true` (verify the certificate and the host name), `skip-verify` (encrypt without verifying), `preferred` (encrypt when the server can), `false` |
| `ssl-mode` | as the `mysql` client: `DISABLED`, `PREFERRED`, `REQUIRED` (encrypt without verifying), `VERIFY_CA` and `VERIFY_IDENTITY` (both verify the certificate and the host name) |
| `ssl-ca` | a PEM file of the certificate authorities to verify the server with; implies verification |
| `timeout`, `readTimeout`, `writeTimeout` | durations such as `10s` |

`mysql://app:secret@db.shop.example:3306/?tls=true` connects only over
verified TLS. The driver's own form (`user:pass@tcp(host:3306)/?tls=true`)
takes the driver's parameters. A `postgres://` URL takes PostgreSQL's
(`sslmode`, `sslrootcert`, …), a `sqlserver://` URL go-mssqldb's TLS
parameters ([SQL Server](#sql-server)).

An `env` or `op` DSN may also be a JDBC URL (`jdbc:mysql://…`,
`jdbc:postgresql://…?user=…&password=…`), an ADO.NET connection string
(`Server=…;Port=…;Database=…;Uid=…;Pwd=…`, Npgsql's
`Host=…;Username=…;Password=…`, SqlClient's `Server=tcp:host,1433;…`, an
ODBC string of SQL Server), as .NET reads `ConnectionStrings__Default`
from the environment, or libpq's keyword form (`host=… dbname=… user=…
password=…`); it is converted as below. A connection string that names no
server (`Server=`, `Host=`) is refused rather than taken for a local one:
a `;` in a password would otherwise cut it short. A `key=value;…` string
that names a server is read as a connection string before anything else,
also when its password holds `://`, `@/` or `@tcp(`.

**From the project's own settings.** Instead of a copy of the DSN,
`dsn:` can name the file where the application keeps its database
settings (`driftgate init` and `driftgate setup` write it from what they
find). DriftGate reads it each time a command runs, so the password
stays where it is and a change of the file is followed. The file is
relative to the project root and must be inside it (`\` is taken for `/`),
also past symbolic links: a link that leads outside the project folder
(also a compose `env_file`) is refused. Nothing of the project runs: PHP,
Python and ERB are read as text. When the setting names a database, it
must be `working`: a command that finds another one (`DB_DATABASE`
changed in a worktree) stops with exit 2 and names both, rather than
restore a database the application no longer runs on.

| Source | Reads | `key` |
|---|---|---|
| `dotenv: .env` | a `.env` file: without `key`, Laravel's `DB_CONNECTION`, `DB_HOST`, `DB_PORT`, `DB_DATABASE`, `DB_USERNAME`, `DB_PASSWORD`, `DB_SOCKET` (MySQL only; 5.5 and later), with `DB_URL` (11 and later) or `DATABASE_URL` (5.8 to 10) over them when set, as Laravel merges them; with `key`, the URL in that variable (`DATABASE_URL`, `DB_URL`, Prisma's). `MYSQL_ATTR_SSL_CA` turns on TLS verified with that CA, as Laravel's `PDO::MYSQL_ATTR_SSL_CA` does. Without `DB_CONNECTION`, a NestJS setting (TypeORM, MikroORM, Sequelize): `DB_TYPE` with `DB_HOST`, `DB_PORT`, `DB_USERNAME`, `DB_PASSWORD` and `DB_DATABASE` or `DB_NAME`; `TYPEORM_CONNECTION` with `TYPEORM_HOST`, `TYPEORM_PORT`, `TYPEORM_USERNAME`, `TYPEORM_PASSWORD`, `TYPEORM_DATABASE`; `DATABASE_TYPE` with `DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_USERNAME`, `DATABASE_PASSWORD`, `DATABASE_NAME`. The type names the driver (`mysql`, `mariadb`, `postgres`, `sqlite`, `better-sqlite3` with the file in the database key, `mssql` as tedious, which encrypts and verifies: for a self-signed certificate give `state:` a DSN of its own with `TrustServerCertificate=true`); a type that names no database makes no Node setting. A key it reads that the file sets twice to different values is an error naming the lines: Laravel 5.5 to 5.7 take the first, later versions the last. `init` and `setup` offer the files the framework reads: Laravel (`artisan`) only `.env`, or `.env.<APP_ENV>` when `APP_ENV` is set in the environment; Symfony (`bin/console`) `.env.<APP_ENV>.local`, `.env.<APP_ENV>`, `.env.local` (not for `test`) and `.env` (`APP_ENV` from the environment, else the files, else `dev`); others `.env.development.local`, `.env.local`, `.env.development` and `.env`. Files that name different databases stop `setup --yes` | optional |
| `appsettings: src/Api/Api.csproj` | ASP.NET: for a `.csproj`, `appsettings.json`, then `appsettings.<environment>.json` (`ASPNETCORE_ENVIRONMENT`, `DOTNET_ENVIRONMENT`, default `Development`) and, in Development, the project's user secrets (`UserSecretsId`; `~/.microsoft/usersecrets/…`, on Windows `%APPDATA%\Microsoft\UserSecrets\…`); for `appsettings.Development.json`, that file over `appsettings.json`. For a `.csproj`, the launch profile `dotnet run` takes (`Properties/launchSettings.json`, the first with `commandName: Project`) sets the environment and its `ConnectionStrings__*` over the files. Comments and trailing commas are allowed | required: `ConnectionStrings:Default` (case does not matter) |
| `properties: src/main/resources/application.properties` | Spring (`spring.datasource.url`, `username`, `password`), Quarkus (`quarkus.datasource.jdbc.url`, …; `%dev.` keys win) and Micronaut (`datasources.default.*`), `.properties` or `.yml`, as Spring Boot reads them: `application.*` in the file's folder, then `application-<profile>.*` for each active profile (`SPRING_PROFILES_ACTIVE`, else `spring.profiles.active`) with the profile of the named file last; `.properties` wins over `.yml`; a document with `spring.config.activate.on-profile` (YAML, or a `.properties` part after a `#---` or `!---` line) counts when its profile is active; `init` and `setup` offer only `application.*` and the files of the active profiles. `${NAME}` and `${NAME:default}` take the properties, then the environment | optional: the prefix, default the first of `spring.datasource`, `quarkus.datasource`, `datasources.default`, `spring.r2dbc` the files set |
| `yaml: config/database.yml` | Rails' `database.yml` (with `<<: *default`; `DATABASE_URL` of the environment over it, as Rails merges it; in ERB only `ENV['X']`, `ENV.fetch('X')`, `ENV.fetch('X', default)`, `ENV.fetch('X') { default }` and `ENV['X'] \|\| default`), or a Go service's `config.yaml`: a URL, or `adapter`/`driver`, `host`, `port`, `database`/`name`/`dbname`, `username`/`user`, `password`, `socket` | default `development` for `database.yml`, else the first of `database`, `db`, `datasource`, `mysql`, `postgres`, `postgresql`; dots separate levels (`development.primary`) |
| `php: app/etc/env.php` | Magento: the array the file returns (`host`, also `host:port`, `dbname`, `username`, `password`) | default `db.connection.default` |
| `python: shop/settings.py` | Django: the literal `DATABASES = {…}` (`ENGINE`, `NAME`, `USER`, `PASSWORD`, `HOST`, `PORT`) | default `default` |
| `compose: compose.yaml` | a MySQL, MariaDB, PostgreSQL or SQL Server ([below](#sql-server)) service (by its image) of a compose file with its override: the port it publishes on this machine and its user from `MYSQL_*`/`MARIADB_*`/`POSTGRES_*` (also through `env_file` and `${VAR}` from the environment and the `.env` next to the file). MySQL and MariaDB: `root` when the root password is set, as DriftGate creates the pristine database | required: the service name |

- `host: 127.0.0.1:3307` replaces the setting's host and port: the address
  of the database from this machine. `host: /path/to/mysqld.sock` (or a
  PostgreSQL socket folder) takes a socket instead. `host: compose:mysql`
  names a service of the project's compose file: the setting's host is
  that service (`DB_HOST=mysql`), reached at the port it publishes on this
  machine, read when a command runs, so a changed `FORWARD_DB_PORT` is
  followed.
- MySQL's `localhost` or no host, and PostgreSQL's no host, are the local
  socket for PDO, mysql2, mysqlclient and libpq, and TCP for others:
  DriftGate does not guess. A `dotenv`, `yaml`, `php` or `python` setting
  with such a host needs `host:` (`127.0.0.1:3306`, or the socket's
  path); without it a command stops and says what to add, and `setup
  --yes` stops with the exact `dsn:` to write (at a terminal it asks).
  JDBC, .NET and compose settings are TCP, and Laravel's missing
  `DB_HOST` is `127.0.0.1`, as its `config/database.php` says.
- The setting becomes a DSN as above, and so does its TLS, in any form
  (`sslmode`, `SslMode`, `SSL Mode`, JDBC `useSSL`/`requireSSL`/
  `verifyServerCertificate`/`sslMode`, pgJDBC `ssl=true` as
  `verify-full`, Prisma's `sslaccept=strict` as `verify-full` and
  `accept_invalid_certs` as `require` (its `sslcert` being the CA),
  `sslca`/`sslrootcert`; `sslcert`/`sslkey` for PostgreSQL only; Prisma's
  `sslidentity` is refused). Prisma's MySQL `?socket=` is the socket. A TLS setting DriftGate cannot pass on is an error,
  never left out. Other parameters (`charset`, `serverVersion`, Prisma's
  `schema`) are left out, except PostgreSQL's `application_name`,
  `connect_timeout` and `target_session_attrs`; so is the database
  (`working` and `database` name it).
- A file that names a driver other than the component's is an error
  (an SQLite file for a `mysql` component), as is a value only code
  computes (`os.environ[...]` in `settings.py`, other Ruby in ERB): put the
  DSN in a variable and use `{ env: NAME }`.
- `.env` files are read as phpdotenv, Node's dotenv and godotenv agree:
  `export`, `'…'` literal, `"…"` with `\n` `\"` `\$` escapes, values over
  several lines in quotes, `#` comments after a space, `${NAME}`,
  `${NAME:-default}` from earlier lines or the environment; a `$` without
  a closed `${…}` is kept (`pa$word`, `pa${ss`). The file's value counts, also when the
  environment sets the same name: for that, use `{ env: NAME }`.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { dotenv: .env, host: "compose:mysql" }   # Laravel Sail: DB_HOST=mysql
    working: shop
  reports:
    driver: postgres
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Reports" }
    working: reports
```

**Anything else** (Redis, files, a search index): `restore` (required) and
optionally `fingerprint`, a command whose output must be the same after
every restore ([recipes](guides/side-checks.md#files-on-disk-and-redis)).
A cache emptied on every restore (`FLUSHALL`) means every request runs
on a cold cache: behaviour that only shows with a warm cache (a value
read from the cache instead of the database) is never checked, and a
server that answers from a warm cache may differ from the capture for
that reason alone. Compare a live server's answer twice, or empty its
cache first, before you take such a difference for a change. `dsn`, `working`, `pristine`, `exclude`, `restore_only`,
`restore_empty` and `row_tracking` need a driver.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: postgres
    dsn: { env: DG_PG_DSN }
    working: app
    pristine: app_pristine
  files:
    restore: ["rsync", "-a", "--delete", "storage-pristine/", "storage/app/"]
    fingerprint: ["sh", "-c", "find storage/app -type f | sort | xargs shasum"]
```

A catalog with requests that change data needs at least one component with
a restore; otherwise `capture` and `run` refuse it (exit 2) unless
`--allow-mutating-without-restore`.

#### SQLite

An SQLite database is a file of the project. DriftGate has SQLite built
in: [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) with
SQLite 3.54.1, SQLite's own C code translated to Go, so the binary needs
no CGO and locks the file as the application's SQLite does. Any SQLite 3
file works, in WAL or rollback-journal mode. CI tests it on Linux, macOS
and Windows with Python's `sqlite3` as the application in another
process.

| Key | Type | Meaning |
|---|---|---|
| `driver` | `sqlite` | |
| `working` | path, required | the database file the application opens, relative to the project root; it must exist when a command runs (the application's migrations make it) |
| `pristine` | path | its pristine copy; default a file of the local store (`pristine/<name>-<hash>.sqlite`, next to the record of its fingerprint), so every checkout and working file has its own. When set, another file inside the project (seed creates it, and its folder when that does not exist yet): not `working` and not one of its `-wal`, `-shm` and `-journal` files, also in letter case |
| `dsn` | a file of the project \| `{env}` \| `{op}` | optional: the setting the application reads (`{ dotenv: .env }`, see [database DSNs](#database-dsns)); the file it names must be `working`, so a changed `DB_DATABASE` stops a command (exit 2) instead of restoring a file the application no longer uses. An `env` or `op` value is a path, a `file:` URI or an `sqlite:` URL |
| `exclude`, `restore_only`, `restore_empty`, `row_tracking` | | as above; a row without a primary key is named by its rowid (`rowid=7`) |

`allow_remote` is refused: there is no server.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { dotenv: .env }       # DB_CONNECTION=sqlite; it must name working
    working: database/database.sqlite
    exclude: [sessions, cache, cache_locks, jobs, job_batches, failed_jobs]
```

**The file.** `working` and `pristine` are relative to the project root
and inside it, also past symbolic links. They, and a path from the
application's settings, are resolved as the system opens them: a `..`
after a symbolic link goes up from where the link leads, and a link out
of the project is refused. Refused too: an in-memory database (`:memory:`, `mode=memory`,
SQLAlchemy's `sqlite://`), a file outside the project, a network share
(`\\server\share\…`), a URL with a host (other than `file://localhost/`),
an encrypted file (SQLCipher, a `Password=` of Microsoft.Data.Sqlite) and
a file that `|DataDirectory|` names. A value in the MySQL driver's form
(`user:password@tcp(host)/db`, `user@/db`) is MySQL's even when the user
reads like a scheme (`file:`, `sqlite:`), so in a DSN or URL a folder
ending in `@` needs `file:///…` (a plain path does in `DB_DATABASE`,
`working:` and `--database`). A relative path is taken as the
framework takes it; where the application opens it from the folder it
runs in, `init` and `setup` add a note to check it:

| Setting | The file |
|---|---|
| Laravel `.env` (`DB_CONNECTION=sqlite`) | `DB_DATABASE`, or the file of `DB_URL` / `DATABASE_URL`; unset, `database/database.sqlite`. A relative `DB_DATABASE` is taken from the project root, with a note: Laravel 11 and later open it from the folder PHP runs in when it is there, else from the root; Laravel 10 and earlier only from the folder PHP runs in (`public/` under `php artisan serve` and PHP-FPM). Leave it unset or make it absolute |
| Django `settings.py` (`django.db.backends.sqlite3`) | `BASE_DIR / 'db.sqlite3'`, `str(BASE_DIR / …)` or `os.path.join(BASE_DIR, …)`, with `BASE_DIR` as `startproject` writes it (`Path(__file__).resolve().parent.parent`, or `os.path.dirname` of `__file__`); a relative string from `manage.py`'s folder. Other code is not run: set `working:` |
| Symfony `.env` (`DATABASE_URL="sqlite:///%kernel.project_dir%/var/data.db"`) | `%kernel.project_dir%` is the Symfony project's folder: the folder of the `.env` file (of the one above `config/` for a file under it) |
| Prisma (`provider = "sqlite"` in `prisma/schema.prisma`) | the `file:` URL of the variable its `url = env("…")` names, relative to `prisma/`, as Prisma takes it |
| Rails `config/database.yml` (`adapter: sqlite3`) | relative to the Rails root; every SQLite database of `development` |
| ASP.NET `appsettings.json` (`Data Source=app.db`) | relative to the `.csproj`'s folder, where `dotnet run` runs |
| any other | relative to the project root |

- **Rails 8** has a file per database (`primary`, `cache`, `queue`,
  `cable`; a new application splits them in production only). Each file
  of `development` is a component of its own; `init` and `setup` write
  the primary and list the others. The cache, queue and cable databases (Solid Cache, Solid Queue, Solid Cable) change without a
  request changing data: leave them out of `state:`, or exclude their
  tables (the cache's may be `restore_empty`).
- **The same machine.** SQLite's locks keep a restore and the
  application's writes apart, so the file must be on a local disk of the
  machine DriftGate runs on, opened by an application on that machine.
  They do not hold across network file systems (NFS, SMB), and Docker
  Desktop's file sharing (macOS, Windows) is not documented to keep them:
  `init` and `setup` note a compose service that mounts the file's folder
  into its container. Run the application on this machine
  (or on Linux, where a bind mount is the same file system).

**Restore, in place.** A restore writes inside the working file, in one
transaction, as the application's own SQLite does; the file is never
copied, renamed or deleted, nor are its `-wal`, `-shm` and `-journal`
files. The application's open connections keep the file they opened: a
copy or a rename over it would leave them reading and writing the old
file (a pass on data that is not the restored data, and lost writes), with
`-wal` and `-shm` files that no longer match the file.

- Only the items that differ are restored, in one `BEGIN IMMEDIATE`
  transaction with the pristine file attached read-only, so the
  application sees all of a restore or none of it. A table with the
  pristine definition is emptied and filled again with `INSERT … SELECT`,
  which SQLite copies as raw records (a table without an `INTEGER PRIMARY
  KEY` with its hidden rowids, so they are kept), and gets its
  `sqlite_sequence` value back; a changed table is copied whole. A table
  whose definition (its or its indexes' `CREATE` statement) differs is
  dropped and created from the pristine file; views and triggers are made
  again from it, and `user_version` and `application_id` set to its own.
- Nothing a restore does runs the application's logic: foreign keys are off
  on DriftGate's connections, so deletes do not cascade, and the triggers
  of the tables it writes are dropped before and created again after,
  inside the transaction. Excluded tables, SQLite's statistics and the
  mark of the pristine copy are not touched.
- A working file replaced on disk (deleted and made again, a copy written
  over it) is noticed at the next fingerprint, restore or seed: DriftGate
  opens the new file and reads it in full.

**Seed.** `driftgate seed` copies one moment of the working file
(`VACUUM INTO`) into `<pristine>.driftgate-new`, marks it (a table
`driftgate_pristine`), empties its excluded tables (with
`secure_delete`, so the rows of sessions and tokens do not stay in its
free pages), sets it to rollback mode (`DELETE`), writes it to disk and
renames it over the pristine file; a seed that stops before leaves the
old copy. The working file is only read. A file at the `pristine` path is
replaced only when it has the mark, is empty, or is the one DriftGate's
record names; any other is someone's data, and seed stops and names it.

**Fingerprint.** It covers `table:<name>` (its `CREATE` statement, those
of its indexes, its `sqlite_sequence` value and its rows), `view:`,
`trigger:`, `pragma:user_version` and `pragma:application_id`. Every
value is hashed with its storage class, so `1`, `1.0`, `'1'` and `x'31'`
differ, as do NULL and `''`; a generated column counts by its definition.
A virtual table (FTS5, R*Tree) counts by its `CREATE` statement, its rows
by its shadow tables; a module DriftGate's SQLite does not have stops a
restore with the advice to exclude the table. Not covered: SQLite's
statistics (`sqlite_stat1` …, which `ANALYZE` and `PRAGMA optimize`
write), temporary objects, excluded tables and the mark. A trigger of an
excluded table is covered: its body may write any table.

A fingerprint is taken at one of three costs; the first one of a command
reads every table:

| Fingerprint | When | 1 MB | 50 MB | 500 MB |
|---|---|---|---|---|
| reused | nothing committed since the last one (`PRAGMA data_version`, the schema version, the file's identity, size and time, and no restore of DriftGate's own) | 22 µs | 28 µs | 54 µs |
| page digests | something was written, but every table's pages hash as at its last reading (only an excluded table was written) | 0.87 ms | 30 ms | 302 ms |
| rows | the tables whose pages changed are read again (one row of the large table) | 3.0 ms | 150 ms | 1.13 s |
| full | every table read | 3.5 ms | 167 ms | 1.67 s |

Seed (1, 50, 500 MB): 12.8 ms, 124 ms, 1.33 s. Restore of a small table with one changed
row: about 0.35 ms; of a table of about 490 MB: 4.5 s. Measured on an
Apple M4 in WAL mode, on a file with one large table with an index,
twenty small tables and an excluded session table. A database session or
cache table written on every request moves `data_version` every time: the
page digests then keep the fingerprint from reading the other tables.

**Locks.** DriftGate's connections wait up to 10 s for a lock the
application holds (`busy_timeout`), and never change the file's journal
mode or locking mode or checkpoint its WAL.

- A fingerprint first waits for a write the application has under way
  (`BEGIN IMMEDIATE`); when one committed meanwhile, it looks again after
  5 ms, up to 20 times, so a request that ends with several transactions
  is seen whole. Holding the write lock only for that moment, it starts
  read transactions on up to five connections, which then read one moment
  of the file while the application goes on.
- In WAL mode the application's writers are not held up by a fingerprint
  or a seed. In rollback mode (`DELETE`, SQLite's default, which Laravel
  and Django keep; Rails sets WAL) a reading transaction keeps writers
  from committing: for the duration of a scan or of a seed's copy, an
  application write waits (or, without a busy timeout, fails with
  `database is locked`). Exclude what background workers write, or switch
  the file to WAL.
- A restore is one write transaction: the application's writers wait for
  it, and its readers see the data before or after it.

**Exclude** what the framework writes on every request or outside
requests, in the same file: sessions, jobs; a cache is `restore_empty`.
`init` and `setup` propose them ([the framework's own
tables](#the-frameworks-own-tables)). An excluded table keeps its
definition in the pristine copy, without rows. `exclude` and
`restore_only` and `restore_empty` may name tables the file does not have.

**Queries** (`params` `sql:`) run on the pristine file, opened read-only
(`mode=ro`, `query_only`, defensive mode), on a connection that lets only
reads compile. Besides what the other drivers refuse, `ATTACH`, `DETACH`,
`PRAGMA`, `VACUUM`, `REINDEX`, the tables that read the file's pages
(`sqlite_dbpage`, `dbstat`, `sqlite_dbdata`, `sqlite_dbptr`) and the
functions `load_extension`, `fts3_tokenizer`, `readfile`, `writefile` and
`edit` are refused. A `DATETIME` value comes back as the text SQLite
holds.

#### SQL Server

DriftGate has SQL Server built in: the core of
[go-mssqldb](https://pkg.go.dev/github.com/microsoft/go-mssqldb), a
pure Go driver, so the binary needs no CGO and no client library. It
supports SQL Server 2017 and later (Developer, Express, Standard and
Enterprise, on Linux, Windows or in a container), not Azure SQL Database,
Azure SQL Managed Instance or Synapse, whose databases cannot be backed up
to a file and restored from it. CI tests SQL Server 2017, 2019, 2022 and
2025 (Developer, on Linux in Docker); SQL Server on Windows is not tested
there. SQL Server 2017 makes itself a certificate with a negative serial
number, which Go refuses to read by default (also with
`TrustServerCertificate`), so no connection to it would work. When a
command makes its first SQL Server component, DriftGate turns on
`GODEBUG=x509negativeserial=1` for its own process: from then on, for
the whole process (the target's HTTPS included, and the commands it
starts, which inherit it), such a certificate is read; its chain and host
name are verified as any other's. A project without SQL Server never has
it, and an `x509negativeserial` you set in `GODEBUG` yourself is kept.

| Key | Type | Meaning |
|---|---|---|
| `driver` | `sqlserver` (`mssql` is accepted) | |
| `dsn` | `{env}` \| `{op}` \| a file of the project | the server and an SQL login: `sqlserver://user:pass@host:1433?encrypt=true`, an ADO.NET, ODBC or JDBC connection string, or the application's setting (`{ appsettings: … }`, `{ dotenv: .env }`; see the application's settings below) |
| `working` | string, required | the database the application uses; names are compared without letter case, as SQL Server does |
| `pristine` | string | its pristine copy, a database on the same server; default `<working>_pristine`; not `working` in any letter case, at most 122 characters (DriftGate makes `<pristine>_dgnew` and `<pristine>_dgx` next to it) |
| `exclude` | list of globs | as above; a glob matches `schema.name` or `name`, without letter case. It also leaves out views and sequences of those names |
| `restore_only`, `restore_empty` | list of globs | as above; matched as `exclude` matches tables (views and sequences are not restore_only) |
| `row_tracking` | list of tables | as above; `schema.name`, or `name` in `dbo`; the table needs a primary key |
| `allow_remote` | bool | as above |

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlserver
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Default" }
    working: Shop
    exclude: [dbo.SessionCache, "HangFire.*"]
```

**The login.** DriftGate signs in with an SQL Server login (a user name
and password; the server must allow SQL Server authentication, as the
container images do). Windows accounts (`Integrated Security`,
`Trusted_Connection`, `DOMAIN\user`), Kerberos and NTLM, and Microsoft
Entra ID (`Authentication=ActiveDirectory…`) are refused by name, as the
driver is built without them. The login needs:

- the server role `dbcreator`: it creates the pristine copy, renames and
  drops the copies it made (it owns them), and restores the working
  database from a backup. `dbcreator` reaches every database of the
  server, not only these: its login may alter any of them (take it
  offline, set it to single user), restore over any of them (`RESTORE …
  WITH REPLACE`) and drop any of them, though it cannot read their data;
- `db_owner` in the working database (or to own it): it backs it up and
  restores it, table by table and, when it must, as a whole.

`sa` or another `sysadmin` needs nothing more; use it only for a local,
disposable server, such as a container on this machine. `driftgate
setup` prints what an administrator runs for any other login (every
statement may run again):

```sql
USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [shop];
USE [Shop];
IF SUSER_SID(N'shop') NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER [shop] FOR LOGIN [shop];
IF SUSER_SID(N'shop') <> (SELECT sid FROM sys.database_principals WHERE name = N'dbo') ALTER ROLE [db_owner] ADD MEMBER [shop];
```

A narrower grant, which `setup` prints too: instead of both, the right to
create databases and the ownership of the working database.

```sql
USE [master]; GRANT CREATE ANY DATABASE TO [shop]; ALTER AUTHORIZATION ON DATABASE::[Shop] TO [shop];
```

The login then reaches only the databases it owns, the working database
and the copies it makes: seeds, restores item by item and as a whole
work, and altering, restoring over or dropping any other database is
refused. It is not the default because it changes the working
database's owner, which an application may rely on (the login becomes
`dbo` there; whatever ran as the old owner, such as `EXECUTE AS OWNER`
or cross-database ownership chaining, changes with it). It fails (error
15110) while the login has a user in that database: drop it first
(`USE [Shop]; DROP USER [shop];`; as `dbo` the login needs none). Either
grant lets the login restore any backup file the server can read into a
new database of its own, and so read it: keep the backups of other
databases out of the server's reach, its backup folder included.

Optional, and proposed by `setup` with it: `GRANT VIEW SERVER
PERFORMANCE STATE TO [shop];` (SQL Server 2022 and later; `VIEW SERVER
STATE` on 2017 and 2019). With it a fingerprint of data that did not
change is reused, and one after a change reads only the tables written to
(see the fingerprint below); a fingerprint
also waits for the application's open transactions. Without it the result
is the same, but every fingerprint reads every table, which takes longer
on a large database; the log says so once. The login's own permissions
are the real limit of what DriftGate's queries can do
([security](security.md)). Either permission also lets the login itself
read every session of the server and the SQL text it runs or ran
(`sys.dm_exec_requests`, the plan cache), literals included; DriftGate's
queries refuse those views, but grant it only to a login the server's
other users may trust with that.

**DSN and TLS.** DriftGate's DSN is go-mssqldb's URL,
`sqlserver://user:pass@host:1433?encrypt=true`; a DSN of the environment
or 1Password may also be an ADO.NET connection string
(`Server=tcp:db,1433;Database=Shop;User Id=shop;Password=…`), an ODBC one
(`Driver={ODBC Driver 18 for SQL Server};Server=…`) or a JDBC URL
(`jdbc:sqlserver://db:1433;databaseName=Shop;user=…;password=…`). Each is
read and the URL written anew from what DriftGate reads: go-mssqldb
ignores a parameter it does not know, so a mistyped `encrypt` would
otherwise go unnoticed. A URL parameter DriftGate does not know, one
given twice, and a `;` in the query (percent-encode it as `%3B`) are
refused. The URL's database is ignored; `working` names it. A go-mssqldb
URL of the environment or 1Password is written for DriftGate, not taken
from an application: without `encrypt` it is encrypted and the
certificate verified (`encrypt=true`); for a self-signed certificate add
`TrustServerCertificate=true`.

The TLS DriftGate connects with is never weaker than the application's
own client. `Encrypt` (`true`, `yes`, `Mandatory`; `false`, `no`,
`Optional`; `Strict`), `TrustServerCertificate`, `HostNameInCertificate`,
a minimum TLS version and certificate files are passed on; a value
DriftGate cannot map is an error, never left out. When the setting leaves
`Encrypt` unset, the client's default is taken:

| Client | Told by | `Encrypt` unset: DriftGate connects with |
|---|---|---|
| Microsoft.Data.SqlClient 4 and later | its `PackageReference` in the `.csproj` | `encrypt=true` (encrypted, certificate verified) |
| Microsoft.Data.SqlClient before 4 | the `.csproj` | `encrypt=false` |
| EF Core (`Microsoft.EntityFrameworkCore.SqlServer`) 7 and later, before 7 | the `.csproj` | `encrypt=true`; before 7 `encrypt=false` |
| System.Data.SqlClient | the `.csproj` | `encrypt=false&TrustServerCertificate=true` |
| ODBC Driver 18 and later | `Driver=` (also Django's `OPTIONS` `driver`) | `encrypt=true` |
| ODBC Driver 17 and earlier, SQL Server Native Client, the old `SQL Server` driver | `Driver=` | `encrypt=false&TrustServerCertificate=true` |
| mssql-jdbc (10.2 and later) | `jdbc:sqlserver://` | `encrypt=true` |
| Prisma | `sqlserver://host:1433;database=…` | `encrypt=true` |
| tedious, node-mssql | `mssql://` | `encrypt=true` |
| go-mssqldb | `sqlserver://user:pass@host:1433` in a Go application's own setting file | `encrypt=false&TrustServerCertificate=true` |
| FreeTDS (its ODBC driver, `dblib://`, Rails' TinyTDS), pdo_sqlsrv (Laravel), pyodbc without a known driver | | `encrypt=true`: `freetds.conf` or the ODBC driver decides, which DriftGate does not read |
| a go-mssqldb URL or an ADO.NET connection string of the environment or 1Password, any other | | `encrypt=true` |

- **`Encrypt` off.** DriftGate still encrypts the sign-in, as every
  client does, and never signs in without TLS (`encrypt=disable`,
  Prisma's `DANGER_PLAINTEXT` are taken as off): a server that answers it
  cannot encrypt (`ENCRYPT_NOT_SUP`, which go-mssqldb alone would answer
  by sending the password in clear) is refused before the password is
  sent. After the sign-in the data is not encrypted. go-mssqldb verifies
  the certificate the sign-in is encrypted with. For the clients that
  never verify it with `Encrypt` off (System.Data.SqlClient, ODBC Driver
  17 and earlier, a Go application's go-mssqldb setting without
  `encrypt`, FreeTDS with `encryption = off` or `request`) DriftGate does
  not either
  (`TrustServerCertificate=true`); for the others, a server with a
  self-signed certificate needs `TrustServerCertificate=true` in the
  application's own setting, and `init` and `setup` say so.
- **A setting that does not say** (`Encrypt` unset, a client DriftGate
  cannot tell) is encrypted and verified; for a self-signed certificate,
  set `Encrypt` and `TrustServerCertificate` in the setting. FreeTDS's
  `encryption = require` encrypts without verifying unless a CA is given.
- **A compose service** on a loopback address of this machine is trusted
  with its own self-signed certificate (`encrypt=true&TrustServerCertificate=true`)
  unless the setting says otherwise; on any other address it is verified.
- **`Strict`** (TDS 8) always verifies and drops `TrustServerCertificate`.
  CI checks it on SQL Server 2025; SQL Server 2022 and 2019 on Linux close
  a TDS 8 connection, and DriftGate does not fall back to another
  encryption.
- **Certificate files**: a CA (go-mssqldb's `certificate`, Prisma's
  `trustServerCertificateCA`) or the server's own certificate
  (`ServerCertificate`, compared byte for byte; not together with
  `HostNameInCertificate` or a CA). A `.pem` or `.der` file (convert a
  `.cer` or `.crt` with `openssl x509`), a regular file. A setting of the
  project names one by a path relative to the project, which stays inside
  it (also past symbolic links), or by an absolute path or one in the home
  folder (`~/certs/ca.pem`) anywhere, such as a system folder
  (`/etc/ssl/certs`); a DSN of the environment or 1Password by an absolute
  path or one in the home folder. The driver only reads it as a
  certificate; no message shows what it holds. A Java trust store (`trustStore`) is refused: give
  the CA as a PEM file. JDBC's `sslProtocol=TLSv1.3` is `tlsmin=1.3`.

`tools/e2e/sqlserver-tls.sh` (run in CI for every version) starts SQL
Server with a certificate of a test CA and forced encryption, and checks
that a verified connection works and that a wrong CA, host or
`hostNameInCertificate` is refused without the password in any message.

**The application's settings.**
`init` and `setup` find SQL Server in the settings below and write the
`dsn:` that reads them ([from the project's own settings](#database-dsns));
the server is `host`, `host,port`, `tcp:host,port` or `host\instance,port`.
The local server (`.`, `(local)`, none) is taken as TCP to `127.0.0.1`,
with a note: a Windows client may reach it through shared memory.

| Framework | `dsn:` | Reads |
|---|---|---|
| ASP.NET, EF Core | `appsettings: <project>.csproj`, `key: "ConnectionStrings:<name>"` | the connection string, as above; the client from the `.csproj` (see the [ASP.NET Core guide](guides/aspnet-core.md)) |
| Laravel | `dotenv: .env` | `DB_CONNECTION=sqlsrv`, `DB_HOST` (also `host\instance` or `host,port`), `DB_PORT` (default 1433), `DB_DATABASE`, `DB_USERNAME`, `DB_PASSWORD`, `DB_URL` (`sqlsrv://`) over them. `DB_ENCRYPT` and `DB_TRUST_SERVER_CERTIFICATE` count only when `config/database.php` passes them on (Laravel ships those lines commented out); without a `config/database.php` a value that weakens TLS is left out, with a note |
| Spring, Quarkus, Micronaut | `properties: …` | `jdbc:sqlserver://host:1433;databaseName=…;encrypt=…` (also `serverName`, `portNumber`, `instanceName`) with the user and password keys |
| Django | `python: settings.py` | `ENGINE` `mssql` (mssql-django) or `sql_server.pyodbc`; `HOST` (also `host\instance`) and `PORT`; `OPTIONS` `driver` (whose default `Encrypt` counts) and `extra_params` (ODBC keys); an ODBC `dsn` is refused |
| Prisma | `dotenv: .env`, `key: DATABASE_URL` | `sqlserver://host:1433;database=…;user=…;password=…;encrypt=…` (a value with `;` in `{…}`) |
| Rails | `yaml: config/database.yml` | `adapter: sqlserver` (activerecord-sqlserver-adapter), `host`, `port`, `database`, `username`, `password` |
| Go | `yaml: config.yaml` | a go-mssqldb URL with `?database=…`; a path with a port is taken for the database, as Rails reads it, with a note (go-mssqldb takes it for an instance); a path without a port is refused |
| Node | `dotenv: .env`, `key: …` | `mssql://user:pass@host:1433/db` (tedious, node-mssql) |
| compose | `compose: compose.yaml`, `key: <service>` | an `mcr.microsoft.com/mssql/server` (or `mssql/rhel/server`) service: `sa` with `MSSQL_SA_PASSWORD` (or `SA_PASSWORD`), at the port it publishes for `MSSQL_TCP_PORT` (default 1433). The image names no database: `setup` takes the application's when its setting names the same server. A password from a file (`MSSQL_SA_PASSWORD_FILE`) and Azure SQL Edge are refused |

A key that names the server, database, user, password or port twice with
different values (`Server` and `Data Source`, `Database` and `Initial
Catalog`) is refused: the clients differ in which one wins. Microsoft's
ODBC drivers do not read `Port=` (write `Server=host,1433`); FreeTDS's
does.

**Refused**, each with a message that names it and no value:

- other sign-ins than an SQL login (above), and Always Encrypted (`Column
  Encryption Setting`), whose columns DriftGate cannot read;
- other ways to the server than TCP to one host and port: named pipes
  (`np:`, `Network Library`), shared memory (`lpc:`), LocalDB, the
  dedicated admin connection (`admin:`), an ODBC data source (`DSN=`), a
  `Failover Partner`, `ApplicationIntent=ReadOnly` (a read-only replica
  cannot be restored), `AttachDbFilename` and `User Instance`, and a named
  instance without its port (the SQL Server Browser would choose the port
  over UDP: add `,port`);
- servers older than SQL Server 2017, Azure SQL Database, Managed
  Instance and Synapse;
- as `working`: a system database (`master`, `model`, `msdb`, `tempdb`),
  and a database in an availability group, with change data capture, in
  replication (published, subscribed, a distributor) or a ledger
  database. Change tracking is noted, not refused: a restore resets the
  rows, not the change tracking versions an application syncs from;
- a server trigger on `CREATE`, `ALTER` or `DROP DATABASE`, which may
  refuse what a seed and a restore do: disable it while DriftGate runs;
- a table with row-level security (an enabled security policy with a
  filter or block predicate), which hides rows from DriftGate's reads or
  blocks its writes: turn the policy off in this database, or exclude the
  table;
- memory-optimized tables with `DURABILITY = SCHEMA_ONLY` that are not
  excluded (their rows are in no backup), and excluded memory-optimized
  tables whose rows are kept (`SCHEMA_AND_DATA`): SQL Server does not let
  their rows be saved through a restore of the whole database.

**The pristine copy** is a second database on the same server,
`<working>_pristine` by default, read-only (`READ_ONLY`) and marked as
DriftGate's by an extended property, `driftgate_pristine`, that names the
working database, the server (`@@SERVERNAME`) and the backup made of it.
A restore uses only a pristine copy with that mark for this working
database and this server, and stops when another seed replaced it while
DriftGate ran.

`driftgate seed`:

1. backs up the working database with `BACKUP … WITH COPY_ONLY` (the
   application's own backup chain stays as it was; compressed except on
   Express, with checksums);
2. restores that backup as `<pristine>_dgnew`, every file moved to a new
   name in the server's data folder (`dg_<hash>_<file>.mdf`, `.ndf`,
   `.ldf`), never over an existing database. A seed stopped while that
   `RESTORE` runs drops the copy, and the next seed drops one that is
   left; when the process is killed before SQL Server registered the copy,
   its `dg_*` files can stay in the data folder, which only a login with
   access to that folder can delete;
3. deletes the rows of the excluded tables there, without firing a
   trigger or running a cascade (a table that is not excluded and whose
   rows refer to rows of an excluded table by a trusted foreign key stops
   the seed: exclude it too, or drop the key);
4. backs `<pristine>_dgnew` up into one of two backup files in the
   server's default backup folder (`driftgate-<hash>-a.bak` and `-b.bak`;
   the data folder on SQL Server 2017), the one the current copy does not
   use, so that copy keeps a valid backup until the new one replaces it;
   a restore of the whole database restores from it;
5. shrinks the copy's log (the backup keeps the working database's log
   size; the read-only copy needs none of it), marks the copy, drops the
   old pristine copy, renames the new one and makes it read-only.

A seed that stops anywhere leaves the old copy, or none, and a
`<pristine>_dgnew` the next seed drops; a database under the `pristine`
name that DriftGate did not make (one with objects, schemas or users and
no mark) is never dropped. The working database is only read. The server
needs room for the pristine copy (about the working database without its
excluded rows) and the two backup files, during a seed also for the new
copy next to the old one, and during a restore of the whole database for
a copy of the excluded tables' rows: count about twice the working
database plus twice its excluded data. DriftGate cannot delete the backup
files (that needs `sysadmin`): when a project no longer uses DriftGate,
drop the pristine database and delete its two files yourself.

**Restore, item by item.** A restore brings back only the items that
differ, in one transaction on the working database (`XACT_ABORT`, a lock
wait of at most 30 s), so the application sees all of a restore or none of
it, and keeps its connections:

- the database's DDL triggers and the DML triggers of the tables it
  writes are disabled, and the foreign keys that refer to those tables
  (and the constraints the pristine rows need not meet, as they are not
  trusted) set to `NOCHECK`, so no trigger fires and no cascade runs;
- a table is emptied (`TRUNCATE` where SQL Server allows it, else
  `DELETE`) and its rows copied from the pristine copy with
  `IDENTITY_INSERT`; a table with a primary key and 5,000 rows or more
  gets only the rows that differ (deleted and copied by key, unless more
  than a quarter changed);
- identity columns are set back where the pristine copy's stand (`DBCC
  CHECKIDENT … RESEED`), sequences with `ALTER SEQUENCE … RESTART WITH`;
- constraints are turned on again as they were (`WITH CHECK` where the
  pristine copy's are trusted, disabled ones stay disabled), then the
  triggers; identity and sequence values are compared with the pristine
  copy's before the commit. Full-text indexes are waited for, up to a
  minute, while SQL Server fills them again.

A large restore in one transaction is logged in full: with the `FULL`
recovery model the working database's log keeps it until a log backup
and grows. A development database is best in `SIMPLE` recovery.

**Restore of the whole database.** Anything a restore cannot bring back
item by item restores the whole working database from the pristine
copy's backup instead (the log says why): a view, procedure, function,
trigger, type, synonym, schema, partition function or scheme, XML schema
collection, rule or default that changed; a table created, dropped or
with another definition; the `database:` item (users, roles,
permissions, database options); a memory-optimized, temporal, graph,
ledger, external or FileTable table, one with a column of Always Encrypted
or of a CLR type of its own; an identity column that cannot be set back
where the pristine copy's stands. Such a restore:

1. checks that the backup file is the one the mark names and that the
   working database still has the files it had at the seed;
2. saves the rows of the excluded tables, and the excluded sequences,
   into `<pristine>_dgx`, a database of DriftGate's own (marked);
3. ends every other session of the working database (`SET OFFLINE WITH
   ROLLBACK IMMEDIATE`; a `sysadmin` login ends them first, which saves
   about 3 s per session);
4. restores the backup `WITH REPLACE` onto the working database's own
   files. Its owner, `TRUSTWORTHY` and `DB_CHAINING` are kept (they live
   in `master`), Service Broker is turned on again when it was on, and the
   recovery model is set back;
5. copies the saved rows back, drops `<pristine>_dgx`, and checks every
   item against the pristine copy's fingerprint.

The application's pooled connections to the working database are ended:
the first use of each one can fail once (go-mssqldb reports `Read: EOF`,
other clients a closed connection), and the pool connects again. Such a
failure is usually a 500 from the application. DriftGate counts the
sessions of other programs the restore ends (not its own; it sees the
sessions of other logins only with `VIEW SERVER STATE`, and without it
counts only those of the application that signs in as DriftGate's own
login) and sends, in the read-only part of the same capture that
follows its first restore (or of the next capture, when it was the last
restore of a capture: the count is kept in the local store, at most 100,
with the component, its working database, a hash of its server's
address and the time, and used once, by a capture of the same database
within an hour; the store is shared by every checkout of the project),
as many requests answered with 5xx once more: a `GET` or
`HEAD` to a target on an allowed host, not a `repeat:`, a step of a
sequence or one the baseline knows writes (a `GET` can write). Each is
sent at most twice, the second answer is kept (a 5xx that stays is the
application's; only a load failure such as a `503` is then
[retried](#retries)), and every report notes it (`sent again …: the first
answer was 500, after the restore of db ended 3 database sessions of
the application`, and a count of such requests in every format). The
count is of every session of other programs the restore ended in the
working database, not only the application's pool: a queue worker or an
open SSMS window adds to it. Close tools such as SSMS, Azure Data Studio
or a database client of the IDE on the working database while you
capture: each of their sessions allows one more request to be sent again.
A request that changes data is never sent again; if
it is the first after such a restore and fails this way, let the
application's pool check a connection before it uses it, or keep no idle
connections. `prepare` runs right after the restore, before any request:
a command of its that asks the application over HTTP (a warm-up `curl`)
meets the ended connections first and stops the capture; run such a
step in a process of its own (`php artisan …`), or let it try again
(`curl -sf --retry 3`, which retries a `500`). From
step 2 until the rows are copied back the working database is
`RESTRICTED_USER`: the application cannot connect, so none of its writes to
excluded tables is lost. Limits of that:

- a login that is a member of `db_owner`, `dbcreator` or `sysadmin` still
  gets in. Of what it writes to excluded tables, rows committed after
  step 2 reads them and before step 3 ends its session are lost; rows it
  writes after the restore are kept, in a table with a primary key, and a
  saved row with the same key is not copied back. In a table without a
  primary key (or with a computed column) its rows are replaced by the
  saved ones. Let the application sign in with a login that is only a
  user of the database;
- a restore stopped while the database is `RESTRICTED_USER` (DriftGate
  killed) leaves it so until the next restore or capture lets every login
  in again (`ALTER DATABASE … SET MULTI_USER` does it by hand);
- a working database you made `RESTRICTED_USER` or `SINGLE_USER` yourself
  stays so. `RESTRICTED_USER` is kept by the RESTORE itself. `SINGLE_USER`
  is set again at the end of the restore of the whole database, after its
  check, and only while the database is still the `RESTRICTED_USER` the
  RESTORE left (ending a `db_owner` session that got in meanwhile); with
  excluded tables the user access is noted in `<pristine>_dgx`, so a
  later restore that completes a stopped one sets it again too, while
  without excluded tables there is no note, and a restore stopped halfway
  leaves the database as the pristine copy's backup is (`MULTI_USER`).
  A capture never gets that far with a `SINGLE_USER` database: it
  fingerprints the database before its first restore and refuses it
  there, since a fingerprint reads on several sessions, and says what to
  do (`SET MULTI_USER`).

Measured on small databases, such a restore takes about 0.8 s. A
restore of the whole database that stops halfway leaves the working
database restoring or offline, unusable until the next restore (a
capture) completes it; that one copies the saved rows back too, and a
seed refuses to run before it. A working database taken
offline by someone else is brought online first; in any other state,
with excluded tables whose rows DriftGate did not save, the restore stops
rather than lose them.

**Fingerprint.** It covers
`table:<schema>.<name>` (its columns with every attribute, identity and
where it stands, indexes, constraints with their trusted and disabled
flags, temporal, graph and memory-optimized settings, row-level security
policies, permissions, extended properties, and its rows), `view:`,
`procedure:`, `function:`, `trigger:` (a database's DDL trigger as
`trigger:database.<name>`), `sequence:` (with where it stands), `type:`,
`synonym:`, `schema:`, `rule:`, `default:`, `partition_function:`,
`partition_scheme:`, `xml_schema:`, and `database:`: the database's users
and roles with their members, the permissions and extended properties
that belong to no other item (on the database itself, on `dbo` and the
other built-in schemas), the options that change what a query does
(compatibility level, collation, `READ_COMMITTED_SNAPSHOT`,
`ALLOW_SNAPSHOT_ISOLATION`, the ANSI defaults, `QUOTED_IDENTIFIER`,
recursive and nested triggers, forced parameterization …) and the
database scoped configurations. A difference in `database:` restores the
whole database.

- A row is encoded on the server as its values' own bytes and hashed by
  DriftGate (SHA-256): case and trailing spaces count whatever the
  collation compares equal, as do a float's `-0` and last bit and a
  `datetime2`'s precision, and NULL and `''` differ. A computed column
  counts by its definition. `CHECKSUM` is not used. A column of a type
  DriftGate cannot read stops the fingerprint, naming it.
- Not covered: statistics (they change how a query runs, not what it
  returns), excluded tables, views and sequences, the mark, and a
  sequence's `start_value`: a restore item by item sets it with `RESTART
  WITH`, and it decides only where a later `RESTART` without a value
  starts.
- **`rowversion`** values are not part of a table's rows. A restore writes
  rows anew and SQL Server gives them new values (above the database's
  `@@DBTS`, which only moves on), so a restored table has other values
  after every restore: a response that shows them (an `ETag`, EF Core's
  concurrency token, a `timestamp` column in JSON) differs between
  captures and needs an [allowlist](#allowlistyaml) rule. An `UPDATE` that
  only gives a row a new `rowversion` is still a change: a table whose
  values moved since DriftGate's own last restore counts as changed.

A fingerprint is taken at one of three costs; the first one of a command
reads every table:

| Fingerprint | When | Time |
|---|---|---|
| reused | nothing moved since the last one: the end of the log (after `sp_flush_log`), every identity column and sequence (a rolled-back `INSERT` and a cached sequence move them without a log record), the database's options; never with memory-optimized tables, nor while the application writes an excluded table (sessions, cache, jobs) on every request: the log moves, and the per-table cost applies | 1.5 ms |
| per table | the tables whose write counters (`sys.dm_db_index_operational_stats`, `sys.dm_db_index_usage_stats`, rows, dates) moved are read again | 23 ms (one table of 100 written to) |
| full | every table read, on up to four connections; always without `VIEW SERVER PERFORMANCE STATE` | 0.39–0.56 s for 57.8 MB |

Restore of a small table with one changed row: 11–12 ms; of one row in a
table of a million rows (by key): about 190 ms. Seed of a small database:
0.7–1.3 s. Measured with SQL Server 2022 Developer in Docker, emulated
(`linux/amd64` under Rosetta on an Apple M4); the server's side is
several times faster on x86. Memory-optimized tables and tables with a
`text`, `ntext` or `image` column are read every time (some of their
writes move no counter DriftGate reads), and after a restart of the
server every table is read once.

**Transactions.** A fingerprint reads committed data only (`READ
COMMITTED`) and waits up to 10 s for a transaction of the application
that wrote to the database and is still open; it reads the tables again
when something committed while it read them, so it never mixes rows from
before and after a commit. Without `VIEW SERVER PERFORMANCE STATE` it
cannot see the open transactions: on a database with
`READ_COMMITTED_SNAPSHOT` a transaction that wrote before the reading and
commits during it is not noticed. A restore holds its locks until it
commits; the application's queries on the tables it writes wait for it.

**Exclude** what the application writes on every request or outside
requests: session tables, job tables (Hangfire's `HangFire.*`), logs; a
cache (ASP.NET's SQL Server distributed cache, `dotnet sql-cache
create`) may be `restore_empty`. `init` and `setup` propose Hangfire's
tables and the Data Protection keys ([the framework's own
tables](#the-frameworks-own-tables)). An excluded table keeps its definition in the pristine copy,
without rows; a restore item by item does not touch it, and a restore of
the whole database saves its rows and copies them back. A table that is
not excluded and whose rows refer to an excluded table's rows stops the
seed (above); a foreign key of an excluded table to restored rows that are
gone stays, no longer trusted, and the log says so.

**Queries** (`params` `sql:`) run on the pristine copy (read-only), in a
transaction that is rolled back. A query must be one `SELECT` (or `WITH …
SELECT`) statement in ASCII outside its quoted texts and names, without
comments, `INTO`, any other statement's keyword (T-SQL needs no `;`
between statements: `SELECT 1 DROP TABLE x` is refused), `NEXT VALUE
FOR` (it moves a sequence outside the transaction), names of three or
four parts (another database or server; for `schema.table.column` use an
alias), and the system procedures,
functions and views that reach files, programs, other servers, sessions
or logins (`sp_…`, `xp_…`, `fn_…`, `dm_…`, `OPENROWSET`,
`OPENDATASOURCE`, `OPENQUERY`, `sys.sql_logins` …).

**Running SQL Server for DriftGate.**

- **Other databases being restored.** SQL Server 2019 and earlier stop a
  statement of one database with error 927 ("Database '…' cannot be
  opened. It is in the middle of a restore") or 952 ("is in transition")
  when another database of the server is restored, created or renamed
  at that moment, even a statement that names only its own database
  (another checkout's seed, a restore elsewhere). A seed, restore,
  fingerprint or read of rows that fails so about another database runs
  again, for up to a minute (the log says so once); each stops in a state
  the next run completes. The application's own queries can fail the same
  way on those versions.

- **The application's login.** Let the application sign in with a login
  that is only a user of its database (`db_datareader`, `db_datawriter`,
  what its migrations need), not `sa` and not DriftGate's login: during a
  restore of the whole database a member of `db_owner`, `dbcreator` or
  `sysadmin` still gets in, and its writes to excluded tables in that time
  can be lost (above). Run the grants in a container with its own
  `sqlcmd`:
  `docker compose exec mssql /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -Q "…"`
  (`/opt/mssql-tools` on SQL Server 2017; `-C` trusts the container's own
  certificate).
- **The `sa` password** of a container must have at least eight
  characters of three kinds (upper case, lower case, digits, symbols);
  otherwise the container stops at its first start, and only its log
  (`docker compose logs mssql`) says why.
- **Apple Silicon.** There is no SQL Server image for ARM: run
  `mcr.microsoft.com/mssql/server` for `linux/amd64` (`platform:
  linux/amd64` in compose), which Docker Desktop runs under Rosetta; it is
  slower, and SQL Server 2017 and 2019 may stop under emulation. Azure SQL
  Edge, once the ARM choice, is retired and refused: use the
  `mssql/server` image, or a SQL Server on another machine
  (`allow_remote: true`).
- **The backup files.** A seed keeps two backups of the pristine copy
  (`driftgate-<hash>-a.bak` and `-b.bak` in the server's default backup
  folder, `/var/opt/mssql/data` in the image). To remove them after
  dropping the pristine copy:
  `docker compose exec mssql sh -c 'rm /var/opt/mssql/data/driftgate-*.bak'`.
  A container without a volume loses them with its databases when it is
  removed; the next `driftgate seed` makes them again.
- **A certificate error** (`certificate signed by unknown authority`,
  `certificate is not valid for …`): the server's certificate is verified,
  as the application's client does. For a container's or a development
  server's self-signed certificate set `TrustServerCertificate=true` in
  the DSN (on a loopback address only, as the application's setting does);
  for a server with a certificate of your own CA, give the CA's file
  (`certificate=/etc/ssl/certs/my-ca.pem`, or `~/…`); for a certificate
  that names another host, `hostNameInCertificate=<that name>`. Nothing
  makes DriftGate's TLS weaker than the application's.

### seed, prepare, guards

| Key | Meaning |
|---|---|
| `seed` | commands that load the data into the working databases (migrations and seeders, a dump import). `driftgate seed` runs them and keeps the pristine copy; `seed --skip-commands` takes the data already loaded |
| `prepare` | commands run after every restore |
| `guards` | `{name, command}`: when the capture has requests that change data (or command steps, repeated requests), each must succeed after the restore and `prepare`, before the first request (so one that fails stops the capture in seconds), and again right before the first request that changes data when read-only requests ran in between (they may take long); a guard that fails stops the capture there (e.g. mail goes to a log, not to people). A guard only checks: it must not write to the databases under `state:` (a command that logs itself to a table writes). If one does, its write becomes part of the state the read-only requests start from (a restore removes it before the requests that change data), it may be reported as a change `prepare` made, and a later search for read-only requests that wrote may report prepare as not repeatable |

<!-- check: config-fragment -->
```yaml
seed:
  - ["sh", "-c", "gunzip -c ~/Data/shop.sql.gz | mysql shop"]
  - ["php", "artisan", "migrate", "--force"]
prepare:
  - ["php", "artisan", "cache:clear"]
guards:
  - name: queue is sync
    command: ["sh", "-c", "grep -q '^QUEUE_CONNECTION=sync' .env"]
```

A command of `seed`, `prepare` or a guard (and only these; restore,
fingerprint, `tokens.command`, adapters and bisect hooks stay lists) may
also be a mapping: `run` is the list, `dir` the folder it runs in
(default the project folder) and `stdin` a file given as its standard
input, still without a shell. Both paths are relative to the project
folder; `~` and `~/` are the home folder. A folder or file that is
missing stops the command before the first of its list runs (exit 3), so
a seed never stops halfway for it. A release before this form refuses a
`driftgate.yaml` that uses it.

<!-- check: config-fragment -->
```yaml
seed:
  - run: ["mysql", "--defaults-extra-file=.my.cnf", "shop"]
    stdin: ~/Data/shop.sql
  - run: ["php", "artisan", "migrate", "--force"]
    dir: api
guards:
  - name: mail goes to the log
    command: { run: ["grep", "-q", "^MAIL_MAILER=log"], stdin: api/.env }
```

A program in a container reads the file through `docker exec`. The
command must pass its standard input on: `docker exec` needs `-i`, and
`docker compose exec` needs `-T` (no terminal); without them the command
gets no input.

<!-- check: config-fragment -->
```yaml
seed:
  - run: ["docker", "exec", "-i", "app", "php"]
    stdin: database/fixtures.php
  - run: ["docker", "compose", "exec", "-T", "db", "mysql", "shop"]
    stdin: ~/Data/shop.sql
```

A step whose result does not depend on the restored data, such as building
a view or configuration cache, belongs before the capture, not in `prepare`
(unless a `state:` component restores the files it writes): `prepare` runs
after every restore, so its time multiplies with the number of restores.

A `prepare` that changes the restored state (a data migration that stamps
`updated_at` on every row) does so before every capture, and a value it
writes anew each time drifts in every run. The capture fingerprints the
state right after the restore and after `prepare` anyway; when the two
differ, it warns that the state changed between the restore and the end
of `prepare` and names the changed tables or components. If the step does
not depend on the restored data, move it to `seed`: the pristine copy then
holds its result. A `tokens.command` runs right after `prepare`; what it
writes (the tokens of a login) is not taken for `prepare`'s: with both
configured, the first capture takes one more fingerprint between them,
and the later restores (before the requests that change data, `--restore
each`) are checked only without a `tokens.command`. Only components with
a fingerprint are seen. The warning never changes the exit code.

### discover

Adapters that find the routes. `driftgate init` writes them; the order of
adapters does not change the result.

| Adapter | Required | Reads |
|---|---|---|
| `laravel` | `command` | `route:list --json` (or the table of Laravel before 8). With `php: ["php"]` it also reads the validation rules of form request classes and builds request bodies from them |
| `symfony` | | `bin/console debug:router --format=json` |
| `rails` | | `bin/rails routes` |
| `django` | | a script run through `manage.py shell` (the `.venv` Python when there is one) |
| `spring` | | `/actuator/mappings` of the running application (needs the actuator `mappings` endpoint exposed) |
| `nextjs` | | the files of the app and pages routers |
| `nuxt` | | the files of `pages`, `server/api` and `server/routes` |
| `openapi` | `file` or `url` (one) | an OpenAPI 3 or Swagger 2 description, from a file (JSON or YAML) or from the running application |
| `command` | `command` | any command that prints a JSON list of routes |

Common keys:

| Key | Meaning |
|---|---|
| `command` | `laravel`: the `route:list` command; `command`: the command that prints routes |
| `php` | `laravel`: how to run PHP inside the application (for validation rules) |
| `file` | `openapi`: path of the description |
| `url` | `openapi`: a path on the target (`/openapi.json`) or a full URL, read only from allowed hosts |
| `identity` | `openapi` with `url`, `spring`: read the description as this identity (some applications describe only what the caller may use) |
| `target` | the service these routes belong to (required with several targets) |
| `dir` | the folder of the service: commands run there and files are found there (default the project root) |
| `base_path` | a prefix of every path (Next.js `basePath`, an API prefix); starts with `/`, does not end with one |

The `command` adapter's output is a JSON list; each entry has `path` and
`method` (or `methods`), and optionally `name`, `auth` (needs an identity),
`body` (a sample request body) and `file` (source file of the handler):

<!-- check: routes-json -->
```json
[
  {"method": "GET", "path": "/api/news/{id}", "file": "src/news.go:42"},
  {"methods": ["PUT", "PATCH"], "path": "/api/news/{id}", "auth": true, "body": {"title": "x"}}
]
```

Paths name parameters in braces: `{id}` is required, `{id?}` optional,
`{slug*}` takes the rest of the path and `{slug*?}` the rest or nothing.
Every path must start with `/`.

<!-- check: config-fragment -->
```yaml
discover:
  - adapter: openapi
    url: /v3/api-docs
    identity: admin
  - adapter: spring
  - adapter: command
    command: ["./scripts/routes.sh"]
    dir: services/billing
    base_path: /billing
```

### templates, generate, params

`templates:` lists the variants `generate` makes for every route method
(default `[ok]`; `init` usually writes `[ok, not-found, json]`, and
`[ok, not-found, json, json-errors]` for Laravel, Symfony, Rails and
Spring Boot):

| Template | Request |
|---|---|
| `ok` | the route with its parameter values (one per value with `foreach`) |
| `not-found` | every required path parameter set to `999999999` (optional ones left out) |
| `invalid-param` | every required path parameter set to `driftgate-invalid` |
| `anonymous` | a route that needs authentication, without an identity |
| `identities` | a route that needs authentication, once per other identity |
| `json` | with `Accept: application/json` |
| `empty-body` | a route that takes a body, with `{}` |
| `json-errors` | each request of `not-found`, `invalid-param`, `anonymous` and `empty-body` once more with `Accept: application/json` (ID suffix `-json`) |

`json` and `json-errors` matter where the framework negotiates by
`Accept`: Laravel, Symfony, Rails and Spring Boot answer a browser with an
error page or a redirect (to the login page, back to the form with the
validation errors) and an API client with a JSON body (401, 404, 422). A
change of that JSON body is invisible without a request that sends the
header. `json-errors` adds one request per error variant, never for `ok`
(that is `json`) or `identities`, and its requests are error variants:
they never cover a route. An `Accept` header the variant has is replaced.
With `[ok, not-found, json, json-errors]` it adds one request per route
method with a required path parameter: at most a third more requests, when
every route has one; `generate` says how many (`… (N of them json-errors)`).
Frameworks that always answer JSON (FastAPI, NestJS, ASP.NET) gain nothing
from it.

To adopt it in a project made before it, add it to `templates:` and run
`driftgate generate --write`, then `driftgate run` and `accept` the new
requests. `generate` and `run` say so once (a `note:`, once per local
store) to a project whose `discover:` uses the `laravel`, `symfony`,
`rails` or `spring` adapter; a project that finds its routes only through
`openapi` gets no note. Releases before `json-errors` refuse a
configuration that names it: everyone working on the project needs a
release that knows it.

A rate limit is not generated: write a repeated request by hand
([repeat](#repeat)), once more with the header, so the JSON body of a 429
is compared too:

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "search-throttle", "method": "GET", "path": "/api/search?q=shoes", "repeat": 61, "storage": "public"},
  {"id": "search-throttle-json", "method": "GET", "path": "/api/search?q=shoes", "repeat": 61, "storage": "public",
   "headers": [["accept", "application/json"]]}
 ]
}
```

`generate.identity` is the identity used for routes that need
authentication; it must be under `identities:` or come from `tokens:`.

`params:` gives route parameters their values. Each entry has a `name`, an
optional `route` (a glob over route paths; empty: every route with that
parameter), an optional `target`, and exactly one source:

| Source | Meaning |
|---|---|
| `value: "12"` | a fixed value |
| `values: ["a", "b"]` | a list; the first value, or every value with `foreach: true` |
| `sql: "SELECT … ORDER BY …"` | a query on the pristine database (`state:` names the component when there are several). One `SELECT` (or `WITH … SELECT`), checked before it is sent and run in a read-only transaction; `ORDER BY` is required so the catalog is the same every time. `foreach: true` makes a request per row |
| `from: { request: <id>, path: $.data.id }` | a value from the response of an earlier request in the same capture (`generate` puts both in a sequence); not with `foreach` |

<!-- check: config-fragment -->
```yaml
params:
  - name: slug
    route: "/blog/*"
    values: ["hello-world", "pricing"]
    foreach: true
  - name: id
    value: "12"
```

The `sql` check is a guard against mistakes, not a security boundary: the
DSN user's privileges are (see [security](security.md)).

### storage and pii

Generated requests store their bodies as **private** (encrypted in the
local store, never in the repository) unless `storage.public` names them by
a glob over the request ID, or `storage.default: public`.

<!-- check: config-fragment -->
```yaml
storage:
  default: private
  public: ["get-api-news*", "get-api-faq*"]
pii:
  allow: ['[a-z.]+@example\.com']
```

Every public body is scanned by the personal-data detector (e-mail, phone,
Serbian personal ID number with its check digit, card number by Luhn,
IBAN). When it finds something, the body is stored as private, the finding
is reported with a masked value, and the capture exits 2. `pii.allow` lists
regular expressions of values that may appear in public bodies (a shared
support address). A catalog entry has its own `storage`.

### observe

Generated requests whose outcome depends on timing or a cache: they are
measured (status, time, size) but never fail the check. `match` is a glob
over `METHOD /route/path`, `/route/path` or the request ID; `target`
limits it to one service.

<!-- check: config-fragment -->
```yaml
observe:
  - match: "GET /up"
    reason: Health check; reports the current time.
```

### requests

What `generate` writes into the requests it makes: whether they change
data (`mutates`) and their JSON body (`body`, written as YAML, sent as
`application/json`). `match` and `target` name requests as `observe`
does; where several entries name a request, the last one with the key
wins. The catalog keeps no change by hand to a generated entry: the next
`generate --write` makes it again, and `run` reports a catalog that
differs from what it generates. Mark it here instead, then `generate
--write`; for read-only requests that changed data, `capture` and `run`
print the entries to add (by route), and the catalog change for entries
written by hand, which say `mutates` and `body` in the catalog itself.
`generate` notes an entry that names no generated request.

- `mutates: false` is for one route that only reads, such as a search
  sent as `POST`: it runs with the read-only requests, at the same time,
  without a restore before it. An entry that says it of a request whose
  method changes data other than `POST` (`DELETE`, `PUT`, `PATCH`, any
  other verb), or of requests of more than one route (`match: "POST
  *"`), stops `generate` (exit 2).
- The body is JSON as the YAML is written: a number keeps its digits
  (`1.0`, a large integer), a date stays its text, `null` is JSON's
  `null` (`body:` with nothing after it is no body). A value JSON would
  read otherwise (`0x1F`, `007`, `.inf`), a tagged value (`!!binary`), a
  key that is not a string and merge keys (`<<`) are refused: quote them,
  or write the keys out. A body is at most 10 MB, aliases included. It replaces the route's sample body, for every template
  but `empty-body`, which keeps its `{}`; for a route that uploads files,
  its fields are the form's fields.

<!-- check: config-fragment -->
```yaml
requests:
  - match: "GET /api/news/*"          # counts views: changes data
    mutates: true
  - match: post-api-search            # a search sent as POST only reads
    mutates: false
    body: { q: news, page: 1 }
```

### uploads

A catalog request can upload a real file, named by its path in an uploads
folder outside the repository. `uploads.dir` (`~/` is the home folder; a
relative path is relative to the folder of `driftgate.yaml`) or
`DRIFTGATE_UPLOADS` names the folder; a folder anywhere inside the git
repository of the project is refused, also through a symbolic link. See [catalog.json, multipart](#multipart).

<!-- check: config-fragment -->
```yaml
uploads:
  dir: ~/Data/shop-uploads
```

### bisect

Hooks of [`driftgate bisect`](guides/bisect.md). They run without a shell,
from the project folder, with `DRIFTGATE_BISECT_DIR` (the worktree of the
commit under test) and `DRIFTGATE_BISECT_COMMIT` set.

| Key | Meaning |
|---|---|
| `checkout` | required for bisect: builds or restarts the application from `$DRIFTGATE_BISECT_DIR`; failing skips the commit |
| `ready` | repeated until it succeeds; failing within `ready_timeout` skips the commit |
| `finish` | once at the end, e.g. to start the application from your checkout again |
| `timeout` | limit of `checkout` (default `10m`) |
| `ready_timeout` | default `1m` |

<!-- check: config-fragment -->
```yaml
bisect:
  checkout: ["sh", "-c", "cd \"$DRIFTGATE_BISECT_DIR\" && make restart"]
  ready: ["curl", "-sf", "http://localhost:8000/up"]
  finish: ["make", "restart"]
  timeout: 5m
```

### data

The data module (`driftgate data …`) makes a copy of a database with
personal values replaced. See [anonymized test data](guides/test-data.md).

| Key | Meaning |
|---|---|
| `driver` | `mysql` (also MariaDB) or `postgres` |
| `source` | `{dsn, database}`: the database that is read (a copy of production data); never written |
| `target` | `{dsn, database}`: the database that receives the copy; `dsn` defaults to the source's; must be another database than the source |
| `allow_remote` | allow a target host outside `target.allow_hosts`; the target is dropped and created again, so it must be a copy |
| `key` | `{env}` \| `{op}`: the key that replaces values deterministically; default the project key |
| `files` | `{source, target}`: the folder of uploaded files and the folder that receives their placeholders (both or neither; relative to the project). The target must be outside the source and empty |

The copy has the tables, views, triggers and sequences of the source;
MySQL/MariaDB also its stored functions, procedures and events (without
their `DEFINER`, with the SQL mode and collation they were made with; an
event keeps its status, so with the event scheduler on it runs on the
copy). Reading a routine's definition needs `SHOW_ROUTINE` (MySQL),
`SELECT` on `mysql.proc` (MariaDB) or being its definer; without it the
run stops and names the routine. A MySQL/MariaDB target's
`max_allowed_packet` bounds every `INSERT`: a row that does not fit in one
stops the run, naming its table and column.

<!-- check: config-fragment -->
```yaml
data:
  driver: mysql
  source: { dsn: { env: DG_PROD_COPY_DSN }, database: shop_prod_copy }
  target: { database: shop_anon }
  files:
    source: ~/Data/shop-storage
    target: ~/Data/shop-storage-anon
```

---

## allowlist.yaml

`.driftgate/allowlist.yaml` (`driftgate/` with `baselines: git`) lists the values that may change between
captures: the time of the response, a session cookie, a token. A value on
the allowlist may differ, but it must still match its format, and every
entry has a reason. Nothing else is hidden: a difference in escaping or
whitespace next to an allowed value is still reported.

A rule judges only a value that changed. A value equal to the baseline's
is never a drift, also when it does not match the format: it is the
accepted baseline, and the verdict must not depend on whether something
else in the same response changed. This holds for JSON values, texts and
attributes of a page, cells and header values.

A JSON body whose whitespace layout changed (compact and pretty-printed,
another indentation, line ends, spaces around `:` and after `,`) is an
`encoding` drift at `$`; the same encoder's output that only nests deeper
is not. A byte order mark that came or went, and another encoding (UTF-8,
UTF-16, UTF-32), are an `encoding` drift at `$` whatever else changed; a
body in UTF-16 or UTF-32 is compared as its text. Other bytes that change no value (keys written with
escapes, `"\u0061"` for `"a"`) are a drift only when nothing else in the
body changed. A value a rule covers, written otherwise (`"a\/b"` for
`"a/b"`), is no drift: the rule covers how it is written; the rest of the
body is compared without it.

<!-- check: allowlist -->
```yaml
version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response; changes with every request.
  - name: set-cookie
    request: post-api-auth-login
    format: 'session=[0-9a-f]{40}; Path=/; HttpOnly'
    reason: A new session id on every login.
  - name: content-type
    ignore_case: true
    reason: Charset names are case-insensitive (RFC 9110); a framework upgrade writes utf-8 for UTF-8.
body:
  - path: $.meta.generated_at
    format: '\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z'
    reason: Time the response was built.
  - request: get-api-feed
    path: $.items
    any_length: true
    reason: The feed shows entries of the last hour.
  - target: web
    path: html*
    format: '/_next/static/(?:[\w~-]+/)*[\w~-][\w.~-]*'
    parts: true
    reason: Asset URLs carry a build hash; the rest of the page is still compared.
  - request: post-login-invalid
    path: $raw
    format: '(?s)<!DOCTYPE html>\n<html>\n    <head>\n.*<meta http-equiv="refresh" content="0;url=''?http://localhost:\d+''?" />.*</html>'
    reason: Symfony 7 quotes the meta refresh URL of its redirect page; status and Location are still compared.
  - request: get-api-users-export
    path: xlsx:Users!A1
    format: 'Exported \d{2}\.\d{2}\.\d{4} \d{2}:\d{2}'
    reason: The export writes its own time in the header cell.
  - path: xlsx:*!F*
    format: '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'
    reason: Column F of every worksheet is a generated id.
arrays:
  - request: get-api-users
    path: $.data
    key: id
    reason: Users are listed newest first; a new user is one new item.
```

**Header rules**

| Key | Meaning |
|---|---|
| `name` | header name (case-insensitive), required |
| `format` | regular expression the whole value must match; required unless `ignore_case` |
| `ignore_case` | the value may change only in letter case (compared lower-cased); not with `format` |
| `reason` | required |
| `request` | glob over request IDs; empty: every request |
| `target` | only requests of this service |

**Body rules**

| Key | Meaning |
|---|---|
| `path` | a glob over body paths, required: a JSON path (`$.token`, `$.items[*].id`), a path into an HTML page (`html>body>p@class`, `html>body>script#__NEXT_DATA__$.buildId`), into the Next.js RSC payload (`self.__next_f$.0.b`), a cell of an exported spreadsheet (`xlsx:Users!A1`, `csv!A1`, below), or `$raw` for the whole body (below). A path that starts with `$` and goes on with anything but `.`, `[` or a wildcard names no place and is refused |
| `format` | regular expression the whole value must match; required unless `any_length` |
| `any_length` | arrays at this path may change length (JSON paths only; not with a cell path). It says nothing about values: without a `format` every value the path reaches is still compared (`$.items*` with `any_length` lets the list grow, not a price inside it change) |
| `parts` | the format matches parts of the value; only those parts may change, the rest of the value is still compared. Needs `format`, not with `any_length`, and the format must not match an empty text |
| `reason` | required |
| `request` | glob over request IDs; empty: every request |
| `target` | only requests of this service |

**Whole body (`path: $raw`).** A rule with the path `$raw` (taken
literally, never matched by a wildcard path) covers the whole body of a
response that is not JSON, such as a framework's redirect page. When the
bytes of the two bodies differ and neither of them is JSON, the current
body as text must match the format (the whole text, as every format);
then the body is not compared further. When it does not match, the body
is compared as usual, so the report still lists what changed. Two JSON
bodies are always compared value by value: name the values that may
change instead. A JSON body that is not JSON now (an HTML error page), or
the other way round, is always a `body` drift, whatever `$raw` rules
there are. A `$raw` rule without `request` applies to every request; give it a `request`
when only some responses are such pages. The format is matched against the body as served,
with its own origin, also when the two sides were captured from different
origins (`localhost:\d+` covers both). Status, headers and side effects
are still compared. A `$raw` rule needs a `format` and takes neither
`any_length` nor `parts`.

**Cells (`path: xlsx:<worksheet>!<cell>`, `csv!<cell>`).** An XLSX or CSV
response is compared cell by cell, and a drift names the cell:
`xlsx:Users!B2` (the worksheet by the name the workbook gives it) or
`csv!B2`. A body rule whose path starts with `xlsx:` or `csv!` applies to
those cells as `format` applies to a JSON value: the current value must
match the whole format, and then it may differ from the baseline; a value
that does not match is a `format` drift, and an emptied cell is the value
`""`. `parts: true` works as for other values; `any_length` does not apply.
Rules apply at the `values` level. Only paths that start with `xlsx:` or
`csv!` reach cells: a rule such as `path: '*'` stays with JSON and HTML.

An XLSX cell has more than its text, and all of it is compared: its type
(number, string, bool, error, date; a shared, inline and formula string
are all `string`) at the `types` level as a `type` drift (the number `1`
that became the text `"1"`), and its number format (`cell-format`, also
another style of the cell: font, fill, borders) and formula
(`cell-formula`) at the `values` level. An empty cell with a formula or
a style of its own is compared too (a sum that came out empty, a fill on
an empty cell), and so is the formatting of the runs of a rich text (a
bold or colored part of a shared or inline string, `cell-format`). A cell
rule lets only the value change, never the type, format or formula. The rest of the workbook
(styles, column widths, merged cells, row heights, the worksheets'
settings) is compared too, as a `body` drift at `xlsx` that names the
first part that differs; that holds also when a rule let a cell change.
Not compared: the document properties (`docProps/`: creation time,
author, application) and how the ZIP archive is written. Rows and cells
without their optional reference (`r`) are placed where the standard puts
them. In a CSV, quoting, line ends, blank lines and empty fields at the
end of a record that differ while the cells are the same are a `body`
drift, the line ends also when a rule let a cell change; a byte order
mark that came or went is a `body` drift whatever else changed. (An [expectation](commands.md#expect) of a drift covers all
of these: they are in its digest.)

- The worksheet is written as in the report: a name of letters, digits,
  `_` and `.` as it is (`Users`, `Članovi`); any other in single
  quotes, a quote doubled (`'Q1 2026'`, `'Hi!'`, `'O''Brien'`). A
  workbook whose `xl/workbook.xml` (or its relationships) is missing or
  cannot be read has its worksheets named after their part in brackets
  (`xlsx:[sheet1]!A1`), which no workbook name can be.
- The cell is a reference in capitals (`A1`) or a glob over one: `B*` is
  column B, `*1` row 1, `xlsx:*!A1` cell A1 of every worksheet, `xlsx:*`
  every cell. A glob's column and row are matched apart: the column is
  the cell's part up to its last letter, the row the rest (with no letter,
  the column is what comes before the first digit), and `*` and `?` match
  letters in the column and digits in the row only. So `*1` is row 1 and
  not A11, `B*` column B and not BA, `B?` B1 to B9, `B*1` the rows of
  column B that end in 1, `*` every cell. `driftgate allowlist check` uses
  the same meaning.
- A path that no cell can have is refused when the allowlist is read:
  `xlsx:Users` (a whole worksheet), `b2`, `2B`, `B`, `A0`, `??` (no
  letter or digit to tell the column from the row), an unquoted
  `xlsx:Q1 2026!A1` or a needlessly quoted `xlsx:'Users'!A1`.
- A worksheet named by its bare part (`xlsx:sheet1!B2`) is refused in a
  rule, with the message to write the name from a report (usually
  `xlsx:Sheet1!B2`); a worksheet the workbook names `sheet1` is written
  `xlsx:'sheet1'!B2`.

**Arrays paired by key (`arrays:`).** The items of a JSON array are
compared by position: one item inserted at the front of a list makes every
item after it differ. An array rule pairs the items of the arrays its path
reaches by a key instead, a member every item has (`id`) or a path into
the item (`user.id`):

- an item only the baseline has is `key-missing`, an item only the current
  response has `key-extra`, both at the item's path with its key:
  `$.data[id=42]` (a string key is quoted: `$.data[slug="a-b"]`; a key
  name other than letters, digits, `_`, `$` and `-` joined by dots is
  quoted too: `$.data["@id"=42]`). An
  `any_length` body rule for the array lets items come and go (they are
  then not reported); there is no `array-length` drift for a keyed array;
- the items both have are compared as usual at `$.data[id=42].name`; a key
  that is `42` on one side and `"42"` on the other pairs, and the `type`
  drift is reported at `$.data[id=42].id`;
- a changed order of the items both have is one `item-order` drift at the
  array, listing the keys (at most 20) in both orders; `any_order: true`
  allows it. Nothing else is ever hidden by an array rule.
- when an item has no key, its key is not an integer or a string, or two
  items share a key (on either side), the array is compared by position
  as without the rule, and the report notes it (`notes` of the request in
  JSON, a `note:` line in text and markdown).

**Items in another order (no rule).** In an array no rule pairs by key,
the same items in another order (rows of an `ORDER BY` on a column with
equal values, which another database server or version returns in
another order) are one `item-order` drift at the array, not a drift at
every index: `$.data: 3 items → same 3 items, other order: 2, 0, 1` (for
every item now, the baseline item it is; at most 20). The items are
equal as the comparison sees them at its level, with the allowlist's
rules; what still differs within a pair (a value written otherwise) is
reported at the baseline item's path. When all items but one of each
side are equal, those two are paired and compared too (`2 of 3 items the
same`); when more items changed, which went where is ambiguous and the
array is compared by position. It stays a drift: the order of a list can
matter to a client. A note names the rule to add, ready to paste under
`arrays:`: with `key:` when every item has an `id` (or `uuid`, `_id`,
`key`, `slug`, `code`) of its own, which pairs the items and still
reports one that changed, came or went; otherwise an array rule with
`any_order: true` and no key, which keeps the items by position and lets
the same items come in another order. An expectation of the drift
(`driftgate expect`) holds only for the same items in the same order.

Body rules reach keyed items with a glob as before (`$.data[*].updated_at`
matches `$.data[id=42].updated_at`); a rule written with an index
(`$.data[0].x`) no longer matches an array paired by key. Drifts of one
key path across items are grouped as one (`$.data[*].name`). In a private
body the key of an item is hidden in the report (`$.data[id=(private)]`,
`$.data["@id"=(private)]`).

| Key | Meaning |
|---|---|
| `path` | a glob over JSON paths of arrays (in a body, in JSON embedded in a page, in the RSC payload), required; the first array rule that matches an array pairs its items |
| `key` | the member that identifies an item, or member names joined by dots (`user.id`); required unless `any_order` |
| `any_order` | the items may come in any order; without `key`, the same items (as above) in another order are no drift, and anything else is compared by position. The rest of the document, how it is written included (an escaped key, spacing inside an item that moved), is compared as without the rule: with the items put back in the baseline's order, orders inside items that moved too, in JSON bodies, in JSON in a page and in UTF-16. A document whose items cannot be put back (a devalue payload, compared decoded) is compared as it is, and its order shows as an `encoding` drift |
| `reason` | required |
| `request` | glob over request IDs; empty: every request |
| `target` | only requests of this service |

`capture --repeat 2` and the first `run` propose entries with a ready format
for values that differ between two captures on the same state; you add the
reason.

`driftgate allowlist check` finds rules that apply to no request, reach
nothing in the baseline, are broader than a value or never decide because
an earlier rule does ([commands](commands.md#allowlist-check)).

---

## exclusions.yaml

`.driftgate/exclusions.yaml` (`driftgate/` with `baselines: git`) lists routes that are deliberately not checked,
each with a reason. `coverage` counts them as excluded, `generate` makes no
requests for them, and an exclusion that matches no route is reported as
stale.

<!-- check: exclusions -->
```yaml
version: 1
routes:
  - route: '/telescope/*'
    reason: 'Laravel Telescope: a development tool.'
  - route: 'DELETE /api/account'
    reason: Deletes the logged-in user; checked by hand before each release.
  - target: web
    route: '/api/draft/*'
    reason: Preview mode of the CMS.
```

| Key | Meaning |
|---|---|
| `route` | `METHOD /path` glob; without a method it matches every method. Required |
| `reason` | required |
| `target` | only routes of this service; empty: every service |

`known_failures:` acknowledges requests that fail in the baseline (a 500
kept for years, an upload broken until a later upgrade step) without
dropping them: `generate` keeps them, `capture` and `compare` treat them as
any other request, and `coverage` and `run` list them instead of failing on
their routes ([coverage](commands.md#coverage)). An exclusion would drop the
request, and its answer would no longer be compared at all. A known failure
whose requests succeed now, or that matches no request, is reported as
stale: remove it once the fix is accepted. Releases before `known_failures:`
refuse the file.

<!-- check: exclusions -->
```yaml
version: 1
routes: []
known_failures:
  - request: post-api-tree-reorder
    reason: Answers 500 since the move to nested sets; fixed in the Laravel 9 step.
    ref: SHOP-412
    status: 500
  - request: 'post-api-uploads-*'
    reason: Uploads break until the storage driver is replaced.
    status: 5xx
```

| Key | Meaning |
|---|---|
| `request` | request ID or glob. Required |
| `reason` | required |
| `ref` | ticket or link |
| `status` | the failure acknowledged: a status (`500`, also among the sends of a repeated request), a class (`5xx`, `4xx`), `error` (no answer) or `timeout` (no answer in time); without it, any failure |

A request takes the first known failure whose glob matches it. When that
one names a status its failure does not have (the 500 became a 404, the
timeout a refused connection), the request is reported as failed, with a
note that its known failure changed. A known failure that acknowledges
failures on more than 5 routes is reported as broad: such a glob also
hides the new failures it reaches.

---

## catalog.json

`.driftgate/catalog.json` (`driftgate/` with `baselines: git`) is the list of requests. `generate` writes entries
with a `source.template`; it never touches entries written by hand or
imported (without a template). Variants of one route are separate entries
with their own ID.

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {
   "id": "get-api-news-id",
   "method": "GET",
   "path": "/api/news/{id}",
   "params": {"id": "12"},
   "headers": [["accept", "application/json"]],
   "storage": "public"
  },
  {
   "id": "post-api-auth-login",
   "method": "POST",
   "path": "/api/auth/login",
   "body": {"email": "admin@example.com", "password": "secret"},
   "mutates": true,
   "storage": "private"
  },
  {
   "id": "get-api-me",
   "method": "GET",
   "path": "/api/me",
   "identity": "admin",
   "level": "types",
   "storage": "private"
  }
 ],
 "sequences": [
  {"id": "login", "restore": "before", "requests": ["post-api-auth-login", "get-api-me"]}
 ]
}
```

**Request fields**

| Field | Meaning |
|---|---|
| `id` | unique, also ignoring case (it names a file, and macOS and Windows ignore case); letters, digits, `. _ -`, starting with a letter or digit, not ending with `.`, and not a device name Windows reserves (`con`, `prn`, `aux`, `nul`, `com1`–`com9`, `lpt1`–`lpt9`, also with an extension) |
| `method`, `path` | the request; `method` is an HTTP method (written in upper case; `post` is read as `POST`; none is `GET`), `path` starts with `/` and may hold `{name}` parameters and a query string |
| `target` | the service, with several targets |
| `params` | values of the path parameters |
| `params_from` | `{"<param>": {"request": "<id>", "path": "$.data.id"}}`: a value from the response of a request earlier in the same sequence |
| `headers` | list of `[name, value]` pairs, sent in this order; the first of a name replaces DriftGate's own (`User-Agent`, `Accept-Encoding: identity`), more of that name are sent too |
| `body` | a JSON body |
| `identity` | send with this identity's token |
| `mutates` | the request changes data: it runs after the read-only ones, one at a time, and needs a state with a restore command. With `capture --restore once` (the default, and `run`) the state is restored once before these requests, so each runs on the data the ones before it left; `--restore each` restores before every one. Without `mutates`, every method other than `GET`, `HEAD` and `OPTIONS` changes data, and capture lists such requests; write `"mutates": false` for one that only reads (a search sent as `POST`), so it runs with the read-only requests on the restored data. For a generated entry, say it under [`requests`](#requests) in `driftgate.yaml` |
| `storage` | `public` or `private`, required |
| `level` | comparison level of this request |
| `observe` | measured, never compared |
| `source` | where the entry came from (`route`, `template`, `import`); written by the tool |
| `multipart` | a `multipart/form-data` body (below); not with `body` |
| `repeat` | send the request N times (1 to 10000) |
| `command`, `output`, `output_ignore`, `dir`, `stdin` | a command step instead of a request (below) |

**Sequences** run their requests in order, together. `restore: "before"`
restores the state before the sequence (`capture --restore each` does that
for every sequence). A request is in at most one sequence.

### multipart

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {
   "id": "import-xlsx",
   "method": "POST",
   "path": "/api/imports",
   "identity": "admin",
   "mutates": true,
   "storage": "private",
   "multipart": {
    "fields": [["kind", "orders"]],
    "files": [{"field": "file", "path": "orders/2025.xlsx", "ref": "…"}]
   }
  },
  {
   "id": "import-synthetic",
   "method": "POST",
   "path": "/api/imports",
   "identity": "admin",
   "mutates": true,
   "storage": "public",
   "multipart": {
    "fields": [["kind", "orders"]],
    "files": [{"field": "file", "synthetic": "xlsx"}]
   }
  }
 ]
}
```

Each file has a `field` and either:

- `path`: a real file, relative to the [uploads folder](#uploads), without
  `.` or `..`. The content never enters the repository; `ref` is a keyed
  hash of it. Leave `ref` out the first time: the capture stops (exit 2) and
  prints the value to set after you checked it is the right file. A changed
  file stops the capture as a configuration change (exit 2), a missing one as
  an environment error (exit 3). The file is checked before anything is
  sent and read again only when its request is sent, so the files of a
  capture are not all held in memory; a file that changes during the
  capture fails its request. A request with a real file must be
  `private`, and so is every response after it until the next restore. It
  runs after the read-only requests, one at a time, like a request that
  changes data (also without `mutates`), and needs a state with a restore.
- `synthetic`: a minimal valid file of a kind (`csv`, `gif`, `jpeg`, `json`,
  `pdf`, `png`, `txt`, `xlsx`), always the same bytes.

Optional: `name` (the file name sent; default the base name of `path`, or
`upload.<ext>`) and `type` (the part's content type). The multipart boundary
is derived from the content, so the same request is the same bytes every
time.

### repeat

`"repeat": 61` sends the request 61 times in a row and compares the last
response together with the statuses of all (`200×60, 429×1`). The sends skip
`rate_limits`, and the request runs on a freshly restored state. On a target
outside its `allow_hosts` (`capture --allow-remote`) a repeated request needs
`--allow-remote-mutations`, like one that changes data, and keeps
`rate_limits`.

### command steps

A command step runs a program instead of sending a request, e.g. a queue
worker inside a sequence. It has no `method`, `path`, body, headers,
parameters or identity, and is not repeated. Its exit code and side effects
are compared like a request's; its standard output only with
`"output": true`, with the text the `output_ignore` regular expressions
match removed first (here, timestamps; [side checks](guides/side-checks.md)
has recipes for a schema dump, compiler and mail output). `dir` is the folder the step runs
in and `stdin` a file given as its standard input, both relative to the
project folder as for [seed and prepare](#seed-prepare-guards); a missing
one stops the capture before anything is sent. A release before these
fields refuses a catalog that uses them (an unknown field). When a step fails, the log shows
the last lines it wrote to standard error, never its standard output: that
is its body, private, or public but not yet scanned for personal data.

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "import-csv", "method": "POST", "path": "/api/imports", "mutates": true, "storage": "public",
   "multipart": {"fields": [["kind", "orders"]], "files": [{"field": "file", "synthetic": "csv"}]}},
  {"id": "queue-work", "command": ["php", "artisan", "queue:work", "--stop-when-empty"],
   "output": true, "output_ignore": ["\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}"], "storage": "public"},
  {"id": "orders", "method": "GET", "path": "/api/orders", "storage": "public"},
  {"id": "like-throttle", "method": "PUT", "path": "/api/posts/12/like", "repeat": 61, "storage": "public"}
 ],
 "sequences": [
  {"id": "imports", "requests": ["import-csv", "queue-work", "orders"]}
 ]
}
```

---

## classify.yaml

`.driftgate/classify.yaml` (`driftgate/` with `baselines: git`) holds a rule for every column of every table of
the data module's source database. `driftgate data classify --write`
proposes rules (marked `proposed: true`); a person checks each and removes
the mark. A column without a rule, a rule still proposed, or a rule for a
table or column that no longer exists stops `anonymize` and `subset`.

<!-- check: classify -->
```yaml
version: 1
tables:
  users:
    columns:
      id: keep
      name: universal
      email: email
      password: password
      remember_token: token
      bio: free-text
      settings:
        kind: personal
        strategy: json
        paths:
          $.phone: universal
          $.theme: keep
        other: free-text
      avatar: file
      created_at: keep
  sessions:
    action: empty
    reason: Sessions of real users; the application makes new ones.
  cache:
    columns:
      key: keep
      value:
        kind: serialized-php
        action: empty-table
  telescope_entries:
    action: skip
    reason: Development tool data.
schema:
  view vip_customers: [email]
  table users:
    kinds: [email]
    reason: The column default is the shared support address.
```

**Tables**

| Key | Meaning |
|---|---|
| `action` | `copy` (default): rows are copied, each column by its rule; `empty`: the structure without rows; `skip`: the table is left out |
| `columns` | column name → rule |
| `reason` | why |
| `proposed` | written by `data classify`; remove it once checked |

**Column rules.** One word for the plain cases (`keep`, `file`,
`serialized-php`, or a strategy name, which means `kind: personal`), or a
mapping:

| Key | Meaning |
|---|---|
| `kind` | `keep` (copied as is), `personal` (replaced by a strategy), `serialized-php` (PHP `serialize()` output), `file` (a path to an uploaded file; the file gets a placeholder) |
| `strategy` | for `personal`: `universal`, `email`, `password`, `token`, `free-text`, `html`, `json`, `null`, `empty` |
| `paths` | for `json`: JSON path glob (starting with `$`) → strategy or `keep` |
| `other` | for `json`: strings under no path, `free-text` (default) or `keep` |
| `action` | for `serialized-php`: `empty-table` (default) or `null` (the column) |
| `reason`, `proposed` | as for tables |

Strategies:

| Strategy | Replacement |
|---|---|
| `universal` | a letter by a letter of the same script (Latin with Serbian diacritics, Cyrillic), a digit by a digit; length and punctuation kept |
| `email` | an address at an `.invalid` domain |
| `password` | a hash of the known test password `driftgate` |
| `token` | a token of the same length |
| `free-text`, `html` | only the personal parts of the text change (names, e-mails, phone numbers) |
| `json` | by path, as `paths` and `other` say |
| `null`, `empty` | `NULL` or an empty value |

The same value gets the same stand-in everywhere (deterministic under the
key), so references and uniqueness hold. A bare `null` in YAML is the
strategy `null`.

**schema** confirms what the detector finds in schema objects (comments,
defaults, views, triggers, functions, checks), which are copied as they
are. The key is the object as `data classify` names it (`view vip`, `table
users`); the value is a list of kinds (`email`, `phone`, `jmbg`, `card`,
`iban`) or a mapping with `kinds`, `reason` and `proposed`. Any other
finding stops the run.

### subset

`data subset` copies a sample: the rows `subset:` chooses as roots, the
rows that follow them, and every row they reference, transitively, so the
copy has no dangling reference.

<!-- check: classify -->
```yaml
version: 1
tables:
  users: { columns: { id: keep, email: email } }
subset:
  nullable: pull
  tables:
    countries: all
    audit_log: none
    users:
      rows: 200
      order: latest
    orders:
      follow: [users]
      per_parent: 5
    news:
      where: "published = 1"
      percent: 10
  references:
    - from: comments(commentable_id)
      to: posts(id)
      when: { commentable_type: 'App\Models\Post' }
    - from: orders(coupon_id)
      nullable: "null"
```

| Key | Meaning |
|---|---|
| `nullable` | for a nullable reference whose row is outside the sample: `pull` (default, take the row in) or `null` (set the reference to NULL) |
| `tables.<t>` | `all`, `none`, or a mapping. A table not listed has only the rows other kept rows reference |
| `rows` | at most this many roots |
| `percent` | roots: this share of the rows (above 0, at most 100), chosen by a hash of the primary key |
| `where` | roots: rows matching this SQL condition |
| `order` | `latest` (default, highest primary key first) or `first` |
| `follow` | parent tables: rows of this table that reference kept rows of those tables are kept too |
| `per_parent` | with `follow`: at most this many rows per parent (by `order`); 0 is all |
| `references` | references the database does not declare (applications without foreign keys, polymorphic columns), written `table(column, …)`. `to` omitted: `from` is a foreign key of the database whose `nullable` rule changes. `when` limits a declared reference to rows whose columns have these values |

A table rule needs `rows`, `percent`, `where` or `follow` (or `all` /
`none`).
