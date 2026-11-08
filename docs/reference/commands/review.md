# Commands: review and record results

The commands that read the last comparison and change what the baseline
holds, plus the checks of the baseline and the allowlist. What to do
with a drift is under [Drift kinds](../drift-kinds.md#what-to-do-with-a-drift).
Paths are those of the local layout ([where the project's files are](../cli.md#where-the-projects-files-are)).

## driftgate review

```
driftgate review [--group N] [--show-private]
```

Lists the drift groups of the last comparison, with the accept command
for each.

| Flag | Meaning |
|---|---|
| `--group N` | show this group with every request it touches |
| `--show-private` | show values from private bodies, compared again from the two sets, on your own machine only (never kept) |

- It asks nothing, so it works in CI.
- A request is accepted whole. When its drifts fall into several groups, `review` names them together in one `accept` command.
- In mode `freeze` it gives the `expect` command instead.
- Expected drifts are listed by ref, apart from the groups to review.
- It also lists the requests sent again after a load failure, as `compare` shows them: passed after a retry, or failed in all their sends. They are not drifts and have nothing to accept.

```console
$ driftgate review
last comparison: capture:run-5729dc0359a3 against baseline:local, 2026-10-29T22:56:13Z (mode evolve, level values)

[1] header-extra header:x-powered-by  (3 requests, 3 drifts)
    example:  post-api-faq  header-extra    header:x-powered-by   → ["demo"]
    requests: post-api-faq, get-api-news, get-api-news-id
    accept:   driftgate accept group:1 group:2 … --reason "<why this change is intended>" --comparison b1b7b001292b
...
```

## driftgate accept

```
driftgate accept <request|glob|group:N>... --reason "…" [--ref <ticket>]
    [--comparison <id>]
```

Takes drifts of the last comparison into its baseline (mode `evolve`),
with a reason in the acceptance log.

| Flag | Meaning |
|---|---|
| `--reason "…"` | why the change is right (required) |
| `--ref <ticket>` | a ticket or link kept with the acceptance |
| `--comparison <id>` | the comparison `review` showed; `accept` refuses a newer one |

- A request that was not captured this time is taken only by its ID; accepting it removes it from the baseline.
- An accepted answer that is a failure (see [coverage](catalog.md#when-a-request-counts-as-failed)) is listed after the acceptance, with the routes of the accepted requests it leaves uncovered. The exit is 0: the reason may explain it, and the acceptance is written. `coverage` and `run` then fail on such a route until it is fixed or acknowledged as a known failure.
- [Expectations](#driftgate-expect) of the accepted requests whose drifts the baseline now holds are closed, with a line in the ref's file.

```console
$ driftgate accept 'get-api-*' post-api-faq --reason "v2 returns numeric ids and a meta object" --ref DEMO-1
accepted 3 requests into baseline local: get-api-news, get-api-news-id, post-api-faq
  header-extra header:x-powered-by
  ...
logged in .driftgate/baselines/local/acceptances.jsonl
```

### What accept refuses

`accept` stops with exit 2 for:

- `mode: freeze`;
- a missing `--reason`;
- a selection that takes only some of a request's drifts;
- a comparison that is stale: the baseline or the capture changed or is gone, or `compare` ran again after `review`;
- a comparison that is not of a capture against a baseline;
- a capture made with `--skip-fingerprint-check`, which is for diagnosis only.

### The acceptance log

- Each acceptance is a line in `.driftgate/baselines/<name>/acceptances.jsonl` (`driftgate/baselines/…` with `baselines: git`).
- A line holds who, when, why, the requests, and the drift signatures. It is chained by the hash of the line before.
- `manifest.json` is written again.
- With `baselines: git`, commit both; the message then says so.

## driftgate expect

```
driftgate expect <request|glob|group:N>... --ref <ticket> --reason "…"
    [--comparison <id>]
driftgate expect --drop <ref> --reason "…" [--baseline <name>]
driftgate expect --import <file> [--baseline <name>]
```

Records drifts of the last comparison that a ticket intends, without
changing the baseline; also in mode `freeze`. `compare` then passes a
request whose every drift is exactly a recorded one, and fails on any
other drift.

| Flag | Meaning |
|---|---|
| `--ref <ticket>` | a ticket or link of the change that intends the drifts (required) |
| `--reason "…"` | why the change makes these drifts, or why they are dropped (required) |
| `--comparison <id>` | the comparison `review` showed; `expect` refuses a newer one |
| `--drop <ref>` | remove the expectations of this ref instead (no requests) |
| `--import <file>` | take a ref's expectations file from another checkout (a worktree) instead, after checking it |
| `--baseline <name>` | with `--drop` or `--import`: the baseline (default: the last comparison's, else `local`) |

- Requests named by their ID or a glob are expected with all their drifts. `group:N` takes one group's drifts, so two tickets that change one request differently each expect their own.
- A request that was not captured is taken only by its ID.
- A drift another ref expects already is left out, and the output says so. Expecting a request with the same ref again adds the drifts selected now; of its earlier drifts, only those that no longer occur are dropped.
- The last comparison is marked again after `expect`, `--drop` and `--import`, so `report` and `review` show the result at once.
- `expect` stops with exit 2 for a missing `--ref` or `--reason`, a comparison against a capture, a stale comparison, or drifts all expected or accepted already.

```console
$ driftgate expect news me --ref API-7 --reason "IDs are numbers in v2"
expected 9 drifts in 2 requests of baseline local for API-7: me, news
  header-extra header:x-powered-by
  type $.entries[*].id string→int
  ...
recorded in .driftgate/baselines/local.expectations/API-7.jsonl (exactly these drifts, with their values; the baseline is unchanged)
```

### How an expectation matches a drift

An expectation names a drift exactly: its kind, its place and both
values, as a digest. A drift with another value, such as a header that
changes again, is not expected.

- A request passes only when every drift of it is expected, by one ref or several; only the others are listed as failing.
- Content a drift line leaves out counts too: a new key's value, the items an array gained, the bytes or later lines of a body, a new element or worksheet.
- A value an allowlist rule lets change, such as a `created_at` in a new item, is held to the rule, so the expectation still holds on the next capture. Any other value is exact, with its JSON type.
- Where a new key, item or element stands counts. When two tickets add keys to one object, expect them where both are in.
- Below the values level, only what that level compares counts. The origins of both sides are written as one, so an expectation holds in a worktree on another port.
- How a JSON or CSV body is written counts too: its layout, escaping (`\/` or `/`), quotes, line ends, blank lines and empty fields. An expectation holds only for a response written the same way.
- Values a rule covers and lists that grow keep an expectation, with two exceptions where it fails when the length or quoting changes. One is a list an `any_length` rule lets grow, in a layout the comparison does not predict (Jackson's `[ 1, 2 ]`, Python 2's `", "`). The other is a CSV cell whose value needs quotes only now and then.

### What cannot be expected

- A side effect: it names what a request changed, not the values.
- A request whose private body is not on this machine: it was not compared whole.
- A value that changes on every capture and no allowlist rule covers: the next capture does not meet it. Allow it by format first.

### Where expectations are stored

Each ref has a file next to the baseline's folder:
`.driftgate/baselines/local.expectations/API-7.jsonl`. Two pull requests
with different tickets change different files, and do not conflict.

- A ref that is not a plain file name of at most 60 characters is written with `_` for other characters, cut to 60, with 8 bytes of its SHA-256 after a `~`. A name another ref's file has is refused, never written over.
- A line is added for each `expect` and `--drop`, and for each expectation `accept` or a [rebase](#driftgate-baselines-rebase) closes. It holds who, when, the ref, the reason, and the requests, each with its drift digests and signatures.
- Values of a private response, or of a request built from one, are named by a keyed hash with the project key; nothing in the file reveals them.
- Lines are chained by the hash of the line before, and sealed; each names the baseline and the ref. A file edited by hand, copied or renamed is refused (exit 3) by `compare`, `run` and `expect`, and fails `verify`. Restore it, or remove it, which removes the expectations of its ref. The chain shows accidents and edits; it is not a signature.
- With `baselines: git`, commit the file with the change; a reviewer sees each expected drift with its ticket and reason. With `baselines: local`, it stays on the developer's machine, and the pull request carries the report.

### Take expectations from another worktree

```sh
driftgate expect --import ../shop-api-7/.driftgate/baselines/local.expectations/API-7.jsonl
```

With `baselines: local`, each worktree has its own `.driftgate/`.
`--import` takes a ref's file after checking its chain, seal and
baseline. A ref the baseline has already is replaced only by a file that
continues it. When the two files went apart, delete this baseline's file
of the ref to take the other; a `--drop` would add a line, and they would
still differ.

### End an expectation

`driftgate expect --drop <ref> --reason "…"` ends every expectation of a
ref, such as when the ticket is merged into the baseline, or its
expectation stays unmet. The line stays in the ref's file.

## driftgate baselines rebase

```
driftgate baselines rebase <name> --from <tag> --reason "…" [--ref <ticket>]
    [--allow-freeze]
```

Makes a capture the content of a baseline, for example after a merge,
with the checks of a baseline made by `run` and one line in the
acceptance log. It closes the expectations the new baseline holds.

| Flag | Meaning |
|---|---|
| `--from <tag>` | the capture that becomes the baseline's content (all of the catalog; required) |
| `--reason "…"` | why the baseline takes the capture (required) |
| `--ref <ticket>` | a ticket or link kept with the log line |
| `--allow-freeze` | rebase in mode freeze too (for changes reviewed elsewhere, such as a merge) |

The task is in [rebase the baseline after a merge](../../guides/workflow/share-baselines.md#rebase-the-baseline-after-a-merge).

- The responses, bodies, `timings.json` and the state fingerprint become the capture's; `concurrency: auto` keeps the baseline's setting.
- The acceptance log gets one line: `"kind": "rebase"`, the capture, the requests that changed, came or went, and the drift signatures.
- The new baseline is written next to the old one and put in its place as `accept` does; its manifest holds the SHA-256 of the bytes as copied.
- The baseline keeps its name, even one named for a date or a release. Make a new baseline with `run --baseline <name>` when the name should say what it holds.
- [Expectations](#driftgate-expect) whose every drift is among the request's drifts from the old baseline to the capture are closed, with a line in the ref's file; so are those of requests no longer in the catalog. The others stay, and those of requests that changed otherwise are reported.
- Exit `0`; `2` for a refused rebase or a missing `--reason`; `3` for an interrupted capture, a baseline or expectations changed outside driftgate, or private bodies not on this machine.

```console
$ driftgate baselines rebase local --from main --reason "main merged API-7 and API-9" --allow-freeze
rebased baseline local on capture main: 7 requests changed
  header-extra header:x-powered-by
  ...
logged in .driftgate/baselines/local/acceptances.jsonl
closed the expectations of API-9 the baseline now holds: faq-create, news-item (.driftgate/baselines/local.expectations/API-9.jsonl)
1 expectation kept: their drifts are not all in the baseline (driftgate expect --drop <ref> --reason "…" removes one)
```

### What a capture needs to become the baseline

The capture must be whole and fit for a baseline, as `run` checks one:

- not interrupted;
- every request of the catalog that the baseline has: a capture with `--only` or `--target` is refused;
- not a read-only capture;
- a state fingerprint, when the baseline has one;
- private bodies named by a keyed hash, on this machine and readable with the key;
- nothing in public bodies and headers that [`verify`](#driftgate-verify) would refuse in a baseline: personal data, credentials.

The new baseline is then checked as `run` checks one. Requests meant to
succeed that failed, the routes they leave uncovered, and responses that
came only after a retry are listed; the exit code stays.

A capture made once, not with `capture --repeat 2`, gets a warning: a
value that changes by itself then shows as a drift on the next run. It
is not refused, as that shows at once and never passes a drift.

### Rebase after the data changed

When the data changed since the baseline (a new seed or migration), a
capture checked against the baseline stops (exit 3). Capture with
`--skip-fingerprint-check` instead: it records the fingerprint of the
data it ran on, which becomes the baseline's. The output says how many
state items differ from the old baseline's.

### Rebase in freeze mode

In mode `freeze`, a rebase needs `--allow-freeze`: the capture's changes
were reviewed where they were made. `accept` stays refused.

## driftgate verify

```
driftgate verify [--baseline <name>] [--adopt] [--migrate-private] [--migrate-state]
```

Checks baselines against their manifest and acceptance log (exit 1 when
they disagree).

| Flag | Meaning |
|---|---|
| `--baseline <name>` | check this baseline only (default: all) |
| `--adopt` | write the manifest of a baseline made before manifests (once) |
| `--migrate-private` | keep the personal data of an older baseline out of git (once, with the key; below) |
| `--migrate-state` | record an older baseline's state fingerprint in the current format (once, after `driftgate seed`; below) |

- `verify` finds hand edits, deleted or extra files and edited log lines, also in a baseline's [expectations](#driftgate-expect) file.
- `capture`, `compare` and `run` refuse such a baseline with exit 3.
- For private bodies, it reports how many the baseline refers to, and how many are on this machine.

```console
$ driftgate verify
OK     baseline local: 5 files, 1 acceptances
         3 private bodies are outside the repository (encrypted in the local store, checked against their ref when read); 3 of them are on this machine
```

### What verify scans in a committed baseline

`verify` scans what each baseline puts into git, as `capture` scans it
before storing it:

- every public body: personal data, credentials, a body that cannot be scanned;
- every record's headers: a value `capture` would mask.

Such a record fails verify (exit 1). It is named with the kind and
where, never the value. `--migrate-private` fixes it the way a capture
now stores it:

- the body goes into the local store as private, encrypted;
- the header values are masked;
- the catalog marks the request `storage: private`. A generated request takes its storage from `storage.public` instead, which the command names.

The values were committed: treat a credential as leaked.

### Line endings on Windows

A file that differs only in its line endings (CRLF where the manifest has
LF) is reported as such, with the git commands that check it out again
([line endings](../upgrading-driftgate.md#line-endings-of-baselines-checked-out-before-gitattributes)).
`run`, `accept` and `verify` put a `.gitattributes` with `* -text` into
the baselines folder when it is missing, as baselines are checked byte
for byte. Without it, git on Windows (`core.autocrlf`) checks their
files out with CRLF line endings, which every check reports as a change. With `baselines: git`, commit it.

### Migrate a baseline of an earlier release

- `--adopt` writes the manifest of a baseline made before manifests, once.
- `--migrate-private` names the private bodies of a baseline made before keyed refs by a keyed hash. It moves those a baseline kept in the repository (`private/`) into the local store.
- `--migrate-state` restores the data from the pristine copy, fingerprints it, and checks every item whose hash differs from the baseline's again, as the baseline's format hashed it. Only when all match does it write the new fingerprint into the baseline's `meta.json`.
- Both migrations keep the acceptance log, add a line to it, rewrite the manifest, and hold the lock of the local store. Commit the baseline after.

## driftgate allowlist check

```
driftgate allowlist check [--baseline <name>] [--format text|json]
```

Checks the allowlist without capturing anything: against the catalog,
against the responses of a baseline, and by itself.

| Flag | Meaning |
|---|---|
| `--baseline <name>` | the baseline whose responses the rules are checked against (default `local`; when `local` does not exist yet, only the catalog) |
| `--format text\|json` | output format (default text) |

- Exit `1` when it finds something, `0` when not.
- Exit `2` for an allowlist, catalog or configuration that cannot be read, or a `--baseline` that does not exist; `3` when the local store cannot be read.
- `compare` reports, after a comparison, the rules that applied to no change ([unused rules](../reports.md#allowlist-rules-that-changed-nothing)). `allowlist check` finds the rules that cannot apply at all, before anything is captured.

```console
$ driftgate allowlist check
header rule 2 (x-request-id): no-place: the baseline's responses to its 12 requests have no header of that name it reaches (renamed or removed?)
body rule 5 ($.entries*): any-length-values: any_length lets arrays change length; the path also reaches values such as $.entries[0].id, and those are still compared (no format): narrow the path to the arrays
body rule 7 (*updated_at): broad: the path starts with a wildcard, so it reaches every kind of body (JSON, pages, the RSC payload) at any depth; start it at $ or html and name the place (*.updated_at → $.data[*].updated_at)
note: 2 response bodies not read from the baseline: api-me (a private body, and the project key is not at hand), mut-login (a private body, and the project key is not at hand)
14 rules checked against the catalog and baseline local: 3 findings
```

The JSON output has `baseline`, `rules` (how many), `findings` and
`notes`. Each finding has `kind`, `rule`, `name` or `path`, `request`,
`target`, `check` and `detail`.

### The checks

Every finding names the rule and one check. The rule is named by its
kind (`header`, `body` or `array`) and its position among the rules of
that kind, from 1: `body rule 4 ($.entries[*].renamed)`.

- `no-request`: the rule applies to no request of the catalog that is compared. Its `request` or `target` matches none; observed requests are never compared.
- `no-place`: none of the baseline's responses to its requests has a place the rule reaches. It is usually a renamed or removed value.
- `any-length-values`: an `any_length` rule without `format` whose path also reaches values (`$.items*` reaches `$.items[0].price`). Those values are still compared, so the path is wider than the arrays it is meant for.
- `raw-without-request`: a `$raw` rule without `request`; it applies to every response that is not JSON.
- `broad`: a body rule that reaches far more than a value ([below](#when-a-rule-is-broad)).
- `shadowed`: an earlier rule always decides first wherever this one applies, so this one never decides.
- `duplicate`: the same rule as an earlier one, apart from the reason.

For `no-place`, the place is a header of that name for a header rule,
and a value (JSON, or a text or attribute of a page) for a `format` rule.
It is an array for `any_length` or an array rule, a cell for a cell rule,
and a body that is not JSON for `$raw`.

For `shadowed`, the first matching rule decides: a whole-value `format`
for a value, `any_length` for a length, a header rule for its header, an
array rule for pairing. A `parts` rule loses to any whole-value rule;
values, cells and `$raw` are looked up apart. It is reported only where
certain. One glob covers another when it has no `?` and matches the
other's text (`$.data[*].at` covers `$.data[3].at`); its `request` and
`target` must cover the other's too.

Private bodies are read with the project key, when it is at hand and the
body is on this machine. Otherwise nothing is concluded from them. A rule
that could reach a place only there, or whose requests have no response
in the baseline yet, is listed in a note as not checked, with the bodies
not read and why. Without the key the check still runs, unlike `compare`;
findings that need no private body are still certain.

### When a rule is broad

A rule is `broad` when:

- its path starts with a wildcard (`*updated_at`, `?.id`): it reaches every kind of body (JSON, pages, the RSC payload) at any depth. Start it at `$` or `html` and name the place (`$.data[*].updated_at`);
- its path names nothing after its root but wildcards and separators (`$*`, `$.*`, `html*`, `html>*`, `xlsx:*`, `xlsx:*!*`, `csv!*`): it reaches the whole body;
- its `format` accepts any value and its path is a glob or `$raw`: every value it reaches may change unchecked. A format accepts any value when it matches each of `x`, `0`, `-1.5`, `true`, `null`, a timestamp, `a b`, `é`, `<p>x</p>` and `{"a":1}`, as `.*` and `.+` do.

A rule with `parts: true` is never `broad` by its path: only what its
format matches may change, wherever that is (an asset hash anywhere in a
page). Header rules name one header, and are never `broad`.

## driftgate bisect

```
driftgate bisect <good> <bad> [--group N] [--only <glob>] [--baseline <name>]
    [--format text|json] [--expected]
```

Finds the first commit of each drift group of the last comparison
(`bisect:` in `driftgate.yaml`).

| Flag | Meaning |
|---|---|
| `--group N` | chase this drift group only (repeatable) |
| `--only <glob>` | chase these requests; without a comparison yet, the requests to chase |
| `--baseline <name>` | the baseline to compare with when there is no comparison yet (default `local`) |
| `--format text\|json` | output format (default text) |
| `--expected` | chase the drifts expectations name too ([`expect`](#driftgate-expect)); without it they are left out, as accepted ones are |

It needs `bisect.checkout` in `driftgate.yaml`
([bisect](../driftgate-yaml.md#bisect)). The task is in the
[bisect guide](../../guides/review/find-the-commit.md).

```sh
driftgate run                    # at the bad commit: drift groups
driftgate bisect v2.3.0 HEAD
```

---
**Related:** [Drift kinds](../drift-kinds.md) · [Reading a report in detail](../reports.md) · [allowlist.yaml](../allowlist-yaml.md)
