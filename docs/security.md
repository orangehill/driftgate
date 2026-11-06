# Security model

DriftGate sends requests to a running application, restores its databases
and runs the project's commands. This page says what it protects, what it
relies on you for, and how to verify a release.

## What a pull request can run in CI

DriftGate checks the code of a pull request, and runs that code's commands
to do it.

- A pull request can change every command DriftGate runs: `seed`,
  `prepare`, guards, restore and fingerprint commands, `bisect` hooks, the
  `tokens` command, the `command` adapter and the `sql` of `params` all come
  from `driftgate.yaml` in the checkout, and the command steps of the
  catalog (`"command": [...]`) from `.driftgate/catalog.json` (`driftgate/catalog.json` with `baselines: git`). A command
  step with `"output": true` puts its standard output into the capture,
  so a public one can carry what the command prints into a committed
  baseline (the personal-data detector scans it like a response body, but
  it does not know your secrets). The `laravel`, `symfony`, `rails`
  and `django` adapters run the repository's own code (`php artisan`,
  `bin/console`, `bin/rails`, `manage.py`).
- In CI those commands run with the job's environment: its variables, its
  token and its network. So:
  - never run DriftGate on `pull_request_target`; use `pull_request`;
  - never put production secrets, production DSNs or the project key
    (`DRIFTGATE_KEY`) into a job that runs for pull requests. The action
    needs no project key (it makes a random one for each job) and warns when
    `DRIFTGATE_KEY` is set in a `pull_request` job;
  - treat the `run.log` artifact as unmasked: it holds the output of the
    project's commands, which GitHub masks only for secrets it knows.
  - DriftGate shows a command it runs or that failed (`seed:`, `prepare:`,
    a command step in `run.log`) with its passwords, secrets, tokens, keys,
    cookies and signatures as `***`, also the password SQL sets
    (`IDENTIFIED BY '…'`, `PASSWORD '…'`); a command with `stdin` and `dir`
    shows them after it (`mysql shop < ~/Data/shop.sql (in api)`), each a
    word of its own, also in the error of one that is missing. Arguments
    are never expanded, so a `$` is part of a value except in a script a
    shell runs (`sh -c "…"`, `pwsh -c`, `cmd /c`, the command `ssh` runs,
    `$(…)`), where only a bare `$NAME` or `${NAME}` is shown. It is a
    courtesy for the log, made to hide too much rather than too little;
    what a command prints is not redacted.
