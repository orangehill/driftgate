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

"Fix" in the tables means change the code until the old answer comes back.
"Accept" means take the new answer into the baseline with
[`driftgate accept`](../commands.md#accept), and "expect" means record it
for a ticket with [`driftgate expect`](../commands.md#expect).

## Status and headers

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `status` | the HTTP status changed, or the request got no answer | a validation or authorization rule changed | fix, accept or expect |
| `header-missing` | a header of the baseline is gone | middleware or the server stack changed | fix, accept or expect |
| `header-extra` | a header is new | middleware or the server stack changed | fix, accept or expect |
| `header-value` | a header's value changed | another server stack, cookie or cache settings | fix; a header rule if it may change |

## JSON bodies

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `key-missing` | a key, or an array item paired by key, is gone | serializer or resource change | fix, accept or expect |
| `key-extra` | a key, or an array item paired by key, is new | serializer or resource change | fix, accept or expect |
| `key-order` | the same keys in another order | serializer change | fix, accept or expect |
| `array-length` | an array has more or fewer items | data or query change | fix; an `any_length` rule if it may change |
| `item-order` | the same items of an array in another order | another database server or ORM; no `ORDER BY` | fix the order; an array rule with `key` or `any_order` |
| `type` | a value's type changed, in JSON or in a spreadsheet cell | a driver returns native numbers now | cast the value in the code |
| `number-literal` | the same number written otherwise, such as `1.0` for `1` | encoder or driver change | fix the encoder, accept or expect |
| `value` | a JSON value changed | a real change, a time, a random ID | fix; a body rule if it may change |
| `format` | a value an allowlist rule covers no longer matches its format | the value's format changed | fix, or update the rule |
| `encoding` | the same values written with other bytes | pretty printing, `\/`, a byte order mark, another charset | fix the encoder, accept or expect |
| `body` | a body that is not JSON changed, or JSON became non-JSON | an error page, a text line, CSV quoting | fix; a `$raw` rule for a whole page |

A `format` drift can come from any value a rule covers: a header, a JSON
value, an HTML text or attribute, or a cell. The rules are in
[allowlist.yaml](../configuration.md#allowlistyaml).

## HTML pages

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `element-missing` | an HTML element is gone | template change | fix, accept or expect |
| `element-extra` | an HTML element is new | template change | fix, accept or expect |
| `element-order` | the same elements in another order | template change | fix, accept or expect |
| `attribute-missing` | an attribute of an element is gone | template change | fix, accept or expect |
| `attribute-extra` | an attribute of an element is new | template change | fix, accept or expect |
| `attribute-value` | an attribute's value changed | template change, a nonce, an asset hash | fix; a body rule with `parts` for hashes |
| `text` | the text of an element changed | content or template change | fix; a body rule if it may change |
| `doctype` | the document type declaration came, went or changed | template change | fix, accept or expect |

## Spreadsheets and CSV files

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `cell` | a cell's value changed | export change | fix; a cell rule if it may change |
| `cell-format` | a cell's number format or style changed | export library change | fix, accept or expect |
| `cell-formula` | a cell's formula changed | export library change | fix, accept or expect |
| `sheet-missing` | a worksheet is gone, or was renamed | export change | fix, accept or expect |
| `sheet-extra` | a worksheet is new | export change | fix, accept or expect |
| `sheet-order` | the worksheets are in another order | export change | fix, accept or expect |

A change in a workbook outside its cells is a `body` drift.

## Command steps and repeated requests

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `exit-code` | a command step ended with another exit code | the command fails now, or fails no more | fix, accept or expect |
| `repeat` | the statuses of a repeated request changed | a rate limit changed | fix, accept or expect |

## Data

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `side-effect` | the request changed other data than in the baseline | a new write, or a `GET` that writes | fix, or accept |

A side effect names what changed, not the values, so it cannot be
expected. Side effects are compared only when both captures took
fingerprints of the [state](../configuration.md#state).

## Requests not captured

| Kind | Meaning | Typical cause | Typical fix |
|---|---|---|---|
| `not-captured` | the baseline has a response, the capture has none | the capture did not send the request (`--only`) | capture again; accept it by its ID to drop it |

A request that was sent and got no answer is a `status` drift, not this
one.

---
**Related:** [Glossary: drift](../glossary.md#drift) · [allowlist.yaml](../configuration.md#allowlistyaml) · [accept](../commands.md#accept) · [expect](../commands.md#expect)
