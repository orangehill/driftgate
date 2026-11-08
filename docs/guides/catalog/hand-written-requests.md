# Write catalog requests by hand

> **Before you start:** have a generated catalog ([make the catalog cover every route](coverage.md)). **Time:** 10 minutes per request.

`generate` cannot make every flow: a login, an upload with a real file, a
queued job, a rate limit. Write those in `.driftgate/catalog.json` by hand.

## Add a request

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "get-api-orders", "method": "GET", "path": "/api/orders?status=open", "identity": "admin", "storage": "private"}
 ]
}
```

Every request has an `id` and its `storage`: `public` or `private`.
`identity` sends it with that user's token. Run `driftgate run`: a new
request is skipped until you `accept` its first answer
([request fields](../../reference/catalog-json.md#request-fields)).

## Log in first, then use the session

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "post-api-auth-login", "method": "POST", "path": "/api/auth/login",
   "body": {"email": "admin@shop.example", "password": "secret"}, "mutates": true, "storage": "private"},
  {"id": "get-api-me", "method": "GET", "path": "/api/me", "storage": "private"}
 ],
 "sequences": [
  {"id": "login", "restore": "before", "requests": ["post-api-auth-login", "get-api-me"]}
 ]
}
```

A sequence runs its requests in order, together. `restore: "before"`
starts it on restored data
([sequences](../../reference/catalog-json.md#sequences)). DriftGate keeps
no cookies between requests, so a session that lives in a cookie needs a
token instead ([logins](logins.md)).

## Use a value from an earlier response

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "post-api-orders", "method": "POST", "path": "/api/orders", "body": {"sku": "A1"}, "mutates": true, "storage": "private"},
  {"id": "get-api-orders-id", "method": "GET", "path": "/api/orders/{id}",
   "params_from": {"id": {"request": "post-api-orders", "path": "$.data.id"}}, "storage": "private"}
 ],
 "sequences": [
  {"id": "order", "requests": ["post-api-orders", "get-api-orders-id"]}
 ]
}
```

`params_from` fills a path parameter from an earlier response in the
same sequence. Use it instead of a fixed id, which may be a real record
on another copy of the data.

## Upload a file

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "import-csv", "method": "POST", "path": "/api/imports", "mutates": true, "storage": "public",
   "multipart": {"fields": [["kind", "orders"]], "files": [{"field": "file", "synthetic": "csv"}]}}
 ]
}
```

A `multipart` body sends form fields and files. `synthetic` sends a small
valid file of a kind (`csv`, `xlsx`, `pdf`, `png` …). `path` sends a real
file from the uploads folder outside the repository, named by a keyed
hash ([multipart](../../reference/catalog-json.md#multipart)).

## Test a rate limit

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "search-throttle", "method": "GET", "path": "/api/search?q=shoes", "repeat": 61, "storage": "public"}
 ]
}
```

`repeat` sends the request N times on restored data, and compares the
last response and all statuses: `200×60, 429×1` ([repeat](../../reference/catalog-json.md#repeat)).

## Run a queue worker or another command

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "queue-work", "command": ["php", "artisan", "queue:work", "--stop-when-empty"], "output": true, "storage": "public"}
 ]
}
```

A command step runs a program instead of sending a request, such as a
queue worker inside a sequence. Its exit code and side effects are
compared, and its output with `"output": true`
([command steps](../../reference/catalog-json.md#command-steps)).

## Mark a request that only reads

`"mutates": false` on an entry: `{"id": "post-api-search", "method": "POST", "path": "/api/search", "mutates": false, …}`.

A `POST` counts as a request that changes data, so it runs after the
read-only ones. This says it only reads, such as a search sent as `POST`. For a generated request, say it under
[`requests:`](../../reference/driftgate-yaml.md#requests) in `driftgate.yaml`.

## Compare a request only as far as its types

`"level": "types"` on an entry compares keys and types, not values, for this request
only ([level](../../reference/driftgate-yaml.md#level)).

## Measure a request without comparing it

`"observe": true` on an entry measures the status, time and size, and never fails the
check. Use it for an answer that depends on timing or a cache
([observe](../../reference/driftgate-yaml.md#observe)).

---
**Next:** [Send requests as a signed-in user](logins.md): tokens for each identity.
**Related:** [catalog.json](../../reference/catalog-json.md) · [How a capture runs](../../reference/capture.md) · [Make the catalog cover every route](coverage.md)
