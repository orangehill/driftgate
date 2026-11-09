# Reviewing drift

`driftgate run` and `driftgate compare` end with a report.
Every drift left after the allowlist gets one answer: fix the code, accept it, or expect it for a ticket.

## Reading the report

```console
DRIFT  get-api-news-id
         header-extra    header:x-powered-by   → ["demo"]
         key-extra       $.entry.meta   → (private)
         type            $.entry.id  string → int
         value           $.entry.published_at  (private) → (private)

Groups:
  [1] 3 drifts in 3 requests: header x-powered-by is new
      GET /api/news  (+2 more routes)
...
capture:run-bd607d2d390c against baseline:local: 3 compared, 0 passed, 3 drifted, 0 skipped, 0 observed
kinds: header-extra 3, key-extra 3, type 3, value 3
```

| Part | Meaning |
|---|---|
| `DRIFT <request>` | one block per drifted request; each line is kind, place, old → new |
| `(private)` | a value of a private body; `review --show-private` shows it on your machine only |
| `Groups:` | drifts of one kind at one place, often one cause; `group:N` selects one |
| summary line | requests compared, passed, drifted, skipped (new, no baseline answer) and observed |
| `kinds:` and lines below | counts, coverage gaps, slow requests; they never change the exit code |

The exit code is the verdict: `0` clean, `1` drift, `3` incomplete ([exit codes](commands.md#exit-codes)).
An incomplete result is never green: fix the cause and run again.

`driftgate review` lists the groups with the command for each; `--group 3` shows one group.
A request lists up to 20 drifts; `--max-drifts N` and `report --format json` show the rest.

## Drift kinds

The kind is the first word of a drift line, and `kind` in JSON reports. The names are stable. `driftgate help kinds` lists them.

| Kind | Meaning | Typical fix |
|---|---|---|
| `status` | the HTTP status changed, or the request got no answer | — |
| `header-missing` | a header of the baseline is gone | — |
| `header-extra` | a header is new | — |
| `header-value` | a header's value changed | a header rule if it changes by itself |
| `key-missing` | a JSON key, or an array item paired by key, is gone | — |
| `key-extra` | a JSON key, or an array item paired by key, is new | — |
| `key-order` | the same keys in another order | — |
| `array-length` | an array has more or fewer items | `any_length` for a list that changes by itself |
| `item-order` | the same items of an array in another order | add `ORDER BY`, or an `any_order` array rule |
| `type` | a value's type changed, in JSON or in a spreadsheet cell | cast the value in the code |
| `number-literal` | the same number of the same type written differently, such as 1.00 for 1.0 | fix the encoder |
| `value` | a JSON value changed | a body rule if it changes by itself |
| `format` | a value an allowlist rule covers no longer matches its format | fix, or update the rule |
| `encoding` | the same values in other bytes: layout, escapes, byte order mark, charset | fix the encoder |
| `body` | a body that is not JSON changed, or JSON became non-JSON | a `$raw` rule for a page that changes by itself |
| `element-missing` | an HTML element is gone | — |
| `element-extra` | an HTML element is new | — |
| `element-order` | the same HTML elements in another order | — |
| `attribute-missing` | an attribute of an HTML element is gone | — |
| `attribute-extra` | an attribute of an HTML element is new | — |
| `attribute-value` | an attribute's value changed | a rule with `parts` for an asset hash |
| `text` | the text of an HTML element changed | — |
| `doctype` | the document type declaration came, went or changed | — |
| `cell` | a spreadsheet or CSV cell's value changed | a rule at `xlsx:<sheet>!<cell>` or `csv!<cell>` |
| `cell-format` | a cell's number format or style changed | — |
| `cell-formula` | a cell's formula changed | — |
| `sheet-missing` | a worksheet is gone | — |
| `sheet-extra` | a worksheet is new | — |
| `sheet-order` | the worksheets are in another order | — |
| `exit-code` | a command step ended with another exit code | — |
| `repeat` | the statuses of a repeated request changed | — |
| `side-effect` | the request changed other data than in the baseline | — |
| `not-captured` | the baseline has a response, the capture has none | capture again, or accept by ID |

A dash means: fix the code, or accept or expect the change. A side effect names what changed, not the values, so it cannot be expected.

## Allowlist rules

`allowlist.yaml`, next to the catalog, names values that change by themselves: a time, a token, a session cookie.
An allowed value may differ, but must match its format, and every rule has a reason.
A value equal to the baseline's is never a drift.

<!-- check: allowlist -->
```yaml
version: 1
headers:
  - name: set-cookie
    request: post-api-auth-login
    format: 'session=[0-9a-f]{40}; Path=/; HttpOnly'
    reason: A new session id on every login.
body:
  - request: get-api-feed
    path: $.items
    any_length: true
    reason: The feed shows entries of the last hour.
arrays:
  - request: get-api-users
    path: $.data
    key: id
    reason: Newest first; a new user is one new item.
```

| Key | In | Meaning |
|---|---|---|
| `name` | headers | header name, any case |
| `ignore_case` | headers | the value may change only in letter case |
| `path` | body, arrays | a glob over places: `$.items[*].id`, `html>body>p@class`, `xlsx:Users!A1`, `csv!A1`, or `$raw` |
| `format` | headers, body | a regular expression the whole new value must match |
| `parts` | body | only the parts the format matches may change |
| `any_length` | body | arrays at the path may change length; their values are still compared |
| `key` | arrays | pair items by this member (`id`, `user.id`) instead of by position |
| `any_order` | arrays | the same items in another order are no drift |
| `request`, `target` | all | only these request IDs (glob); only this service |
| `reason` | all | required |

- `$raw` covers a whole body that is not JSON, such as a redirect page. Status and headers are still compared.
- The first `run` and `capture --repeat 2` propose rules with a ready format. You add the reason.
- `driftgate allowlist check` finds rules that reach nothing, are too broad, or are shadowed.
- Never allow a value that changed because of the upgrade. That hides the regression the gate exists for.

## Accept, expect or rebase

| Situation | Command |
|---|---|
| not intended (the usual case in an upgrade) | fix the code until the old answer comes back |
| intended; the team may change the baseline (`mode: evolve`) | `driftgate accept group:3 --reason "…" --ref SHOP-12` |
| intended by a ticket; the baseline stays | `driftgate expect group:3 --ref SHOP-12 --reason "…"` |
| a new request, or one no longer captured | `driftgate accept <id> --reason "…"` (evolve; in freeze: rebase, below) |
| the baseline should become a capture, after a merge | `driftgate baselines rebase local --from <tag> --reason "…"` |

- `accept` takes whole requests into the baseline, and logs who, when and why in `acceptances.jsonl`.
- Pass `--comparison <id>` as `review` prints it, so you accept exactly what you reviewed.
- `expect` records these exact drifts, values included. A drift with another value still fails.
- A ticket's expectations live in `<baseline>.expectations/<ref>.jsonl`. `expect --drop <ref> --reason "…"` ends them.
- `accept` and `rebase` close the expectations the baseline now holds. With `baselines: git`, commit all these files.

Details and flags: [accept](commands.md#accept), [expect](commands.md#expect), [baselines rebase](commands.md#baselines-rebase).

## Mode freeze

In `mode: freeze`, the default, `accept` is refused and every drift fails unless a ticket expects it.
Use it while the API must not change, such as during an upgrade ([modes](how-it-works.md#modes-evolve-and-freeze)).

- New requests: when nothing else drifted, `run` prints the rebase that adds them, with `--allow-freeze`.
- A rebase next to a drift would take the drift into the baseline. Fix or expect the drift first.

## Bisect

`driftgate bisect <good> <bad>` names the first commit of each drift group.
Run `driftgate run` at the bad commit first, so bisect knows the groups.

<!-- check: config-fragment -->
```yaml
bisect:
  checkout: ["sh", "-c", "cd \"$DRIFTGATE_BISECT_DIR\" && composer install -q && make restart"]
  ready: ["curl", "-sf", "http://localhost:8000/up"]
  finish: ["make", "restart"]
```

```sh
driftgate bisect v2.3.0 HEAD             # every open group
driftgate bisect v2.3.0 HEAD --group 2   # one group
```

- Each commit is checked out in a temporary worktree; `checkout` must start the application from `$DRIFTGATE_BISECT_DIR`.
- A commit whose `checkout` fails, or that is not `ready` in time, is skipped. The answer may then be a range.
- The good commit must be clean and the bad one must drift in the chosen groups. Otherwise bisect stops (exit 2).
- An SQLite state is refused: use `git bisect` with `driftgate run` at each step.
