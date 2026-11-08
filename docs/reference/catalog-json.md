# catalog.json

`catalog.json` is the list of requests every capture sends. `generate`
writes entries, and you can write entries by hand.

The file is `.driftgate/catalog.json`, or `driftgate/catalog.json` with
`baselines: git` ([where the project's files are](cli.md#where-the-projects-files-are)).
`driftgate help catalog` lists its keys.

## An example

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {
   "id": "get-api-news-id",
   "method": "GET",
   "path": "/api/news/{id}",
   "params": {"id": "12"},
   "headers": [["accept", "application/json"]],
   "storage": "public"
  },
  {
   "id": "post-api-auth-login",
   "method": "POST",
   "path": "/api/auth/login",
   "body": {"email": "admin@example.com", "password": "secret"},
   "mutates": true,
   "storage": "private"
  },
  {
   "id": "get-api-me",
   "method": "GET",
   "path": "/api/me",
   "identity": "admin",
   "level": "types",
   "storage": "private"
  }
 ],
 "sequences": [
  {"id": "login", "restore": "before", "requests": ["post-api-auth-login", "get-api-me"]}
 ]
}
```

## Request fields

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "get-api-me", "method": "GET", "path": "/api/me", "identity": "admin", "level": "types", "storage": "private"}
 ]
}
```

