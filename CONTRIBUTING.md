# Contributing to DriftGate

Thank you for helping. DriftGate is a regression gate for step-by-step
framework upgrades, and it is kept small on purpose. Please read this before
you open an issue or a pull request.

## What we take

- **Bug reports and fixes: yes.** A wrong pass, a wrong fail, a crash, a
  secret that is shown or written where it should not be, a message that
  says the wrong thing, a documentation error.
- **New features: only by exception.** The scope is closed. A new feature
  needs a concrete case it solves that the existing commands and settings
  cannot. Open an issue first and describe that case before you write
  code. A pull request with a feature nobody agreed to may be closed
  without review.
- **No per-case options.** A behaviour that is right for one project only
  belongs in that project's configuration or its own scripts, not in a new
  flag.

## Reporting a bug

Open an issue with:

- the DriftGate version (`driftgate version`) and the operating system;
- the database and its version, when the bug involves state;
- the command you ran and its full output;
- what you expected and what happened.

Remove secrets, private response bodies and personal data before you
paste output. DriftGate redacts what it knows to be secret, but it cannot
know everything that is private in your application.

Never report a security problem in a public issue: see
[SECURITY.md](SECURITY.md).

## Pull requests

- Open the pull request against `development`, not `main`. Changes reach
  `main` when a release is made.
- One change per pull request, with a message that says what it changes and
  why.
- Add a test that fails without the change. A fix for a wrong pass needs a
  test that would have passed wrongly.
- Run before you push:

  ```sh
  gofmt -l .        # must print nothing
  go vet ./...
  go test ./...
  go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
  ```

  Tests that need MySQL, MariaDB, PostgreSQL or SQL Server are skipped
  unless the matching `DRIFTGATE_TEST_*` variable is set; CI runs them all.
- Update the documentation under `docs/` when behaviour changes.

## Principles a change is judged by

- **Never a false pass.** When DriftGate cannot decide, it fails or says so;
  it never reports a pass it cannot prove.
- **Local first.** DriftGate is built for checks on a developer's machine
  before a pull request; CI is a second place it runs.
- **Faster is better, never at the cost of the first rule.**

## License

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
