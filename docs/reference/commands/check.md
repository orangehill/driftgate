# Commands: capture and compare

The commands that send the catalog and compare the answers. How a
capture runs is in [How a capture runs](../capture.md), and what a
report shows is in [Reading a report in detail](../reports.md).
Paths are those of the local layout ([where the project's files are](../cli.md#where-the-projects-files-are)).

## driftgate run

```
driftgate run [--baseline <name>] [--allow-mutating-without-restore]
    [--skip-private] [--read-only] [--target <service>]
```

Routes, catalog, capture and compare in one command. The first run makes
the baseline from two equal captures.

| Flag | Meaning |
|---|---|
| `--baseline <name>` | the baseline to make or compare with (default `local`) |
| `--allow-mutating-without-restore` | send requests that change data even without a state restore (not uploads of real files) |
| `--skip-private` | accept that private bodies not on this machine are not compared |
| `--read-only` | capture only the requests that do not change data, as `capture --read-only` |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- `--skip-private`: the status, headers and side effects of those requests are still compared. Without the flag, the result is incomplete (exit 3).
- `--read-only` works only with a baseline: the first run makes it from every request.
- `--target`: the check is then reported as partial.
- `run` needs `.driftgate/catalog.json` to exist; `init` writes an empty one. An empty catalog is refused.
- A catalog with requests that change data needs a state with a restore (`state:`), or `--allow-mutating-without-restore`.
- When no request of the first capture gets an answer, `run` stops there (exit 3). It says how to start the application: its compose file, or the framework's own command, such as `php artisan serve`.

```sh
driftgate run
driftgate run --target api            # one service only; reported as partial
driftgate run --skip-private          # CI: private bodies are not on the runner
```

### The steps run takes

1. `discover` finds the routes.
2. `generate` builds the catalog.
3. `coverage` checks the routes against the catalog and the baseline.
4. `seed` runs when a pristine copy is needed.
5. `capture` sends the catalog.
6. `compare` compares the capture with the baseline.

The first run writes the route inventory and the catalog it finds. Later
runs do not. A changed inventory or catalog is reported as drift; adopt
it with `discover --write` or `generate --write`. `run` still captures
with the existing catalog and compares, so one run gives the whole
picture.

Every run checks the baseline in its coverage step. A baseline that
cannot be read for that check is a warning, and the catalog alone
decides coverage.

### How the first run makes the baseline

- Without the baseline, `run` captures twice on the same state, and makes the baseline only when both captures agree. Otherwise it proposes allowlist entries with a format; you add the reason.
- The second capture runs its `prepare:` commands and requests at least a second after the first one ended. It waits for what its restore did not take. A timestamp the application writes, such as `updated_at`, then differs between the two captures and is found, also when both are shorter than a second.
- A baseline response that came only after a [retry](../capture.md#retries-of-requests-that-failed-under-load) is named in a warning, and keeps the mark in its record.
- The new baseline is checked as [coverage](catalog.md#driftgate-coverage) checks one. Requests that failed in it are listed, and routes they leave uncovered make the run exit 1. The baseline is kept.

### Capture names in several worktrees

The capture of `run` is named `run-<id>`, where `<id>` is fixed for the
folder of the project. With `baselines: git`, the checkouts of a project
(git worktrees) share the local store, but never each other's capture:
each keeps its own last comparison. With `baselines: local`, each
worktree has a store of its own. `compare run` compares this checkout's
`run` capture, when no baseline or capture is named `run`.

## driftgate capture

```
driftgate capture <tag> [--only <glob>] [--force] [--restore once|each]
    [--repeat N] [--baseline <name>] [--allow-remote]
    [--allow-remote-mutations] [--allow-mutating-without-restore]
    [--skip-fingerprint-check] [--calibrate] [--read-only] [--quiet]
    [--target <service>]
```

Restores the state, sends the catalog and stores the responses under a
tag.

| Flag | Meaning |
|---|---|
| `--only <glob>` | only requests whose ID matches |
| `--force` | replace an existing capture with this tag |
| `--restore once\|each` | `once` (default) or `each`; see [Restore once or before each sequence](../capture.md#restore-once-or-before-each-sequence) |
| `--repeat N` | capture N times on the same state and propose allowlist entries for values that differ (default 1) |
| `--baseline <name>` | the state must match the one this baseline was captured from (default `local`) |
| `--allow-remote` | allow a service outside `allow_hosts` |
| `--allow-remote-mutations` | with `--allow-remote`, also send requests that change data or are repeated to it |
| `--allow-mutating-without-restore` | send requests that change data even without a state restore (not uploads of real files) |
| `--skip-fingerprint-check` | do not check the state fingerprint (diagnostics only; the capture is marked unreliable) |
| `--calibrate` | decide `concurrency: auto` for the baseline ([how](../capture.md#parallel-requests-and-concurrency-auto)) |
| `--read-only` | send only the requests that do not change data; see [below](#capture-only-the-requests-that-do-not-change-data) |
| `--quiet` | no progress output |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- `--repeat N`: each run writes at least a second after the one before ended, as `run` does. The capture keeps the median response time of each request.
- `--calibrate` measures, compares a capture at the chosen setting with a sequential one, and records the setting in the baseline. It implies `--repeat 2`, and does not go with `--only`.

How the requests are ordered, restored, retried and checked is in
[How a capture runs](../capture.md).

```console
$ driftgate capture before --repeat 2
— run 1 of 2 —
restoring api …
200  get-api-news  6ms
...
2 runs on the same state gave the same responses ✓
```

### Capture only the requests that do not change data

`--read-only` sends only the requests that do not change data: `GET`,
`HEAD` and `OPTIONS`, and those marked `mutates: false`, such as a
search sent as `POST`.

- It leaves out the requests that change data, and command steps unless they are marked `mutates: false`.
- It leaves out every request of a sequence that holds one of them, as its later requests may need the earlier ones.
- The capture says how many it left out. It records `read_only`, and the IDs it left out as `read_only_skipped`, in its `meta.json`.
- A comparison of it skips those, whatever the catalog says by then (`not sent: capture --read-only`). It lists those that change data under what the result does not cover.
- A request missing from it for any other reason still drifts.

A read-only capture is never a baseline for the requests it left out.

- The first `run` makes the baseline from every request, and refuses `--read-only`.
- `baselines rebase --from` refuses a read-only capture.
- Compared against a read-only capture (`--against capture:<tag>`), the requests it left out were never checked. They are listed as such, and the result is incomplete (exit 3); they are not new requests to accept.

`--read-only` only selects requests. The project's own commands
(`prepare`, `tokens.command`, `seed`, guards, restore commands) run as
configured, may write, and do not go through `connect_to`. So
`--read-only` is refused when `prepare:` is configured, and a
`tokens.command` gets a warning. Leave those out of the configuration
for a server that must not change.

With a configuration without `state:`, nothing is restored, seeded or
fingerprinted. The task, with `connect_to`, logins and the frontend's own
requests, is in [Check a live server without writing to it](../../guides/workflow/live-server.md).

### Capture tags

- Captures live in the local store (`.driftgate/store/captures/<tag>`; with `baselines: git`, `~/.driftgate/<project>/captures/<tag>`), never in the repository.
- A tag is a simple name: letters, digits, `. _ -`. It may not end with `.`, nor be a device name Windows reserves, such as `con` or `nul`.

## driftgate compare

```
driftgate compare <tag> [--against <baseline|tag>]
    [--level status|shape|types|values] [--only <glob>]
    [--format text|json|junit|markdown] [--max-drifts N]
    [--skip-fingerprint-check] [--show-private] [--skip-private]
    [--target <service>]
```

Compares a capture with a baseline or another capture (exit 1 on drift).

| Flag | Meaning |
|---|---|
| `--against <baseline\|tag>` | the baseline name or capture tag to compare with (default `local`) |
| `--level status\|shape\|types\|values` | the level for this comparison; only stricter than the configured one |
| `--only <glob>` | only requests whose ID matches |
| `--format text\|json\|junit\|markdown` | output format (default text) |
| `--max-drifts N` | list up to N drifts per request (default 20) |
| `--skip-fingerprint-check` | compare captures made with the same flag |
| `--show-private` | show values from private bodies, on your own machine only (never kept) |
| `--skip-private` | accept that private bodies not on this machine are not compared |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- `--max-drifts N` lists every drift up to N. Of a request whose status changed, the default shows the change, its location and content type, and one line for the rest ([status changes](../reports.md#a-request-whose-status-changed)).
- `--skip-private` is as for [`run`](#driftgate-run).

`<tag>` `run` is this checkout's capture of `driftgate run`, when no
baseline or capture is named `run`. What the output shows is in
[Reading a report in detail](../reports.md).

```console
$ driftgate compare before --only 'get-*'

capture:before against baseline:local: 2 compared, 2 passed, 0 drifted, 0 skipped, 0 observed
No drift ✓
compared (--only): 2 requests: 2 reads, 0 mutating, 0 command steps; 0 of them multipart
```

### The last comparison

The comparison is kept as the last comparison, which `report`, `review`,
`accept`, `expect` and `bisect` read.

### Compare two captures

`--against capture:<tag>` compares with another capture instead of a
baseline. Such a comparison takes no expectations, and `accept` and
`expect` refuse it ([review](review.md)).

## driftgate report

```
driftgate report [--format text|json|junit|markdown] [--max-drifts N] [--exit-code]
```

Writes the last comparison again, for example as a pull-request comment.

| Flag | Meaning |
|---|---|
| `--format text\|json\|junit\|markdown` | output format (default text) |
| `--max-drifts N` | as for [`compare`](#driftgate-compare) |
| `--exit-code` | exit with the verdict of the comparison, as `compare` did ([below](#use-report-as-a-gate)) |

- It writes the saved report, which never holds values from private bodies ([report formats](../reports.md#report-formats)).
- It exits `2` when there is no comparison yet or a flag is wrong, and `3` when the local store cannot be read.

```sh
driftgate report --format markdown > report.md
driftgate report --format junit > junit.xml
driftgate report --exit-code > /dev/null   # 1 when the last comparison drifted
```

### Use report as a gate

By default `report` exits `0` also when the comparison drifted. It only
writes a result that `compare` or `run` already gave, and those two are
the gate. A CI step usually runs `run` and then `report` once per format;
the GitHub Action writes markdown and JUnit under `bash -e`. A `report`
that failed on drift would stop that step before the second file is
written.

`--exit-code` makes `report` exit with the verdict of the comparison it
writes, exactly as `compare` did:

- `1` on drift;
- `3` when the result is incomplete: private requests whose body is not on this machine (without `--skip-private`), or nothing compared. This wins over drift, as in `compare`;
- `0` when clean.

It is the comparison's verdict only. When `run` failed for changed
routes, a stale catalog or a capture that stopped, the saved comparison
does not hold that, so gate on `run` itself.

---
**Related:** [How a capture runs](../capture.md) · [Reading a report in detail](../reports.md) · [Command line basics](../cli.md)
