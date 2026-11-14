# Configuration

`driftgate.yaml` configures a project. `driftgate init` and `driftgate setup`
write it, and `driftgate help config` lists its top-level keys. An unknown
key is an error, not a warning.

## Where the files are

`baselines:` chooses the layout.

| | `local` (default) | `git` |
|---|---|---|
| `driftgate.yaml` | `.driftgate/driftgate.yaml` | `driftgate.yaml` at the root |
| `catalog.json`, `allowlist.yaml`, `routes.json`, `exclusions.yaml`, `classify.yaml`, `baselines/` | `.driftgate/` | `driftgate/`, committed |
| local store: captures, private bodies | `.driftgate/store/` | `~/.driftgate/<project>/` |
| project key | `key:`, then `DRIFTGATE_KEY`, then `.driftgate/key` | `key:`, then `DRIFTGATE_KEY`, then the macOS Keychain |
| for | checks on your machine before a pull request | a baseline shared by the team or CI |

Git must ignore `.driftgate/`; `init` adds it to `.gitignore`. Commands find
the project from any folder inside it. `driftgate baselines local|git`
moves a project to the other layout.

## A complete example

A Laravel API on MySQL, with Redis:

<!-- check: config -->
```yaml
version: 1
project: shop
mode: evolve                 # drifts may be accepted; freeze: never
level: values

target:
  base_url: http://localhost:8000
  concurrency: 4             # read-only requests in parallel
  rate_limits:
    - match: "/api/search*"
      per_minute: 50

identities:
  admin: { env: DG_TOKEN_ADMIN }
  user: { op: "op://Shop/driftgate-user/token" }

state:
  db:
    driver: mysql
    dsn: { dotenv: .env }    # read DB_* from the application's .env
    working: shop
    exclude: [sessions, jobs, failed_jobs]
    restore_empty: [cache, cache_locks]
    row_tracking: [orders, users: { ignore: [last_seen_on] }]
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
templates: [ok, not-found, json, empty-body, json-errors]
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

Commands are lists: the program and its arguments, run from the project
folder without a shell. Write a shell explicitly: `["sh", "-c", "…"]`.

## Keys

### Project

| Key | Type | Meaning |
|---|---|---|
| `version` | int | `1`; required |
| `project` | name | letters, digits, `. _ -`; names the store and the Keychain entry; required |
| `mode` | `freeze` \| `evolve` | `freeze` (default): every drift fails unless expected; `evolve`: drifts may be accepted ([modes](how-it-works.md#modes-evolve-and-freeze)) |
| `level` | `status` \| `shape` \| `types` \| `values` | how far a comparison goes; default `values` ([levels](how-it-works.md#compare-and-levels)) |
| `baselines` | `local` \| `git` | the layout; default: where `driftgate.yaml` is |
| `store` | path | the local store; never inside the repository |
| `key` | `{env}` \| `{op}` | where the project key comes from ([key](security.md#the-project-key)) |

### Target

`target:` describes one service with the keys below ([several services](frameworks.md#several-services)).

| Key | Type | Meaning |
|---|---|---|
| `targets.<name>` | target | several services by name, each with these keys; not with `target` |
| `base_url` | URL | where requests go |
| `allow_hosts` | list | host names, IPs or CIDR ranges requests may reach; default: localhost and private ranges |
| `connect_to` | host[:port] | connect there instead of where DNS points; `Host` and TLS keep `base_url`'s name |
| `user_agent` | string | default `driftgate/1` |
| `timeout` | duration | per request; default `60s` |
| `concurrency` | int \| `auto` | read-only requests sent in parallel; default `1` |
| `follow_redirects` | bool | follow redirects instead of recording them; default `false` |
| `rate_limits[].match`, `per_minute` | glob, int | space the requests whose path matches the glob |
| `aliases` | list of origins | other origins of the application's own URLs, such as `APP_URL` |
| `retry` | `0`–`3` | how often a read-only request that failed under load is sent again; default `1` |

### Identities and tokens

| Key | Type | Meaning |
|---|---|---|
| `identities.<name>` | `{env}` \| `{op}` \| `{}` | a user; its requests carry `Authorization: Bearer <token>` |
| `tokens.command` | command | prints `{"<identity>": "<token>", …}` |
| `tokens.file` | path | a file with that JSON object; keep it out of git |

An identity's own `env` or `op` wins over `tokens`. Use `command` or
`file`, not both ([tokens](catalog.md#identities-and-tokens)).

### State

`state.<name>` is a component restored before every capture
([databases](databases.md)).

| Key | Type | Meaning |
|---|---|---|
| `driver` | `mysql` \| `postgres` \| `sqlite` \| `sqlserver` | also `mariadb` and `mssql`; without a driver, the component uses commands |
| `dsn` | DSN source | the server ([below](#dsn-sources)) |
| `working` | string | the database the application uses; SQLite: its file |
| `pristine` | string | its pristine copy; default `<working>_pristine` |
| `exclude` | globs | tables never fingerprinted or restored: sessions, logs |
| `restore_only` | globs | tables restored every time, but never compared |
| `restore_empty` | globs | `restore_only` tables restored to no rows: caches |
| `row_tracking` | tables | also record which rows each request changes and the values it writes; `name: { ignore: [columns] }` leaves columns out ([row tracking](databases.md#row-tracking)) |
| `allow_remote` | bool | allow a host that no `allow_hosts` names; it must be a copy |
| `restore` | command | without a driver: restores the component; required |
| `fingerprint` | command | without a driver: its output must be the same after every restore |

### DSN sources

A `dsn:` of `state:` or `data:` never holds the password in the file. It
names one source; `key` and `host` refine a file source.

| Key | Reads |
|---|---|
| `env` | an environment variable: a URL (`mysql://`, `postgres://`, `sqlserver://`), JDBC or ADO.NET string |
| `op` | a 1Password reference, `op://vault/item/field` |
| `dotenv` | a `.env` file: Laravel's `DB_*`, `DB_URL`, `DATABASE_URL`, or a NestJS setting |
| `appsettings` | ASP.NET settings of a `.csproj`; `key` required: `ConnectionStrings:Default` |
| `properties` | Spring, Quarkus or Micronaut `application.properties` or `.yml` |
| `yaml` | Rails' `database.yml`, or a Go service's `config.yaml` |
| `php` | Magento's `app/etc/env.php` |
| `python` | Django's `settings.py`, a literal `DATABASES` |
| `compose` | a database service of a compose file; `key` is the service name |
| `key` | which setting of the file to read |
| `host` | replaces the host and port: `127.0.0.1:3307`, a socket path, or `compose:<service>` |

