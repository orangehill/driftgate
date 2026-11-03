# DriftGate

[![CI](https://github.com/orangehill/driftgate/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/orangehill/driftgate/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/orangehill/driftgate)](https://github.com/orangehill/driftgate/releases/latest)
[![Coverage](https://coveralls.io/repos/github/orangehill/driftgate/badge.svg?branch=main)](https://coveralls.io/github/orangehill/driftgate?branch=main)
[![Go Reference](https://pkg.go.dev/badge/github.com/orangehill/driftgate.svg)](https://pkg.go.dev/github.com/orangehill/driftgate)
[![License](https://img.shields.io/github/license/orangehill/driftgate)](LICENSE)

A regression gate for HTTP APIs: prove that every route still returns the
same response after a framework upgrade, a refactoring or a new feature.

![DriftGate records a baseline, then reports drift after the API changes](docs/demo.gif)

DriftGate records how a running application responds (the baseline). After
every change (a framework or language upgrade, a refactoring, a new feature)
it restores the same data, sends the same requests and compares the responses
with the baseline: status, headers and body, literally. Every difference (a
drift) is reported with the request, the path in the response, its kind, and
what it was and is now. It also records what each request changed in the
database and reports a change of that behaviour as drift.

It is one static binary, works from the outside over HTTP against any
backend, and needs nothing added to the application. Its primary use is
local: checking each step of a framework upgrade on your machine before the
pull request. Running it in CI is optional.

User documentation: [`docs/`](docs/README.md).

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

- CI tests the database code on MariaDB 10.3, 10.6 and 11.4, MySQL 8.0 and 8.4
  and PostgreSQL 13, 16 and 17, and runs the tests on Windows too.
- Releases are signed with cosign
  ([release verification](docs/security.md#release-verification)).

## Install

**Release binaries** (linux, darwin and windows on amd64 and arm64). Verify
the signature of `SHA256SUMS` first, then the binary against it:

```sh
gh release download v1.0.2 --repo orangehill/driftgate \
  --pattern driftgate-linux-amd64 --pattern 'SHA256SUMS*'
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/orangehill/driftgate/.github/workflows/release.yml@refs/tags/v1.0.2 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS   # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
install -m 0755 driftgate-linux-amd64 /usr/local/bin/driftgate
```

SBOMs, provenance and reproducible builds:
[security, release verification](docs/security.md#release-verification).

**From source** (the Go version in `go.mod`):

```sh
go install github.com/orangehill/driftgate/cmd/driftgate@latest
# or, in a checkout:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o driftgate ./cmd/driftgate
```

A source build reports its version as `dev`.

**Docker image**, built from the [`Dockerfile`](Dockerfile) (no image is
published yet):

```sh
docker build -t driftgate .
docker run --rm -v "$PWD:/work" --network host driftgate run
```

The image holds only the binary (non-root, no shell). Commands that
`driftgate.yaml` runs (seed, restore, `php artisan` …) must exist where
DriftGate runs; for those, copy the binary into the application's image:
`COPY --from=driftgate /usr/local/bin/driftgate /usr/local/bin/`.

## Quick start

**A new project: `driftgate setup`.** In the project's folder, with the
application running, it recognises the framework, reads the database and
the address from the project's own settings (`.env`, `appsettings.json`,
compose…), writes `.driftgate/`, makes the key and, after asking, the
pristine copy of the database, then the first baseline, and says what to
check by hand ([setup](docs/commands.md#setup)). After every upgrade
step, `driftgate run` compares with that baseline.

**A worktree per change.** `.driftgate/` is not in git, so a worktree
added with `git worktree add` starts without it: `driftgate setup` there
takes the main worktree's project with its baseline and key, switches
`working:` to the database and `base_url` to the address the worktree's
`.env` names (after asking; `--yes` says yes), and makes no
baseline of its own; `driftgate run` then compares the worktree's code
with the main worktree's baseline
([baselines](docs/configuration.md#baselines)).

`driftgate init` writes only the configuration (with the database found
commented out), for the steps one by one.
The steps below use the demo API in this repository, so every output
shown is real.

```sh
go build -o /tmp/demoserver ./tools/demoserver     # in a checkout of this repository
/tmp/demoserver -addr 127.0.0.1:8920 -version v1 &  # the application
mkdir shop && cd shop
driftgate init --project shop
driftgate key create   # the key, in .driftgate/key
```

```
$ driftgate init --project shop
created .driftgate/driftgate.yaml, .driftgate/catalog.json and .driftgate/allowlist.yaml
no known project type found; add routes under discover: or write the catalog by hand
...
Everything DriftGate keeps is in .driftgate/, which git ignores (.gitignore).
```

Everything is in the project's `.driftgate/` folder, which `init` adds to
`.gitignore` (with `--baselines git`, `driftgate.yaml` and `driftgate/`
go into the repository instead, and the key into the macOS Keychain; see
[baselines](docs/configuration.md#baselines)). The demo is not a
framework `init` knows, so tell DriftGate its address, its routes and how
to reset its data. In `.driftgate/driftgate.yaml` set `base_url:
http://127.0.0.1:8920` and `mode: evolve` (so that changes can be
accepted), and add:

```yaml
state:
  api:                       # the demo keeps its data in memory; its reset is the restore
    restore: ["curl", "-sf", "-X", "POST", "http://127.0.0.1:8920/admin/reset"]
discover:
  - adapter: command         # any command that prints the routes as JSON
    command: ["cat", "routes-src.json"]
params:
  - name: id
    value: "12"
```

and `routes-src.json` in the project folder (`shop/`, where you run the
commands; the `command` adapter runs there):

```json
[
 {"method":"GET","path":"/api/news"},
 {"method":"GET","path":"/api/news/{id}"},
 {"method":"POST","path":"/api/faq","body":{"q":"?"}}
]
```

**The first run makes the baseline.** `run` finds the routes, generates the
catalog, captures twice on the same restored state, and makes the baseline
only when both captures agree:

```
$ driftgate run
== discover ==
3 routes (command 3)
  route-added   GET /api/news
  route-added   GET /api/news/{id}
  route-added   POST /api/faq
wrote …/shop/.driftgate/routes.json
...
== capture ==

captured 3 requests into run-5729dc0359a3
...
2 runs on the same state gave the same responses ✓

== baseline ==
baseline local made from two equal captures: …/shop/.driftgate/baselines/local
```

The capture is named after the checkout (`run-<id>`), so two worktrees of
one project never share it. With `--baselines git` the last line also
says to commit `driftgate/`.

**Change something.** Restart the demo as `v2`, which plants changes, and
run again (exit code 1):

```
$ kill %1; /tmp/demoserver -addr 127.0.0.1:8920 -version v2 &
$ driftgate run
...
DRIFT  get-api-news-id
         header-extra    header:x-powered-by   → ["demo"]
         key-extra       $.entry.meta   → (private)
         type            $.entry.id  string → int
         value           $.entry.published_at  (private) → (private)

Groups:
  [1] 3 drifts in 3 requests: header x-powered-by is new
      GET /api/news  (+2 more routes)
...
capture:run-5729dc0359a3 against baseline:local: 3 compared, 0 passed, 3 drifted, 0 skipped, 0 observed
```

Values show as `(private)` because generated requests are stored as private
by default; `driftgate compare run --show-private` shows them on your own
machine.

**Review and accept.** `review` lists the drift groups with the exact
`accept` command. Accepting updates the baseline and appends the reason to
`.driftgate/baselines/local/acceptances.jsonl`:

```
$ driftgate review
...
$ driftgate accept 'get-api-*' post-api-faq --reason "v2 returns numeric ids and a meta object" --ref DEMO-1
accepted 3 requests into baseline local: get-api-news, get-api-news-id, post-api-faq
  header-extra header:x-powered-by
  ...
logged in .driftgate/baselines/local/acceptances.jsonl
```

The next run still reports one drift: v2 stamps a new FAQ entry with the
current time (`$.entry.created_at`). A value that changes by itself goes on
the allowlist, with its format and a reason, in `.driftgate/allowlist.yaml`:

```yaml
body:
  - request: post-api-faq
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Creation time of the new FAQ entry; v2 sets it to the current time.
```

```
$ driftgate run
...
capture:run-5729dc0359a3 against baseline:local: 3 compared, 3 passed, 0 drifted, 0 skipped, 0 observed
...
No drift ✓
```

With `baselines: local` nothing of this goes into git: the baseline stays
on this machine, for the checks before each pull request. To share it
with a team or a CI job, `driftgate baselines git` moves it into the
repository; then commit `driftgate.yaml` and `driftgate/`
([baselines](docs/commands.md#baselines)). Exit codes: `0` clean,
`1` drift, `2` configuration error, `3` environment error or an incomplete
result.

## GitHub Action

```yaml
on: pull_request            # never pull_request_target
permissions:
  contents: read
  pull-requests: write      # the report as a pull-request comment
jobs:
  driftgate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      # … start the application and its database …
      - uses: orangehill/driftgate@v1.0.2   # a release tag or a commit SHA
        with:
          version: v1.0.2   # download and verify the signed release binary; default: build from source
          args: --skip-private
```

The action runs `driftgate run` against the committed baseline (it never
makes one in CI), keeps the exit code, and leaves the report as one updated
pull-request comment, in the job summary and as a JUnit file. Full guide:
[CI with GitHub Actions](docs/guides/github-actions.md).

## Documentation

- [Commands](docs/commands.md): every command and flag, exit codes
  (also `allowlist check`, which finds rules that cannot apply, and
  `verify --migrate-state` / `--migrate-private` for baselines of an earlier
  release)
- [Configuration](docs/configuration.md): `driftgate.yaml`,
  `allowlist.yaml`, `exclusions.yaml`, `classify.yaml`, `catalog.json`
- [Security model](docs/security.md)
- Guides: [Laravel](docs/guides/laravel.md) ·
  [Next.js and Nuxt pages](docs/guides/nextjs-nuxt.md) ·
  [OpenAPI frameworks](docs/guides/openapi.md) ·
  [Several services](docs/guides/multi-service.md) ·
  [CI with GitHub Actions](docs/guides/github-actions.md) ·
  [Anonymized test data](docs/guides/test-data.md) ·
  [Upgrading a framework](docs/guides/framework-upgrade.md) ·
  [Bisect](docs/guides/bisect.md) ·
  [Recording through a proxy](docs/guides/record.md)

## Author

DriftGate is written and maintained by Slavenko Božić ([@slavenkobozic-ohd](https://github.com/slavenkobozic-ohd)) at [Orange Hill](https://github.com/orangehill).

## License

Apache 2.0, see [`LICENSE`](LICENSE).
