# Set up your own project

> **Before you start:** [install DriftGate](install.md), start your application and its database, and do the [tutorial](tutorial.md). **Time:** 20 minutes.

`driftgate setup` takes a project from nothing to its first baseline. Run
it on the old version of your application, before you upgrade anything.

## Run setup

```sh
cd <your application>
driftgate setup
```

In the project's folder, setup:

1. recognises the framework;
2. reads the database and the address from the project's own settings: `.env`, `appsettings.json`, compose files and others;
3. writes `.driftgate/` and makes the key;
4. after asking, makes the pristine copy of the database: the data every capture starts from;
5. checks that the application answers, and makes the first baseline;
6. prints a summary, and what to check by hand.

It works with MySQL, MariaDB, PostgreSQL, SQL Server 2017 and later, and
an SQLite file such as a new Laravel application's
`database/database.sqlite`. After every upgrade step, `driftgate run`
compares with this baseline.

## Answer the questions setup asks

Setup asks before it creates a database, and when the project has
several settings or addresses to choose from.

- Answer at the terminal, or pass `--yes` to take the best choice everywhere.
- Where a value would have to be guessed, `--yes` stops and says what to pass, such as `--url` or `--database`.

The questions and the choices are listed under
[driftgate setup](../reference/commands/project.md#driftgate-setup).

## Check what setup wrote

Read the summary setup prints. It names the framework, the database and
where its setting was found, the address, the mode, and the routes
covered. Then look at the files it wrote in `.driftgate/`:

- `driftgate.yaml`: the configuration ([driftgate.yaml](../reference/driftgate-yaml.md));
- `catalog.json`: the requests every capture sends ([catalog.json](../reference/catalog-json.md));
- `allowlist.yaml`: the values that may change ([allowlist.yaml](../reference/allowlist-yaml.md)).

The mode is `freeze`: every drift fails, and nothing can be accepted.
Set `mode: evolve` when the team may accept intended changes
([mode](../reference/driftgate-yaml.md#mode)).

## When setup stops

Setup stops with an exact instruction, and the command that goes on from
there. Common reasons:

- the application does not answer at its address: start it, and run setup again;
- the database user may not create the pristine copy: setup prints what to grant;
- the two first captures differ: add the allowlist entries it proposes, with a reason, and run `driftgate run`.

A second `driftgate setup` goes on where the first stopped. Every reason
it may stop for is under
[When setup stops](../reference/commands/project.md#when-setup-stops).

## Set up step by step with init

`driftgate init` writes only the configuration, with the database it
found commented out. Use it to go through the steps yourself:

```sh
driftgate init --project shop    # the configuration
driftgate key create             # the key
driftgate run                    # routes, catalog and the first baseline
```

Uncomment `state:` first: `run` needs it to restore the database.
Without it, `run` refuses requests that change data, unless
`--allow-mutating-without-restore`. The [tutorial](tutorial.md) goes through these steps on the
demo.

## Choose your framework guide

- [Laravel](../guides/frameworks/laravel.md), with Herd, Docker, Sail or SQLite
- [ASP.NET Core and EF Core on SQL Server](../guides/frameworks/aspnet-core.md)
- [Next.js and Nuxt pages](../guides/frameworks/nextjs-nuxt.md)
- [Frameworks with an OpenAPI description](../guides/frameworks/openapi.md): Spring Boot, FastAPI, NestJS and others
- [Several services and monorepos](../guides/frameworks/multi-service.md)

To work on a change in a git worktree of its own, see
[setup in a git worktree](../reference/commands/project.md#setup-in-a-git-worktree).

---
**Next:** [Upgrade a framework](../guides/workflow/framework-upgrade.md): check each upgrade step against the baseline.
**Related:** [driftgate setup](../reference/commands/project.md#driftgate-setup) · [How DriftGate works](../concepts/how-it-works.md)
