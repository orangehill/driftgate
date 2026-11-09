# driftgate.yaml

The configuration of a project. `driftgate help config` lists its
top-level keys in the terminal. Paths are those of the local layout
([where the project's files are](cli.md#where-the-projects-files-are)).

## A complete example

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

The example is a Laravel API on MySQL with Redis.

## Rules every file follows

<!-- check: config-fragment -->
```yaml
version: 1
project: shop
target:
  base_url: http://localhost:8000
```

- Every file has a `version` field, now `1`. A newer version is refused.
- In the YAML files an unknown key is an error, not a warning.
- A second document after `---` in `driftgate.yaml` or `exclusions.yaml` is an error too: it would be ignored.

## Top-level keys

Each key links its section; [the complete example](#a-complete-example)
uses most of them.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | required | `1` |
| `project` | string | required | a simple name (letters, digits, `. _ -`); names the local store and the Keychain entry |
| [`mode`](#mode) | `freeze` \| `evolve` | `freeze` | whether drifts may be accepted |
| [`level`](#level) | `status` \| `shape` \| `types` \| `values` | `values` | how far a comparison goes |
| [`baselines`](#baselines) | `local` \| `git` | where `driftgate.yaml` is | where the project's files, the store and the key are |
| [`store`](#store) | path | `.driftgate/store` (local), `~/.driftgate/<project>` (git) | the local store |
| [`key`](#key) | `{env}` \| `{op}` | `DRIFTGATE_KEY`, then `.driftgate/key` (local) or the Keychain (git) | where the project key comes from |
| [`target`](#target-and-targets) | mapping | | the application (one service) |
| [`targets`](#target-and-targets) | map of name → target | | several services; not together with `target` |
| [`identities`](#identities-and-tokens) | map of name → `{env}` \| `{op}` | | users, and where their tokens come from |
| [`tokens`](#identities-and-tokens) | `{command}` \| `{file}` | | a source of all tokens as one JSON object |
| [`state`](#state) | map of name → component | | what is restored before captures |
| [`seed`](#seed-prepare-guards) | list of commands | | load the data; `driftgate seed` then keeps the pristine copy |
| [`prepare`](#seed-prepare-guards) | list of commands | | run after every restore |
| [`guards`](#seed-prepare-guards) | list | | must succeed before the first request, and before any request that changes data |
| [`discover`](#discover) | list of adapters | | where the routes come from |
| [`templates`](#templates) | list | `[ok]` | the request variants `generate` makes per route |
| [`generate`](#parameter-values) | `{identity}` | | the identity for routes that need authentication |
| [`params`](#parameter-values) | list | | values of route parameters |
| [`storage`](#storage-and-pii) | `{default, public}` | private | whether generated requests store bodies as public or private |
| [`pii`](#storage-and-pii) | `{allow}` | | values the personal-data detector lets through in public bodies |
| [`observe`](#observe) | list | | generated requests that are measured but never compared |
| [`requests`](#requests) | list | | whether generated requests change data, and their bodies |
| [`uploads`](#uploads) | `{dir}` | | the folder of real files that catalog requests upload |
| [`bisect`](#bisect) | mapping | | the hooks of `driftgate bisect` |
| [`data`](#data) | mapping | | the data module (`driftgate data …`) |

## How commands are written

<!-- check: config-fragment -->
```yaml
prepare:
  - ["php", "artisan", "cache:clear"]
  - ["sh", "-c", "php artisan config:cache && php artisan route:cache"]
```

Commands are lists: the program and its arguments, run without a shell
from the project folder. That holds for `seed`, `prepare`, guards,
restore and fingerprint, `tokens.command`, adapters and bisect hooks.

- A shell is written explicitly (`["sh", "-c", "…"]`), and shows in the diff.
- `seed`, `prepare` and guards may also name a folder and a standard input ([below](#commands-with-a-folder-or-standard-input)).
- Commands never receive `DRIFTGATE_KEY` or the variable named by `key.env` ([security](../security.md#commands-a-pull-request-controls)).

## mode

<!-- check: config-fragment -->
```yaml
mode: evolve
```

- `freeze`, the default: every drift fails unless a ticket expects exactly that drift ([`expect`](commands/review.md#driftgate-expect)), and `accept` is refused.
- `evolve`: drifts may be accepted, with a reason ([`accept`](commands/review.md#driftgate-accept)).

## level

<!-- check: config-fragment -->
```yaml
level: types
```

How far a comparison goes. Each level includes the ones before it.

| Level | Compares |
|---|---|
| `status` | the status code only |
| `shape` | also header names, keys, key order, nesting and array lengths |
| `types` | also the type of every value |
| `values` | also exact values, header values and body bytes (the default) |

A catalog entry can set its own level, and `compare --level` can only
make a comparison stricter. What each level compares in each part of a
response is in [How responses are compared](comparison.md#comparison-levels).

## baselines

<!-- check: config-fragment -->
```yaml
baselines: local
```

Where the project's files are: `local` or `git`
([the two layouts](../concepts/project-layout.md)).

- Without the key, the place of `driftgate.yaml` decides: a file in `.driftgate/` is `local`, one at the project root is `git`. A configuration of an earlier release stays as it was.
- A `baselines:` value that disagrees with the place of the file is refused (exit 2). [`driftgate baselines local|git`](commands/project.md#driftgate-baselines) moves the files.
- `driftgate init` makes a `local` project; `--baselines git` makes the other.
- A worktree has its own `.driftgate/`, which [`driftgate setup`](commands/project.md#setup-in-a-git-worktree) fills from the main worktree.

With `local`, nothing DriftGate writes leaves the project folder or
enters git:

- `init` adds `.driftgate/` to `.gitignore`, which it makes when missing. It also adds it to `.dockerignore` when there is one, or a `Dockerfile` or compose file that would copy the folder.
- In a monorepo it does so in every folder up to the top of the work tree with Docker files, by its path from there (`api/.driftgate/`).
- A file that has the entry already, as git or Docker reads its lines, is left as it is; git keeps leading spaces, and line endings are kept.
- Before a command writes the store (captures, private bodies, the last comparison) in a git work tree, it checks that git ignores `.driftgate/`.
- The project's `.gitignore`, or `.git/info/exclude` at the top of the work tree, is enough when it has the entry and no negation (`!…`) that takes any of it back. Otherwise it asks `git check-ignore` about the folder, the key, the store and the baselines.
- `key create` and `driftgate baselines` always ask git, which also finds files of the folder that git tracks.
- If git does not ignore one of them, the command stops (exit 2), and says what to add, take out or untrack.

## store

<!-- check: config-fragment -->
```yaml
store: ~/Data/shop-store
```

The local store holds captures, encrypted private bodies and pristine
metadata.

- Precedence: `DRIFTGATE_STORE`, then `store:`, then `.driftgate/store` (local) or `~/.driftgate/<project>` (git).
- A relative `store:` is relative to the project folder; `~/` is the home folder. A store inside the project's git repository, also from a subfolder, is refused (exit 2; [why](../security.md#uploaded-files)).

## key

<!-- check: config-fragment -->
```yaml
key:
  op: "op://Shop/driftgate-key/password"
```

The project key (32 bytes, base64) encrypts private bodies (AES-256-GCM),
and names them by a keyed hash (HMAC).

| Form | Source |
|---|---|
| no `key:`, `baselines: local` | `DRIFTGATE_KEY`, then `.driftgate/key` (`driftgate key create`) |
| no `key:`, `baselines: git` | `DRIFTGATE_KEY`, then the macOS Keychain (`driftgate key create`) |
| `key: { env: NAME }` | the environment variable `NAME` |
| `key: { op: "op://vault/item/field" }` | 1Password, through `op read` |

- `.driftgate/key` is open to its owner only (mode `600`); a file others can read is refused. On Windows the folder's access list protects it.
- Without a key, private bodies cannot be captured or compared (exit 3). Changing the key is not supported: bodies encrypted with the old key become unreadable ([handing a capture over](../guides/workflow/share-baselines.md#hand-a-capture-to-another-developer)).

## target and targets

<!-- check: config-fragment -->
```yaml
targets:
  api:
    base_url: http://localhost:8001
    concurrency: 4
  web:
    base_url: http://localhost:3001
    allow_hosts: [localhost, web.shop.example]
```

`target:` describes the application; `targets:` names several services,
such as an API and a frontend ([several services](../guides/frameworks/multi-service.md)).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `base_url` | URL | | where requests go; the request path is appended |
| `allow_hosts` | list of host names, IPs or CIDR ranges | local and private addresses (below) | the hosts requests may go to |
| `connect_to` | host or IP, optional `:port` | | send the requests to this address instead of the one DNS gives ([below](#connect_to)) |
| `user_agent` | string | `driftgate/1` | the `User-Agent` header of every request |
| `timeout` | duration (`30s`) | `60s` | per request; `0` is the default, a negative one is refused |
| `concurrency` | int, or `auto` | `1` | read-only requests sent in parallel ([auto](capture.md#parallel-requests-and-concurrency-auto)) |
| `follow_redirects` | bool | `false` | follow redirects instead of recording them (at most 10; below) |
| `rate_limits` | list of `{match, per_minute}` | | space the requests whose path (or `METHOD path`) matches the glob; `per_minute` must be positive |
| `aliases` | list of origins | | other origins of the application's own URLs (`APP_URL`, a second host name; [how](comparison.md#your-own-urls-on-another-port)) |
| `retry` | int, `0` to `3` | `1` | how often a read-only request that failed under load is sent again ([below](#retry)) |

- The default `allow_hosts` is `localhost` and `127.0.0.0/8`, `::1`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`. A list replaces it.
- A host name in the list may resolve anywhere; any other name must resolve only to allowed addresses. Redirects are checked too ([allowed hosts](../security.md#allowed-hosts)).
- With `follow_redirects`, a redirect from `https` to `http` fails. One to another host or port keeps only `User-Agent`, `Accept`, `Accept-Encoding` and `Accept-Language` ([redirects](../security.md#redirects-and-terminal-output)).

### connect_to

<!-- check: config-fragment -->
```yaml
target:
  base_url: https://shop.example
  connect_to: 203.0.113.10        # or 203.0.113.10:8443, [2001:db8::1]:443
  allow_hosts: [203.0.113.10]
```

`connect_to` sends every request to a fixed address, like
`curl --connect-to` or `--resolve`, such as while DNS still names the old server
([the task](../guides/workflow/live-server.md#reach-a-new-server-before-dns-points-to-it)).

- The requests keep `base_url`'s host name in the `Host` header, the TLS server name and the certificate check; only the connection goes to `connect_to`.
- Without a port, every port of the name goes to that host on the same port: an `http` to `https` redirect goes to `203.0.113.10:443`.
- With a port, `base_url`'s port goes to it, and any other port of the name is refused (`connect_to covers shop.example:443 only`). A redirect to another host is not changed.
- `allow_hosts` applies to the address connected to, not to the host name. Such a service needs `--allow-remote` like any remote one.
- `record`, `discover` and `setup` connect the same way; the project's own commands (`prepare`, `tokens.command`, `seed`, guards) do not.
- A capture records the setting per service in its `meta.json` (`connect_to`).

### retry

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:8000
  retry: 0
```

How many times a read-only `GET`, `HEAD` or `OPTIONS` request that timed
out, lost its connection, or got a `502`, `503` or `504` is sent again;
`0` means never ([retries](capture.md#retries-of-requests-that-failed-under-load)).

## identities and tokens

<!-- check: config-fragment -->
```yaml
tokens:
  command: ["php", "artisan", "driftgate:tokens"]
identities:
  admin: {}
  editor: {}
  auditor: { env: DG_TOKEN_AUDITOR }
```

A request with `"identity": "admin"` is sent with
`Authorization: Bearer <token>`. Tokens come from one of:

| Key | Meaning |
|---|---|
| `identities.<name>: { env: NAME }` | the environment variable `NAME` |
| `identities.<name>: { op: "op://…" }` | 1Password |
| `tokens: { command: [...] }` | a command that prints a JSON object `{"<identity>": "<token>"}` |
| `tokens: { file: path }` | a file with that JSON object; keep it out of the repository |

- `tokens` takes `command` or `file`, not both. In `tokens.file`, `~/` is the home folder, and a relative path is relative to the project.
- An identity's own `env` or `op` takes precedence over `tokens`.
- A catalog request whose identity has no token stops the capture ([logins](../guides/catalog/logins.md)).

## state

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: postgres
    dsn: { env: DG_DB_DSN }
    working: shop
```

The components restored before captures, so every capture starts from
the same data ([State: restoring the data](state.md)).

## seed, prepare, guards

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

| Key | Meaning |
|---|---|
| `seed` | commands that load the data into the working databases: migrations and seeders, a dump import |
| `prepare` | commands run after every restore |
| `guards` | `{name, command}`: checks that must succeed before requests that change data are sent |

- `driftgate seed` runs the `seed` commands and keeps the pristine copy; `seed --skip-commands` takes the data already loaded.
- Guards run when the capture has requests that change data, command steps or repeated requests. One that fails stops the capture, such as when mail would go to people ([when](capture.md#guards-and-prepare-during-a-capture)).
- A guard only checks: it must not write to the databases under `state:`, as a command that logs itself to a table does.

### Commands with a folder or standard input

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

A command of `seed`, `prepare` or a guard may be a mapping: `run` is the
list, `dir` the folder it runs in (default the project folder), and
`stdin` a file given as its standard input. Other commands stay lists.

- Both paths are relative to the project folder; `~` and `~/` are the home folder.
- A missing folder or file stops the command before the first of its list runs (exit 3), so a seed never stops halfway for it.
- A program in a container needs its standard input passed on: `docker exec -i`, or `docker compose exec -T`. For the first step above, that is `run: ["docker", "compose", "exec", "-T", "db", "mysql", "shop"]` with the same `stdin`.
- A release before this form refuses it ([keys an earlier release refuses](upgrading-driftgate.md#keys-an-earlier-release-refuses)).

### What belongs in prepare

`prepare` runs after every restore, so its time multiplies with the
number of restores.

- A step whose result does not depend on the restored data, such as a view or configuration cache, belongs before the capture. The exception is a step whose files a `state:` component restores.
- A step that changes the restored state, such as a migration that stamps `updated_at` on every row, makes that value drift in every run. Move it to `seed`, so the pristine copy holds its result.
- The capture warns when `prepare` changed the restored state ([how](capture.md#guards-and-prepare-during-a-capture)).

The migrate command belongs in `prepare`, such as `php artisan migrate --force`.
It does nothing when no migration is pending. When a step of an upgrade
adds one, every capture runs it after the restore, and the baseline's
data stays valid. Without it, a capture stops when a migration ran on the
working database ([why](capture.md#a-migration-run-outside-driftgate)).

## discover

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

Adapters that find the routes. `driftgate init` writes them; their
order does not change the result.

| Adapter | Required | Reads |
|---|---|---|
| `laravel` | `command` | `route:list --json`, or the table of Laravel before 8 |
| `symfony` | | `bin/console debug:router --format=json` |
| `rails` | | `bin/rails routes` |
| `django` | | a script run through `manage.py shell` (the `.venv` Python when there is one) |
| `spring` | | `/actuator/mappings` of the running application (needs the actuator `mappings` endpoint exposed) |
| `nextjs` | | the files of the app and pages routers |
| `nuxt` | | the files of `pages`, `server/api` and `server/routes` |
| `openapi` | `file` or `url` (one) | an OpenAPI 3 or Swagger 2 description, from a file (JSON or YAML) or the running application |
| `command` | `command` | any command that prints a JSON list of routes |

| Key | Meaning |
|---|---|
| `command` | `laravel`: the `route:list` command; `command`: the command that prints routes |
| `php` | `laravel`: how to run PHP inside the application; it then builds bodies from form request rules |
| `file` | `openapi`: the path of the description |
| `url` | `openapi`: a path on the service (`/openapi.json`) or a full URL, read only from allowed hosts |
| `identity` | `openapi` with `url`, `spring`: read the description as this identity, as some describe only what it may use |
| `target` | the service these routes belong to (required with several services) |
| `dir` | the folder of the service: commands run there and files are found there (default the project root) |
| `base_path` | a prefix of every path (Next.js `basePath`, an API prefix); starts with `/`, does not end with one |

### Output of the command adapter

<!-- check: routes-json -->
```json
[
  {"method": "GET", "path": "/api/news/{id}", "file": "src/news.go:42"},
  {"methods": ["PUT", "PATCH"], "path": "/api/news/{id}", "auth": true, "body": {"title": "x"}}
]
```

A JSON list. Each entry has `path` and `method` (or `methods`), and
optionally `name`, `auth` (needs an identity), `body` (a sample request
body) and `file` (the handler's source file). Paths start with `/` and
name parameters in braces: `{id}` is required, `{id?}` optional,
`{slug*}` takes the rest of the path, and `{slug*?}` the rest or nothing.

## templates, generate, params

<!-- check: config-fragment -->
```yaml
templates: [ok, not-found, json, json-errors]
generate:
  identity: admin
identities:
  admin: { env: DG_TOKEN_ADMIN }
params:
  - name: id
    value: "12"
```

`templates:` lists the variants `generate` makes for every route method,
`generate:` the identity it uses, and `params:` the values of route
parameters.

### Templates

The default is `[ok]`. `init` usually writes `[ok, not-found, json]`.
It adds `json-errors` for Laravel, Symfony, Rails and Spring Boot, and
`empty-body` for Laravel and FastAPI, whose discovery reads the rules
the framework validates request bodies against. Laravel gets
`[ok, not-found, json, empty-body, json-errors]`.

| Template | Request |
|---|---|
| `ok` | the route with its parameter values (one per value with `foreach`) |
| `not-found` | every required path parameter set to `999999999`, optional ones left out |
| `invalid-param` | every required path parameter set to `driftgate-invalid` |
| `anonymous` | a route that needs authentication, without an identity |
| `identities` | a route that needs authentication, once per other identity |
| `json` | with `Accept: application/json` |
| `empty-body` | a route that takes a body and has a sample, with `{}`: its validation error |
| `json-errors` | each error variant once more with `Accept: application/json` ([below](#json-errors)) |

A rate limit is not generated: write a
[repeated request](catalog-json.md#repeat) by hand.

Without `empty-body`, no request checks a validation error: a changed
validation message passes. Laravel, Symfony, Rails and Spring Boot
answer the `empty-body` request with a redirect back; its
[`json-errors`](#json-errors) twin gets the message. `coverage` lists the
covered routes whose body is validated (they have a sample) and whose
validation error no request shows.

### json-errors

`json-errors` sends each request of `not-found`, `invalid-param`,
`anonymous` and `empty-body` once more with `Accept: application/json`,
replacing an `Accept` the variant has. Its request IDs end in `-json`.

- Laravel, Symfony, Rails and Spring Boot answer a browser with an error page or a redirect (to the login page, or back to the form). They answer an API client with a JSON body (401, 404, 422), whose change is invisible without the header.
- It adds one request per error variant, never for `ok` (that is `json`) or `identities`. Its requests are error variants: they never cover a route.
- With `[ok, not-found, json, json-errors]` that is one request per route method with a required path parameter: at most a third more. `generate` says how many (`… (N of them json-errors)`).
- Frameworks that always answer JSON (FastAPI, NestJS, ASP.NET) gain nothing from it.
- Without `not-found`, `invalid-param`, `anonymous` or `empty-body` in `templates:`, it makes nothing, and `generate` says so in one note.
- For a route without a parameter value, `not-found` and `invalid-param` still make their requests. When `not-found` or `invalid-param` is a template, `json-errors` makes their JSON twins there, and is not listed as skipped.
- To adopt it in an existing project, see [keys an earlier release refuses](upgrading-driftgate.md#keys-an-earlier-release-refuses).

### Parameter values

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

`generate.identity` is the identity for routes that need authentication;
it must be under `identities:`, or come from `tokens:`. Each `params:`
entry has a `name`, an optional `route` (a glob over route paths; empty:
every route with that parameter), an optional `target`, and one source:

| Source | Meaning |
|---|---|
| `value: "12"` | a fixed value |
| `values: ["a", "b"]` | a list; the first value, or every value with `foreach: true` |
| `sql: "SELECT … ORDER BY …"` | a query on the pristine database; `foreach: true` makes a request per row |
| `from: { request: <id>, path: $.data.id }` | a value from an earlier response in the same capture; not with `foreach` |

- `sql` runs on the pristine database; `state:` names the component when there are several.
- It is one `SELECT` (or `WITH … SELECT`) with `ORDER BY`, so the catalog is the same every time. It is checked before it is sent, and runs in a read-only transaction.
- That check is a guard against mistakes, not a security boundary ([the queries of params](../security.md#the-queries-of-params)).
- With `from`, `generate` puts both requests in a sequence.

## storage and pii

<!-- check: config-fragment -->
```yaml
storage:
  default: private
  public: ["get-api-news*", "get-api-faq*"]
pii:
  allow: ['[a-z.]+@shop\.example']
```

Generated requests store their bodies as private: encrypted in the local
store, never in the repository. `storage.public` names public ones by a
glob over the request ID; `storage.default: public` makes all public. A
catalog entry has its own `storage`.

`pii.allow` lists regular expressions of values that may appear in public
bodies, such as a shared support address. Every public body is scanned
([the personal-data detector](../security.md#the-personal-data-detector)).

## observe

<!-- check: config-fragment -->
```yaml
observe:
  - match: "GET /up"
    reason: Health check; reports the current time.
```

Generated requests whose outcome depends on timing or a cache are
measured (status, time, size), never compared. `match` is a glob over
`METHOD /route/path`, `/route/path` or the request ID; `target` limits
it to one service. A catalog entry has its own `observe`.

## requests

<!-- check: config-fragment -->
```yaml
requests:
  - match: "GET /api/news/*"          # counts views: changes data
    mutates: true
  - match: post-api-search            # a search sent as POST only reads
    mutates: false
    body: { q: news, page: 1 }
```

What `generate` writes into the requests it makes: `mutates`, and a JSON
`body` written as YAML. `match` and `target` name requests as `observe`
does; where several entries name a request, the last one with the key
wins.

- Mark a generated entry here, then run `generate --write`: the next `generate --write` would undo a change by hand, and `run` reports a catalog that differs from what it generates.
- For read-only requests that changed data, `capture` and `run` print the entries to add, by route; for entries written by hand, the catalog change instead.
- `generate` notes an entry that names no generated request.
- `mutates: false` is for one route that only reads, such as a search sent as `POST`: it runs with the read-only requests, without a restore before it.
- `mutates: false` for another method that changes data (`DELETE`, `PUT`, `PATCH`, any other verb), or for requests of more than one route (`match: "POST *"`), stops `generate` (exit 2).

The body is JSON as the YAML is written, sent as `application/json`:

- A number keeps its digits (`1.0`, a large integer), a date stays its text, and `null` is JSON's `null`; `body:` with nothing after it is no body.
- A value JSON would read otherwise (`0x1F`, `007`, `.inf`), a tagged value (`!!binary`), a key that is not a string, and merge keys (`<<`) are refused: quote them, or write the keys out.
- A body is at most 10 MB, aliases included.
- It replaces the route's sample body for every template but `empty-body`, which keeps `{}`. For a route that uploads files, its fields are the form's fields.

## uploads

<!-- check: config-fragment -->
```yaml
uploads:
  dir: ~/Data/shop-uploads
```

The folder of the real files that catalog requests upload
([multipart](catalog-json.md#multipart)). `DRIFTGATE_UPLOADS` takes
precedence; `~/` is the home folder, and a relative path is relative to
the project folder. A folder inside the project's git repository is
refused, also through a symbolic link.

## bisect

<!-- check: config-fragment -->
```yaml
bisect:
  checkout: ["sh", "-c", "cd \"$DRIFTGATE_BISECT_DIR\" && make restart"]
  ready: ["curl", "-sf", "http://localhost:8000/up"]
  finish: ["make", "restart"]
  timeout: 5m
```

The hooks of [`driftgate bisect`](commands/review.md#driftgate-bisect),
run with `DRIFTGATE_BISECT_DIR` (the worktree of the commit under test)
and `DRIFTGATE_BISECT_COMMIT` set.

| Key | Meaning |
|---|---|
| `checkout` | required for bisect: builds or restarts the application from `$DRIFTGATE_BISECT_DIR`; failing skips the commit |
| `ready` | repeated until it succeeds; failing within `ready_timeout` skips the commit |
| `finish` | once at the end, such as to start the application from your checkout again |
| `timeout` | the limit of `checkout` (default `10m`) |
| `ready_timeout` | default `1m` |

## data

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

The data module ([`driftgate data …`](commands/data.md)) makes a copy of
a database with personal values replaced
([anonymized test data](../guides/ci/test-data.md)).

| Key | Meaning |
|---|---|
| `driver` | `mysql` (also MariaDB) or `postgres` |
| `source` | `{dsn, database}`: the database that is read, a copy of production data; never written |
| `target` | `{dsn, database}`: the database that receives the copy, not the source; `dsn` defaults to the source's |
| `allow_remote` | allow a target host outside `target.allow_hosts`; the target is dropped and created again, so it must be a copy |
| `key` | `{env}` \| `{op}`: the key that replaces values deterministically; default the project key |
| `files` | `{source, target}`: uploaded files, and an empty folder outside the source that gets their placeholders |

- `files` takes both folders or neither, relative to the project.
- The copy has the source's tables, views, triggers and sequences; on MySQL and MariaDB also its functions, procedures and events. They are copied without their `DEFINER`, with their SQL mode and collation; an event keeps its status, so it runs on the copy with the scheduler on.
- Reading a routine's definition needs `SHOW_ROUTINE` (MySQL), `SELECT` on `mysql.proc` (MariaDB), or being its definer; without it, the run stops and names the routine.
- A MySQL or MariaDB target's `max_allowed_packet` bounds every `INSERT`: a row that does not fit stops the run, naming its table and column.

---
**Related:** [State: restoring the data](state.md) · [catalog.json](catalog-json.md) · [allowlist.yaml](allowlist-yaml.md) · [Command line basics](cli.md)
