# Next.js and Nuxt pages

DriftGate checks server-rendered pages and API routes of Next.js (app and
pages routers) and Nuxt. Checked on Next.js 16 and Nuxt 4: two builds of
the same code pass without drift under the rules `init` writes, while a
change of text, an attribute or a value from `getServerSideProps` gives a
drift at the exact path.

## Routes

`init` recognises the project by `next.config.*` or `nuxt.config.*` and
writes the `nextjs` or `nuxt` adapter. Routes come from the files:

- Next.js: the `app` and `pages` routers; route handlers per method;
- Nuxt: `pages`, `server/api` and `server/routes`.

With a `basePath` (or the application under a prefix), set it on the
adapter:

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

Dynamic segments (`[slug]`, `[...path]`) need values from `params:`;
`generate` names every parameter it has no value for.

Run the production build (`next build && next start`, `nuxt build` and its
server), not the development server, so the pages are what users get.

## How pages are compared

An HTML response is compared byte for byte first, then as an element tree:

- a difference has a path such as `html>body>div#app>ul>li[3]@class`; one
  inserted element is one difference, not a change of everything after it;
- whitespace between elements and comments do not count, except inside
  `pre`, `textarea`, `script` and `style`, and whitespace next to an
  inline element (`a`, `b`, `span`, `img`, …) between it and a text or
  another inline element, which a browser shows as a space:
  `<b>a</b> <i>b</i>` reads "a b" and differs from `<b>a</b><i>b</i>`
  ("ab"), as `a <b>x</b>` differs from `a<b>x</b>`. Such a drift shows
  the element's content as it reads: `"a <b>"` → `"a<b>"`;
- JSON embedded in the page is compared as JSON, with a path to the value:
  `__NEXT_DATA__` (pages router), the app router payload
  (`self.__next_f`), the Nuxt payload (decoded, with key names), and
  `application/json` / `ld+json` scripts. For example
  `html>body>script#__NEXT_DATA__$.props.pageProps.items[1].name: "Pear" → "Plum"`.

Not seen: comments, the doctype, `&amp;` versus `&`, attribute order. Paths
into the app router payload are long and follow the shape of the React
tree; references such as `$L3` stay text.

## What changes with every build

`init` writes allowlist entries for what a new build or a new request
changes by itself:

- Next.js: the build id (`__NEXT_DATA__` and the app router payload), CSP
  nonces, hashed asset URLs under `/_next/static/`, and the `ETag` header;
- Nuxt: the build id in `window.__NUXT__.config`, hashed asset URLs under
  `/_nuxt/`, CSP nonces.

The asset rules use `parts: true`: only the hash inside a URL may change,
so `/_next/static/<hash>/app.js` passes while `/cdn/x.js` in its place is a
drift. A rule for your own values works the same way:

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

For values that differ between two captures of the same build (a render
time, a random id), `capture --repeat 2` and the first `run` propose
entries with a format. Values that change only with a new build show up as
drift after the next build; check that they are build artefacts before you
allow them.

## A Prisma database on SQLite

With `provider = "sqlite"` and `url = env("DATABASE_URL")` in
`prisma/schema.prisma`, `init` and `setup` read the file from `.env`.
Prisma takes a relative `file:` URL from the folder of `schema.prisma`,
and so does DriftGate: `DATABASE_URL="file:./dev.db"` is `prisma/dev.db`.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { dotenv: .env, key: DATABASE_URL }   # it must name working
    working: prisma/dev.db
```

The pristine copy is a file of DriftGate's store, and restores write
inside `dev.db` while the server keeps it open (see
[SQLite](../reference/databases/sqlite.md)). `_prisma_migrations` is
fingerprinted and restored like any other table. Run `prisma migrate dev`
(or `migrate deploy`) before `setup`, so the file exists.

## API next to pages

A Next.js API route is compared like any JSON API. In a monorepo where the
frontend and the backend are separate services, see
[several services](multi-service.md).
