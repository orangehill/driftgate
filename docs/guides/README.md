# Guides

Each guide is one task, step by step. The rules and edge cases are in
the [reference](../reference/README.md).

## Upgrade step by step

- [Upgrade a framework step by step](workflow/framework-upgrade.md)
- [Upgrade the server: database, PHP, web server](workflow/server-upgrade.md)
- [Work on each change in its own worktree](workflow/worktrees.md)
- [Share a baseline with your team or CI](workflow/share-baselines.md)
- [Check a live server without writing to it](workflow/live-server.md)

## Build the catalog

- [Make the catalog cover every route](catalog/coverage.md)
- [Add the requests a browser or frontend sends](catalog/record.md)
- [Write catalog requests by hand](catalog/hand-written-requests.md)
- [Send requests as a signed-in user](catalog/logins.md)
- [Check what the application does besides answering](catalog/side-checks.md)

## Review the result

- [Read a drift report](review/read-the-report.md)
- [Allow values that change by themselves](review/changing-values.md)
- [Fix, accept or expect a drift](review/fix-accept-or-expect.md)
- [Find the commit that introduced a drift](review/find-the-commit.md)

## Your framework

- [Laravel](frameworks/laravel.md)
- [ASP.NET Core and EF Core](frameworks/aspnet-core.md)
- [Next.js and Nuxt pages](frameworks/nextjs-nuxt.md)
- [Frameworks with an OpenAPI description](frameworks/openapi.md)
- [Several services and monorepos](frameworks/multi-service.md)

## CI and test data

- [CI with GitHub Actions](ci/github-actions.md)
- [Anonymized test data for CI](ci/test-data.md)

---
**Related:** [Documentation](../README.md) · [Reference](../reference/README.md) · [Glossary](../glossary.md)
