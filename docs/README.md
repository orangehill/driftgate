# DriftGate documentation

DriftGate is a regression gate for HTTP APIs: it records how a running
application responds, restores the same data after every change, sends the
same requests and reports every difference. Its primary use is local:
checking each step of a framework upgrade before the pull request; CI is
optional. Start with the [README](../README.md) (install and a 5-minute
quick start). This documentation describes DriftGate 1.0.
What each release adds and changes is in the
[changelog](../CHANGELOG.md).

## Reference

- [Commands](commands.md): every command and flag, exit codes, environment
  variables, interrupts and the store lock; among them
  [`allowlist check`](commands.md#allowlist-check) and
  [`verify`](commands.md#verify) with `--migrate-state` and
  `--migrate-private`
- [What to use for what](configuration.md#what-to-use-for-what): a task
  or symptom (an upload, rows in another order, a command, a gap) and the
  feature for it
- [Configuration](configuration.md): `driftgate.yaml`, `allowlist.yaml`,
  `exclusions.yaml`, `catalog.json`, `classify.yaml`
- [Security model](security.md): what a pull request can run, secrets,
  personal data, allowed hosts, release verification
- [Drift kinds](reference/drift-kinds.md): what each kind of drift means
  and how it is usually fixed
- [Glossary](glossary.md): every term these pages use, with one definition

## Guides

- [Laravel](guides/laravel.md), with Herd, Docker / Sail and SQLite
- [ASP.NET Core and EF Core on SQL Server](guides/aspnet-core.md)
- [Next.js and Nuxt pages](guides/nextjs-nuxt.md)
- [Frameworks with an OpenAPI description](guides/openapi.md): Spring Boot,
  ASP.NET, FastAPI, NestJS, Quarkus, Micronaut, Magento and others
- [Several services and monorepos](guides/multi-service.md)
- [CI with the GitHub Action](guides/github-actions.md): keys, private
  bodies, `--skip-private`, the pristine database cache
- [Anonymized test data for CI](guides/test-data.md): the data module end to
  end
- [Upgrading a framework or language version](guides/framework-upgrade.md),
  and [the server](guides/framework-upgrade.md#upgrading-the-server-database-php-web-server):
  database, PHP, web server, two stacks compared
- [Finding the commit that introduced a drift](guides/bisect.md)
- [Building the catalog by recording through a proxy](guides/record.md)
- [Side checks in the same gate](guides/side-checks.md): a schema dump and a compiler
  output, the settings of the stack, files and Redis, mail, the production install

Every `yaml` and `json` example in these pages carries a
`<!-- check: … -->` marker in its source, and the docs test
(`go test ./internal/docscheck/`) loads each one with DriftGate's own
parsers on every change; the examples in another tool's format (a GitHub
workflow, a compose file, an OpenAPI description) are checked as plain YAML
or JSON.
The same test checks every link and anchor between the pages.
