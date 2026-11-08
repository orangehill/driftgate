# Laravel

> **Before you start:** [install DriftGate](../../start/install.md), and have the application running. **Time:** 30 minutes.

Laravel 5.5 and later; checked on a fresh Laravel 13 application.

## Set up a Laravel project

```sh
cd shop                # where artisan and composer.json are
driftgate setup
```

Setup recognises Laravel, and writes:

- a `laravel` adapter: routes from `php artisan route:list --json`; with `php: ["php"]`, request bodies built from the form request classes' rules;
- `templates: [ok, not-found, json, json-errors]` ([templates](../../reference/driftgate-yaml.md#templates));
- exclusions with reasons for Debugbar, Ignition, Telescope, Horizon and `/storage/*`;
- allowlist entries for `Date`, and the throttle headers that depend on time: `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`.
- Rules written in a controller (`$request->validate([...])`) are not read: such routes get no sample body, and `coverage` lists them.
- The database and `base_url` come from `.env` (`DB_*`, `APP_URL`). With Sail, from the compose file: the `mysql` service's port and its root user, fine for a local container only.

## Where the application runs

### Herd or Valet

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://shop.test
```

The site is `http://<folder>.test`. Herd resolves it to `127.0.0.1`,
which `allow_hosts` allows by default, so only `base_url` changes.

### php artisan serve

`php artisan serve` answers at `http://localhost:8000`. Setup writes `base_url` from `APP_URL`, so set `APP_URL=http://localhost:8000` in `.env`, or pass `--url`.

### Docker Compose or Sail

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost
discover:
  - adapter: laravel
    command: ["docker", "compose", "exec", "-T", "laravel.test", "php", "artisan", "route:list", "--json"]
    php: ["docker", "compose", "exec", "-T", "laravel.test", "php"]
```

Publish the application's port, and run the adapter's commands in the
container; `-T` (no TTY) passes the rules script to PHP. For Sail, the
service is usually `laravel.test`. DriftGate runs on the host, and a
database port published on `127.0.0.1` counts as local. Run the seed
commands through `docker compose exec -T laravel.test …` too.

### PHP's opcache delays your changes

```ini
; php.ini, in development
opcache.revalidate_freq=0
```

Herd and `php artisan serve` use `opcache.revalidate_freq=2`: a file
changed less than 2 seconds before a request may still run as it was.
Wait 2 seconds between a change and a capture, or set the value above.

## The database

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

Every capture restores `working`, so it must be a database you can rewrite.

- `driftgate seed`, or the first `run`, runs the seed commands and keeps `shop_pristine`. To start from a dump: `["sh", "-c", "gunzip -c ~/Data/shop.sql.gz | mysql shop"]`, then `migrate --force`.
- Exclude logs, sessions and queued jobs, and make the cache `restore_empty`; `setup` proposes them ([the framework's own tables](../../reference/state.md#the-frameworks-own-tables)). A large `failed_jobs` slows fingerprints: empty it in a seed command, or exclude it.
- A Redis cache or session store is a component of its own: `redis: { restore: ["redis-cli", "FLUSHALL"] }`. A view counter in Redis that is not restored makes the second capture differ.
- Laravel's default seeder writes `Hash::make()` and `now()`, which differ on every run. Seeding again then makes other data, and the next capture stops (exit 3): the state differs from the baseline's. Write a fixed password hash and a fixed time (`Carbon::setTestNow()`) in the seeder, or seed from a fixed dump with `driftgate seed --skip-commands`.
- A migration you run on the working database is undone by the next restore: seed again after it.

### SQLite

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

Laravel 11 and later start on SQLite, in `database/database.sqlite`,
with sessions, cache and queue as tables of the same file. `setup` finds
it, proposes those tables, and keeps the pristine copy in its store.

- Run `php artisan migrate` first, which makes the file; `setup` takes its data as pristine.
- Leave `DB_DATABASE` unset, or make it absolute ([which file a setting names](../../reference/databases/sqlite.md#which-file-a-framework-setting-names)).
- Restores write inside the file while the application keeps it open; it sees the restored data on its next query. On a Laravel 12 application, a capture of 18 requests took about 0.1–0.2 s ([SQLite](../../reference/databases/sqlite.md)).

### SQL Server

<!-- check: yaml -->
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

- `DB_ENCRYPT` and `DB_TRUST_SERVER_CERTIFICATE` count only when `config/database.php` passes them on. Uncomment the `encrypt` and `trust_server_certificate` lines of its `sqlsrv` connection.
- Laravel 11 and 12 have that file only after `php artisan config:publish database`; Laravel 13 ships it. Without the lines, the container's self-signed certificate fails.
- Make the database with the container's `sqlcmd`: `docker compose exec mssql /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -Q "CREATE DATABASE shop"`.
- `setup` signs in as `sa` for DriftGate; the application keeps a login that is only a user of `shop` ([why](../../reference/databases/sql-server.md#what-the-login-needs)).

## Routes that need a login

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

The tokens command runs after every restore, and must leave the same
state each time. One that issues new tokens writes new rows, so captures
differ. Print tokens issued once, such as Sanctum tokens the seeder
creates with fixed values, or use a [tokens file](../catalog/logins.md).
A cookie-session application needs a token too
([sessions with cookies](../catalog/logins.md#sessions-with-cookies)).

<!-- check: config-fragment -->
```yaml
requests:
  - match: post-api-login              # the login route, with a test user's real credentials
    body: { email: admin@shop.example, password: secret }
```

The generated login sends sample values and answers 401 or 422, so its
route counts as failed and `run` exits 1. Give it a real test user under
[`requests`](../../reference/driftgate-yaml.md#requests).

## The first run

```sh
driftgate run
```

On a fresh application the session cookies differ, so the first run
proposes allowlist entries: add a reason to each, and run again.

- Routes that upload files get a synthetic file of a kind their rules accept (`file`, `image`, `mimes:`, `mimetypes:`). A real import needs a [real file](../catalog/hand-written-requests.md#upload-a-file).
- A queued job runs as a [command step](../catalog/hand-written-requests.md#run-a-queue-worker-or-another-command): `["php", "artisan", "queue:work", "--stop-when-empty"]`.
- Schema dumps, settings, files and mail are checked as [side checks](../catalog/side-checks.md).
- With `baselines: git`, commit `driftgate.yaml` and `driftgate/`, and protect the baselines ([CODEOWNERS](../../security.md#protect-the-baselines)).

---
**Next:** [Read a drift report](../review/read-the-report.md): what the first comparison shows.
**Related:** [State: restoring the data](../../reference/state.md) · [SQLite](../../reference/databases/sqlite.md) · [Upgrade a framework](../workflow/framework-upgrade.md)
