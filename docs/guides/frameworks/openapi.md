# Frameworks with an OpenAPI description

> **Before you start:** [install DriftGate](../../start/install.md), and have the application serve or write its description. **Time:** 20 minutes.

One adapter, `openapi`, covers every framework that serves or writes an
OpenAPI 3 or Swagger 2 description.

## Supported frameworks

`init` writes the adapter for these; any other framework works the same
way with a file or a URL.

| Framework | Description | Checked on |
|---|---|---|
| Spring Boot | `/v3/api-docs` (springdoc-openapi); without it, the `spring` adapter reads `/actuator/mappings` | Spring Boot 4.1 |
| ASP.NET | `/openapi/v1.json`, or `/swagger/v1/swagger.json` with Swashbuckle | .NET 9 |
| FastAPI | `/openapi.json` | FastAPI |
| NestJS | `/<setup path>-json` (`@nestjs/swagger`) | NestJS 12 |
| Quarkus | `/q/openapi?format=json` (smallrye-openapi) | Quarkus 3.40 |
| Micronaut | the file micronaut-openapi writes at build time (`target/classes/META-INF/swagger/*.yml`) | Micronaut 5.2 |
| Magento 2 | `/rest/default/schema?services=all` | Magento 2.4.9 |
| Symfony, Django, Rails | API Platform or NelmioApiDocBundle, drf-spectacular, rswag, next to the native adapter | Symfony 7 and 8, Django 5 and 6, Rails 8.1 |

When a framework has both a native adapter and a description, `init`
writes both. The description gives request bodies; the native adapter
also sees routes the description leaves out.

## Configure the openapi adapter

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:8080
discover:
  - adapter: openapi
    url: /v3/api-docs          # a path on the service, or a full URL on an allowed host
    identity: admin            # read the description as this identity
  # or: file: openapi.yaml     # JSON or YAML, relative to the project (or to dir:)
```

- `url` is read from the running application, only from allowed hosts.
- `file` suits a description written at build time, or kept in the repository.
- Some applications describe only what the caller may use, so read the description as an identity. Magento lists 47 routes anonymously, and 326 as an administrator.

Every key is under [discover](../../reference/driftgate-yaml.md#discover).

## How requests are built from the description

```sh
driftgate discover --write && driftgate generate --write
```

- Request bodies come from the description's examples, or are built from the schema.
- `multipart/form-data` with `format: binary` (Swagger 2: `type: file`) gets a synthetic file.
- Required query parameters get a value from `params:`, or a sample from the schema, so a request does not fail with 400.
- Path parameters get their values from `params:`; `generate` names every parameter it has no value for ([parameter values](../../reference/driftgate-yaml.md#parameter-values)).

## An example description

<!-- check: yaml -->
```yaml
openapi: 3.0.3
info: {title: demo, version: "1"}
paths:
  /api/news:
    get:
      parameters:
        - {name: page, in: query, required: true, schema: {type: integer, example: 1}}
      responses: {"200": {description: ok}}
  /api/news/{id}:
    get:
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses: {"200": {description: ok}}
  /api/faq:
    post:
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {q: {type: string, example: "?"}}}
      responses: {"201": {description: created}}
```

```console
$ driftgate discover --write
3 routes (3 from openapi)
  route-added   GET /api/news
  route-added   GET /api/news/{id}
  route-added   POST /api/faq
$ driftgate generate --write
3 generated requests; …: 3 added, 0 removed, 0 changed
```

Checked against the demo API. The catalog has `GET /api/news?page=1`,
with the required query parameter from its example, and `/api/news/{id}`
with `id` from `params:`. It has `POST /api/faq` with the body
`{"q": "?"}`, marked as a request that changes data.

## Framework notes

```properties
management.endpoints.web.exposure.include=mappings
```

- **Spring Boot**: `init` allows `$.timestamp` of Spring's error body, and excludes `/actuator/*` and `/error` with reasons. Without springdoc, the `spring` adapter needs `spring-boot-starter-actuator` and the setting above.
- **Micronaut**: the description exists only after a build. Build, then run `init` again.
- **Magento**: Magento keeps its URL in the database, so set `base_url` to the store's URL. `/rest/all` would also write to every store view, so `init` uses the default store view.
- **ASP.NET with EF Core on SQL Server**: see [ASP.NET Core and EF Core](aspnet-core.md).
- **ASP.NET template**: it returns random data. The first `run` refuses the baseline, and proposes exact rules (`-?\d+` for the temperature).

---
**Next:** [Read a drift report](../review/read-the-report.md): what the first comparison shows.
**Related:** [discover](../../reference/driftgate-yaml.md#discover) · [driftgate generate](../../reference/commands/catalog.md#driftgate-generate) · [Upgrade a framework](../workflow/framework-upgrade.md)
