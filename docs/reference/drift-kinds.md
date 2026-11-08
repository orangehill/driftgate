# Drift kinds

Every drift has a kind: the class of what changed. The kind is the first
word of a drift line, and the `kinds:` line of a report counts the drifts
by kind.

```console
$ driftgate review
...
[1] header-extra header:x-powered-by  (3 requests, 3 drifts)
...
```

The names are stable, so CI can rely on them: JSON reports carry each
drift's `kind`. `driftgate help kinds` lists every kind in the terminal.
The [level](driftgate-yaml.md#level) of a comparison decides
which kinds it can report.

## What to do with a drift

- Fix the code when the change is not intended.
- [Accept](commands/review.md#driftgate-accept) the change when it is intended, in mode `evolve`.
- [Expect](commands/review.md#driftgate-expect) the change for a ticket: the baseline stays as it is, also in mode `freeze`.
- Add an [allowlist rule](allowlist-yaml.md) only for a value that changes by itself, such as a time or a token.

The "Typical fix" column adds only what is specific to a kind. A dash
means nothing beyond the list above.

## Status and headers

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `status` | the HTTP status changed, or the request got no answer | a validation or authorization rule changed | — |
| `header-missing` | a header of the baseline is gone | middleware or the server stack changed | — |
| `header-extra` | a header is new | middleware or the server stack changed | — |
| `header-value` | a header's value changed | another server stack, cookie or cache settings | a header rule for a value that changes by itself |

## JSON bodies

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `key-missing` | a JSON key, or an array item paired by key, is gone | serializer or resource change | — |
| `key-extra` | a JSON key, or an array item paired by key, is new | serializer or resource change | — |
| `key-order` | the same keys in another order | serializer change | — |
| `array-length` | an array has more or fewer items | data or query change | an `any_length` rule for a list that changes by itself |
| `item-order` | the same items of an array in another order | another database server or ORM; no `ORDER BY` | add `ORDER BY`; an `any_order` array rule if order does not matter |
| `type` | a value's type changed, in JSON or in a spreadsheet cell | a driver returns native numbers now | cast the value in the code |
| `number-literal` | the same number written differently, such as 1.0 for 1 | encoder or driver change | fix the encoder |
| `value` | a JSON value changed | a real change, a time, a random ID | a body rule for a value that changes by itself |
| `format` | a value an allowlist rule covers no longer matches its format | the value's format changed | fix, or update the rule |
| `encoding` | the same values in other bytes: layout, escapes, byte order mark, charset | pretty printing, `\/`, a byte order mark, another charset | fix the encoder |
| `body` | a body that is not JSON changed, or JSON became non-JSON | an error page, a text line, CSV quoting | a `$raw` rule for a whole page that changes by itself |

An array rule's `key` only pairs the items: a changed order is still an
`item-order` drift. A `format` drift can come from any value a rule
covers: a header, a JSON value, an HTML text or attribute, or a cell.

## HTML pages

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `element-missing` | an HTML element is gone | template change | — |
| `element-extra` | an HTML element is new | template change | — |
| `element-order` | the same HTML elements in another order | template change | — |
| `attribute-missing` | an attribute of an HTML element is gone | template change | — |
| `attribute-extra` | an attribute of an HTML element is new | template change | — |
| `attribute-value` | an attribute's value changed | template change, a nonce, an asset hash | a body rule with `parts` for a hash that changes by itself |
| `text` | the text of an HTML element changed | content or template change | — |
| `doctype` | the document type declaration came, went or changed | template change | — |

## Spreadsheets and CSV files

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `cell` | a spreadsheet or CSV cell's value changed | export change | a body rule at `xlsx:<sheet>!<cell>` or `csv!<cell>` if it changes by itself |
| `cell-format` | a cell's number format or style changed | export library change | — |
| `cell-formula` | a cell's formula changed | export library change | — |
| `sheet-missing` | a worksheet is gone | export change | — |
| `sheet-extra` | a worksheet is new | export change | — |
| `sheet-order` | the worksheets are in another order | export change | — |

A renamed worksheet gives both `sheet-missing` and `sheet-extra`. A change
in a workbook outside its cells is a `body` drift.

## Command steps and repeated requests

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `exit-code` | a command step ended with another exit code | the command fails now, or fails no more | — |
| `repeat` | the statuses of a repeated request changed | a rate limit changed | — |

## Data

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `side-effect` | the request changed other data than in the baseline | a new write, or a `GET` that writes | — |

A side effect names what changed, not the values, so it cannot be
expected. Side effects are compared only when both captures took
fingerprints of the [state](state.md).

## Requests not captured

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `not-captured` | the baseline has a response, the capture has none | the capture did not send the request (`--only`) | capture again; accept or expect it by its ID |

A request that was sent and got no answer is a `status` drift, not this
one.

---
**Related:** [Glossary: drift](../glossary.md#drift) · [allowlist.yaml](allowlist-yaml.md) · [accept](commands/review.md#driftgate-accept) · [expect](commands/review.md#driftgate-expect)
