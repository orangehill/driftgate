# DriftGate

[![CI](https://github.com/orangehill/driftgate/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/orangehill/driftgate/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/orangehill/driftgate)](https://github.com/orangehill/driftgate/releases/latest)
[![Coverage](https://coveralls.io/repos/github/orangehill/driftgate/badge.svg?branch=main)](https://coveralls.io/github/orangehill/driftgate?branch=main)
[![Go Reference](https://pkg.go.dev/badge/github.com/orangehill/driftgate.svg)](https://pkg.go.dev/github.com/orangehill/driftgate)
[![License](https://img.shields.io/github/license/orangehill/driftgate)](LICENSE)

DriftGate is a regression gate for step-by-step framework upgrades. It
captures your API's responses and database side effects on the old version,
then compares them after every upgrade step.

![DriftGate records a baseline, then reports drift after the API changes](docs/demo.gif)

## Features

- Compares status, headers and body literally: `42` instead of `"42"` is a drift.
- Records what each request changed in the database, and reports a change of that.
- Restores the same data before every capture, and proves it with a fingerprint.
- Finds the routes itself: Laravel, Symfony, Rails, Django, Spring Boot, Next.js, Nuxt, OpenAPI.
- Generates the request catalog, and fails on a route without a request.
- MySQL, MariaDB, PostgreSQL, SQLite and SQL Server.
- Keeps response bodies with personal data encrypted on your machine, out of git.
- One static binary for Linux, macOS and Windows; nothing to add to the application.
- A GitHub Action for the same check in CI.

## Install

```sh
go install github.com/orangehill/driftgate/cmd/driftgate@latest
```

Signed release binaries and how to verify them are in
[getting started](docs/getting-started.md#install).

## First run

Start your application on its old version, then:

```sh
driftgate setup          # config, key, pristine copy of the database, first baseline
# upgrade one step
driftgate run            # compare with the baseline; exit 1 on drift
driftgate review         # the drifts, grouped, with the command to accept each
```

## Documentation

| Page | What is in it |
|---|---|
| [Getting started](docs/getting-started.md) | install, a 5-minute demo, your own project |
| [How it works](docs/how-it-works.md) | baseline, state, modes, what a pass proves |
| [Configuration](docs/configuration.md) | every key of `driftgate.yaml` |
| [Catalog](docs/catalog.md) | the requests: generate, record, write by hand, coverage |
| [Review](docs/review.md) | reading the report, drift kinds, allowlist, accept and expect |
| [Databases](docs/databases.md) | MySQL, MariaDB, PostgreSQL, SQLite, SQL Server |
| [Frameworks](docs/frameworks.md) | Laravel, ASP.NET Core, Next.js, Nuxt, OpenAPI |
| [Workflows](docs/workflows.md) | upgrades, worktrees, live servers, CI, test data |
| [Commands](docs/commands.md) | every command and flag, exit codes |
| [Security](docs/security.md) | the key, private bodies, CI, release verification |
| [Troubleshooting](docs/troubleshooting.md) | messages, their causes and fixes |

Changes per release are in the [changelog](CHANGELOG.md).

## Contributing

Bug reports and fixes are welcome; new features only by exception. See
[CONTRIBUTING.md](CONTRIBUTING.md). Report security problems privately, as
[SECURITY.md](SECURITY.md) says.

## Author

DriftGate is written and maintained by Slavenko Božić ([@slavenkobozic-ohd](https://github.com/slavenkobozic-ohd)) at [Orange Hill](https://github.com/orangehill).

## License

Apache 2.0, see [`LICENSE`](LICENSE).
