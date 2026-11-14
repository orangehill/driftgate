# Frameworks

`driftgate setup` recognises the framework and writes its adapter and templates.
This page lists what to check or add by hand.

## Laravel

Laravel 5.5 and later. Run `driftgate setup` where `artisan` is.

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://shop.test            # Herd or Valet; php artisan serve: http://localhost:8000
discover:
  - adapter: laravel
    command: ["php", "artisan", "route:list", "--json"]
    php: ["php"]                        # reads form request rules for sample bodies
templates: [ok, not-found, json, empty-body, json-errors, anonymous, identities]
state:
  db:
    driver: mysql
    dsn: { dotenv: .env }
    working: shop
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
    restore_only: [personal_access_tokens]
seed:
  - ["php", "artisan", "migrate:fresh", "--seed", "--force"]
prepare:
  - ["php", "artisan", "migrate", "--force"]
tokens:
  command: ["php", "artisan", "driftgate:tokens"]
identities:
  admin: {}
  user: {}
generate:
  identity: admin
```

| Part | What to know |
|---|---|
| `templates` | `empty-body` sends `{}` to routes with rules; its `json-errors` twin compares the 422 message. |
| Rules in controllers | `$request->validate([...])` is not read: such routes get no sample body, and `coverage` lists them. |
| Own auth middleware | `auth`, `auth:sanctum`, `jwt.auth` routes get `generate.identity`. Name others by class, as `route:list --json` shows it: `discover[].auth: [GetUserFromToken]`. |
| `prepare` | Runs after every restore. Keep it to migrations the upgrade step adds; a migration that rewrites rows belongs in `seed`. |
| Docker or Sail | Run the adapter in the container: `["docker", "compose", "exec", "-T", "laravel.test", "php", ...]`. |
| SQLite | `driver: sqlite`, `working: database/database.sqlite`; run `php artisan migrate` before setup. |

### Repeatable seeders

The default seeder writes `Hash::make()` and `now()`, which differ on every run.
Seeding again then stops the next capture (exit 3).

- Write a fixed password hash and a fixed time (`Carbon::setTestNow()`) in the seeder.
- Or seed from a fixed dump with `driftgate seed --skip-commands`.

### Tokens with Sanctum

The tokens command runs after every restore. It must print the same
tokens without writing new rows.

```php
// database/seeders/DatabaseSeeder.php, after the test users
User::where('email', 'admin@shop.example')->first()->tokens()->create([
    'name' => 'driftgate', 'token' => hash('sha256', 'dg-admin-token'), 'abilities' => ['*'],
]);

// routes/console.php
Artisan::command('driftgate:tokens', function () {
    $this->line(json_encode(['admin' => 'dg-admin-token', 'user' => 'dg-user-token']));
});
```

Sanctum writes `last_used_at` on every authenticated request. So
`personal_access_tokens` is `restore_only`: kept as seeded at every
restore, its writes never fingerprinted or compared. `init` and `setup`
propose it when `composer.json` requires `laravel/sanctum`. A request
that makes or deletes a token is then not seen as a side effect: check
login and logout by their responses.

The generated login sends sample values and fails: give it a real test user under
`requests:` ([configuration](configuration.md#storage-observe-and-requests)).

### Web forms answer 419

DriftGate keeps no session, so a POST, PUT, PATCH or DELETE to a
`routes/web.php` route answers 419. Routes in `routes/api.php` have no
CSRF check.

```php
// bootstrap/app.php (Laravel 11+; before: $except of VerifyCsrfToken)
->withMiddleware(function (Middleware $middleware) {
    $middleware->validateCsrfTokens(except: ['contact', 'orders/*']);
})
```

Keep that exception out of what you deploy. Without it, both sides compare only the 419.

## ASP.NET Core

Setup writes an `openapi` adapter and `base_url` from `launchSettings.json`.
It reads the DSN with `dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Default" }`.
Run the EF Core migrations first. The login's rights are in [databases](databases.md#sql-server).

## Next.js and Nuxt

The `nextjs` and `nuxt` adapters read routes from the files. Set
`base_path` for a `basePath`, and give dynamic segments values under
`params:`. Capture the production build (`next build && next start`),
never the dev server. `init` allows build ids, nonces and hashed asset
URLs.

## OpenAPI

<!-- check: config-fragment -->
```yaml
discover:
  - adapter: openapi
    url: /v3/api-docs          # or file: openapi.yaml
    identity: admin            # some APIs describe only what the caller may use
```

For Spring Boot, FastAPI, NestJS, Quarkus, Micronaut and Magento. Path parameters need `params:`.

## Several services

`init` finds services in the root, `apps/*`, `services/*` and
`packages/*`, and writes `targets:` with one adapter per service
(`target:` and `dir:`). `state:` is shared by all. `driftgate run` checks
every service; `--target api` checks one and reports the check as partial.
