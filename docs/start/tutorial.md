# Tutorial: your first baseline

> **Before you start:** [install DriftGate](install.md), and have a checkout of this repository with Go. **Time:** 15 minutes.

You make a baseline of a demo API: its stored answers, which every later
run is compared with. Then you change the API, and see DriftGate report
it. The output is real; long paths are shortened to `…`.

## Start the demo application

```sh
go build -o /tmp/demoserver ./tools/demoserver      # in the checkout of this repository
/tmp/demoserver -addr 127.0.0.1:8920 -version v1 &  # any free port; use it in base_url and restore
```

## Create the project

```sh
mkdir ~/driftgate-demo && cd ~/driftgate-demo     # outside the checkout
driftgate init --project shop
driftgate key create
```

```console
$ driftgate init --project shop
created .driftgate/driftgate.yaml, .driftgate/catalog.json and .driftgate/allowlist.yaml
no known project type found; add routes under discover: or write the catalog by hand
...
Everything DriftGate keeps is in .driftgate/, which git ignores (.gitignore).
```

`key create` makes the key that encrypts [private bodies](../glossary.md#private-body)
([where the files are](../concepts/project-layout.md)).

## Tell DriftGate about the demo

The demo is not a framework `init` knows. In `.driftgate/driftgate.yaml`,
set `base_url: http://127.0.0.1:8920` and `mode: evolve` (so changes can
be accepted), and add:

<!-- check: config-fragment -->
```yaml
state:
  api:                       # the demo keeps its data in memory; its reset is the restore
    restore: ["curl", "-sf", "-X", "POST", "http://127.0.0.1:8920/admin/reset"]
discover:
  - adapter: command         # any command that prints the routes as JSON
    command: ["cat", "routes-src.json"]
params:
  - name: id
    value: "12"
```

Save the routes as `routes-src.json` in `~/driftgate-demo`, where you run the commands:

<!-- check: routes-json -->
```json
[
 {"method":"GET","path":"/api/news"},
 {"method":"GET","path":"/api/news/{id}"},
 {"method":"POST","path":"/api/faq","body":{"q":"?"}}
]
```

## Try to make the baseline

```sh
driftgate run
```

The first `run` finds the routes, writes the catalog, and captures twice
on the same restored data. It makes the baseline only when both agree,
and here they do not:

```console
$ driftgate run
...
2 runs on the same state gave different responses.
Proposed allowlist entries (check every format and write the reason):
body:
  - request: 'post-api-faq'
    path: '$.entry.created_at'
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: TODO — differs between captures of the same state

no baseline was made: add the proposed allowlist entries (with reasons) and run again
```

## Allow a value that changes by itself

The demo stamps a new FAQ entry with the current time, so
`$.entry.created_at` differs on every capture. In
`.driftgate/allowlist.yaml`, replace `body: []` with the proposed entry
and a real reason. The file then reads:

<!-- check: allowlist -->
```yaml
version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response; changes with every request.
body:
  - request: post-api-faq
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Creation time of the new FAQ entry; the demo sets it to the current time.
```

The value may change now, but only within its format:

```console
$ driftgate run
...
2 runs on the same state gave the same responses ✓

== baseline ==
baseline local made from two equal captures: …/driftgate-demo/.driftgate/baselines/local
```

## Change the application and see the drift

Restart the demo as `v2`, which changes its answers, and run again
(exit code 1: drift):

```console
$ kill %1; /tmp/demoserver -addr 127.0.0.1:8920 -version v2 &
$ driftgate run
...
DRIFT  get-api-news-id
         header-extra    header:x-powered-by   → ["demo"]
         key-extra       $.entry.meta   → (private)
         type            $.entry.id  string → int
         value           $.entry.published_at  (private) → (private)
...
capture:run-b299c63a4c38 against baseline:local: 3 compared, 0 passed, 3 drifted, 0 skipped, 0 observed
values of private bodies are hidden as (private); driftgate review --show-private shows them on this machine
```

Each line names a [drift kind](../reference/drift-kinds.md), the place,
and the old and new value. Generated requests keep their bodies private.

## Accept an intended change

Say version 2 is right. `review` lists the drift groups and the `accept`
command; `accept` takes the new answers into the baseline, with a reason:

```console
$ driftgate accept 'get-api-*' post-api-faq --reason "v2 returns numeric ids and a meta object" --ref DEMO-1
accepted 3 requests into baseline local: get-api-news, get-api-news-id, post-api-faq
...
logged in .driftgate/baselines/local/acceptances.jsonl
$ driftgate run
...
No drift ✓
```

The reason is kept in the acceptance log. Stop the demo with `kill %1`.

## What you learned

- A [baseline](../glossary.md#baseline) holds the answers every later run is compared with.
- Every capture starts from the [same data](../concepts/how-it-works.md#why-every-capture-starts-from-the-same-data).
- A value that changes by itself gets an [allowlist rule](../glossary.md#allowlist-rule) with a format and a reason.
- A [drift](../glossary.md#drift) is fixed in the code, or [accepted](../glossary.md#accept) when it is intended.
- Nothing of this went into git: the [local layout](../concepts/project-layout.md#the-local-layout-and-the-git-layout) keeps it on your machine.

---
**Next:** [How DriftGate works](../concepts/how-it-works.md): the ideas behind what you just did.
**Related:** [Drift kinds](../reference/drift-kinds.md) · [driftgate run](../reference/commands/check.md#driftgate-run)
