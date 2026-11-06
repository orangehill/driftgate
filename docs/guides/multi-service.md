# Several services and monorepos

A project can have several services deployed together: a Laravel API, a
Next.js frontend, microservices. Each has its own address, allowed hosts,
limits and adapters, and every route, catalog entry and rule knows its
service. One `run` checks them all on the same restored state and gives one
report and one exit code.

## init in a monorepo

`init` looks in the root, its subfolders, `apps/*`, `services/*` and
`packages/*`, and reads the ports from `docker-compose.yml`:

```
$ driftgate init --project mono
created .driftgate/driftgate.yaml, .driftgate/catalog.json and .driftgate/allowlist.yaml
found api (laravel, apps/api), web (nextjs, apps/web); routes come from discover: in .driftgate/driftgate.yaml
every run checks all of them; init --service <name> (repeatable) keeps only the services you name
...
```

It writes:

<!-- check: config-fragment -->
```yaml
targets:
  api:
    base_url: http://localhost:8001   # from docker-compose
  web:
    base_url: http://localhost:3001   # from docker-compose
discover:
  - adapter: laravel
    target: api
    dir: apps/api
    command: ["php", "artisan", "route:list", "--json"]
    php: ["php"]
  - adapter: nextjs
    target: web
    dir: apps/web
templates: [ok, not-found, json, json-errors]
```

Every adapter, exclusion, allowlist entry and `observe` entry carries its
service's name (`target: api`). `dir` is the service's folder: its commands
run there and its files are found there.

## Rules

- Use `target:` for one service or `targets:` for several, not both.
  Service names are simple names (letters, digits, `. _ -`).
- With several targets, every adapter needs `target:`; every catalog entry
  carries `"target"`. `params`, `observe`, allowlist and exclusion entries
  without `target` apply to every service.
- `state:` is shared: every database is restored before the capture, since
  services depend on each other's data.
- A database host must be local or named in some service's `allow_hosts`.

## Running

```sh
driftgate run                  # every service; one report, one exit code
driftgate run --target api     # quick local work: one service
```

By default the whole project is checked; drift is reported with the
service's name. `--target <service>` (on `run`, `discover`, `generate`,
`coverage`, `capture`, `compare`; repeatable or comma-separated) works on
the named services only, and `run` then says that the check is partial and
which services were not checked. In CI, run without `--target`.

`har import` needs `--target` with several services: a HAR file belongs to
one of them.
