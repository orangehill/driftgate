# DriftGate documentation

DriftGate is a regression gate for HTTP APIs: it records how a running
application responds, restores the same data after every change, sends the
same requests and reports every difference. Its primary use is local:
checking each step of a framework upgrade before the pull request; CI is
optional. Start with the [README](../README.md) (install and a 5-minute
quick start). This documentation describes DriftGate 1.0.

## Reference

- [Commands](commands.md): every command and flag, exit codes, environment
  variables, interrupts and the store lock; among them
  [`allowlist check`](commands.md#allowlist-check) and
  [`verify`](commands.md#verify) with `--migrate-state` and
  `--migrate-private`
- [Configuration](configuration.md): `driftgate.yaml`, `allowlist.yaml`,
  `exclusions.yaml`, `catalog.json`, `classify.yaml`
- [Security model](security.md): what a pull request can run, secrets,
  personal data, allowed hosts, release verification

## Guides

- [Laravel](guides/laravel.md), with Herd, Docker / Sail and SQLite
- [Next.js and Nuxt pages](guides/nextjs-nuxt.md)
- [Frameworks with an OpenAPI description](guides/openapi.md): Spring Boot,
  ASP.NET, FastAPI, NestJS, Quarkus, Micronaut, Magento and others
- [Several services and monorepos](guides/multi-service.md)
- [CI with the GitHub Action](guides/github-actions.md): keys, private
  bodies, `--skip-private`, the pristine database cache
- [Anonymized test data for CI](guides/test-data.md): the data module end to
  end
- [Upgrading a framework or language version](guides/framework-upgrade.md)
- [Finding the commit that introduced a drift](guides/bisect.md)
- [Building the catalog by recording through a proxy](guides/record.md)

The configuration examples marked `<!-- check: … -->` in the source of
these pages were loaded with DriftGate's own parsers when the pages were
written.