Percent-encode special characters in a URL's user and password. The URL's
database is ignored: `working` names it. A setting that names another
database than `working` stops the command (exit 2).

### Seed, prepare and guards

| Key | Type | Meaning |
|---|---|---|
| `seed` | commands | load the data: migrations, seeders, a dump; `driftgate seed` then keeps the pristine copy |
| `prepare` | commands | run after every restore |
| `guards[].name`, `command` | name, command | checks that must pass before requests that change data, such as "mail goes to the log" |
| `run`, `dir`, `stdin` | list, path, path | a command as a mapping: the list, its folder, a file for its standard input |

A `prepare` step that changes the restored data drifts on every run: move
it to `seed`.

### Discovery and generation

| Key | Type | Meaning |
|---|---|---|
| `discover` | list | where the routes come from; `init` writes it |
| `discover[].adapter` | name | `laravel`, `symfony`, `rails`, `django`, `spring`, `nextjs`, `nuxt`, `openapi`, `command` |
| `discover[].command` | command | `laravel`: its `route:list`; `command`: prints the routes as JSON |
| `discover[].php` | command | `laravel`: runs PHP in the application, to build bodies from validation rules |
| `discover[].file`, `discover[].url` | path, URL | `openapi`: the description, from a file or the running service |
| `discover[].identity` | name | `openapi` with `url`, `spring`: read as this identity |
| `discover[].target`, `discover[].dir`, `discover[].base_path` | | the service, its folder, and a prefix of every path |
| `templates` | list | request variants per route ([templates](catalog.md#generate-and-templates)); default `[ok]` |
| `generate.identity` | name | the identity for routes that need authentication |
| `params[].name`, `route`, `target` | name, glob, service | which route parameter; `route` and `target` narrow it |
| `params[].value`, `values`, `foreach` | string, list, bool | a fixed value, or several; `foreach: true` sends one request per value |
| `params[].sql`, `state` | query, component | `SELECT … ORDER BY …` on the pristine database; read-only |
| `params[].from.request`, `path` | ID, JSON path | a value from an earlier response, such as `$.data.id` |

### Storage, observe and requests

| Key | Type | Meaning |
|---|---|---|
| `storage.default` | `private` \| `public` | how generated requests keep bodies; default `private`: encrypted, never in git |
| `storage.public` | globs | request IDs whose bodies are public |
| `pii.allow` | regexes | values the personal-data detector lets through in public bodies |
| `observe[].match`, `target`, `reason` | glob, service, text | generated requests measured but never compared |
| `requests[].match`, `target`, `mutates`, `body` | glob, service, bool, YAML | whether generated requests change data, and their JSON body |

`match` is a glob over `METHOD /route/path`, `/route/path` or the request
ID. Set `mutates` here, not in the catalog: `generate --write` keeps it.

### Uploads, bisect and data

| Key | Type | Meaning |
|---|---|---|
| `uploads.dir` | path | real files that requests upload; outside the repository |
| `bisect.checkout` | command | builds or restarts the application from `$DRIFTGATE_BISECT_DIR`; required for bisect |
| `bisect.ready` | command | repeated until it succeeds |
| `bisect.finish` | command | once at the end |
| `bisect.timeout`, `bisect.ready_timeout` | duration | defaults `10m` and `1m` |
| `data.driver` | `mysql` \| `postgres` | the data module's server |
| `data.source.dsn`, `database` | DSN source, name | a copy of production data; read only |
| `data.target.dsn`, `database` | DSN source, name | receives the anonymized copy; `dsn` defaults to the source's |
| `data.allow_remote` | bool | allow a target host outside `allow_hosts`; it is dropped and made again |
| `data.key` | `{env}` \| `{op}` | the key that replaces values; default the project key |
| `data.files.source`, `target` | paths | uploaded files, and a folder for their placeholders |
