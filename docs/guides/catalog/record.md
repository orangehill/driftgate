# Add the requests a browser or frontend sends

> **Before you start:** have the application running and a catalog ([set up your project](../../start/your-project.md)). **Time:** 20 minutes.

A frontend loads data with requests of its own, such as a filter, a
search or a panel's widgets. They are checked only if they are in the
catalog. `driftgate record` puts a proxy in front of the application, and
proposes a catalog request for every distinct request it sees.

## Record a session

```sh
driftgate record                       # listens on http://127.0.0.1:8088
```

Use the application through the proxy, then stop with Ctrl-C. `record`
lists the new entries, the route each reaches, and which routes were
uncovered before:

```
  rec-get-api-users-id  GET /api/users/{id}  (route GET /api/users/{id}, uncovered before, private)
  rec-get-api-me  GET /api/me  (route GET /api/me, identity admin, private)
```

Each new request is printed as it is seen. Requests still in flight get
two seconds after Ctrl-C or SIGTERM, and open streams are cut.

## Point a browser, frontend or E2E suite at the proxy

- **A browser:** open `http://127.0.0.1:8088/` and use the application.
- **A frontend in development:** set its API base URL to `http://127.0.0.1:8088`, such as `NEXT_PUBLIC_API_URL` or `VITE_API_URL`.
- **An E2E suite** (Playwright, Cypress): set its base URL, such as Playwright's `use.baseURL`, and run it while `record` runs.

Every request goes to `target.base_url`; with several services, pass
`--target`. The response comes back unchanged, and a redirect to the
application is pointed back at the proxy. Open the proxy by its address,
not by a name such as `shop.test` that resolves to 127.0.0.1.

## Add the recorded requests to the catalog

```sh
driftgate record --write               # use the application, then Ctrl-C
driftgate run                          # captures the new requests too
```

Nothing is written until you ask: `--write` adds the new entries to
`catalog.json` on stop. Existing entries never change, and a request the
catalog already has is not proposed again. Recording only proposes
requests: `capture` still replays the catalog on restored data.

Requests are told apart by method, route, query parameter names, identity
and body shape. So `/api/users/1` and `/api/users/2` are one entry, and a
poll with a new `?since=` each time is one entry too.

- A request that carries a credential or personal data is left out and reported, to add by hand: a login, typically. The report names it by method and path, with a token or personal data in the path shown as `{token}` or `{personal}`.
- A request signed in with an unknown token gets `generate.identity`, or is marked "signed in; set identity". The token itself is never written.
- An uploaded file becomes a synthetic file of the same type; its bytes and name are never written.
- An entry with an identity, or whose response held personal data, is private.

The rules are under [what becomes a catalog entry](../../reference/commands/catalog.md#what-becomes-a-catalog-entry).

## Record only some paths

```sh
driftgate record --include '/api/*' --exclude '/api/health*'
```

Static files, the noise of development servers, event streams and
websockets pass through unrecorded already. Requests to other hosts, such
as analytics, never reach the proxy. `--include` and `--exclude` narrow
it further by path. A request body over 16 MiB passes through and is
recorded without its body.

## Import a HAR file instead

```sh
driftgate har import ~/Downloads/admin-session.har            # list
driftgate har import ~/Downloads/admin-session.har --write    # add to the catalog
```

A HAR file saved from the browser's developer tools gives the same
entries as recording the same session
([driftgate har import](../../reference/commands/catalog.md#driftgate-har-import)).

## Sign in through the proxy

Sign in through the proxy like any user. The proxy speaks plain HTTP to
the browser. A cookie the application sets for its own domain, or only
for HTTPS, may then not be kept for `127.0.0.1`. Then point the application's
session cookie settings at the local address while you record.

- The proxy listens on loopback only. `--allow-non-loopback` allows another address, and then anyone who reaches it uses the application as you.
- With `--listen 0.0.0.0` it answers for any IP address of the machine, never for a host name.
- On another address, it records only requests a browser marks as your own; the rest is forwarded, counted, and not recorded. Record those on `127.0.0.1`.
- A link from another site, or a sign-in provider sending the browser back, passes through and is not recorded.

What the proxy refuses, and why, is under
[driftgate record](../../security.md#driftgate-record).

---
**Next:** [Check what the application does besides answering](side-checks.md): commands, settings, files, mail.
**Related:** [driftgate record](../../reference/commands/catalog.md#driftgate-record) · [Security: driftgate record](../../security.md#driftgate-record) · [Make the catalog cover every route](coverage.md)
