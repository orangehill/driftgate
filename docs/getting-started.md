# Getting started

## Install

DriftGate is one static binary for Linux, macOS and Windows, on amd64 and
arm64. Download a release with its checksums:

```sh
gh release download v1.0.2 --repo orangehill/driftgate \
  --pattern driftgate-linux-amd64 --pattern 'SHA256SUMS*'
```

Take the file for your system: `driftgate-<os>-<arch>`, with `.exe` on Windows.

### Verify the download

Verify the signature of `SHA256SUMS`, then the binary, then install it:

```sh
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/orangehill/driftgate/.github/workflows/release.yml@refs/tags/v1.0.2 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS      # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
sudo install -m 0755 driftgate-linux-amd64 /usr/local/bin/driftgate
driftgate version
```

Build provenance and SBOMs: [release verification](security.md#release-verification).

### Other ways to install

- From source, with the Go version `go.mod` names: `go install github.com/orangehill/driftgate/cmd/driftgate@latest`.
- Docker: `docker build -t driftgate .` in a checkout. The commands `driftgate.yaml` runs must exist in the image.

## The demo in 5 minutes

The repository has a demo API. Build and start it:

```sh
git clone https://github.com/orangehill/driftgate && cd driftgate
go build -o /tmp/demoserver ./tools/demoserver
/tmp/demoserver -addr 127.0.0.1:8920 -version v1 &
mkdir ~/driftgate-demo && cd ~/driftgate-demo
driftgate init --project shop && driftgate key create
```

In `.driftgate/driftgate.yaml`, set `base_url: http://127.0.0.1:8920` and
`mode: evolve`, and add:

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

Save the routes as `routes-src.json` in `~/driftgate-demo`:

<!-- check: routes-json -->
```json
[
 {"method":"GET","path":"/api/news"},
 {"method":"GET","path":"/api/news/{id}"},
 {"method":"POST","path":"/api/faq","body":{"q":"?"}}
]
```

Run `driftgate run`. It captures twice, and the two captures differ in
`$.entry.created_at`, a timestamp. It proposes an allowlist rule instead
of a baseline. In `.driftgate/allowlist.yaml`, replace `body: []` with that
rule and a reason:

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

`driftgate run` now makes the baseline. Restart the demo as `v2` and run again:

```console
$ kill %1; /tmp/demoserver -addr 127.0.0.1:8920 -version v2 &
$ driftgate run
DRIFT  get-api-news-id
         header-extra    header:x-powered-by   → ["demo"]
         key-extra       $.entry.meta   → (private)
         type            $.entry.id  string → int
...
kinds: header-extra 3, key-extra 3, type 3, value 3
```

Exit 1: drift. If version 2 is right, accept it with a reason:

```sh
driftgate accept 'get-api-*' post-api-faq --reason "v2 returns numeric ids" --ref DEMO-1
driftgate run            # No drift ✓
kill %1
```

## Your own project

### Run setup

Start the old version of your application and its database, then:

```sh
driftgate setup
```

Setup reads the framework, database and address from the project's
settings, such as `.env`. It writes `.driftgate/` and the key, and, after
asking, the pristine copy. Then it makes the first baseline.

- `--yes` answers every question with the best choice, and stops where a value would be guessed.
- The mode is `freeze`: nothing can be accepted. Set `mode: evolve` to accept intended changes.
- A second `driftgate setup` goes on where the first stopped.

After every upgrade step, `driftgate run` compares with this baseline.

### When setup stops

Setup stops with an instruction and the command that goes on. Other messages are in [troubleshooting](troubleshooting.md).

| Stop | What to do |
|---|---|
| the application does not answer | start it, or pass `--url` |
| the database user may not create the pristine copy | grant what setup prints ([rights](databases.md#what-every-database-needs)) |
| the two first captures differ | add the proposed allowlist rules with reasons, then `driftgate run` |
| a question without a terminal | pass `--yes`, or the flag it names |
| several database settings disagree | run setup at a terminal to choose |

### Step by step with init

`driftgate init` writes only the configuration, with the database it
found commented out. Uncomment `state:` first: without it, `run` refuses
requests that change data.

```sh
driftgate init --project shop    # .driftgate/driftgate.yaml, catalog.json, allowlist.yaml
driftgate key create             # the project key
driftgate run                    # routes, catalog and the first baseline
```

## What to commit

| Layout | Commit | Never commit |
|---|---|---|
| `baselines: local` (default) | nothing | `.driftgate/`, which git ignores |
| `baselines: git` | `driftgate.yaml` and `driftgate/` | the key, the local store |

Use `local` to check upgrade steps on your machine. Use `git` when a team
or CI shares the baseline: `driftgate baselines git` moves the files
([layouts](configuration.md#where-the-files-are)).
