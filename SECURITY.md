# Security policy

## Reporting a vulnerability

Please do not report security problems in public issues, discussions or
pull requests.

Report them privately through GitHub: **Security → Report a
vulnerability** in this repository. Include:

- the DriftGate version and operating system;
- what an attacker can do and under which conditions;
- the steps to reproduce it.

We acknowledge a report within **5 working days** and keep you informed
until it is fixed. We credit reporters in the release notes unless you ask
us not to.

## Supported versions

Security fixes are made for the latest release only. Please upgrade before
you report.

## What counts

Examples of what we treat as a vulnerability:

- a secret (a DSN password, a token, the project key) shown in output, logs
  or reports, or written to a file that may be committed;
- private response bodies stored or shown unencrypted;
- a way for configuration or a catalog to run commands or reach hosts that
  the [security model](docs/security.md) says it cannot;
- a weakened TLS check towards a database or the application.

What DriftGate relies on you for, such as never running it on
`pull_request_target` and keeping production secrets out of pull request
jobs, is described in [docs/security.md](docs/security.md). Problems that
come from not following it are not vulnerabilities in DriftGate, but we
welcome reports that the documentation is unclear.

## Verifying a release

Releases are signed. See
[release verification](docs/security.md#release-verification).
