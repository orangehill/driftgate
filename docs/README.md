# DriftGate documentation

DriftGate is a regression gate for HTTP APIs: it checks after every change
that a running application still answers the same way. These pages
describe the release they are tagged with; what each release changes is
in the [changelog](../CHANGELOG.md).

## New to DriftGate

1. [Install DriftGate](start/install.md) (5 minutes)
2. [Tutorial: your first baseline](start/tutorial.md) (15 minutes)
3. [How DriftGate works](concepts/how-it-works.md) (10 minutes)
4. [Set up your own project](start/your-project.md) (20 minutes)
5. [Upgrade a framework step by step](guides/framework-upgrade.md)

Two more concepts: [what a green result proves](concepts/what-a-pass-proves.md),
and [where DriftGate keeps its files](concepts/project-layout.md).

## I have a task

- [Upgrade a framework or a language version](guides/framework-upgrade.md), or [the server](guides/framework-upgrade.md#upgrading-the-server-database-php-web-server)
- [Find the commit that introduced a drift](guides/bisect.md)
- [Build the catalog by recording through a proxy](guides/record.md)
- [Check more than responses](guides/side-checks.md): a schema dump, the stack's settings, files, Redis, mail
- Your framework: [Laravel](guides/laravel.md) · [ASP.NET Core](guides/aspnet-core.md) · [Next.js and Nuxt](guides/nextjs-nuxt.md) · [OpenAPI frameworks](guides/openapi.md) · [several services](guides/multi-service.md)
- CI: [the GitHub Action](guides/github-actions.md) · [anonymized test data](guides/test-data.md)

## What it can do

[What to use for what](configuration.md) names the feature for a task or
a symptom: an upload, rows in another order, a command, a value that may
change, a gap in the check.

## Look something up

- [Reference](reference/README.md): every command, file and key, and how a capture, a comparison and a report work
- [Drift kinds](reference/drift-kinds.md): what each kind of drift means
- [Glossary](glossary.md): every term these pages use
- [Security model](security.md): what a pull request can run, secrets, personal data, release verification

## Principles

- [Never a false pass](concepts/how-it-works.md#never-a-false-pass): a result that cannot be trusted is never green.
- [Local first, CI second](concepts/how-it-works.md#local-first-ci-second): check each step on your machine before the pull request.
- [One upgrade step at a time](concepts/how-it-works.md#one-upgrade-step-at-a-time): small steps give few drifts with one cause.

Every `yaml` and `json` example in these pages is loaded with
DriftGate's own parsers by the docs test (`go test ./internal/docscheck/`),
which also checks every link and anchor.
