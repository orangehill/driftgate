# Building the catalog by recording through a proxy

`driftgate record` puts a proxy between a client and the application and
proposes a catalog request for every distinct request it sees. Use it to
build or extend the catalog from real use: you clicking through the
application, a frontend in development, or an existing E2E suite
(Playwright, Cypress) pointed at it. It does not replace `capture`:
recording only proposes requests, and the check still replays the catalog
on a restored state.

## Record a session

```sh
driftgate record                       # listens on http://127.0.0.1:8088
```

Point the client at the proxy instead of the application:

- **a browser:** open `http://127.0.0.1:8088/` and use the application;
- **a frontend in development:** set its API base URL to
  `http://127.0.0.1:8088` (for example `NEXT_PUBLIC_API_URL`, `VITE_API_URL`);
- **an E2E suite:** set its base URL, e.g. Playwright's `use.baseURL`, and
  run it while `record` runs.

Every request goes to `target.base_url` (with several services, pass
`--target`), and the response comes back unchanged; a redirect to the
application is pointed back at the proxy. Each new request is printed as it
is seen. Stop with Ctrl-C (or SIGTERM, e.g. from a script; requests still
in flight get two seconds, open streams are cut): `record` lists
the new entries, the routes of `routes.json` each one reaches, and which of
those routes no catalog request reached before.

```
  rec-get-api-users-id  GET /api/users/{id}  (route GET /api/users/{id}, uncovered before, private)
  rec-get-api-me  GET /api/me  (route GET /api/me, identity admin, private)
```

Nothing is written until you ask: run it again with `--write` to add the
entries to `catalog.json` on stop. Existing entries are never changed, and a
request the catalog already has is not proposed again. Then capture as
usual:

```sh
driftgate record --write               # use the application, then Ctrl-C
driftgate run                          # captures the new requests too
```

## What becomes an entry

The traffic goes through the same pipeline as `driftgate har import`, so a
HAR file of the same session gives the same entries:

- **Routes.** With a route inventory, `/api/users/1` becomes
  `/api/users/{id}` with `params: {id: "1"}`, and `/api/users/2` is the same
  entry. Requests are told apart by method, route, the names of the query
  parameters, identity and the shape of the JSON body, so a poll with a new
  `?since=` each time is one entry.
- **Identities.** A request that carries the token of a configured identity
  (`identities:` or `tokens:`) gets that identity; one signed in with another
  token gets `generate.identity`, or is marked "signed in; set identity".
  The token itself is never written.
- **Credentials and personal data.** A request whose query or body carries a
  credential (a password, a token) or personal data is left out and
  reported, to be added by hand: a login, typically. The report names it by
  method and path, never with its query, and a token or personal data in
  the path is shown as `{token}` or `{personal}`.
- **Uploads.** Form fields are kept; every file becomes a synthetic file of
  the same type. The uploaded bytes and the file name are never written.
- **Storage.** Entries follow `storage:` like `generate`; an entry with an
  identity, or whose response held personal data, is private. Every
  response of the entry counts, not only the first: one poll that returns a
  name makes it private. A gzip or deflate response is decoded for the
  scan; one in another encoding (`br`, `zstd`), over 16 MiB or cut off
  cannot be checked and makes the entry private too.
- **Left out:** static files (scripts, styles, source maps, images, fonts,
  media), the noise of development servers (`/_next/static`, `/_next/image`,
  `/_nuxt`, `/build`, Vite, the favicon), event streams and websockets (they
  pass through and `record` says so). Requests to other hosts, such as
  analytics, never reach a reverse proxy in the first place. `--include` and
  `--exclude` narrow it further by path:

```sh
driftgate record --include '/api/*' --exclude '/api/health*'
```

A request body over 16 MiB passes through and is recorded without its body.
A link from another site (or a sign-in provider sending the browser back)
passes through too, and is not recorded.

## HTTPS and security

The proxy speaks plain HTTP to the client and the scheme of
`target.base_url` (http or https) to the application. HTTPS from the client
is deliberately not supported: it would need a local certificate authority
installed and trusted on the machine, and that is a larger risk than the
feature is worth. A browser or test suite on the same machine works with
`http://127.0.0.1`. Sign in through the proxy: a cookie the application sets
with `Domain=` its own domain, or only for HTTPS, may not be kept for
`127.0.0.1`; then point the application's session cookie settings at the
local address while recording.

- It listens on loopback only; `--allow-non-loopback` allows another
  address, and then anyone who reaches it uses the application as you.
  There a browser sends no `Sec-Fetch-Site` over plain HTTP, so a link or
  an image on another site cannot be told from your own requests: only a
  request with `Sec-Fetch-Site: same-origin` or `none`, or with an `Origin`
  or `Referer` of the proxy's address, is recorded. The rest (the first
  page you type in, `curl`, an API test suite) is forwarded but not
  recorded, and the summary says how many; record those on `127.0.0.1`.
- It answers only for its own address: `127.0.0.1`, `[::1]` or `localhost`
  with its port (with `--allow-non-loopback`, also the `--listen` host, or
  any IP address for `0.0.0.0`). Any other `Host` gets `421 Misdirected
  Request`: a web page that points its own name at 127.0.0.1 (DNS
  rebinding) cannot read the application through the proxy. Open it by
  address, not through a name such as `myapp.test` that resolves to
  127.0.0.1; a redirect comes back to the address you used.
- It refuses requests from other sites with `403`: what the browser marks
  `Sec-Fetch-Site: cross-site` (a link to the proxy still works and is not
  recorded), and a request that changes data or opens a websocket with an
  `Origin` other than the proxy or a local address. Pages served through
  the proxy, and a frontend in development on another local port
  (`http://localhost:3000`), work as usual; so does any client that is not
  a browser.
- It forwards only to `target.base_url`, only if that host is in
  `allow_hosts`; it is not a forward proxy (`CONNECT` is refused).
- It keeps nothing from the responses: they are read only to scan them for
  personal data.
- The request rate is not limited; the proxy is local.

See also [the security model](../security.md).
