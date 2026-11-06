# Laravel

Laravel 5.5 and later. Checked on a fresh Laravel 13 application: all
routes found, bodies built from validation rules, a byte-identical catalog
on every `generate`, a baseline, a clean second run, and a new route that
failed `discover` and `coverage`.

## 1. init

In the project root (where `artisan` and `composer.json` are):

```sh
driftgate init
```

`init` recognises Laravel and writes:

- a `laravel` adapter: routes from `php artisan route:list --json` (the
  table of Laravel before 8), and with `php: ["php"]` the validation rules of
  form request classes, from which `generate` builds request bodies;
- `templates: [ok, not-found, json, json-errors]` (`json-errors`: the error variants once more as an API client asks for them, see [templates](../configuration.md#templates-generate-params));
- exclusions with reasons for Debugbar, Ignition, Telescope, Horizon and
  `/storage/*`;
- allowlist entries for `Date` and the throttle headers that depend on time
  (`X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`).

Rules written directly in a controller (`$request->validate([...])`) are not
read; such routes get no sample body, and `coverage` lists them.

`state:` and `base_url` come from `.env` (`DB_*`, `APP_URL`; an SQLite
file, the default of Laravel 11 and later, see [SQLite](#sqlite)); with Sail,
from the compose file: the `mysql` service's published port and its root
user, which can create the pristine copy (fine for Sail's local container;
on a shared server use a limited user). `init` writes `state:` commented
out; `driftgate setup` turns it on after asking and goes on to the key,
the pristine copy and the first baseline ([setup](../commands.md#setup)).

## 2. Where the application runs

**Herd or Valet.** The site is `http://<folder>.test`. Herd resolves `.test`
to `127.0.0.1`, which the default `allow_hosts` allows, so only `base_url`
changes:

```yaml
target:
  base_url: http://shop.test
```

**`php artisan serve`.** `http://localhost:8000`, as `init` writes.

**Docker Compose or Sail.** Publish the application's port on the host and
run the adapter's commands inside the container. With `-T` (no TTY) the
validation-rules script can be passed to PHP on its standard input. For Sail
the service is usually `laravel.test`:

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost
discover:
  - adapter: laravel
    command: ["docker", "compose", "exec", "-T", "laravel.test", "php", "artisan", "route:list", "--json"]
    php: ["docker", "compose", "exec", "-T", "laravel.test", "php"]
```

DriftGate itself runs on the host. A database whose port is published on
`127.0.0.1` counts as local.

**PHP's opcache.** Herd and `php artisan serve` keep compiled files with
`opcache.revalidate_freq=2`: a file changed less than 2 seconds before a
request may still run as it was. Between a change and a capture (and in
`bisect` hooks) wait 2 seconds, or set `opcache.revalidate_freq=0` in
development.

## 3. The database

DriftGate restores the database the application uses (`working`) from a
pristine copy before every capture, so `working` must be a database you can
rewrite, not one you keep work in.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql                    # MariaDB too; postgres for PostgreSQL; sqlite below
    dsn: { env: DG_DB_DSN }          # export DG_DB_DSN=mysql://root:secret@127.0.0.1:3306/
    working: shop                    # DB_DATABASE of the application
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
seed:
  - ["php", "artisan", "migrate:fresh", "--seed", "--force"]
guards:
  - name: mail goes to the log
    command: ["sh", "-c", "grep -q '^MAIL_MAILER=log' .env"]
```

- `driftgate seed` (or the first `run`) runs the seed commands and keeps
  `shop_pristine`. To start from a dump instead:
  `["sh", "-c", "gunzip -c ~/Data/shop.sql.gz | mysql shop"]` followed by
  `["php", "artisan", "migrate", "--force"]`.
- Exclude tables that only hold logs, sessions or queued jobs; `setup`
  proposes Laravel's own. The cache is `restore_empty`: emptied at every
  restore, never compared, so a cached value or a rate limit's counter
  does not carry over from one capture to the next, and the cache the
  application had when the pristine copy was made is not kept
  ([restore_only](../configuration.md#restore_only)). A large
  `failed_jobs` table makes fingerprints slow; empty it in a seed
  command, or exclude it.
- With a Redis cache or session store, add it as a component:
  `redis: { restore: ["redis-cli", "FLUSHALL"] }`. A view counter in Redis
  that is not restored makes the second capture differ.
- With Sail or Docker, run the seed commands through
  `docker compose exec -T laravel.test …` as well.

### SQLite

Laravel 11 and later start on SQLite: `DB_CONNECTION=sqlite` with
`DB_DATABASE` unset is `database/database.sqlite`, and the sessions,
cache and queue (`SESSION_DRIVER`, `CACHE_STORE`, `QUEUE_CONNECTION` =
`database`) are tables of the same file. `setup` finds the file, proposes
to exclude those tables and to restore the cache only (default yes),
grants nothing, and makes the pristine copy as a file of DriftGate's
store:

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { dotenv: .env }               # it must name working
    working: database/database.sqlite
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
```

- Restores write inside the file while `php artisan serve` (or Herd) keeps
  it open; the application sees the restored data on its next query. See
  [SQLite](../configuration.md#sqlite) for what is restored and how.
- Leave `DB_DATABASE` unset or make it absolute. A relative one is taken
  from the project root, as Laravel 11 and later do when it is not in the
  folder PHP runs in; Laravel 10 and earlier open it from that folder
  only (`public/` under `php artisan serve` and PHP-FPM), and `setup`
  notes it.
- `setup` takes the data already in the file as pristine (`seed
  --skip-commands`); run the migrations first (`php artisan migrate`),
  which make the file.
- Laravel keeps the file in rollback mode, where a fingerprint keeps
  writers waiting while it reads (a few milliseconds on a small file). A
  queue worker on the same file writes `jobs`, which is excluded.
- Checked on a Laravel 12 application: fingerprints 4–5 ms, restores
  3–4 ms, about 0.1–0.2 s per capture of 18 requests.

### SQL Server

`DB_CONNECTION=sqlsrv` with `DB_HOST`, `DB_PORT`, `DB_DATABASE`,
`DB_USERNAME` and `DB_PASSWORD`, here for SQL Server in a container:

```yaml
# compose.yaml
services:
  mssql:
    image: mcr.microsoft.com/mssql/server:2022-latest
    platform: linux/amd64                # also on Apple Silicon (Rosetta)
    environment:
      ACCEPT_EULA: "Y"
      MSSQL_PID: Developer
      MSSQL_SA_PASSWORD: ${MSSQL_SA_PASSWORD}   # 8+ characters of three kinds
    ports: ["127.0.0.1:1433:1433"]
```

```ini
# .env
DB_CONNECTION=sqlsrv
DB_HOST=127.0.0.1
DB_PORT=1433
DB_DATABASE=shop
DB_USERNAME=shop
DB_PASSWORD=…
DB_TRUST_SERVER_CERTIFICATE=true
```

- `DB_ENCRYPT` and `DB_TRUST_SERVER_CERTIFICATE` count only when
  `config/database.php` passes them on: uncomment the `encrypt` and
  `trust_server_certificate` lines of its `sqlsrv` connection (Laravel
  ships them commented out). Laravel 11 and 12 start without the file
  until `php artisan config:publish database` writes it; Laravel 13
  ships it. Without them DriftGate takes the
  client's own default and verifies the container's self-signed
  certificate, which fails.
- Make the database and the application's login with the container's
  `sqlcmd` (`docker compose exec mssql /opt/mssql-tools18/bin/sqlcmd -C
  -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -Q "CREATE DATABASE shop"`);
  `setup` reads the compose service and signs in as `sa` for DriftGate,
  and the application keeps a login that is only a user of `shop`
  ([why](../configuration.md#sql-server)).
- `seed: [["php", "artisan", "migrate:fresh", "--seed", "--force"]]` as
  above; exclude `sessions`, `jobs`, `job_batches` and `failed_jobs` and
  restore only `cache` and `cache_locks` when they are tables of the
  database (`setup` proposes them).

## 4. Authenticated routes

DriftGate sends `Authorization: Bearer <token>` for a request with an
identity. A small artisan command that prints the tokens of test users as a
JSON object is the usual source:

<!-- check: config-fragment -->
```yaml
tokens:
  command: ["php", "artisan", "driftgate:tokens"]    # prints {"admin": "…", "user": "…"}
identities:
  admin: {}
  user: {}
generate:
  identity: admin
templates: [ok, not-found, json, anonymous, identities]
```

The tokens command runs after every restore and the `prepare` commands, so
tokens it creates in the database exist for the requests that follow. Session-based (cookie) login can be
written as a sequence in the catalog: a login request, then the requests
that use it.

## 5. The first run

```sh
driftgate key create
driftgate run
```

The first run captures twice. On a fresh Laravel application it refuses
the baseline because of the session cookies and proposes allowlist entries
with an exact format (cookie names and attributes literal; token, date and
lifetime as classes); add a reason to each and run again.

Routes that upload files get a synthetic file of a kind their rules accept
(`file`, `image`, `mimes:`, `mimetypes:`). To check a real import, write a
catalog entry with a real file from the [uploads folder](../configuration.md#multipart).
A queued job can run as a [command step](../configuration.md#command-steps):
`["php", "artisan", "queue:work", "--stop-when-empty"]`.

With `baselines: git`, commit `driftgate.yaml` and `driftgate/`, and add
the CODEOWNERS line `init --baselines git` prints; with `baselines: local`
nothing goes into git.
