# DriftGate

[![CI](https://github.com/orangehill/driftgate/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/orangehill/driftgate/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/orangehill/driftgate)](https://github.com/orangehill/driftgate/releases/latest)
[![Coverage](https://coveralls.io/repos/github/orangehill/driftgate/badge.svg?branch=main)](https://coveralls.io/github/orangehill/driftgate?branch=main)
[![Go Reference](https://pkg.go.dev/badge/github.com/orangehill/driftgate.svg)](https://pkg.go.dev/github.com/orangehill/driftgate)
[![License](https://img.shields.io/github/license/orangehill/driftgate)](LICENSE)

A regression gate for HTTP APIs: prove that every route still returns the
same response after a framework upgrade, a refactoring or a new feature.

![DriftGate records a baseline, then reports drift after the API changes](docs/demo.gif)

## What DriftGate does

DriftGate records how a running application responds: the baseline.
After every change, it restores the same data, sends the same requests,
and compares the responses with the baseline: status, headers and body,
literally. Every difference, a drift, is reported with the request, the
place in the response, its kind, and what it was and is now. It also
records what each request changed in the database, and reports a change
of that as drift.

It is one static binary that works from the outside, over HTTP, against
any backend. Its main use is local: checking each step of a framework
upgrade on your machine before the pull request
([how it works](docs/concepts/how-it-works.md)).

## Why DriftGate

- **It compares literally, on the real application and its real database.**
  `42` instead of `"42"` after a PHP upgrade is reported as
  `$.entry.id: type string → int`; a tool that compares by meaning, fakes the
  database or checks only an OpenAPI schema does not see it.
- **It sees changes to the data, not only to the response.** A request that
  starts writing to another table, or a GET that starts changing a row, is
  drift even when the response is the same.
- **It asks nothing of the application or of production.** No SDK, agent,
  proxy or production traffic. It finds the routes itself (Laravel, Symfony,
  Rails, Django, Spring Boot, Next.js, Nuxt and any framework that serves an
  OpenAPI description), proposes the request catalog, and fails CI on a new
  route without a check.
- **Personal data stays out of git.** Responses with personal data stay
  encrypted on the machine that recorded them; the repository names them
  only by a keyed hash, and reports never show their values.

## Results

- Verified on two real upgrades of a Laravel API from 5.5 to 13 (622 and
  494 requests), at every step to the final state.
- Measured on a synthetic set of 622 requests with 66 MB of
  bodies (Linux, i7-11800H, medians):

  | Step | Time | Memory |
  |---|---|---|
  | capture, concurrency 1 | 627 ms | 39 MB |
  | capture, concurrency 4 | 446 ms | 71 MB |
  | compare | 26.5 ms | 22 MB |
  | seed, MySQL 8.4 | 6.6 s | |
  | seed, PostgreSQL 16 | 2.6 s | |
  | command start | 1.74 ms | |

- SQLite, a 50 MB file (Apple M4): a fingerprint 28 µs when nothing
  changed, 150 ms after a change to its large table; seed 124 ms. A
  Laravel 12 application on SQLite: about 0.1–0.2 s per capture of 18
  requests.
- SQL Server 2022 in Docker, emulated on an Apple M4: a fingerprint
  0.76 ms when nothing changed, 33 ms after a change to one table of 100,
  0.51 s for a full read of 57.8 MB; a restore of a small table 16–18 ms.
- CI tests the database code on MariaDB, MySQL, PostgreSQL, SQL Server and SQLite, on Linux, macOS and Windows ([supported versions](docs/reference/state.md#supported-database-versions)).
- Releases are signed with cosign
  ([release verification](docs/security.md#release-verification)).

## Install

Download a signed release binary for Linux, macOS or Windows, verify it,
and put it on your `PATH`. Or build it from source with
`go install github.com/orangehill/driftgate/cmd/driftgate@latest`.
Every way, with the verification commands: [Install DriftGate](docs/start/install.md).

## Get started

- [Tutorial: your first baseline](docs/start/tutorial.md): a demo API, its baseline, a change, and its drift, in 15 minutes.
- [Set up your own project](docs/start/your-project.md): `driftgate setup` on your application.
- [Upgrade a framework step by step](docs/guides/workflow/framework-upgrade.md): the check after every upgrade step.

In CI, the GitHub Action runs the same check on every pull request
([CI with GitHub Actions](docs/guides/ci/github-actions.md)).

## Documentation

Everything else, by task and by topic, is in the
[documentation](docs/README.md). Two pages to start from:
[what DriftGate can do](docs/capabilities.md), and
[troubleshooting](docs/troubleshooting.md) when something goes wrong.

## Contributing

Bug reports and fixes are welcome; new features only by exception. See [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems privately, as [SECURITY.md](SECURITY.md) says.

## Author

DriftGate is written and maintained by Slavenko Božić ([@slavenkobozic-ohd](https://github.com/slavenkobozic-ohd)) at [Orange Hill](https://github.com/orangehill).

## License

Apache 2.0, see [`LICENSE`](LICENSE).
