# How responses are compared

What a comparison checks in each part of a response, and what it takes
for the same. The kinds of drift it reports are in
[Drift kinds](drift-kinds.md).

## Comparison levels

The [level](driftgate-yaml.md#level) says how far a comparison goes;
the levels and what each adds are listed there.

- The exit code of a command step, the statuses of a repeated request, and whether a request was captured are compared at every level.
- Headers, bodies and side effects are compared from `shape` on.
- Below `types`, a changed type is a drift only when an object or array is on either side.

## Headers

- Header names are compared without letter case.
- Below `values`, only whether a header is there counts; from `values` on, its value too.
- The values of a header sent several times are compared in any order.
- When the body differs, a different `Content-Length` says nothing new, and is no drift.
- A header rule lets a value change within its format, or only in letter case ([header rules](allowlist-yaml.md#header-rules)).

## JSON bodies

Two JSON bodies are compared value by value, at the paths the drifts
name (`$.data[3].id`).

### The bytes of a JSON body

- A JSON body whose whitespace layout changed is an `encoding` drift at `$`. That covers compact and pretty-printed output, another indentation, line ends, and spaces around `:` and after `,`.
- The same encoder's output that only nests deeper is not.
- A byte order mark that came or went, and another encoding (UTF-8, UTF-16, UTF-32), are an `encoding` drift at `$`, whatever else changed.
- A body in UTF-16 or UTF-32 is compared as its text.
- Other bytes that change no value are a drift only when nothing else in the body changed. Examples are keys written with escapes, and `"\u0061"` for `"a"`.
- A value a rule covers, written differently (`"a\/b"` for `"a/b"`), is no drift: the rule covers how it is written. The rest of the body is compared without it.

### Rows in another order

The items of a JSON array are compared by position. In an array that no
rule pairs by key, the same items in another order are one `item-order`
drift at the array, not a drift at every index:

```
$.data: 3 items → same 3 items, other order: 2, 0, 1
```

The list says, for every item now, the baseline item it is; it shows at
most 20. Rows of an `ORDER BY` on a column with equal values, which
another database server or version returns in another order, look like
this.

- The items are equal as the comparison sees them at its level, with the allowlist's rules. What still differs within a pair (a value written differently) is reported at the baseline item's path.
- When all items but one of each side are equal, those two are paired and compared too (`2 of 3 items the same`).
- When more items changed, which went where is ambiguous, and the array is compared by position.
- It stays a drift: the order of a list can matter to a client.
- An [expectation](commands/review.md#driftgate-expect) of the drift holds only for the same items in the same order.

A note names the rule to add, ready to paste under `arrays:`:

- with `key:`, when every item has an `id` (or `uuid`, `_id`, `key`, `slug`, `code`) of its own. That pairs the items, and still reports one that changed, came or went;
- otherwise, an array rule with `any_order: true` and no key, which keeps the items by position and lets the same items come in another order.

### Arrays paired by a key

One item inserted at the front of a list makes every item after it
differ. An [array rule](allowlist-yaml.md#array-rules) pairs the items by
a key instead: a member every item has (`id`), or a path into the item
(`user.id`).

- An item only the baseline has is `key-missing`, and an item only the current response has `key-extra`. Both are at the item's path with its key: `$.data[id=42]`.
- A string key is quoted: `$.data[slug="a-b"]`. A key name other than letters, digits, `_`, `$` and `-` joined by dots is quoted too: `$.data["@id"=42]`.
- An `any_length` body rule for the array lets items come and go, and they are then not reported. There is no `array-length` drift for a keyed array.
- The items both have are compared as usual, at `$.data[id=42].name`.
- A key that is `42` on one side and `"42"` on the other pairs, and the `type` drift is reported at `$.data[id=42].id`.
- A changed order of the items both have is one `item-order` drift at the array, listing the keys (at most 20) in both orders. `any_order: true` allows it.
- Nothing else is ever hidden by an array rule.

When an item has no key, its key is not an integer or a string, or two
items share a key (on either side), the array is compared by position as
without the rule. The report notes it: `notes` of the request in JSON, a
`note:` line in text and markdown.

- Drifts of one key path across items are grouped as one: `$.data[*].name`.
- In a private body, the key of an item is hidden in the report: `$.data[id=(private)]`, `$.data["@id"=(private)]`.

## HTML pages

When both responses are HTML, they are compared byte for byte first, then
element by element, at paths such as `html>body>p@class`.

- The document type declaration that came or went is structure; a changed one counts from `values` on.
- Texts and attribute values are compared from `values` on.
- One inserted element is one difference, not a change of everything after it.
- A value in embedded JSON has a path into it: `html>body>script#__NEXT_DATA__$.props.pageProps.items[1].name: "Pear" → "Plum"`.
- JSON in a page is compared as JSON: `__NEXT_DATA__`, the app router payload (`self.__next_f`), the Nuxt payload (decoded, with key names), and `application/json` or `ld+json` scripts.
- Whitespace between elements does not count, except in `pre`, `textarea`, `script` and `style`.
- Whitespace next to an inline element, between it and a text or another inline element, does count: a browser shows it as a space. `<b>a</b> <i>b</i>` reads "a b", unlike `<b>a</b><i>b</i>`, and `a <b>x</b>` differs from `a<b>x</b>`.
- Such a drift shows the element's content as it reads: `"a <b>"` → `"a<b>"`.
- Inline elements are those a browser lays out in a line. Among them are `a`, `b`, `button`, `code`, `em`, `i`, `img`, `input`, `label`, `select`, `small`, `span`, `strong`, `sub`, `sup`, `svg` and `time`.
- Comments, `&amp;` against `&`, and the order of attributes do not count.
- Paths into the app router payload follow the shape of the React tree, and references such as `$L3` stay text.
- A page the parser refuses, nested deeper than 512 elements, is compared as bytes, and the drift says why.
- Body rules reach a page with an `html` path ([body rules](allowlist-yaml.md#body-rules)).

How Next.js and Nuxt pages are set up is in the
[Next.js and Nuxt guide](../guides/frameworks/nextjs-nuxt.md).

## Spreadsheets and CSV files

An XLSX or CSV response is compared cell by cell, and a drift names the
cell: `xlsx:Users!B2`, with the worksheet by the name the workbook gives
it, or `csv!B2`. A [cell rule](allowlist-yaml.md#spreadsheet-and-csv-cells)
lets a cell's value change.

An XLSX cell has more than its text, and all of it is compared:

- its type, at the `types` level, as a `type` drift. The types are number, string, bool, error and date; a shared, inline and formula string are all `string`. So the number `1` that became the text `"1"` drifts;
- its number format, and another style of the cell (font, fill, borders), as `cell-format`, at the `values` level;
- its formula, as `cell-formula`, at the `values` level;
- an empty cell with a formula or a style of its own, such as a sum that came out empty, or a fill on an empty cell;
- the formatting of the runs of a rich text, such as a bold or colored part of a shared or inline string, as `cell-format`.

The rest of the workbook is compared too: styles, column widths, merged
cells, row heights and the worksheets' settings. A change there is a
`body` drift at `xlsx` that names the first part that differs, also when
a rule let a cell change.

- Not compared: the document properties (`docProps/`: creation time, author, application), and how the ZIP archive is written.
- Rows and cells without their optional reference (`r`) are placed where the standard puts them.

In a CSV:

- quoting, line ends, blank lines and empty fields at the end of a record that differ while the cells are the same are a `body` drift. The line ends count also when a rule let a cell change;
- a byte order mark that came or went is a `body` drift, whatever else changed.

An [expectation](commands/review.md#driftgate-expect) of a drift covers
all of these: they are in its digest.

## Side effects in the database

A side effect is a change a request made to the state. A capture finds it
by comparing [fingerprints](state.md#fingerprints) before and after the
request.

- A `side-effect` drift says that a request changed other items than in the baseline.
- With [`row_tracking`](state.md#database-components), a capture also records which rows and columns each request changes, without values. Their changes are compared too.
- A table the baseline did not track is not compared ([why](state.md#database-components)).
- Excluded and `restore_only` tables are no side effect ([restore_only](state.md#restore_only)).
- Side effects are compared only when both captures took fingerprints of the state.
- A side effect names what changed, not the values, so it cannot be expected.

## Your own URLs on another port

A capture records the origin of every service's `base_url` in its
`meta.json`: `scheme://host[:port]`, lower case, without a default port.

When the two compared sets were captured from different origins,
`compare` (also `run` and `bisect`) takes both origins, and the
`aliases`, as one. Before comparing, they are replaced by a neutral
origin on both sides, in memory only, and a drift shows each side's real
URL. This lets a copy of the stack on another port, or a CI runner, be
compared with a baseline without a drift for every absolute URL the
application builds from the request's `Host`.

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:8081   # a second copy of the application
  aliases: [http://app.test]
```

`aliases` lists other origins under which the application writes its own
URLs, such as `APP_URL` or a second host name. The replacement applies
to:

- `<origin>` followed by anything that does not continue the host or port (`/`, `?`, `#`, a quote, the end…), also escaped as in JSON (`http:\/\/…`);
- `//host[:port]`: a scheme-relative URL, or another scheme such as `ws://`. The scheme is kept, so `http` → `https` is still a drift;
- a bare `host:port`, only when the origin has a port: a bare host name is too common a word (an e-mail domain, a database host).

What is not replaced:

- `localhost:80801`, `a.localhost:8080` and `localhost:8080@evil.test` are not the origin `http://localhost:8080`.
- A URL of any other host, port, scheme or path is compared as it is.
- Nothing is replaced when the origins are the same, or a service was captured on one side only.

Bodies are rewritten for the comparison only: stored captures, baselines
and private bodies are never changed. A set that records no origin at all
is not compared with one that does: `compare` stops (exit 3), and names
the command that captures it again.

The report says when the origins were taken as the same:

```text
origins: http://localhost:8080 ≙ http://localhost:8081 (own URLs compared as the same)
```

## Paths of the project folder

A debug error page or JSON writes the application's folder into its
stack trace, as Laravel does with `APP_DEBUG`. Two checkouts of the
project are in different folders: the main worktree and a git worktree,
or a developer's machine and CI.

A capture records the folder of the project it was made in in
`meta.json`, and its real path when a symbolic link leads there. With
`baselines: git`, that goes into the repository.

### How a folder is recorded

A folder of two names or more in a home folder of these forms is
recorded as `~/…`, which names no user:

- `/Users/<name>`, `/home/<name>`, `/root`, `/var/root`;
- `/mnt/<drive>/Users/<name>` (WSL, also when `$HOME` is elsewhere);
- `C:\Users\<name>`, a name without spaces.

`Users` and `home` match in any letter case, as macOS and Windows open
them. A `~/…` folder stands for that path in any home folder of those
forms, so a baseline made on one machine compares with another user's
capture, or CI's. `/home/runner/work/…` is in the runner's home.

- The home folder itself, and one name in it, are not recorded. Their path names the user, and a record of them would take any path of any home folder, or a URL's, for the project's. Their traces are compared as they are.
- The home folder of the machine that captures (`$HOME`) counts as a person's when it is outside the system's folders, or in their `home` folder, and of two names or more. Examples are `/nfs/home/<name>`, `/data/home/<name>` and `/var/home/<name>`.
- So does the real path of a home of those forms linked elsewhere (`/home/<name>` to `/nfs/home/<name>`). A folder there is recorded as `~/…` too, and `compare` on that machine takes that home for one. On another machine, its traces may then differ, but never hide a difference.
- A home whose name has a space is left out: its folders are not recorded.
- A folder elsewhere is recorded as it is, absolute, and compares on any machine. Examples are a container's `/srv/…`, a home of one name such as `/app`, or a service account's home under `/var`, `/srv`, `/opt` or `/usr`, such as `/var/lib/jenkins/…`.

### How the folders are compared

`compare` replaces each side's own folders by one neutral folder,
`/driftgate-project`, when the two sides were captured in different
folders, or either recorded one as `~/…`, since two homes may hold it.

- A baseline's folders include those `accept` added: `accept` adds the folders of the capture it takes bodies from to the baseline's.
- The folders are replaced in bodies and headers, in memory only.
- They are replaced as written, with `/` escaped as `\/` (PHP's `json_encode`), and with `\` doubled (a Windows path in JSON).
- A `Content-Length` that differs only by them is no drift.
- Only where a path starts (after a quote, white space, a bracket, `=`, `,` or `>`) and a name ends.
- A folder whose name starts with the project's (`shop2` for `shop`), the folder inside another path or URL, or another file or line, still differ.
- A folder of one name (`/app`, as in a container) is not replaced: it is also a URL's path.
- Limits: a relative URL path of the form `/home/<name>/` followed by the project's names, and a body that holds `/driftgate-project` itself, are taken for the project's folder.
- A set made by an earlier release records no folder, and is compared as it is.

---
**Related:** [Drift kinds](drift-kinds.md) · [allowlist.yaml](allowlist-yaml.md) · [Reading a report in detail](reports.md)
