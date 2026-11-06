# Database connection settings

How DriftGate reads the `dsn:` of a component under
[`state:`](state.md#database-components) or [`data:`](driftgate-yaml.md#data):
the server, the user and the password of a database.

## Where a DSN comes from

The DSNs of `state:` and `data:` hold the password. So they come from the
environment (`{ env: NAME }`), 1Password (`{ op: "op://…" }`) or
[the project's own settings](#read-the-dsn-from-the-projects-own-settings),
never from `driftgate.yaml` itself. No error message shows them.

- A DSN is read only by a command that works with the database: `seed`, `capture`, `run`, `setup`, and the commands that run or query it.
- `compare`, `report`, `review`, `accept` and `expect` read captures only. On a machine where the variable is not set or the settings file is not there, they still run; the YAML itself is checked.

## DSN URLs and their parameters

A DSN is a URL of the server:

- MySQL and MariaDB: `mysql://user:pass@host:3306/`, or the driver's own form `user:pass@tcp(host:3306)/`;
- PostgreSQL: `postgres://user:pass@host:5432/`;
- SQL Server: `sqlserver://user:pass@host:1433?encrypt=true`.

Percent-encode special characters (`@ : / ? # %`) in the user name and
password. The database named in the URL is ignored: `working`,
`pristine` and `database` name it.

A `mysql://` or `mariadb://` URL takes these parameters. Any other
parameter stops with an error instead of being ignored.

| Parameter | Meaning |
|---|---|
| `tls` | `true` (verify the certificate and the host name), `skip-verify` (encrypt without verifying), `preferred` (encrypt when the server can), `false` |
| `ssl-mode` | as the `mysql` client: `DISABLED`, `PREFERRED`, `REQUIRED`, `VERIFY_CA` or `VERIFY_IDENTITY` |
| `ssl-ca` | a PEM file of the certificate authorities to verify the server with; implies verification |
| `timeout`, `readTimeout`, `writeTimeout` | durations such as `10s` |

- `ssl-mode=REQUIRED` encrypts without verifying. `VERIFY_CA` and `VERIFY_IDENTITY` both verify the certificate and the host name.
- `mysql://app:secret@db.shop.example:3306/?tls=true` connects only over verified TLS.
- The driver's own form (`user:pass@tcp(host:3306)/?tls=true`) takes the driver's parameters.
- A `postgres://` URL takes PostgreSQL's parameters (`sslmode`, `sslrootcert`, …).
- A `sqlserver://` URL takes go-mssqldb's TLS parameters ([DSN and TLS](databases/sql-server.md#dsn-and-tls)).

## Connection strings

An `env` or `op` DSN may also be one of these forms. It is converted to
a DSN as [below](#tls-settings-that-are-passed-on).

- A JDBC URL: `jdbc:mysql://…`, `jdbc:postgresql://…?user=…&password=…`.
- An ADO.NET connection string, as .NET reads `ConnectionStrings__Default` from the environment: `Server=…;Port=…;Database=…;Uid=…;Pwd=…`, Npgsql's `Host=…;Username=…;Password=…`, or SqlClient's `Server=tcp:host,1433;…`.
- An ODBC string of SQL Server.
- libpq's keyword form: `host=… dbname=… user=… password=…`.

A connection string that names no server (`Server=`, `Host=`) is refused
rather than taken for a local one: a `;` in a password would otherwise
cut it short. A `key=value;…` string that names a server is read as a
connection string before anything else, also when its password holds
`://`, `@/` or `@tcp(`.

## Read the DSN from the project's own settings

Instead of a copy of the DSN, `dsn:` can name the file where the
application keeps its database settings. `driftgate init` and
`driftgate setup` write it from what they find.

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { dotenv: .env, host: "compose:mysql" }   # Laravel Sail: DB_HOST=mysql
    working: shop
  reports:
    driver: postgres
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Reports" }
    working: reports
```

- DriftGate reads the file each time a command runs. The password stays where it is, and a change of the file is followed.
- The file is relative to the project root and must be inside it; `\` is taken for `/`.
- That holds past symbolic links too: a link that leads outside the project folder is refused, also a compose `env_file`.
- Nothing of the project runs: PHP, Python and ERB are read as text.
- When the setting names a database, it must be `working`. A command that finds another one (`DB_DATABASE` changed in a worktree) stops with exit 2 and names both, rather than restore a database the application no longer runs on.
- A file that names a driver other than the component's is an error, such as an SQLite file for a `mysql` component.
- So is a value only code computes, such as `os.environ[...]` in `settings.py`, or other Ruby in ERB. Put the DSN in a variable, and use `{ env: NAME }`.

Each source below names the file. `key` picks the setting within it.

### dotenv

`dotenv: .env` reads a `.env` file ([how](#how-env-files-are-read)).
`key` is optional.

- Without `key`, it reads Laravel's `DB_CONNECTION`, `DB_HOST`, `DB_PORT`, `DB_DATABASE`, `DB_USERNAME`, `DB_PASSWORD` and `DB_SOCKET` (MySQL only; Laravel 5.5 and later).
- `DB_URL` (Laravel 11 and later) or `DATABASE_URL` (5.8 to 10) goes over them when set, as Laravel merges them.
- With `key`, it reads the URL in that variable: `DATABASE_URL`, `DB_URL`, or Prisma's.
- `MYSQL_ATTR_SSL_CA` turns on TLS verified with that CA, as Laravel's `PDO::MYSQL_ATTR_SSL_CA` does.
- A key it reads that the file sets twice to different values is an error naming the lines. Laravel 5.5 to 5.7 take the first, later versions the last.

Without `DB_CONNECTION`, it reads a NestJS setting (TypeORM, MikroORM,
Sequelize):

- `DB_TYPE` with `DB_HOST`, `DB_PORT`, `DB_USERNAME`, `DB_PASSWORD`, and `DB_DATABASE` or `DB_NAME`;
- `TYPEORM_CONNECTION` with `TYPEORM_HOST`, `TYPEORM_PORT`, `TYPEORM_USERNAME`, `TYPEORM_PASSWORD`, `TYPEORM_DATABASE`;
- `DATABASE_TYPE` with `DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_USERNAME`, `DATABASE_PASSWORD`, `DATABASE_NAME`.

The type names the driver: `mysql`, `mariadb`, `postgres`, `sqlite`,
`better-sqlite3` (with the file in the database key), or `mssql`.

- `mssql` is read as tedious, which encrypts and verifies. For a self-signed certificate, give `state:` a DSN of its own with `TrustServerCertificate=true`.
- A type that names no database makes no Node setting.

`init` and `setup` offer the files the framework reads:

- Laravel (`artisan`): only `.env`, or `.env.<APP_ENV>` when `APP_ENV` is set in the environment;
- Symfony (`bin/console`): `.env.<APP_ENV>.local`, `.env.<APP_ENV>`, `.env.local` (not for `test`) and `.env`. `APP_ENV` comes from the environment, else the files, else `dev`;
- others: `.env.development.local`, `.env.local`, `.env.development` and `.env`.

Files that name different databases stop `setup --yes`.

### appsettings

`appsettings: src/Api/Api.csproj` reads ASP.NET's settings. `key` is
required: `ConnectionStrings:Default`, in any letter case.

- For a `.csproj`, it reads `appsettings.json`, then `appsettings.<environment>.json`. The environment is `ASPNETCORE_ENVIRONMENT`, `DOTNET_ENVIRONMENT`, or by default `Development`.
- In Development it also reads the project's user secrets (`UserSecretsId`): `~/.microsoft/usersecrets/…`, on Windows `%APPDATA%\Microsoft\UserSecrets\…`.
- For a `.csproj`, the launch profile `dotnet run` takes sets the environment, and its `ConnectionStrings__*` go over the files. That is the first profile with `commandName: Project` in `Properties/launchSettings.json`.
- For `appsettings.Development.json`, it reads that file over `appsettings.json`.
- Comments and trailing commas are allowed.

### properties

`properties: src/main/resources/application.properties` reads the
settings of Spring, Quarkus and Micronaut, as `.properties` or `.yml`.

- Spring: `spring.datasource.url`, `username`, `password`.
- Quarkus: `quarkus.datasource.jdbc.url`, …; `%dev.` keys win.
- Micronaut: `datasources.default.*`.
- `key` is optional: the prefix. By default it is the first of `spring.datasource`, `quarkus.datasource`, `datasources.default` and `spring.r2dbc` that the files set.

It reads the files as Spring Boot does:

- `application.*` in the file's folder, then `application-<profile>.*` for each active profile, with the profile of the named file last.
- The active profiles come from `SPRING_PROFILES_ACTIVE`, else `spring.profiles.active`.
- `.properties` wins over `.yml`.
- A document with `spring.config.activate.on-profile` counts when its profile is active. That is a YAML document, or a `.properties` part after a `#---` or `!---` line.
- `${NAME}` and `${NAME:default}` take the properties, then the environment.
- `init` and `setup` offer only `application.*` and the files of the active profiles.

### yaml

`yaml: config/database.yml` reads Rails' `database.yml`, or a Go
service's `config.yaml`.

- Rails: with `<<: *default`, and `DATABASE_URL` of the environment over it, as Rails merges it.
- In ERB it reads only `ENV['X']`, `ENV.fetch('X')`, `ENV.fetch('X', default)`, `ENV.fetch('X') { default }` and `ENV['X'] || default`.
- A Go service's file: a URL, or `adapter`/`driver`, `host`, `port`, `database`/`name`/`dbname`, `username`/`user`, `password` and `socket`.
- `key` defaults to `development` for `database.yml`. Otherwise it is the first of `database`, `db`, `datasource`, `mysql`, `postgres` and `postgresql`.
- Dots in `key` separate levels: `development.primary`.

### php

`php: app/etc/env.php` reads Magento's settings: the array the file
returns, with `host` (also `host:port`), `dbname`, `username` and
`password`. `key` defaults to `db.connection.default`.

### python

`python: shop/settings.py` reads Django's literal `DATABASES = {…}`:
`ENGINE`, `NAME`, `USER`, `PASSWORD`, `HOST` and `PORT`. `key` defaults
to `default`.

### compose

`compose: compose.yaml` reads a MySQL, MariaDB, PostgreSQL or SQL Server
service of a compose file, with its override. `key` is required: the
service name.

- The service is found by its image.
- It reads the port the service publishes on this machine, and its user from `MYSQL_*`, `MARIADB_*` or `POSTGRES_*`.
- Those come also through `env_file`, and through `${VAR}` from the environment and the `.env` next to the file.
- MySQL and MariaDB: the user is `root` when the root password is set, as DriftGate creates the pristine database.
- SQL Server: see [the application's settings](databases/sql-server.md#the-applications-settings).

## Set the host or a socket

- `host: 127.0.0.1:3307` replaces the setting's host and port: the address of the database from this machine.
- `host: /path/to/mysqld.sock`, or a PostgreSQL socket folder, takes a socket instead.
- `host: compose:mysql` names a service of the project's compose file, the setting's host (`DB_HOST=mysql`). It is reached at the port it publishes on this machine, read when a command runs, so a changed `FORWARD_DB_PORT` is followed.

MySQL's `localhost` or no host, and PostgreSQL's no host, are the local
socket for PDO, mysql2, mysqlclient and libpq, and TCP for others.
DriftGate does not guess.

- A `dotenv`, `yaml`, `php` or `python` setting with such a host needs `host:`: `127.0.0.1:3306`, or the socket's path.
- Without it a command stops and says what to add. `setup --yes` stops with the exact `dsn:` to write; at a terminal it asks.
- JDBC, .NET and compose settings are TCP.
- Laravel's missing `DB_HOST` is `127.0.0.1`, as its `config/database.php` says.

## TLS settings that are passed on

A setting from the project, or a connection string, becomes a DSN, and so
does its TLS, in any form:

- `sslmode`, `SslMode`, `SSL Mode`;
- JDBC `useSSL`, `requireSSL`, `verifyServerCertificate` and `sslMode`;
- pgJDBC `ssl=true`, as `verify-full`;
- Prisma's `sslaccept=strict` as `verify-full`, and `accept_invalid_certs` as `require`, its `sslcert` being the CA;
- `sslca` and `sslrootcert`; `sslcert` and `sslkey` for PostgreSQL only.

Prisma's `sslidentity` is refused, and Prisma's MySQL `?socket=` is the
socket.

- A TLS setting DriftGate cannot pass on is an error, never left out.
- Other parameters (`charset`, `serverVersion`, Prisma's `schema`) are left out. PostgreSQL's `application_name`, `connect_timeout` and `target_session_attrs` are kept.
- The database is left out too: `working` and `database` name it.

## How .env files are read

`.env` files are read as phpdotenv, Node's dotenv and godotenv agree:

- `export`, `'…'` literal, and `"…"` with `\n`, `\"` and `\$` escapes;
- values over several lines in quotes;
- `#` comments after a space;
- `${NAME}` and `${NAME:-default}`, from earlier lines or the environment;
- a `$` without a closed `${…}` is kept: `pa$word`, `pa${ss`.

The file's value counts, also when the environment sets the same name.
To take the environment's, use `{ env: NAME }`.

---
**Related:** [state](state.md#database-components) · [Where the database may be](state.md#where-the-database-may-be) · [SQL Server: DSN and TLS](databases/sql-server.md#dsn-and-tls) · [Security model](../security.md)
