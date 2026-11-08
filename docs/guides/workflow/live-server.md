# Check a live server without writing to it

> **Before you start:** have a catalog ([set up your project](../../start/your-project.md)). **Time:** 20 minutes.

You check a server that must not change, such as production or a new
server before the switch. You capture before and after a change, and
compare.
DriftGate then sends only requests that do not change data.

## Configure a server that must not change

<!-- check: config-fragment -->
```yaml
version: 1
project: shop
level: types
target:
  base_url: https://shop.example
  allow_hosts: [shop.example]
```

- Leave out `state:`: nothing is then restored, seeded or fingerprinted on the server.
- Leave out `prepare:` and `tokens.command`: they would run as configured, and may write. `--read-only` is refused with `prepare:`.
- Use `level: types`: the data on a live server is not under your control. A pass then means the contract is unchanged, not the behaviour ([which level to use](../../concepts/how-it-works.md#which-level-to-use)).
- Add allowlist rules for the fields that change by themselves.

## Capture before and after a change

```sh
driftgate capture live-before --read-only
# then make the change on the server
driftgate capture live-after --read-only
driftgate compare live-after --against capture:live-before
```

`--read-only` sends only `GET`, `HEAD` and `OPTIONS` requests, and those
marked `mutates: false`. Requests that change data are never sent, so
`--allow-remote-mutations` is not needed.

- A server outside the local and private ranges needs its address in `allow_hosts`, or the captures need `--allow-remote`.
- Another developer's traffic on a shared server shows as drift in the data the responses hold.
- A side effect is not recorded without `state:`.

What `--read-only` leaves out is under
[capture only the requests that do not change data](../../reference/commands/check.md#capture-only-the-requests-that-do-not-change-data).

## Reach a new server before DNS points to it

<!-- check: config-fragment -->
```yaml
target:
  base_url: https://shop.example
  connect_to: 203.0.113.10        # the new server, whatever DNS says
  allow_hosts: [203.0.113.10]
```

`connect_to` sends every request to that address, and keeps the host
name for the `Host` header and TLS. `allow_hosts` then names the address
connected to ([connect_to](../../reference/driftgate-yaml.md#connect_to)).

## Send requests that need a login

<!-- check: config-fragment -->
```yaml
identities:
  admin: { env: DG_TOKEN_ADMIN }
```

A login usually writes, such as a last-login time or a session row. So do
not let DriftGate log in on such a server. Give each identity a token
issued beforehand, such as an API or personal access token, from the
environment or a password manager
([identities and tokens](../../reference/driftgate-yaml.md#identities-and-tokens)).

## Add the requests the frontend sends itself

```sh
driftgate record --write        # on a copy where writes are allowed
```

A page may load data with its own requests: a filter, a search, a
panel's widgets. It is checked only if those requests are in the catalog. Use the frontend once through
[`driftgate record`](../catalog/record.md) on a copy where writes are allowed, and
add what it proposes. A green result says nothing about a request the
catalog does not have.

---
**Next:** [Read a drift report](../review/read-the-report.md): what the comparison shows.
**Related:** [driftgate capture](../../reference/commands/check.md#driftgate-capture) · [connect_to](../../reference/driftgate-yaml.md#connect_to) · [What a green result proves](../../concepts/what-a-pass-proves.md)
