# Send requests as a signed-in user

> **Before you start:** have test users in your seed data. **Time:** 15 minutes.

A request with an `identity` is sent with `Authorization: Bearer <token>`.
You give each identity a token, from one of the sources below.

## Take tokens from the environment or 1Password

<!-- check: config-fragment -->
```yaml
identities:
  admin: { env: DG_TOKEN_ADMIN }
  user: { op: "op://Shop/driftgate-user/token" }
```

Each identity names where its token comes from: an environment variable,
or a 1Password reference read with `op read`. The token is never written
into a file DriftGate keeps
([identities and tokens](../../reference/driftgate-yaml.md#identities-and-tokens)).

## Take tokens from a command

<!-- check: config-fragment -->
```yaml
tokens:
  command: ["php", "artisan", "driftgate:tokens"]
identities:
  admin: {}
  editor: {}
```

The command prints one JSON object, `{"admin": "<token>", "editor": "<token>"}`,
such as an artisan command that logs the test users in. It runs right
after `prepare`, so tokens it creates in the database exist for the
requests that follow. An identity's own `env` or `op` takes precedence.

## Take tokens from a file

<!-- check: config-fragment -->
```yaml
tokens:
  file: ~/.config/driftgate/shop-tokens.json
identities:
  admin: {}
```

The file holds the same JSON object. Keep it out of the repository; a
relative path is relative to the project. `tokens` takes `command` or
`file`, not both.

## A repeatable tokens command for Laravel Sanctum

```php
// database/seeders/DatabaseSeeder.php, after the test users
User::where('email', 'admin@shop.example')->first()->tokens()->create([
    'name' => 'driftgate', 'token' => hash('sha256', 'dg-admin-token'), 'abilities' => ['*'],
]);

// routes/console.php
Artisan::command('driftgate:tokens', function () {
    $this->line(json_encode(['admin' => 'dg-admin-token', 'user' => 'dg-user-token']));
});
```

The tokens command runs after every restore. One that issues new tokens
writes rows with new values and times each time, so a response that
shows them differs. Here the seeder writes the token once, with a fixed
value. Sanctum keeps only its hash, and looks a token without `id|` up by
that hash. The command prints the plain text. Seed the `user` token the
same way; Sanctum comes with `php artisan install:api`.

## Generate requests for routes that need a login

<!-- check: config-fragment -->
```yaml
identities:
  admin: { env: DG_TOKEN_ADMIN }
generate:
  identity: admin
templates: [ok, not-found, json, anonymous, identities]   # the list init wrote, plus the last two
```

`generate.identity` sends the routes that need a login as that user.
`anonymous` adds each such route without a login, and `identities` once
per other identity. A change of who may see what is then a drift
([templates](../../reference/driftgate-yaml.md#templates)).

A catalog request whose identity has no token stops the capture.

## Sessions with cookies

DriftGate keeps no cookies between requests. Each request sends only its
catalog headers and its identity's token. So an application that signs
in with a session cookie needs a token for DriftGate:

- an API or personal access token for each test user, from a token command or the environment;
- or a route that accepts a bearer token, such as Laravel Sanctum's.

A [login request](hand-written-requests.md#check-the-login-itself) in the
catalog still checks the login itself.

---
**Next:** [Add the requests a browser or frontend sends](record.md): record what the frontend uses.
**Related:** [identities and tokens](../../reference/driftgate-yaml.md#identities-and-tokens) · [Write catalog requests by hand](hand-written-requests.md) · [Security model](../../security.md)
