# Frameworks with an OpenAPI description

One adapter, `openapi`, covers every framework that serves or writes an
OpenAPI 3 or Swagger 2 description. `init` writes it for these:

| Framework | Description | Checked on |
|---|---|---|
| Spring Boot | `/v3/api-docs` (springdoc-openapi); without springdoc the `spring` adapter reads `/actuator/mappings` | Spring Boot 4.1 |
| ASP.NET | `/openapi/v1.json`, or `/swagger/v1/swagger.json` with Swashbuckle | .NET 9 |
| FastAPI | `/openapi.json` | FastAPI |
| NestJS | `/<setup path>-json` (`@nestjs/swagger`) | NestJS 12 |
| Quarkus | `/q/openapi?format=json` (smallrye-openapi) | Quarkus 3.40 |
| Micronaut | the file micronaut-openapi writes at build time (`target/classes/META-INF/swagger/*.yml`) | Micronaut 5.2 |
| Magento 2 | `/rest/default/schema?services=all` | Magento 2.4.9 |
| Symfony, Django, Rails | API Platform or NelmioApiDocBundle, drf-spectacular, rswag, next to the native adapter | Symfony 7 and 8, Django 5 and 6, Rails 8.1 |

Any other framework works the same way with a file or a URL.

## The adapter

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:8080
discover:
  - adapter: openapi
    url: /v3/api-docs          # a path on the service, or a full URL on an allowed host
  # or: file: openapi.yaml     # JSON or YAML, relative to the project (or to dir:)
```

- `url` is read from the running application, only from allowed hosts.
  `file` suits a description written at build time or kept in the
  repository.
- Request bodies come from the description's examples, or are built from
  the schema. `multipart/form-data` with `format: binary` (Swagger 2:
  `type: file`) gets a synthetic file.
- Required query parameters get a value from `params:` or a sample from the
  schema, so a request does not fail with 400 for a parameter it did not
  send. Give path parameters their values in `params:`; `generate` names
  every parameter it has no value for.
- Some applications describe only what the caller may use. Read the
  description as an identity: `identity: admin` (Magento lists 47 routes
  anonymously and 326 as an administrator).

When a framework has both a native adapter and a description, `init` writes
both: the description gives request bodies, the native adapter also sees
routes the description leaves out.

## Example

Checked with a three-route description against the demo API:

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

```
$ driftgate discover --write
3 routes (3 from openapi)
  route-added   GET /api/news
  route-added   GET /api/news/{id}
  route-added   POST /api/faq
$ driftgate generate --write
3 generated requests; …: 3 added, 0 removed, 0 changed
```

The generated catalog has `GET /api/news?page=1` (the required query
parameter from its example), `/api/news/{id}` with `id` from `params:`, and
`POST /api/faq` with the body `{"q": "?"}`, marked as a request that changes
data.

## Framework notes

- **Spring Boot**: `init` allows `$.timestamp` of Spring's error body and
  excludes `/actuator/*` and `/error` with reasons. Without springdoc, the
  `spring` adapter needs `spring-boot-starter-actuator` and
  `management.endpoints.web.exposure.include=mappings`.
- **Micronaut**: the description exists only after a build; build, then
  run `init` again.
- **Magento**: Magento keeps its URL in the database; set `base_url` to the
  store's URL. `/rest/all` would also write to every store view, so `init`
  uses the default store view.
- **ASP.NET with EF Core on SQL Server**: see
  [ASP.NET Core and EF Core on SQL Server](aspnet-core.md).
- **ASP.NET template**: it returns random data; the first `run` refuses the
  baseline and proposes exact rules (`-?\d+` for the temperature).
