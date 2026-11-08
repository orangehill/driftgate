# Next.js and Nuxt

> **Before you start:** [install DriftGate](../../start/install.md), and build the application. **Time:** 30 minutes.

DriftGate checks the server-rendered pages and API routes of Next.js (app
and pages routers) and Nuxt. On Next.js 16 and Nuxt 4, two builds of the
same code passed without drift. A change of a text, an attribute or a
value from `getServerSideProps` gave a drift at the exact path.

## Routes from the files

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:3000
discover:
  - adapter: nextjs
    base_path: /shop
params:
  - name: slug
    route: "/shop/blog/*"
    values: ["hello-world", "pricing"]
    foreach: true
```

`init` recognises the project by `next.config.*` or `nuxt.config.*`, and
writes the `nextjs` or `nuxt` adapter. Routes come from the files:

- Next.js: the `app` and `pages` routers, with route handlers per method;
- Nuxt: `pages`, `server/api` and `server/routes`.

Set `base_path` for a `basePath`, or an application under a prefix.
Dynamic segments (`[slug]`, `[...path]`) need values from `params:`;
`generate` names every parameter it has no value for.

## Capture the production build

```sh
next build && next start         # Nuxt: nuxt build, then its server
driftgate run
```

Capture the production build, not the development server, so the pages
are what users get. A page is compared as an element tree, at paths such
as `html>body>div#app>ul>li[3]@class`. JSON in the page, such as
`__NEXT_DATA__` or the Nuxt payload, is compared as JSON
([HTML pages](../../reference/comparison.md#html-pages)).

## Values that change with every build

<!-- check: allowlist -->
```yaml
version: 1
body:
  - path: html>body>footer>p
    format: '\d{4}'
    parts: true
    reason: The copyright year in the footer.
  - path: html>body>script#__NEXT_DATA__$.props.pageProps.generatedAt
    format: '\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z'
    reason: Time the page was rendered.
```

`init` writes allowlist entries for what a new build or request changes
by itself:

- Next.js: the build id, CSP nonces, hashed asset URLs under `/_next/static/`, and the `ETag` header;
- Nuxt: the build id in `window.__NUXT__.config`, hashed asset URLs under `/_nuxt/`, CSP nonces.

The asset rules use `parts: true`: only the hash inside a URL may change.
`/cdn/x.js` in its place is still a drift. Rules for your own values work
the same way, as above.

`capture --repeat 2` and the first `run` propose rules for the values
that differ between two captures. Values that change only with a
new build show as drift after the next build. Check that they are build
artefacts before you allow them ([changing values](../review/changing-values.md)).

## A Prisma database on SQLite

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { dotenv: .env, key: DATABASE_URL }   # it must name working
    working: prisma/dev.db
```

With `provider = "sqlite"` and `url = env("DATABASE_URL")` in
`prisma/schema.prisma`, `init` and `setup` read the file from `.env`.
Prisma takes a relative `file:` URL from the folder of `schema.prisma`,
and so does DriftGate: `file:./dev.db` is `prisma/dev.db`.

- Run `prisma migrate dev` (or `migrate deploy`) before `setup`, so the file exists.
- `_prisma_migrations` is fingerprinted and restored like any other table.
- Restores write inside `dev.db` while the server keeps it open ([SQLite](../../reference/databases/sqlite.md)).

## API routes next to pages

```sh
driftgate run --target api       # with the frontend and the backend as two services
```

A Next.js API route is compared like any JSON API. In a monorepo where
the frontend and the backend are separate services, see
[several services](multi-service.md).

---
**Next:** [Read a drift report](../review/read-the-report.md): what the first comparison shows.
**Related:** [HTML pages](../../reference/comparison.md#html-pages) · [allowlist.yaml](../../reference/allowlist-yaml.md) · [Upgrade a framework](../workflow/framework-upgrade.md)
