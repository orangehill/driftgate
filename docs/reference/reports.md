# Reading a report in detail

What [`driftgate compare`](commands/check.md#driftgate-compare), `run`
and [`driftgate report`](commands/check.md#driftgate-report) show, line
by line and in each format. The kinds of drift are in
[Drift kinds](drift-kinds.md).

## The verdict line

The verdict says whether the comparison passed: `No drift ✓`, or
`No unexpected drift ✓` when every drift is expected.

A comparison that compared nothing is incomplete (exit 3), never green.
A typo in `--only`, renamed request IDs, or a baseline made for another
catalog would otherwise pass without checking anything. The report says
why.

## Drift groups and their numbers

Drifts of the same kind at the same place are one group. Groups are
numbered by size. The groups the default view leaves out (only in
requests whose status changed) come after the ones it lists, so the
listed groups start at [1]. `review --group N` and `accept group:N` use
the same numbers.

## Long values

A drift line shows at most 80 characters of each value; `review` and
`review --group` show 100. When either value is longer, both are cut to
the same window around the first character where they differ, with `…`
where text is left out. Two long texts that differ in the middle show
that difference. The group's explanation does the same with fewer
characters.

- A quoted text keeps both its quotes, with `…` outside them where text is left out.
- A `Set-Cookie` value longer than 16 characters, such as a session token, shows as its length: `laravel-session=…(342); path=/`. Its attributes are shown in full, so a changed `samesite` is seen. A private value stays `(private)` without `review --show-private`.

## A request whose status changed

When a request's status changes, what follows from it is summarized in
one line: the body, and headers other than `location` and
`content-type`. The groups list the status changes with their requests.

- The groups that only follow from a status change are left out. `review` lists them, with the numbers `accept group:N` takes.
- `--max-drifts N` lists every drift as usual.
- The JSON report always has them all: `status_change` and `collapsed` (how many drifts the summary line stands for) per request, and `status_changes` for the report.

```console
$ driftgate compare before
...
DRIFT  home
         status 200 → 302, redirects to /login
         header-extra    header:location   → ["/login"]
         body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
DRIFT  api-news
         status 200 → 500
         header-value    header:content-type  ["application/json"] → ["application/problem+json"]
         body differs: 5 drifts, mostly json; 1 other header differs (x-total); listed with --max-drifts N and in the JSON report

Groups:
  [1] 4 drifts in 4 requests: status changed (for example 200 → 302)
      3 requests: 200 → 302 to /login (home, about, contact)
      1 request: 200 → 500 (api-news)
  [3] 3 drifts in 3 requests: header location is new
  … 9 more groups only in requests whose status changed, summarized there (driftgate review lists them)
```

## The count of drifts by kind

With drift, one line counts the drifts by kind, the most first, status
included: `kinds: header-extra 622, array-length 188, type 154,
side-effect 6`. It stands right before the line on one change in almost
every request. Text and markdown show the line; JSON has `kinds`, a
count per kind.

## One change in almost every request

When one change is in nine of ten requests with an unexpected drift or
more, a line says so. It needs at least five such requests, and expected
drifts do not count. The line also says how many drift in something
else once the change is set aside:

```
one change in 622 of 622 drifted requests: header server changed value (…); set aside, 6 of 622 drift in something else
```

- For a header that names the server stack (`server`, `x-powered-by`, `via`) whose new value is one value on every request, it adds the rule that allows exactly that value. Paste it under `headers:` in `allowlist.yaml`: `- {name: 'server', format: 'nginx/1\.25', reason: '…'}`.
- Any other change gets no rule. A change between builds is fixed, accepted (`driftgate accept group:N`, mode `evolve`) or expected (`driftgate expect group:N --ref <ticket>`, mode `freeze`).
- A value that changes from run to run is found with `capture --repeat 2`, which proposes rules for it.
- Where `accept` and `expect` refuse the comparison, the line names neither. For two captures, or one baseline against another, it says to fix the change or name it in the pull request; `baselines rebase` records a switch of the server stack.
- For a capture made with `--skip-fingerprint-check`, it says to capture again without the flag first.

JSON: `dominant`, with `group`, `signature`, `explanation`, `requests`,
`drifted`, `elsewhere`, `rule`, and `captures` or `unreliable` in those
cases.

## What the result does not cover

After the verdict, the report says what the result does not cover. This
is information: the exit code never depends on it.

One line counts the compared requests by what they do, for all of the
catalog, or `compared (--only)`:

```
catalog: 622 requests: 572 reads, 50 mutating, 0 command steps; 0 of them multipart
```

A zero is what the gate never sends: with no `multipart` request, no
upload is checked. The line is shown with gaps, and when the catalog
sends no upload or no command step.

Under `Not covered by this result (driftgate coverage lists the
routes):` it gives a count, and up to five names (`and N more`), for
each gap:

- route methods without a request meant to succeed, with a route inventory (`routes.json`) and without `--only`;
- route methods whose body is validated but whose validation errors no request checks (template `empty-body`), on the same terms;
- requests meant to succeed whose answer in the set compared against is an error, which the comparison keeps green (`failed in baseline:local`, or the capture compared against);
- requests observed: measured, not compared;
- every request that was skipped, not compared, by what it is: a request that changes data, a read, or a command step. Each line gives the reason, such as no baseline, or not sent by `capture --read-only` (also a read in a sequence with a write);
- known failures (`known_failures:` in `exclusions.yaml`).

There is no list when there is no gap. `run` uses the coverage its
coverage step found; `compare` takes it once from `routes.json` and the
records it read already.

- JSON: `not_covered`, with `catalog` (`requests`, `reads`, `mutating`, `commands`, and `multipart` among them), `only`, `against` and `routes_checked`. Then `no_request`, `no_invalid_body`, `failed_in_baseline`, `observed` and `known_failures`, each with a `count` and up to ten `names` (request IDs or routes), left out when empty. `skipped` is a list of the same, each with `what` (`mutating`, `read` or `command`) and `reason`.
- Markdown: a short list.
- JUnit: the lines in the suite's `system-out`.

### What a green result does not prove

What a pass covers, and what it does not, is in
[What a green result proves](../concepts/what-a-pass-proves.md).

## Expected drifts in a report

Against a baseline with
[expectations](commands/review.md#driftgate-expect), a drift an
expectation names exactly is expected. A request every drift of which is
expected does not fail the comparison. The exit code is `1` only while
a drift is not expected.

- The requests that fail come first, each with the drifts that are not expected, and one line for those that are.
- Text lists the expected requests after them, as one line per ref.
- Markdown puts them in a collapsed block after the failing requests, the first thing left out when the comment would grow too long, with an `Expected` column in the table.
- JUnit makes them passing test cases, with their drifts in `system-out`.
- Groups all of whose drifts are expected are marked `(expected)`, and the summary counts `N drifted (M expected)`.
- JSON has `expected` and `expected_refs`, the refs per result (`expected`), and per drift `expected` with each drift's `digest`.

Expectations whose drifts the comparison did not have are listed as a
warning (`unmet_expectations`). The request did not drift, drifted
otherwise, drifted in part of what was expected (`partly met`), or is not
in the catalog. In JUnit they are also in the `system-out` of the
request's test case.

## Requests without a baseline response

Requests the baseline has no response for are skipped, and listed in
every format: `no_baseline` in JSON, a line in text and markdown. They
do not make the result incomplete while other requests were compared,
since every new request starts without a baseline.

The line says how a new request joins the baseline, in the mode of the
comparison, and `review` repeats it:

- mode `evolve`: `driftgate accept <request>... --reason "…"`;
- mode `freeze`, which refuses `accept`: `driftgate baselines rebase <baseline> --from <capture> --reason "…" --allow-freeze`, with the baseline and capture of the comparison. The rebase takes the whole capture, its drifts too. So the command is printed only when nothing else drifted, an expected drift included; otherwise the line says to fix or expect the drifts first. After `run --only`, it first needs a capture of the whole catalog. A comparison that is not of a capture against a baseline gets the general form, `driftgate baselines rebase <baseline> --from <capture> --allow-freeze`; `accept` is never named in mode `freeze`.

## Requests sent again after a restore

A restore of a whole SQL Server database ends the application's database
sessions ([SQL Server](databases/sql-server.md)). A request the
capture or the baseline sent again after such a restore has a note with
the first answer. Every format counts them in a line of its own, so the
count shows when the notes are cut after ten:

- JSON: `resent` (the IDs), and `"resent": true` on each result;
- text and markdown: a line;
- JUnit: a `resent` property and the line in the suite's `system-out`.

## Requests that passed only after a retry

A request whose response came only after a
[retry](capture.md#retries-of-requests-that-failed-under-load), and that
passed, also as an expected drift, is never shown as a plain pass. It
counts as passed: the exit code does not change.

- Text has a `RETRY` line with the first outcome, and the verdict says `No drift ✓ (1 passed only after a retry)`.
- Markdown says it in the headline and in a note.
- JSON has `retry` on the request and `passed_after_retry` on the report.
- JUnit gives the test case a `system-out` line (an observed or skipped one, its `skipped` message), and the suite's `system-out` the note.

A drift that came after a retry says so among its drifts.

- One that failed in every send says `failed in all 2 sends`, and is never counted as passed after a retry.
- When it passes anyway (the baseline failed the same way, or an expected drift), text has a `RETRY` line (`passed; failed in all 2 sends: …`), the note names it, and JUnit gives its test case that line.
- A request that needed a retry in the baseline and again now is not load. Every format says so louder: `it needed a retry in the baseline too: not load, check it`; JSON `retried_in_both`.
- Requests whose baseline response came after a retry are listed too: `baseline_retry`, `baseline_retried`.

## Response times

Response times are shown as information, never as a gate: one
measurement per request is too noisy for a gate.

- The capture keeps the time of every request in `timings.json`, also in a baseline made by `run`.
- When both sides have times, the report gives the 50th and 95th percentile of each side over the compared requests.
- It lists the requests that became much slower: at least twice as slow and at least 200 ms slower. The ratio alone would flag 3 → 7 ms, and the difference alone the jitter of slow requests.
- Nothing is shown when a side has no times.
- A capture with `--repeat N`, and the two captures `run` makes a baseline from, keep the median time of each request over the runs. Of an even number of runs, that is the mean of the two middle times.

In each format:

- JSON: `timings`, with `requests`, `baseline` and `current` (`p50_ms`, `p95_ms`), and `slower` (`id`, `baseline_ms`, `current_ms`, the most added time first);
- text and markdown: one line with up to ten slower requests;
- JUnit: suite properties (`p50_ms`, `p95_ms`, `slower:<id>`) and the line in the suite's `system-out`, never a failed test case.

## Allowlist rules that changed nothing

Allowlist rules that applied to no change in the comparison are listed:
`unused_rules` in JSON, a warning in text and markdown, never a failure.

- `no request`: all the rule's requests are gone or renamed.
- `no change`: the values it names did not change this time.
- A rule listed comparison after comparison is probably stale: the value it names was renamed or removed. Not so for a value that changes only sometimes, such as a render time in whole milliseconds: keep its rule.
- With `--only` or `--target`, rules of the requests not compared are not listed.
- Nor is a rule that its requests were not compared far enough for: below `--level values` a header or value rule never applies, so it is not called stale.
- A rule `init` wrote for the framework (a throttle's `x-ratelimit-*`) is not listed while it is as `init` wrote it: the same header or path, request, format, options and reason. One you changed is listed.

## Report formats

`compare --format` and `report --format` take `text`, `json`, `junit` or
`markdown`.

Without `compare --show-private`, no format shows values from private
bodies. With it, every format of that output shows them, so never post
it. The saved report, which `report`, `review` and `accept` read, never
holds them.

### Text and markdown

Markdown stays under GitHub's comment limit of 65,536 characters, with a
note on what was left out.

### JSON

The JSON report has every drift, also those the text view summarizes.
The fields are named under the sections above.

### JUnit

Every request is a test case. A drifted one fails; a skipped or observed
one is skipped.

An incomplete result adds a test case `incomplete`, with an `<error>`
that says why: private bodies not on this machine, requests a read-only
set left out, or nothing compared. The suite's `errors` count is then
not 0, so a CI that reads only the file does not see it pass.

---
**Related:** [Drift kinds](drift-kinds.md) · [driftgate compare](commands/check.md#driftgate-compare) · [driftgate review](commands/review.md#driftgate-review)
