# Several services and monorepos

> **Before you start:** [install DriftGate](../../start/install.md), and have every service running. **Time:** 20 minutes.

A project can have several services deployed together: a Laravel API, a
Next.js frontend, microservices. One `run` checks them all on the same
restored state, and gives one report and one exit code.

## Set up a monorepo

```console
$ driftgate init --project shop
created .driftgate/driftgate.yaml, .driftgate/catalog.json and .driftgate/allowlist.yaml
found api (laravel, apps/api), web (nextjs, apps/web); routes come from discover: in .driftgate/driftgate.yaml
every run checks all of them; init --service <name> (repeatable) keeps only the services you name
...
```

`init` looks in the root, its subfolders, `apps/*`, `services/*` and
`packages/*`, and reads the ports from `docker-compose.yml`. It writes:

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

`dir` is the service's folder: its commands run there, and its files are
found there.

## Rules for several services

- Every adapter needs `target:`, and every catalog entry carries `"target"`.
- `params`, `observe`, allowlist and exclusion entries without `target` apply to every service.
- `state:` is shared: every database is restored before the capture, as services depend on each other's data.
- Use `target:` for one service or `targets:` for several, not both. Service names are simple names: letters, digits, `. _ -` ([target and targets](../../reference/driftgate-yaml.md#target-and-targets)).

## Check one service or all

```sh
driftgate run                  # every service; one report, one exit code
driftgate run --target api     # quick local work: one service
```

By default the whole project is checked, and drift names its service.
`--target <service>` works on the named services only, and `run` then says
that the check is partial, and which services were not checked. In CI,
run without `--target` ([common flags](../../reference/cli.md#common-flags)).

`har import` needs `--target` with several services: a HAR file belongs
to one of them.

---
**Next:** [Read a drift report](../review/read-the-report.md): what the first comparison shows.
**Related:** [target and targets](../../reference/driftgate-yaml.md#target-and-targets) · [Common flags](../../reference/cli.md#common-flags) · [Upgrade a framework](../workflow/framework-upgrade.md)