- The `sql` of `params` is checked to be a single `SELECT` (or `WITH …
  SELECT`) without statements and functions that write or reach the
  server's files, programs or other connections, and runs in a read-only
  transaction on the pristine copy. That is a guard against mistakes, not a
  boundary against a hostile query. The boundary is the DSN user: limit it to
  the working and pristine databases (for PostgreSQL with `CREATEDB`), not a
  superuser and not one with MySQL's `FILE` privilege. The root user of a
  compose service is acceptable only for a local, disposable database in a
  container on this machine (what `driftgate setup` prefers when the
  application's setting names that service); a database on a shared server
  gets a limited user. An SQLite file has no users: there the query runs
  on the pristine file opened read-only, on a connection that lets only
  reads compile, without `ATTACH`, `PRAGMA` or the functions and tables
  that reach other files or the file's pages
  ([SQLite](configuration.md#sqlite)). On SQL Server a read-only database
  still runs `EXEC xp_cmdshell` and `OPENROWSET`, so the query is also
  refused with any other statement's keyword (T-SQL needs no `;` between
  statements), `NEXT VALUE FOR`, names of three or four parts (another
  database or server), and the system procedures, functions and views
  that reach files, programs, other servers, sessions or logins
  ([SQL Server](configuration.md#sql-server)). Its login needs
  `dbcreator` and `db_owner` in the working database, not `sysadmin`
  (`sa` only for a local container). `dbcreator` lets the login take any
  database of the server offline, restore over it or drop it; the
  narrower grant (`CREATE ANY DATABASE` and ownership of the working
  database) reaches only the databases the login owns, but changes the
  working database's owner. Either lets it restore any backup file the
  server can read. `VIEW SERVER STATE` (2017, 2019) and `VIEW SERVER
  PERFORMANCE STATE` (2022), optional, let the login read the SQL text of
  every session.
- Commands run without a shell (a shell is written explicitly and shows in
  the diff), data from responses never goes into their arguments, and they
  do not receive `DRIFTGATE_KEY`, the variable `key.env` names, or any
  other variable whose value is the project key (base64 or hex). That is
  defence in depth, not a boundary: a command can still read the key from
  wherever DriftGate gets it (1Password, the Keychain, a file), so keep the
  key out of jobs that run pull requests.
- Protect the baselines with a CODEOWNERS entry
  (`/driftgate/baselines/ @<owner>`): `manifest.json` and the hash-chained
  `acceptances.jsonl` catch hand and accidental edits (`driftgate verify`),
  as the chain and seal of a ref's expectations file
  (`driftgate expect`) do, but they are not a signature. Real protection
  comes from git history and review: an expectation lets a drift pass.

## Where requests and connections go

- **Requests** go only to hosts in the target's `allow_hosts`; by default
  `localhost` and local and private addresses. DriftGate checks the address
  it actually connects to (no second DNS lookup) and every redirect. A
  target outside the list needs `capture --allow-remote`; requests that
  change data on it, and repeated requests (`repeat:`), need
  `--allow-remote-mutations` as well, and its `rate_limits` hold for every
  send.
- **Every request and route path must start with `/`**, from every source
  (catalog, `routes.json`, adapters, HAR import, record): the path is appended to the
  target's address, and `@host/…` would send an identity's token to another
  host. A failed request is recorded without its URL.
- **Databases under `state:`** are restored, which rewrites their tables, so
  they must be copies: on this machine (a local address or a socket) or on a
  host some target's `allow_hosts` names. A private address alone is not
  enough, as it may be production. Every host the driver may connect to is
  checked (PostgreSQL `host=`, several hosts, `PG*` variables; SQL Server
  takes one host over TCP, and a named instance without its port, a
  failover partner, named pipes and shared memory are refused), and every
  connection is checked again on the address it actually goes to, so a
  name that resolves elsewhere after the check (DNS rebinding) is not
  reached. A database
  elsewhere needs `allow_remote: true` on that component, so a wrong DSN
  cannot reach a production server. The data module's target follows the
  same rule (`data.allow_remote`). An SQLite database is a file inside the
  project (also past symbolic links): a file outside it, on a network
  share (`\\server\share`) or in memory is refused, `allow_remote` does not
  apply, and a restore writes inside the file, in one transaction, without
  firing its triggers or cascading its foreign keys. The pristine copy of
  an SQLite file keeps no rows of excluded tables (sessions, tokens), not
  even in its free pages.
- Against production, DriftGate sends no request that changes data without
  two separate flags. It is meant for a copy of the data: CI, staging, a
  developer machine.
- **`driftgate record`** listens on a loopback address only (another one
  needs `--allow-non-loopback`): it forwards requests with no sign-in of its
  own, so whoever reaches it acts as the developer. It is a reverse proxy to
  `target.base_url` and nothing else (no `CONNECT`, an absolute URL still
  goes to the service), connects only to `allow_hosts`, and refuses to start
  for a target outside them. It answers only for its own address: a request
  whose `Host` is not the listen address (`127.0.0.1`, `[::1]` or
  `localhost` with its port; with `--allow-non-loopback`, that host exactly)
  gets 421, so a web page cannot point its own name at 127.0.0.1 (DNS
  rebinding) to read the application through the proxy or plant requests in
  the catalog. A redirect is pointed back at the address the client used,
  never at a `Host` it sent. Pages of other sites are refused too (403):
  every browser request marked `Sec-Fetch-Site: cross-site` but a plain
  navigation (a link, a sign-in provider sending the browser back), which is
  forwarded and not recorded, and a request that changes data or opens a
  websocket with an `Origin` that is not the proxy or another local
  address. On an address that is not loopback (`--allow-non-loopback`) a
  browser sends no `Sec-Fetch-Site` over plain HTTP, so a link or an image
  on another site's page looks like the developer's own request: there a
  request is recorded only when it carries `Sec-Fetch-Site: same-origin` or
  `none`, or an `Origin` or `Referer` of the proxy's own address; any other
  (a page of another site, a typed URL, `curl`, an API test suite) is
  forwarded but not recorded, and the summary counts them. It serves plain
  HTTP on purpose: HTTPS from the client would need a local certificate
  authority to install and trust, which is a larger risk than the feature
  is worth.
  Nothing it sees is written but the proposed requests: `Authorization`,
  `Cookie` and tokens are only compared in memory with the configured
  identities, a request with a credential or personal data in its query or
  body is left out and reported (the note names it by its path, without the
  query and with personal data or a token in the path masked), uploaded
  files become synthetic ones, and response bodies are read only to scan
  them for personal data (the entry is then private). Every response of a request is scanned, not only the first.
  A response body that is not text (an image, a PDF) is not read and
  counts as personal data.
  A gzip or deflate body is decoded for the scan, within the same 16 MiB on
  the decoded size; a body in another encoding (`br`, `zstd`), one that does
  not decode, one over 16 MiB and one cut off (the client or the service
  went away) count as personal data, so the entry is private. The client
  gets the body as the service sent it. Request and response bodies are
  held in memory up to 16 MiB per request, a larger request body passes
  through unrecorded. The request rate is not limited: the proxy
  is local and as fast as the service.
- No telemetry and no update checks. The only network traffic goes to the
  targets and to whatever the project's own commands contact.

## Secrets

- Database DSNs, tokens and keys come only from environment variables or
  1Password (`{ env: NAME }`, `{ op: "op://…" }`), never from the
  configuration file itself. A database DSN can also come from the file
  where the project already keeps it (`{ dotenv: .env }`,
  `{ appsettings: … }`, …; see
  [database DSNs](configuration.md#database-dsns)): `driftgate.yaml` holds
  only the file's name and key, the file is read when a command runs, and
  nothing of it is copied or written anywhere. The file must be inside the
  project, also past symbolic links; only the .NET user secrets of a
  project's `UserSecretsId` are read from outside it, and never written.
  The setting's TLS is kept: one DriftGate cannot pass on stops the
  command instead of connecting without it. A setting that names another
  database than `working` (for SQLite, another file) stops the command,
  so a restore never resets a database the application does not run on; one whose host may be the
  local socket or TCP needs `host:` (DriftGate does not guess). SQL
  Server's TLS is never weaker than the application's client: `Encrypt`
  and `TrustServerCertificate` are passed on, a setting that leaves
  `Encrypt` unset gets the client's own default (encrypted and verified
  when DriftGate cannot tell the client; a go-mssqldb URL of the
  environment or 1Password without `encrypt` is encrypted and verified),
  the sign-in is encrypted even with `Encrypt` off (a server that answers
  it cannot encrypt is refused before the password is sent), and `Strict`
  is never weakened; a certificate file
  a setting names by a relative path must be inside the project (an
  absolute path or `~/` may name a system CA; the file is only read as a
  certificate, and no message shows what it holds)
  ([SQL Server](configuration.md#sql-server)). A
  connection string is never read as a URL or the MySQL driver's form, so
  no part of its password is taken for the user and shown. No project code runs to read
  a setting: PHP, Python and Ruby (ERB) are read as text. A password read
  from a file is not put into the environment of the commands DriftGate
  runs.
- No message shows a database DSN, also when it cannot be parsed: messages
  reach the console, `run.log` and pull-request comments.
- `Authorization`, `Cookie` and `Set-Cookie` values are masked in captures
  so that their format can still be checked (letters become `x`, digits
  `0`), as is any other header named as a credential, with or without
  `X-` (`X-CSRF-Token`, `X-Api-Key`, `Access-Token`, `Token`, `Api-Key`;
  not the standard `Access-Control-*`, `Public-Key-Pins`, `Signature`),
  and, in `Location`, `Content-Location`, `Link` and `Refresh`, the values
  of query and fragment parameters named or shaped as credentials
  (`token=`, `signature=`, `X-Amz-Signature=`, `#access_token=`), a
  fragment shaped as a token, and path segments shaped as one (a reset
  link).
- With `follow_redirects`, a redirect from `https` to `http` is refused, and
  one to another host or port gets neither the identity's token nor the
  catalog's own headers.
- Text output escapes control characters from responses, so a response
  cannot start a line of its own (where GitHub would read `::warning::` as a
  workflow command) or send terminal sequences. The action also wraps the
  output of `run` in `stop-commands`.

## Personal data

**Personal data never goes into git, not even encrypted.**

- A response body stored as **private** (the default for generated
  requests) is encrypted with AES-256-GCM and kept only in the local store
  of the machine that captured it (`.driftgate/store/private/`, which git
  ignores, or with `baselines: git` `~/.driftgate/<project>/private/`). This
  holds for baselines too: the committed baseline names a private body only
  by a keyed hash (HMAC-SHA256 under a key derived from the project key), so
  the repository does not even hold a plain hash of personal data.
- The **project key** comes from `key:` in `driftgate.yaml` (an
  environment variable or 1Password), `DRIFTGATE_KEY`, or what `driftgate
  key create` makes: `.driftgate/key` (`baselines: local`; open to its owner
  only, mode 600, which Windows does not have, so there it is not checked) or the macOS Keychain (`baselines: git`). Treat it like a
  production password; changing it is not supported.
- With `baselines: local` the key, the store and the baselines are in the
  project's `.driftgate/` folder. DriftGate adds it to `.gitignore` and,
  before it writes the store or the key there, checks with `git
  check-ignore` that git ignores the folder; when git does not, the command
  stops (see [baselines](configuration.md#baselines)). `driftgate baselines
  git` checks what would enter the repository as `verify` does and never
  copies the key file into it.
- Where a private body is not available (CI, another machine), the body is
  not compared, not even by its hash; the rest of the committed record
  (status, headers, side effects) is. The request gets the note `private
  body not on this machine`, is listed in the report, and the result is
  incomplete (exit 3). `--skip-private` accepts a partial check: the exit
  code comes from what could be compared, so a private request that went
  from 200 to 500 is still drift (exit 1). When nothing at all could be
  compared, the result stays incomplete.
- Reports (text, JSON, JUnit, Markdown), the last comparison and the
  acceptance log show the kind and place of a difference in a private body,
  never its values (an expectation names them by a keyed hash); an object key that is not a plain name (an e-mail
  address used as a key) is hidden in the path. `compare --show-private`
  shows values on your own machine only and keeps nothing.
- Every **public** body is scanned by a personal-data detector: e-mail
  addresses, phone numbers, Serbian personal ID numbers (with the check
  digit), card numbers (Luhn) and IBANs; and for credentials: a JWT
  anywhere, a token-shaped value under a key named as a credential
  (`access_token`, `refresh_token`, `csrf_token`, `api_key`, `signature`;
  `hash` alone does not count) or as the `value` of an object whose
  `name` or `key` is one (`{"name":"api_key","value":…}`, AWS's `Key` and
  `Value`), and a URL with a credential in its query or fragment (a signed
  or presigned link, an OAuth redirect). Outside JSON the same holds for a
  form body (`access_token=…&token_type=bearer`), for links anywhere in a
  text (an HTML page) and for XML elements and attributes
  (`<access_token>…</access_token>`); an HTML page's CSRF meta tag and CSP
  nonce are not taken. A finding stores the body as
  private, reports it with a masked value and stops with exit 2.
  `pii.allow` lists values that are not personal (a shared support address).
- The body is scanned as the repository would hold it: decoded from `gzip`
  or `deflate` first (a catalog request may ask for them with its own
  `Accept-Encoding`; gzip data is decoded also without a
  `Content-Encoding`), text from UTF-16 or UTF-32 (by its byte order mark
  or declared charset), and a workbook (XLSX) part by part. Text is read as
  UTF-8, or with a textual type in a legacy charset that keeps ASCII
  (`windows-1250`, `ISO-8859-2`). A public body that cannot be scanned
  (another encoding such as `br`, an image, a PDF, an archive, text with
  NUL bytes, invalid UTF-8 with control characters, or a charset that
  changes ASCII such as `UTF-7`) is stored as private and listed after the
  capture ("cannot be scanned for personal data"); the exit code does not
  change. Mark such requests `storage: private`.
- `driftgate verify` scans committed baselines the same way: every public
  body and every record's headers. A baseline made by an older DriftGate
  or edited by hand and committed with its manifest fails (exit
  1) for a record with personal data, a credential, an unmasked credential
  header or a public body that cannot be scanned, naming the record, the
  kind and where, never the value. Run it in CI next to the gate.
  `driftgate verify --baseline <name> --migrate-private` stores such a body
  private, masks such header values and marks the requests private in the
  catalog; the values stay in the history of git.
- Real files that catalog requests upload stay in an uploads folder outside
  the repository; the catalog holds only their path and a keyed hash. The
  responses of such a request, and every response after it until the next
  restore, are stored as private. Such a request needs a state with a
  restore command (and is refused with `--allow-mutating-without-restore`),
  so the file's rows do not stay in the application after the capture.
- The local store and the folders DriftGate creates are open to their owner
  only; `store:` and `uploads.dir` may not point anywhere inside the git
  repository of the project (relative ones are relative to `driftgate.yaml`).
- The data module (`driftgate data`) makes pseudonymised copies (GDPR /
  Serbian ZZPL), not anonymous ones: whoever has the key and the original
  data can link them. Run it only locally or on your own runner. Its output
  is made only when two independent checks find nothing: the detector over
  every value and the schema, and a search for every original personal
  value.

## Release verification

A release is built only from a tag `vX.Y.Z` on a commit of `main`, by
`.github/workflows/release.yml`, after the CI checks of `ci.yml` itself
(format, vet, staticcheck, tests against MariaDB, MySQL and PostgreSQL,
of SQLite on Linux, macOS and Windows, of SQL Server 2017, 2019, 2022 and 2025
with a check of its TLS against a server with a certificate of a test CA,
fuzzing, `govulncheck` without findings, and the size report and the package limits of
the binaries on the six platforms, which pull requests pass too). It is
published from the `release` environment. Each release has:

- `driftgate-<os>-<arch>` (`.exe` on Windows) for linux, darwin and windows
  on amd64 and arm64: static binaries (`CGO_ENABLED=0`, `-trimpath`,
  `-s -w`);
- `driftgate-<os>-<arch>.cdx.json`: a CycloneDX SBOM per binary;
- `SHA256SUMS`: the SHA-256 of every binary and SBOM;
- `SHA256SUMS.sigstore.json`: a keyless Sigstore signature of `SHA256SUMS`
  (cosign with GitHub OIDC; the certificate names the release workflow and
  the tag; the signature is in the public Rekor log);
- SLSA build provenance for every file.

Verify the signature first, then the checksum:

```sh
gh release download v1.0.2 --repo orangehill/driftgate \
  --pattern driftgate-linux-amd64 --pattern 'SHA256SUMS*'
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/orangehill/driftgate/.github/workflows/release.yml@refs/tags/v1.0.2 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS      # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
gh attestation verify driftgate-linux-amd64 --repo orangehill/driftgate
```

The GitHub Action does the same for `version: v1.0.2` or `version: latest`:
it sets up a pinned cosign, verifies the signature with the identity of the
action's repository at that tag and the checksum, and installs nothing when
either fails. `version: source` builds from the action's own source.

Builds are reproducible: the build date is the commit time, and the release
workflow builds every binary twice and stops unless the bytes are the same.
Rebuilding a tag with the Go version from `go.mod` gives the same SHA-256:

```sh
pkg=github.com/orangehill/driftgate/internal/cli
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w \
  -X $pkg.Version=v1.0.2 -X $pkg.Commit=$(git rev-parse HEAD) \
  -X $pkg.Date=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ)" \
  -o driftgate-linux-amd64 ./cmd/driftgate
```
