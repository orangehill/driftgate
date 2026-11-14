# Security

DriftGate sends requests to a running application, restores its
databases, and runs the project's commands. This page says what it
protects, and what it leaves to you. Report a vulnerability as
[SECURITY.md](../SECURITY.md) says.

## The project key

The project key encrypts private bodies. It names them, and the row values `row_tracking` records, by a keyed hash.
Treat it like a production password.

| Source, in order | When |
|---|---|
| `key: { env: NAME }` or `key: { op: "op://…" }` | set in `driftgate.yaml` |
| `DRIFTGATE_KEY` | no `key:` |
| `.driftgate/key` | `baselines: local`, made by `driftgate key create` |
| the macOS Keychain | `baselines: git`, made by `driftgate key create` |

- Without the key, private bodies cannot be captured or compared (exit 3), and row values are not recorded.
- Changing the key is not supported: bodies encrypted with the old one become unreadable.
- `.driftgate/key` must be readable by its owner only.
- With `baselines: local`, commands stop (exit 2) when git does not ignore `.driftgate/`.

## Private bodies

Personal data never goes into git, not even encrypted.

| Storage | Where the body is | Checked |
|---|---|---|
| `private` (default for generated requests) | encrypted in the local store of the capturing machine | the committed baseline holds only a keyed hash |
| `public` | in the baseline, in plain text | every body is scanned for personal data and credentials |

- On another machine, or in CI, a private body is not compared. The result is incomplete (exit 3).
- `--skip-private` accepts that partial check. Status, headers and side effects are still compared.
- Reports show the kind and place of a private difference, never its values. `--show-private` shows them locally only.
- The detector finds e-mail addresses, phone numbers, card numbers, IBANs, JWTs and credential-shaped values.
- A finding in a public body makes it private and stops with exit 2. `pii.allow` lists values that are not personal.
- A public body that cannot be scanned (an image, `br` encoding) is stored private.
- `driftgate verify` scans committed baselines the same way. Run it in CI.
- Uploaded files stay in `uploads.dir`, outside the repository. `store:` and `uploads.dir` may not point inside it.
- Credential headers (`Authorization`, `Cookie`, `Set-Cookie`) are masked in captures.

## What a PR job may run

A pull request can change every command DriftGate runs:

- `seed`, `prepare`, guards, restore and fingerprint commands, bisect hooks, `tokens.command`;
- the `command` adapter and the `sql` of `params`;
- the catalog's command steps;
- the framework adapters, which run the repository's code (`php artisan`, `bin/rails`, `manage.py`).

So treat a pull-request job as running untrusted code:

- Use `pull_request`, never `pull_request_target`.
- Never give it production secrets, production DSNs or `DRIFTGATE_KEY`. The action makes a random key per job.
- Commands do not receive the project key, but they can still read it from its source. Keep it out of such jobs.
- Treat the `run.log` artifact as unmasked: it holds the commands' output.
- Protect the baselines with CODEOWNERS: `/driftgate/baselines/ @<owner>`.
- `verify` catches accidental edits, not malicious ones. It is no signature; git review is.

The `params` SQL check is a guard against mistakes, not a security
boundary. The database user's rights are the boundary.

## Remote and live databases

Requests and connections stay local unless you allow more.

| Setting | Allows |
|---|---|
| `target.allow_hosts` | request hosts; default `localhost` and private ranges such as `10.0.0.0/8` |
| `capture --allow-remote` | a service outside `allow_hosts` |
| `--allow-remote-mutations` | with `--allow-remote`: requests that change data, and repeated requests |
| `--read-only` | requests that do not change data: safe methods unless `mutates: true`, and `mutates: false` |
| `allow_remote: true` on a component | a database host that is not local nor in `allow_hosts` |
| `data.allow_remote` | the data module's target on another host |

- A restore rewrites tables. A database under `state:` must be a copy you may reset.
- A private address alone does not prove a copy; it may be production.
- Never point `state:` at a live server. Check a live server with `--read-only` and no restore ([live server](workflows.md#live-server)).
- Limit the DSN user to the working and pristine databases. Not a superuser, and not MySQL's `FILE` privilege.
- SQL Server's `dbcreator` reaches every database on the server ([SQL Server](databases.md#sql-server)).
- `driftgate record` listens on loopback only. Anyone who reaches it acts as you; `--allow-non-loopback` changes that.
- DSNs and tokens come from the environment, 1Password or the project's settings file. No message shows a DSN.

## Release verification

Every release has signed checksums, an SBOM per binary, and build
provenance. Verify the signature of `SHA256SUMS` first, then the binary:

```sh
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/orangehill/driftgate/.github/workflows/release.yml@refs/tags/v1.0.2 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS      # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
```

Verify the build provenance of a file:

```sh
gh attestation verify driftgate-linux-amd64 --repo orangehill/driftgate
```

The GitHub Action verifies the signature and checksum for `version: v1.0.2`
or `version: latest`. It installs nothing when either fails.

Builds are reproducible. Rebuilding a tag with the Go version from
`go.mod` gives the same SHA-256:

```sh
pkg=github.com/orangehill/driftgate/internal/cli
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w \
  -X $pkg.Version=v1.0.2 -X $pkg.Commit=$(git rev-parse HEAD) \
  -X $pkg.Date=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ)" \
  -o driftgate-linux-amd64 ./cmd/driftgate
```
