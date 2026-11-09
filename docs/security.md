# Security model

DriftGate sends requests to a running application, restores its
databases, and runs the project's commands. This page says what it
protects, what it relies on you for, and how to verify a release.

## What a pull request can run in CI

DriftGate checks the code of a pull request, and runs that code's
commands to do it.

### Commands a pull request controls

A pull request can change every command DriftGate runs.

- From `driftgate.yaml` in the checkout: `seed`, `prepare`, guards, restore and fingerprint commands, `bisect` hooks, the `tokens` command, the `command` adapter and the `sql` of `params`.
- From the catalog: its command steps (`"command": [...]`).
- The `laravel`, `symfony`, `rails` and `django` adapters run the repository's own code: `php artisan`, `bin/console`, `bin/rails`, `manage.py`.
- A command step with `"output": true` puts its standard output into the capture. A public one can carry what the command prints into a committed baseline. The personal-data detector scans it like a response body, but it does not know your secrets.

Commands run without a shell; a shell is written explicitly, and shows
in the diff ([how commands are written](reference/driftgate-yaml.md#how-commands-are-written)).

- Data from responses never goes into their arguments.
- They do not receive `DRIFTGATE_KEY`, the variable `key.env` names, or any other variable whose value is the project key (base64 or hex).
- That is defence in depth, not a boundary. A command can still read the key from wherever DriftGate gets it (1Password, the Keychain, a file), so keep the key out of jobs that run pull requests.

### Keep secrets out of pull-request jobs

In CI the project's commands run with the job's environment: its
variables, its token and its network. So:

- Never run DriftGate on `pull_request_target`; use `pull_request`.
- Never put production secrets, production DSNs or the project key (`DRIFTGATE_KEY`) into a job that runs for pull requests.
- The action needs no project key: it makes a random one for each job, and warns when `DRIFTGATE_KEY` is set in a `pull_request` job.
- Treat the `run.log` artifact as unmasked. It holds the output of the project's commands, which GitHub masks only for secrets it knows.

### How commands appear in logs

DriftGate shows a command it runs, or one that failed (`seed:`,
`prepare:`, a command step in `run.log`), with its secrets as `***`.

- Hidden are passwords, secrets, tokens, keys, cookies and signatures, also the password SQL sets (`IDENTIFIED BY '…'`, `PASSWORD '…'`).
- A command with `stdin` and `dir` shows them after it, each a word of its own: `mysql shop < ~/Data/shop.sql (in api)`. So does the error of one that is missing.
- Arguments are never expanded, so a `$` is part of a value. The exception is a script a shell runs (`sh -c "…"`, `pwsh -c`, `cmd /c`, the command `ssh` runs, `$(…)`), where only a bare `$NAME` or `${NAME}` is shown.
- It is a courtesy for the log, made to hide too much rather than too little. What a command prints is not redacted.

### The queries of params

The `sql` of `params` is checked to be a single `SELECT` (or
`WITH … SELECT`). It may hold no statement or function that writes, or
reaches the server's files, programs or other connections. It runs in a
read-only transaction on the pristine copy.

That check is a guard against mistakes, not a boundary against a hostile
query. The boundary is the DSN user
([what the database user needs](reference/state.md#what-the-database-user-needs)).

- Limit the user to the working and pristine databases (for PostgreSQL, with `CREATEDB`). Not a superuser, and not one with MySQL's `FILE` privilege.
- The root user of a compose service is acceptable only for a local, disposable database in a container on this machine. `driftgate setup` prefers it when the application's setting names that service.
- A database on a shared server gets a limited user.
- An SQLite file has no users. The query runs on the pristine file opened read-only, on a connection that lets only reads compile ([queries on SQLite](reference/databases/sqlite.md#queries-for-params)).
- On SQL Server, a read-only database still runs `EXEC xp_cmdshell` and `OPENROWSET`, so more is refused there ([queries on SQL Server](reference/databases/sql-server.md#queries-for-params)).
- The SQL Server login's rights reach further than one database: `dbcreator`, or the narrower grant, can restore any backup file the server can read ([what the login needs](reference/databases/sql-server.md#what-the-login-needs)).
- `dbcreator` can also take offline, restore over or drop any database of the server; the narrower grant reaches only the databases the login owns.
- The optional `VIEW SERVER STATE` (2017, 2019) and `VIEW SERVER PERFORMANCE STATE` (2022) let the login read the SQL text of every session.

### Protect the baselines

Protect the baselines with a CODEOWNERS entry:
`/driftgate/baselines/ @<owner>`.

- `manifest.json` and the hash-chained `acceptances.jsonl` catch hand and accidental edits (`driftgate verify`). So do the chain and seal of a ref's expectations file (`driftgate expect`).
- None of them is a signature. Real protection comes from git history and review: an expectation lets a drift pass.

## Where requests and connections go

### Allowed hosts

Requests go only to hosts in the service's `allow_hosts`; by default,
`localhost` and the local and private addresses
([target and targets](reference/driftgate-yaml.md#target-and-targets)).

- DriftGate checks the address it really connects to, with no second DNS lookup, and every redirect.
- A service outside the list needs `capture --allow-remote`.
- Requests that change data on it, and repeated requests (`repeat:`), need `--allow-remote-mutations` as well, and its `rate_limits` hold for every send.
- Against production, DriftGate sends no request that changes data without these two separate flags. It is meant for a copy of the data: CI, staging, a developer machine.
- Every request and route path must start with `/`, from every source: the catalog, `routes.json`, adapters, HAR import, `record`. The path is appended to the service's address, and `@host/…` would send an identity's token to another host.
- A failed request is recorded without its URL.

### Databases must be copies

Databases under `state:` are restored, which rewrites their tables, so
they must be copies. A database must be on this machine (a local address
or a socket), or on a host some service's `allow_hosts` names.

- A private address alone is not enough, as it may be production.
- Every host the driver may connect to is checked: PostgreSQL `host=`, several hosts, `PG*` variables.
- SQL Server takes one host over TCP. A named instance without its port, a failover partner, named pipes and shared memory are refused.
- Every connection is checked again on the address it really goes to. A name that resolves elsewhere after the check (DNS rebinding) is not reached.
- A database elsewhere needs `allow_remote: true` on that component, so a wrong DSN cannot reach a production server.
- The data module's target database follows the same rule (`data.allow_remote`).
- An SQLite database is a file inside the project, also past symbolic links. A file outside it, on a network share (`\\server\share`) or in memory is refused, and `allow_remote` does not apply ([the file must be on this machine](reference/databases/sqlite.md#the-file-must-be-on-this-machine)).
- An SQLite restore writes inside the file, in one transaction, without firing its triggers or cascading its foreign keys.
- The pristine copy of an SQLite file keeps no rows of excluded tables (sessions, tokens), not even in its free pages.

### driftgate record

[`driftgate record`](reference/commands/catalog.md#driftgate-record)
forwards requests with no sign-in of its own, so whoever reaches it acts
as the developer. It listens on a loopback address only; another one
needs `--allow-non-loopback`.

#### What it forwards to

- It is a reverse proxy to `target.base_url` and nothing else: no `CONNECT`, and an absolute URL still goes to the service.
- It connects only to `allow_hosts`, and refuses to start for a service outside them.

#### Who it answers

- It answers only for its own address. A request whose `Host` is not the listen address gets 421: `127.0.0.1`, `[::1]` or `localhost` with its port, or with `--allow-non-loopback` that host exactly.
- So a web page cannot point its own name at 127.0.0.1 (DNS rebinding) to read the application through the proxy, or plant requests in the catalog.
- A redirect is pointed back at the address the client used, never at a `Host` it sent.
- Pages of other sites are refused (403). That is every browser request marked `Sec-Fetch-Site: cross-site`, except a plain navigation (next).
- So is a request that changes data or opens a websocket with an `Origin` that is not the proxy or another local address.
- A plain cross-site navigation, such as a link or a sign-in provider sending the browser back, is forwarded and not recorded.
- On an address that is not loopback, a browser sends no `Sec-Fetch-Site` over plain HTTP, so a link or an image on another site's page looks like the developer's own request.
- There a request is recorded only when it carries `Sec-Fetch-Site: same-origin` or `none`, or an `Origin` or `Referer` of the proxy's own address. Any other, such as a page of another site, a typed URL, `curl` or an API test suite, is forwarded but not recorded. The summary counts them.
- It serves plain HTTP on purpose. HTTPS from the client would need a local certificate authority to install and trust, a larger risk than the feature is worth.

#### What it keeps

- Nothing it sees is written but the proposed requests.
- `Authorization`, `Cookie` and tokens are only compared in memory with the configured identities.
- A request with a credential or personal data in its query or body is left out and reported. The note names it by its path, without the query, and with personal data or a token in the path masked.
- Uploaded files become synthetic ones.
- Response bodies are read only to scan them for personal data; a finding makes the entry private. Every response of a request is scanned, not only the first.
- A response body that is not text (an image, a PDF) is not read, and counts as personal data.
- A gzip or deflate body is decoded for the scan, within 16 MiB of decoded size.
- A body in another encoding (`br`, `zstd`), one that does not decode, and one over 16 MiB count as personal data, so the entry is private. So does one cut off because the client or the service went away.
- The client gets the body as the service sent it.
- Request and response bodies are held in memory up to 16 MiB per request. A larger request body passes through, and the request is proposed without its body, with a note to add it by hand.
- The request rate is not limited: the proxy is local and as fast as the service.

### No telemetry

No telemetry and no update checks. The only network traffic goes to the
services, and to whatever the project's own commands contact.

## Secrets

### Database settings and their TLS

Database DSNs, tokens and keys come only from environment variables or
1Password (`{ env: NAME }`, `{ op: "op://…" }`), never from the
configuration file itself.

A DSN can also come from the file where the project already keeps it,
such as `{ dotenv: .env }` or `{ appsettings: … }`
([database connection settings](reference/database-settings.md)).

- `driftgate.yaml` holds only the file's name and key. The file is read when a command runs, and nothing of it is copied or written anywhere.
- The file must be inside the project, also past symbolic links. Only the .NET user secrets of a project's `UserSecretsId` are read from outside it, and never written.
- No project code runs to read a setting: PHP, Python and Ruby (ERB) are read as text.
- A password read from a file is not put into the environment of the commands DriftGate runs.
- A connection string is never read as a URL or the MySQL driver's form, so no part of its password is taken for the user and shown.
- A setting that names another database than `working` (for SQLite, another file) stops the command. A restore never resets a database the application does not run on.
- A setting whose host may be the local socket or TCP needs `host:`; DriftGate does not guess.

The setting's TLS is kept. A TLS setting DriftGate cannot pass on stops
the command, instead of connecting without it.

- SQL Server's TLS is never weaker than the application's client. `Encrypt` and `TrustServerCertificate` are passed on, and `Strict` is never weakened ([DSN and TLS](reference/databases/sql-server.md#dsn-and-tls)).
- The SQL Server sign-in is encrypted even with `Encrypt` off. A server that answers it cannot encrypt is refused before the password is sent.
- A certificate file a setting names by a relative path must be inside the project. An absolute path or `~/` may name a system CA. The file is only read as a certificate, and no message shows what it holds.

No message shows a database DSN, also when it cannot be parsed. Messages
reach the console, `run.log` and pull-request comments.

### Masked headers

`Authorization`, `Cookie` and `Set-Cookie` values are masked in
captures, so that their format can still be checked: small letters
become `x`, capitals `X`, digits `0`. So is any other header named as a
credential:

- with or without `X-`: `X-CSRF-Token`, `X-Api-Key`, `Access-Token`, `Token`, `Api-Key`;
- not the standard `Access-Control-*`, `Public-Key-Pins` or `Signature`.

In `Location`, `Content-Location`, `Link` and `Refresh`, these are
masked too:

- the values of query and fragment parameters named or shaped as credentials: `token=`, `signature=`, `X-Amz-Signature=`, `#access_token=`;
- a fragment shaped as a token;
- path segments shaped as one, as in a reset link.

An allowlist format that a capture proposes takes nothing from a masked
value. The mask keeps a value's length, so a cookie's value is proposed
as text without a `;` of the lengths seen, such as `[^;]{40,44}`. A
cookie emptied or set to `deleted` then fails the format. A header
masked whole is proposed as any text (`.+`).

### Redirects and terminal output

- With `follow_redirects`, a redirect from `https` to `http` is refused. One to another host or port gets neither the identity's token nor the catalog's own headers.
- Text output escapes control characters from responses. A response cannot start a line of its own, where GitHub would read `::warning::` as a workflow command, or send terminal sequences.
- The action also wraps the output of `run` in `stop-commands`.

## Personal data

**Personal data never goes into git, not even encrypted.**

### Private bodies

A response body stored as **private**, the default for generated
requests, is encrypted with AES-256-GCM. It is kept only in the local
store of the machine that captured it, which git never holds
([store](reference/driftgate-yaml.md#store)).

- This holds for baselines too. The committed baseline names a private body only by a keyed hash (HMAC-SHA256, under a key derived from the project key). The repository does not even hold a plain hash of personal data.
- The [project key](reference/driftgate-yaml.md#key) comes from `key:`, `DRIFTGATE_KEY`, or what `driftgate key create` makes. Treat it like a production password; changing it is not supported.
- With `baselines: local`, the key, the store and the baselines are in `.driftgate/`. Before DriftGate writes the store or the key there, it checks that git ignores the folder, and stops (exit 2) when git does not ([baselines](reference/driftgate-yaml.md#baselines)).
- `driftgate baselines git` checks what would enter the repository as `verify` does, and never copies the key file into it.

Where a private body is not available, as in CI or on another machine,
the body is not compared, not even by its hash. The rest of the
committed record (status, headers, side effects) is.

- The request gets the note `private body not on this machine`, is listed in the report, and the result is incomplete (exit 3).
- `--skip-private` accepts a partial check: the exit code comes from what could be compared. A private request that went from 200 to 500 is still drift (exit 1).
- When nothing at all could be compared, the result stays incomplete.

Reports (text, JSON, JUnit, markdown), the last comparison and the
acceptance log show the kind and place of a difference in a private
body, never its values.

- An expectation names the values by a keyed hash.
- An object key that is not a plain name, such as an e-mail address used as a key, is hidden in the path.
- `compare --show-private` shows values on your own machine only, and keeps nothing ([report formats](reference/reports.md#report-formats)).

### The personal-data detector

Every **public** body is scanned for personal data:

- e-mail addresses and phone numbers;
- Serbian personal ID numbers, with the check digit;
- card numbers (Luhn) and IBANs.

It also scans for credentials:

- a JWT anywhere;
- a token-shaped value under a key named as a credential: `access_token`, `refresh_token`, `csrf_token`, `api_key`, `signature`. `hash` alone does not count.
- the `value` of an object whose `name` or `key` is such a name: `{"name":"api_key","value":…}`, AWS's `Key` and `Value`;
- a URL with a credential in its query or fragment, such as a signed or presigned link, or an OAuth redirect.

Outside JSON, the same holds for a form body
(`access_token=…&token_type=bearer`), links anywhere in a text such as
an HTML page, and XML elements and attributes
(`<access_token>…</access_token>`). An HTML page's CSRF meta tag and CSP
nonce are not taken.

A finding stores the body as private, reports it with a masked value,
and stops with exit 2. [`pii.allow`](reference/driftgate-yaml.md#storage-and-pii)
lists values that are not personal, such as a shared support address.

The body is scanned as the repository would hold it:

- decoded from `gzip` or `deflate` first. A catalog request may ask for them with its own `Accept-Encoding`, and gzip data is decoded also without a `Content-Encoding`.
- text from UTF-16 or UTF-32, by its byte order mark or declared charset;
- a workbook (XLSX) part by part;
- text as UTF-8, or with a textual type in a legacy charset that keeps ASCII (`windows-1250`, `ISO-8859-2`).

A public body that cannot be scanned is stored as private, and listed
after the capture ("cannot be scanned for personal data"); the exit code
does not change. Mark such requests `storage: private`. It cannot be
scanned when it is:

- in another encoding, such as `br`;
- an image, a PDF or an archive;
- text with NUL bytes, or invalid UTF-8 with control characters;
- in a charset that changes ASCII, such as `UTF-7`.

`driftgate verify` scans committed baselines the same way: every public
body and every record's headers. Run it in CI next to the gate
([what verify scans](reference/commands/review.md#what-verify-scans-in-a-committed-baseline)).

- A record fails it (exit 1) when it has personal data, a credential, an unmasked credential header, or a public body that cannot be scanned. That happens to a baseline made by an older DriftGate, or one edited by hand and committed with its manifest.
- It names the record, the kind and where, never the value.
- `driftgate verify --baseline <name> --migrate-private` stores such a body private, masks such header values, and marks the requests private in the catalog. The values stay in the history of git.

### Uploaded files

Real files that catalog requests upload stay in an uploads folder
outside the repository ([multipart](reference/catalog-json.md#multipart)).

- The catalog holds only their path and a keyed hash.
- The responses of such a request, and every response after it until the next restore, are stored as private.
- Such a request needs a state with a restore command, and is refused with `--allow-mutating-without-restore`, so the file's rows do not stay in the application after the capture.
- The local store and the folders DriftGate creates are open to their owner only.
- `store:` and `uploads.dir` may not point anywhere inside the git repository of the project ([store](reference/driftgate-yaml.md#store), [uploads](reference/driftgate-yaml.md#uploads)).

### The data module

The data module ([`driftgate data`](reference/commands/data.md)) makes
pseudonymised copies (GDPR, Serbian ZZPL), not anonymous ones: whoever
has the key and the original data can link them.

- Run it only locally, or on your own runner.
- Its output is made only when two independent checks find nothing: the detector over every value and the schema, and a search for every original personal value.

## Release verification

A release is built only from a tag `vX.Y.Z` on a commit of `main`, by
`.github/workflows/release.yml`, after the CI checks of `ci.yml` itself.
It is published from the `release` environment.

The CI checks, which pull requests pass too, are:

- format, vet, staticcheck, `govulncheck` without findings, and fuzzing;
- tests against MariaDB, MySQL and PostgreSQL, and of SQLite on Linux, macOS and Windows;
- tests of SQL Server 2017, 2019, 2022 and 2025, with a check of its TLS against a server with a certificate of a test CA;
- the size report and the package limits of the binaries on the six platforms.

### What a release contains

- `driftgate-<os>-<arch>` (`.exe` on Windows) for linux, darwin and windows on amd64 and arm64: static binaries (`CGO_ENABLED=0`, `-trimpath`, `-s -w`);
- `driftgate-<os>-<arch>.cdx.json`: a CycloneDX SBOM per binary;
- `SHA256SUMS`: the SHA-256 of every binary and SBOM;
- `SHA256SUMS.sigstore.json`: a keyless Sigstore signature of `SHA256SUMS`. It is made with cosign and GitHub OIDC; the certificate names the release workflow and the tag, and the signature is in the public Rekor log.
- SLSA build provenance for every file.

The download and its verification, signature first and then the
checksum, are step by step in
[Install DriftGate](start/install.md#verify-the-download).
The build provenance of a file is verified with:

```sh
gh attestation verify driftgate-linux-amd64 --repo orangehill/driftgate
```

The GitHub Action verifies the signature and the checksum for `version: v1.0.2` or
`version: latest`.

- It sets up a pinned cosign, and verifies the signature with the identity of the action's repository at that tag, and the checksum.
- It installs nothing when either fails.
- `version: source` builds from the action's own source.

### Reproducible builds

The build date is the commit time, and the release workflow builds
every binary twice and stops unless the bytes are the same. Rebuilding a
tag with the Go version from `go.mod` gives the same SHA-256:

```sh
pkg=github.com/orangehill/driftgate/internal/cli
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w \
  -X $pkg.Version=v1.0.2 -X $pkg.Commit=$(git rev-parse HEAD) \
  -X $pkg.Date=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ)" \
  -o driftgate-linux-amd64 ./cmd/driftgate
```

---
**Related:** [driftgate.yaml](reference/driftgate-yaml.md) · [State: restoring the data](reference/state.md) · [Reference](reference/README.md)