| Field | Meaning |
|---|---|
| `id` | the request's unique name ([Request IDs](#request-ids)) |
| `method`, `path` | the request (below) |
| `target` | the service, with several services |
| `params` | values of the path parameters |
| `params_from` | a parameter's value from the response of an earlier request in the same sequence |
| `headers` | a list of `[name, value]` pairs, sent in this order (below) |
| `body` | a JSON body |
| `identity` | send with this identity's token |
| `mutates` | the request changes data (below) |
| `storage` | `public` or `private`, required |
| `level` | the comparison level of this request |
| `observe` | measured, never compared |
| `source` | where the entry came from (`route`, `template`, `import`); written by the tool |
| `multipart` | a `multipart/form-data` body ([below](#multipart)); not with `body` |
| `repeat` | send the request N times, 1 to 10000 ([below](#repeat)) |
| `command`, `output`, `output_ignore`, `dir`, `stdin` | a command step instead of a request ([below](#command-steps)) |

- `method` is an HTTP method, written in upper case. `post` is read as `POST`, and none is `GET`.
- `path` starts with `/`, and may hold `{name}` parameters and a query string.
- `params_from` is written `{"<param>": {"request": "<id>", "path": "$.data.id"}}`.
- `headers`: the first of a name replaces DriftGate's own (`User-Agent`, `Accept-Encoding: identity`); more of that name are sent too.
- For a generated entry, say `mutates` under [`requests`](driftgate-yaml.md#requests) in `driftgate.yaml`.

What `mutates` does:

- A request that changes data runs after the read-only ones, one at a time, and needs a state with a restore command.
- With `capture --restore once` (the default, and `run`), the state is restored once before these requests, so each runs on the data the ones before it left. `--restore each` restores before every one ([restore once or before each sequence](capture.md#restore-once-or-before-each-sequence)).
- Without `mutates`, every method other than `GET`, `HEAD` and `OPTIONS` changes data, and capture lists such requests.
- Write `"mutates": false` for a request that only reads, such as a search sent as `POST`. It then runs with the read-only requests on the restored data.

## Request IDs

- An ID is unique, also ignoring case: it names a file, and macOS and Windows ignore case.
- It has letters, digits, `. _ -`, starts with a letter or digit, and does not end with `.`.
- It is not a device name Windows reserves (`con`, `prn`, `aux`, `nul`, `com1`–`com9`, `lpt1`–`lpt9`), also with an extension.
- Variants of one route are separate entries, each with its own ID.

## Sequences

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "post-api-auth-login", "method": "POST", "path": "/api/auth/login", "mutates": true, "storage": "private"},
  {"id": "get-api-me", "method": "GET", "path": "/api/me", "identity": "admin", "storage": "private"}
 ],
 "sequences": [
  {"id": "login", "restore": "before", "requests": ["post-api-auth-login", "get-api-me"]}
 ]
}
```

Sequences run their requests in order, together.

- `restore: "before"` restores the state before the sequence; `capture --restore each` does that for every sequence.
- A request is in at most one sequence.
- DriftGate keeps no cookies between requests: a later request signs in with its `identity`, not with the login's session ([logins](../guides/catalog/logins.md#sessions-with-cookies)).

## multipart

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {
   "id": "import-xlsx",
   "method": "POST",
   "path": "/api/imports",
   "identity": "admin",
   "mutates": true,
   "storage": "private",
   "multipart": {
    "fields": [["kind", "orders"]],
    "files": [{"field": "file", "path": "orders/2025.xlsx", "ref": "…"}]
   }
  },
  {
   "id": "import-synthetic",
   "method": "POST",
   "path": "/api/imports",
   "identity": "admin",
   "mutates": true,
   "storage": "public",
   "multipart": {
    "fields": [["kind", "orders"]],
    "files": [{"field": "file", "synthetic": "xlsx"}]
   }
  }
 ]
}
```

Each file has a `field` and either `path` or `synthetic`.

`path` is a real file, relative to the
[uploads folder](driftgate-yaml.md#uploads), without `.` or `..`.

- The content never enters the repository; `ref` is a keyed hash of it.
- Leave `ref` out the first time: the capture stops (exit 2) and prints the value to set, after you checked it is the right file.
- A changed file stops the capture as a configuration change (exit 2), and a missing one as an environment error (exit 3).
- The file is checked before anything is sent, and read again only when its request is sent, so the files of a capture are not all held in memory.
- A file that changes during the capture fails its request.
- A request with a real file must be `private`, and so is every response after it until the next restore.
- It runs after the read-only requests, one at a time, like a request that changes data, also without `mutates`. It needs a state with a restore.

`synthetic` is a minimal valid file of a kind: `csv`, `gif`, `jpeg`,
`json`, `pdf`, `png`, `txt` or `xlsx`. It is always the same bytes.

Optional keys of a file:

- `name`: the file name sent; default the base name of `path`, or `upload.<ext>`;
- `type`: the part's content type.

The multipart boundary is derived from the content, so the same request
is the same bytes every time.

## repeat

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "search-throttle", "method": "GET", "path": "/api/search?q=shoes", "repeat": 61, "storage": "public"},
  {"id": "search-throttle-json", "method": "GET", "path": "/api/search?q=shoes", "repeat": 61, "storage": "public",
   "headers": [["accept", "application/json"]]}
 ]
}
```

`"repeat": 61` sends the request 61 times in a row. It compares the last
response together with the statuses of all, such as `200×60, 429×1`.

- The sends skip `rate_limits`, and the request runs on a freshly restored state.
- On a service outside its `allow_hosts` (`capture --allow-remote`), a repeated request needs `--allow-remote-mutations`, like one that changes data, and keeps `rate_limits`.

A rate limit is not generated: write a repeated request by hand. Write it
once more with an `accept` header, so the JSON body of a 429 is compared
too.

## command steps

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "import-csv", "method": "POST", "path": "/api/imports", "mutates": true, "storage": "public",
   "multipart": {"fields": [["kind", "orders"]], "files": [{"field": "file", "synthetic": "csv"}]}},
  {"id": "queue-work", "command": ["php", "artisan", "queue:work", "--stop-when-empty"],
   "output": true, "output_ignore": ["\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}"], "storage": "public"},
  {"id": "orders", "method": "GET", "path": "/api/orders", "storage": "public"},
  {"id": "like-throttle", "method": "PUT", "path": "/api/posts/12/like", "repeat": 61, "storage": "public"}
 ],
 "sequences": [
  {"id": "imports", "requests": ["import-csv", "queue-work", "orders"]}
 ]
}
```

A command step runs a program instead of sending a request, such as a
queue worker inside a sequence.

- A step has no `method`, `path`, body, headers, parameters or identity, and is not repeated.
- Its exit code and side effects are compared like a request's.
- Its standard output is compared only with `"output": true`. The text the `output_ignore` regular expressions match is removed first; here, timestamps.
- `dir` is the folder the step runs in, and `stdin` a file given as its standard input. Both are relative to the project folder, as for [seed and prepare](driftgate-yaml.md#commands-with-a-folder-or-standard-input).
- A missing `dir` or `stdin` stops the capture before anything is sent.
- When a step fails, the log shows the last lines it wrote to standard error, never its standard output. That is its body: private, or public but not yet scanned for personal data.
- [Side checks](../guides/catalog/side-checks.md) has recipes for a schema dump, compiler and mail output.

A release before `dir` and `stdin` refuses a catalog that uses them, as
an unknown field ([keys an earlier release refuses](upgrading-driftgate.md#keys-an-earlier-release-refuses)).

## Entries generate writes and entries you write

- `generate` writes entries with a `source.template`.
- It never touches entries written by hand or imported, which have no template.
- The catalog keeps no change by hand to a generated entry: mark it under [`requests`](driftgate-yaml.md#requests) in `driftgate.yaml` instead.

---
**Related:** [driftgate.yaml](driftgate-yaml.md) · [driftgate generate](commands/catalog.md#driftgate-generate) · [How a capture runs](capture.md)
