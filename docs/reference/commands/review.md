# Commands: review and record results

The commands that read the last comparison and change what the baseline
holds, plus the checks of the baseline and the allowlist. What to do
with a drift is under [Drift kinds](../drift-kinds.md#what-to-do-with-a-drift).

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

Records drifts of the last comparison that a change intends, by ticket,
without changing the baseline; this works also in mode `freeze`.
`compare` then passes a request every drift of which is exactly a
recorded one, and fails on any other drift.

| Flag | Meaning |
|---|---|
| `--ref <ticket>` | a ticket or link of the change that intends the drifts (required) |
| `--reason "…"` | why the change makes these drifts, or why they are dropped (required) |
| `--comparison <id>` | the comparison `review` showed; `expect` refuses a newer one |
| `--drop <ref>` | remove the expectations of this ref instead (no requests) |
| `--import <file>` | take a ref's expectations file from another checkout (a worktree) instead, after checking it |
| `--baseline <name>` | with `--drop` or `--import`: the baseline (default: the last comparison's, else `local`) |

It is meant for a team in mode `freeze` whose pull requests carry
intended changes. Each ticket's drifts are recorded once, and the gate
stays red for everything else. The baseline, with its files, manifest and
acceptance log, does not change.

- Requests named by their ID or a glob are expected with all their drifts.
- `group:N` takes the drifts of a group, so two tickets that change one request in different ways each expect their own drifts.
- A request that was not captured is taken only by its ID.
- The last comparison is marked again after `expect`, `--drop` and `--import`, so `report` and `review` show the result at once.

```console
$ driftgate expect news me --ref API-7 --reason "IDs are numbers in v2"
expected 9 drifts in 2 requests of baseline local for API-7: me, news
  header-extra header:x-powered-by
  type $.entries[*].id string→int
  ...
recorded in .driftgate/baselines/local.expectations/API-7.jsonl (exactly these drifts, with their values; the baseline is unchanged)
```

### How an expectation matches a drift

Each drift is recorded as a digest of its kind, place and both values.
A drift is expected only when an expectation in force names exactly that
drift, never one with another value (a header that changes again, a date
in another format).

- A request is expected only when every one of its drifts is, by one ref or several. A request with one drift that is not expected fails, and only that drift is listed as failing.
- Where a drift's description leaves content out, the digest covers that content too, as the comparison sees it:
  - the new key's value, or the items an array gained;
  - a body that differs only in its bytes, or the lines after the first changed one;
  - a new element or worksheet, and the rest of the workbook when worksheets come, go or move.
- A value an allowlist rule lets change (a `created_at` in a new item) is written as the rule, so the expectation holds on the next capture. A value no rule names is exact, with its JSON type.
- A new key, item or element is expected with where it stands. When two tickets add keys to one object, expect them where both are in.
- Below the values level, only what that level compares counts: types, structure, names.
- The origins of both sides, and their aliases, are written as one neutral origin. An expectation recorded in a worktree that runs on another port holds in the others.

How the body is written counts too, wherever the drift is: a header,
the status, or another part of a page.

- Each JSON or CSV document of the response whose bytes differ is read for what it writes beyond its values. That is its layout and the whitespace the layout does not say, how its keys and strings are escaped (`\/` or `/`), a CSV file's quotes, line ends, blank lines and empty fields.
- Where that changed, it is in the digest of every drift of the response: an expectation holds only for a response written the same way.
- A rule covers how the values it lets change are written, and an encoder writes the rest the same way for other values and lengths. So values a rule lets change, and lists that grow, keep an expectation.
- Two cases do not. One is a list an `any_length` rule lets grow, in a layout the comparison does not predict: Jackson's default `[ 1, 2 ]` and `"a" : 1`, or Python 2's `", "` before a line end. The other is a CSV cell a rule lets change whose value needs quotes only now and then. There an expectation fails when the length or the quoting changes.

### What cannot be expected

- A side effect names what a request changed, not the values, so it cannot be expected.
- Nor can a request whose private body is not on this machine: it was not compared whole.
- A value that changes on every capture and no allowlist rule covers makes an expectation that the next capture does not meet. Allow it by format first.
- A drift another ref expects already is left out, and the output says so: one ticket does not take another's drifts.
- Expecting a request with the same ref again adds the drifts selected now to that ref's expectation of it. Of its earlier drifts, only those that no longer occur are dropped.

`expect` stops with exit 2 for a missing `--ref` or `--reason`, a
comparison against a capture, a stale comparison, or a selection whose
drifts are all expected (by another ref) or accepted already.

### Where expectations are stored

- Each ref has a file of its own, `<baseline>.expectations/<ref>.jsonl`, next to the baseline's folder: `.driftgate/baselines/local.expectations/API-7.jsonl`.
- A ref that is not a plain file name of at most 60 characters is written with `_` for other characters, cut to 60 characters, with 8 bytes of its SHA-256 after a `~`. A name another ref's file has is refused, never written over.
- Two pull requests with different tickets change different files, and do not conflict.
- A line is added for each `expect` and `--drop`, and for each expectation that `accept` or a [rebase](#driftgate-baselines-rebase) closes because the baseline now holds its drifts. A line holds who, when, the ref, the reason, and the requests with their drift digests and signatures.
- The values of a private response, or of a request built from one, are named by a keyed hash with the project key, as private bodies are. Nothing in the file reveals them.
- Lines are chained by the hash of the line before, a last line seals the file, and every line names the baseline and the ref.
- A file edited by hand, copied from another baseline or renamed is refused (exit 3) by `compare`, `run` and `expect`, and fails `verify`. Restore it, or remove it, which removes the expectations of its ref.
- This shows accidents and hand edits, as the acceptance log's chain does; it is not a signature.

With `baselines: git`, the ref's file is part of the pull request
(`driftgate/baselines/<name>.expectations/`), next to the code that
makes the drift; commit it with the change. A reviewer sees each
expected drift's signature, the ticket and the reason. With
`baselines: local`, the expectations stay on the developer's machine,
like the baseline. The pull request carries what the report says
(`report --format markdown`), and the gate is that developer's run.

### Take expectations from another worktree

With `baselines: local`, each worktree has its own `.driftgate/`. To
carry a ticket's expectations from one to another:

```sh
driftgate expect --import ../shop-api-7/.driftgate/baselines/local.expectations/API-7.jsonl
```

- It takes the file after checking its chain, seal and baseline.
- A ref the baseline has already is replaced only by a file that continues it.
- When the two files of a ref went apart, delete this baseline's file of the ref to take the other. A `--drop` would add a line, and they would still differ.

### End an expectation

`driftgate expect --drop <ref> --reason "…"` ends every expectation of a
ref. Use it when the ticket is merged and its change is in the baseline,
or when its expectation stays unmet. The line stays in the ref's file.

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

A baseline goes stale when changes reviewed in other pull requests are
merged: every later comparison shows their drift. Capture the merged code
(`driftgate capture main`), then
`driftgate baselines rebase local --from main --reason "…"`.

- The responses, bodies, `timings.json` and the state fingerprint become the capture's.
- The acceptance log is kept and gets one line: `"kind": "rebase"`, the capture, the requests that changed, are new or are no longer in the catalog, and the drift signatures.
- The manifest is written again. `concurrency: auto` keeps the setting the baseline recorded.
- The new baseline is written next to the old one and put in its place as `accept` does. Its manifest holds the SHA-256 of the bytes as they were copied.
- The baseline keeps its name: one named for a date or a release (`release-2026-10`) still has that name after a rebase on other code. Make a new baseline with `run --baseline <name>` when the name should say what it holds.
- [Expectations](#driftgate-expect) whose drifts the rebase took into the baseline are closed, with a line in the ref's file. So are those of requests no longer in the catalog. An expectation's drifts are taken when every drift it names is among the request's drifts from the old baseline to the capture.
- The other expectations stay, and those of requests that changed otherwise are reported.
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
succeed that failed in it, and the routes they leave uncovered, are
listed. Responses that came only after a retry are named in a warning.
As with `accept`, these never change the exit code: the rebase is
written.

`run` makes a baseline only from two equal captures. A rebase takes one
capture. When it was made once, and not with `capture --repeat 2` (which
records that its runs agreed), the rebase warns: a value that changes by
itself then shows as a drift on the next run. It is not refused, as that
shows at once and never passes a drift. Capture with `--repeat 2` to
check first.

### Rebase after the data changed

When the data changed since the baseline (a new seed or migration), a
capture checked against the baseline stops (exit 3). Capture with
`--skip-fingerprint-check` instead: it records the fingerprint of the
data it ran on, which becomes the baseline's. The output says how many
state items differ from the old baseline's.

### Rebase in freeze mode

Mode `freeze` means the baseline changes only with intent. A rebase
there needs `--allow-freeze`, which says that the capture's changes were
reviewed where they were made. `accept` stays refused.

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
| `--migrate-state` | record the state fingerprint of a baseline made by an earlier release in the current format, after checking the data is the baseline's (once, after `driftgate seed`; below) |

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
LF) is reported as such, with the git commands that check it out again.

`run`, `accept` and `verify` put a `.gitattributes` with `* -text` into
the baselines folder when it is missing. Baselines are checked byte for
byte, and git on Windows (`core.autocrlf`) would otherwise check their
files out with CRLF line endings, which every check then reports as a
change. With `baselines: git`, commit it.

A checkout made before that file has the CRLF files already. These
commands check them out again as they were committed:

```sh
git rm -r -q --cached driftgate/baselines
git checkout HEAD -- driftgate/baselines
```

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

The place a rule reaches, for `no-place`, depends on the rule:

- a header of that name, for a header rule;
- a value (a JSON value, or a text or attribute of a page), for a rule with a `format`;
- an array, for `any_length` and for an array rule (`arrays:`);
- a cell, for a cell rule;
- a body that is not JSON, for `$raw`.

Which rule decides first, for `shadowed`:

- The first matching rule with a whole-value `format` decides a value, and the first `any_length` rule an array length.
- The first header rule of a name decides its header, and the first array rule how an array's items pair.
- A `parts` rule loses to any whole-value rule that matches.
- Values, cells and `$raw` rules are looked up apart.
- Shadowed rules are found only where that is certain. One glob covers another when it has no `?` and matches the other's text: `$.data[*].at` covers `$.data[3].at`. A rule's `request` and `target` must cover the other's too.

Private bodies are read with the project key when it is at hand and the
body is on this machine, as `compare` reads them. Otherwise they are not
read, and nothing is concluded from them.

- A rule that could reach a place only in such a body, or whose requests have no response in the baseline yet, is listed in a note as not checked, instead of `no-place`.
- The note also lists the bodies not read, and why.
- Without the key the check still runs, and does not stop with exit 3 as `compare` does. Findings that need no private body are still certain.

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
([bisect](../../configuration.md#bisect)). The task is in the
[bisect guide](../../guides/bisect.md).

```sh
driftgate run                    # at the bad commit: drift groups
driftgate bisect v2.3.0 HEAD
```

---
**Related:** [Drift kinds](../drift-kinds.md) · [Reading a report in detail](../reports.md) · [allowlist.yaml](../../configuration.md#allowlistyaml)
