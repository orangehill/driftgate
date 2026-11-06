# allowlist.yaml

`allowlist.yaml` lists the values that may change between captures: the
time of the response, a session cookie, a token. A value on the
allowlist may differ, but it must still match its format, and every
entry has a reason.

The file is `.driftgate/allowlist.yaml`, or `driftgate/allowlist.yaml`
with `baselines: git` ([where the project's files are](cli.md#where-the-projects-files-are)).
`driftgate help allowlist` lists its keys.

## An example

<!-- check: allowlist -->
```yaml
version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response; changes with every request.
  - name: set-cookie
    request: post-api-auth-login
    format: 'session=[0-9a-f]{40}; Path=/; HttpOnly'
    reason: A new session id on every login.
  - name: content-type
    ignore_case: true
    reason: Charset names are case-insensitive (RFC 9110); a framework upgrade writes utf-8 for UTF-8.
body:
  - path: $.meta.generated_at
    format: '\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z'
    reason: Time the response was built.
  - request: get-api-feed
    path: $.items
    any_length: true
    reason: The feed shows entries of the last hour.
  - target: web
    path: html*
    format: '/_next/static/(?:[\w~-]+/)*[\w~-][\w.~-]*'
    parts: true
    reason: Asset URLs carry a build hash; the rest of the page is still compared.
  - request: post-login-invalid
    path: $raw
    format: '(?s)<!DOCTYPE html>\n<html>\n    <head>\n.*<meta http-equiv="refresh" content="0;url=''?http://localhost:\d+''?" />.*</html>'
    reason: Symfony 7 quotes the meta refresh URL of its redirect page; status and Location are still compared.
  - request: get-api-users-export
    path: xlsx:Users!A1
    format: 'Exported \d{2}\.\d{2}\.\d{4} \d{2}:\d{2}'
    reason: The export writes its own time in the header cell.
  - path: xlsx:*!F*
    format: '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'
    reason: Column F of every worksheet is a generated id.
arrays:
  - request: get-api-users
    path: $.data
    key: id
    reason: Users are listed newest first; a new user is one new item.
```

## A rule judges only a changed value

- A value equal to the baseline's is never a drift, also when it does not match the format. It is the accepted baseline, and the verdict must not depend on whether something else in the same response changed.
- This holds for JSON values, texts and attributes of a page, cells and header values.
- Nothing else is hidden: a difference in escaping or whitespace next to an allowed value is still reported.
- A value a rule covers, written otherwise (`"a\/b"` for `"a/b"`), is no drift: the rule covers how it is written. The rest of the body is compared without it ([the bytes of a JSON body](comparison.md#the-bytes-of-a-json-body)).

## Header rules

| Key | Meaning |
|---|---|
| `name` | the header name (case-insensitive), required |
| `format` | a regular expression the whole value must match; required unless `ignore_case` |
| `ignore_case` | the value may change only in letter case (compared lower-cased); not with `format` |
| `reason` | required |
| `request` | a glob over request IDs; empty: every request |
| `target` | only requests of this service |

## Body rules

| Key | Meaning |
|---|---|
| `path` | a glob over body paths, required (below) |
| `format` | a regular expression the whole value must match; required unless `any_length` |
| `any_length` | arrays at this path may change length (JSON paths only, not with a cell path) |
| `parts` | the format matches parts of the value; only those parts may change |
| `reason` | required |
| `request` | a glob over request IDs; empty: every request |
| `target` | only requests of this service |

A `path` is one of:

- a JSON path: `$.token`, `$.items[*].id`;
- a path into an HTML page: `html>body>p@class`, `html>body>script#__NEXT_DATA__$.buildId`;
- a path into the Next.js RSC payload: `self.__next_f$.0.b`;
- a cell of an exported spreadsheet: `xlsx:Users!A1`, `csv!A1` ([below](#spreadsheet-and-csv-cells));
- `$raw`, for the whole body ([below](#the-whole-body)).

A path that starts with `$` and goes on with anything but `.`, `[` or a
wildcard names no place, and is refused.

- `any_length` says nothing about values. Without a `format`, every value the path reaches is still compared: `$.items*` with `any_length` lets the list grow, not a price inside it change.
- `parts`: the rest of the value is still compared. It needs `format`, does not go with `any_length`, and the format must not match an empty text.

### The whole body

A rule with the path `$raw` covers the whole body of a response that is
not JSON, such as a framework's redirect page. `$raw` is taken
literally, and never matched by a wildcard path.

- When the bytes of the two bodies differ and neither is JSON, the current body as text must match the format (the whole text, as every format). The body is then not compared further.
- When it does not match, the body is compared as usual, so the report still lists what changed.
- Two JSON bodies are always compared value by value: name the values that may change instead.
- A JSON body that is not JSON now (an HTML error page), or the other way round, is always a `body` drift, whatever `$raw` rules there are.
- A `$raw` rule without `request` applies to every request. Give it a `request` when only some responses are such pages.
- The format is matched against the body as served, with its own origin, also when the two sides were captured from different origins: `localhost:\d+` covers both.
- Status, headers and side effects are still compared.
- A `$raw` rule needs a `format`, and takes neither `any_length` nor `parts`.

### Spreadsheet and CSV cells

A body rule whose path starts with `xlsx:` or `csv!` applies to cells, as
`format` applies to a JSON value. What is compared in a workbook is in
[Spreadsheets and CSV files](comparison.md#spreadsheets-and-csv-files).

- The current value must match the whole format, and then it may differ from the baseline. A value that does not match is a `format` drift.
- An emptied cell is the value `""`.
- `parts: true` works as for other values; `any_length` does not apply.
- Rules apply at the `values` level.
- A cell rule lets only the value change, never the type, format or formula.
- Only paths that start with `xlsx:` or `csv!` reach cells: a rule such as `path: '*'` stays with JSON and HTML.

The worksheet is written as in the report.

- A name of letters, digits, `_` and `.` is written as it is: `Users`, `Članovi`.
- Any other name is in single quotes, with a quote doubled: `'Q1 2026'`, `'Hi!'`, `'O''Brien'`.
- When the workbook's `xl/workbook.xml` (or its relationships) is missing or cannot be read, the worksheets are named after their part in brackets: `xlsx:[sheet1]!A1`. No workbook name can look like that.

The cell is a reference in capitals (`A1`), or a glob over one.

- `B*` is column B, `*1` row 1, `xlsx:*!A1` cell A1 of every worksheet, and `xlsx:*` every cell.
- A glob's column and row are matched apart. The column is the cell's part up to its last letter, the row the rest. With no letter, the column is what comes before the first digit.
- `*` and `?` match letters in the column and digits in the row only. So `*1` is row 1 and not A11, `B*` column B and not BA, `B?` B1 to B9, `B*1` the rows of column B that end in 1, and `*` every cell.
- `driftgate allowlist check` uses the same meaning.

A path that no cell can have is refused when the allowlist is read.

- Refused are `xlsx:Users` (a whole worksheet), `b2`, `2B`, `B`, `A0`, and `??` (no letter or digit to tell the column from the row).
- Also refused are an unquoted `xlsx:Q1 2026!A1`, and a needlessly quoted `xlsx:'Users'!A1`.
- A worksheet named by its bare part (`xlsx:sheet1!B2`) is refused, with the message to write the name from a report, usually `xlsx:Sheet1!B2`. A worksheet the workbook names `sheet1` is written `xlsx:'sheet1'!B2`.

## Array rules

An array rule pairs the items of the arrays its path reaches by a key,
instead of by position. What that changes in a comparison is in
[Arrays paired by a key](comparison.md#arrays-paired-by-a-key).

| Key | Meaning |
|---|---|
| `path` | a glob over JSON paths of arrays, required (below) |
| `key` | the member that identifies an item, or member names joined by dots (`user.id`); required unless `any_order` |
| `any_order` | the items may come in any order (below) |
| `reason` | required |
| `request` | a glob over request IDs; empty: every request |
| `target` | only requests of this service |

- `path` reaches arrays in a body, in JSON embedded in a page, and in the RSC payload. The first array rule that matches an array pairs its items.
- `key` is a member every item has (`id`), or a path into the item (`user.id`).
- `any_order` without `key`: the same items in another order are no drift, and anything else is compared by position.
- With `any_order`, the rest of the document, how it is written included, is compared as without the rule. That includes an escaped key, or spacing inside an item that moved.
- The items are put back in the baseline's order first, orders inside items that moved too. This works in JSON bodies, in JSON in a page and in UTF-16.
- A document whose items cannot be put back (a devalue payload, compared decoded) is compared as it is, and its order shows as an `encoding` drift.

Body rules reach keyed items with a glob as before:
`$.data[*].updated_at` matches `$.data[id=42].updated_at`. A rule
written with an index (`$.data[0].x`) no longer matches an array paired
by key.

## Rules DriftGate proposes

`capture --repeat 2` and the first `run` propose entries with a ready
format for values that differ between two captures on the same state.
You add the reason.

## Check the rules

[`driftgate allowlist check`](commands/review.md#driftgate-allowlist-check)
finds rules that apply to no request, reach nothing in the baseline, are
broader than a value, or never decide because an earlier rule does.

---
**Related:** [How responses are compared](comparison.md) · [Drift kinds](drift-kinds.md) · [driftgate allowlist check](commands/review.md#driftgate-allowlist-check)
