# How it works

DriftGate records how a running application answers, and checks after
every change that it still answers the same way. It works over HTTP and
needs nothing added to the application.

## The cycle

```
discover → generate → seed → restore → capture → compare → review
 routes     catalog   pristine  same     send the  each     accept,
                      copy      data     catalog   drift    expect or fix
```

`driftgate run` does everything up to `compare`. `review`, `accept` and
`expect` are yours ([review](review.md)).

## Baseline and capture

A capture sends every request of the [catalog](catalog.md) and stores the
answers: status, headers and body. It also stores what each request
changed in the database.

The baseline is the capture every later one is compared with. The first
`run` captures twice on the same data, and makes the baseline only when
both agree. A value that changed by itself, such as a time, gets an
[allowlist rule](review.md#allowlist-rules) with a format and a reason.

## Compare and levels

A comparison is literal: `42` instead of `"42"` is a drift. Every drift
has a [kind](review.md#drift-kinds), a place, and both values. The level
says how far a comparison goes; each includes the ones before it.

| Level | Compares |
|---|---|
| `status` | the status code |
| `shape` | also header names, keys, key order, nesting and array lengths |
| `types` | also the type of every value |
| `values` | also exact values, header values and body bytes (the default) |

Keep `values` locally, where the data is the same every time. Use `types`
against data DriftGate does not restore, such as a live server. A pass at
`types` proves the contract, not the behaviour.

## State

Every capture must start from the same data, or a drift could come from
the data instead of the code.

| Step | What happens |
|---|---|
| seed | `driftgate seed`, or the first `run`, copies the working database to a pristine copy |
| restore | before each capture, the working database is restored from the pristine copy |
| fingerprint | a hash of the restored data; when it differs from the baseline's, the capture stops (exit 3) |
| side effects | what each request changed; a request that writes elsewhere is a drift |

`state:` names what is restored: databases with a built-in driver, or
anything else through `restore` and `fingerprint` commands
([configuration](configuration.md#state), [databases](databases.md)).

## Modes: evolve and freeze

| Mode | A drift |
|---|---|
| `freeze` (default) | fails, unless a ticket [expects](review.md#accept-expect-or-rebase) exactly that drift; `accept` is refused |
| `evolve` | may be accepted into the baseline, with a reason |

Every acceptance is a line in the baseline's acceptance log.

## What a pass proves

A pass (`No drift ✓`, exit 0) means that every request of the catalog
gave the same answer as in the baseline, at its level. It also made the
same changes to the data, on the same starting data.

## What a pass does not prove

A pass proves nothing about requests that are not in the catalog. Check
the gaps the report counts after the verdict, and `driftgate coverage`.

- Routes without a request meant to succeed, and requests whose baseline answer is already an error.
- Requests only observed, and requests that change data that were skipped.
- File uploads, unless the catalog has [`multipart`](catalog.md#multipart) requests.
- Background jobs, queue workers and scheduled commands, unless a [command step](catalog.md#command-steps) runs them.
- Requests a frontend sends by itself, unless you [record](catalog.md#record) them.
- Behaviour with a warm cache, when a restore empties the cache.
- Excluded tables: they are never compared.
- A request that passed only after a retry is reported as such, never as a plain pass.

## Incomplete results

A result that cannot be trusted is incomplete (exit 3), never green:

- the restored data differs from the baseline's;
- the comparison compared nothing: an `--only` typo, renamed request IDs;
- private bodies are not on this machine, without `--skip-private`;
- the service or database cannot be reached, or the capture was interrupted.

Exit 3 is not a regression: fix the cause and run again.

## Key terms

- **Baseline**: the stored answers every capture is compared with; named `local` by default.
- **Capture**: one run of the catalog against the application, stored under a tag.
- **Catalog**: the requests every capture sends (`catalog.json`).
- **Drift**: a difference between a capture and the baseline.
- **Allowlist rule**: lets one value change, within a format, with a reason.
- **Pristine copy**: the copy of the database every capture starts from.
- **Fingerprint**: a hash that proves the restored data is the same.
- **Side effect**: what a request changed in the data.
- **Private body**: a response body kept encrypted in the local store, never in git.
- **Project key**: the key that encrypts private bodies.
- **Local store**: captures and private bodies on this machine, outside git.
- **Accept**: take an intended drift into the baseline (mode `evolve`).
- **Expectation**: a drift a ticket intends, recorded without changing the baseline.
- **Service**: a running application DriftGate sends requests to; `target` in the configuration.
