# Production operation

Production runs one long-running Go process connected to the application MySQL
database, read-only DWH and MSO databases, and Fincloud. Put an HTTPS reverse
proxy in front of it. Provisioning those systems is outside this repository.

## Configuration

Start from `.env.example`. Settings that matter for operation:

```dotenv
APP_ENV=production
APP_URL=https://trs.example.internal
APP_BIND_HOST=127.0.0.1
APP_PORT=8080
APP_SHUTDOWN_TIMEOUT=45s

SESSION_SECURE=true

AUTH_LOGIN_FAILURE_WINDOW=15m
AUTH_LOGIN_MAX_FAILURES=5
AUTH_LOGIN_LOCKOUT=15m

DB_NETWORK=tcp
DB_HOST=127.0.0.1
DB_PORT=3306
DB_NAME=trs
DB_USER=trs_runtime
DB_PASSWORD=replace-me

SLIK_STORAGE_DIR=/opt/trs/data/slik
```

`APP_BIND_HOST` empty listens on every interface (container default). Behind a
reverse proxy on the same host, set `127.0.0.1`. Production does not enforce
HTTPS or secure cookies; set `APP_URL` to HTTPS and `SESSION_SECURE=true`
whenever the proxy terminates TLS.

### Local MySQL Unix socket

For MySQL on the same Linux host, replace the TCP database block with:

```dotenv
DB_NETWORK=unix
DB_SOCKET=/var/run/mysqld/mysqld.sock
DB_NAME=trs
DB_USER=trs
DB_PASSWORD=
```

`DB_NETWORK` defaults to `tcp`; unknown or empty values are rejected. Unix mode
requires the socket, ignores `DB_HOST`/`DB_PORT`, and never falls back to TCP.
DWH and MSO keep their own `*_DBSTRING` DSNs.

## Build and installation

From a reviewed commit:

```sh
npm ci
make build
sha256sum bin/trs bin/migrate migrations/*.sql
```

Ship `bin/trs`, `bin/migrate`, the `migrations/` directory, and
`deploy/trs.service`. Do not ship `.env`, frontend source maps, or test
databases.

```sh
APP_ENV=production ./bin/migrate up --confirm-database trs
```

The migration command requires a configured database name and verifies that the
selected database equals it (and, in production, the `--confirm-database`
value). It accepts only an empty first installation or a gap-free prefix of the
embedded migrations. Production `migrate down` is disabled.

Web startup never runs migrations; it refuses to start until every embedded
migration is applied. Goose history rows from other applications sharing the
database are ignored.

Route traffic only after `GET /ready` returns `200`. `/health` is process
liveness; `/ready` checks the application database and its applied schema.

## Database privileges

Use separate migration and runtime accounts when possible. Migrations require
DML plus `CREATE`, `ALTER`, `DROP`, `INDEX`, and `REFERENCES` on the application
database. Runtime requires `SELECT, INSERT, UPDATE, DELETE` there, and only
`SELECT` on DWH and MSO.

## systemd

`deploy/trs.service` is a template; adjust the user, paths, and environment file
before installation. Keep `/etc/trs/trs.env` root-owned with mode `0600`. The
unit makes the filesystem read-only except `/opt/trs/data`, which must hold
`SLIK_STORAGE_DIR`.

`TimeoutStopSec=60s` exceeds the default `APP_SHUTDOWN_TIMEOUT` of 45 seconds;
raise both together. SIGTERM first drains in-flight HTTP requests, then stops the
snapshot scheduler, SLIK workers (interrupted jobs resume from checkpoints), and
session cleanup, and finally logs out the Fincloud system session.

## Backup and restore

Store MySQL client credentials in a permission-restricted option file rather
than command arguments:

```sh
mysqldump --defaults-extra-file=/secure/mysql-client.cnf \
  --single-transaction --skip-add-locks --no-tablespaces --set-gtid-purged=OFF trs > trs.sql
sha256sum trs.sql > trs.sql.sha256
mysql --defaults-extra-file=/secure/mysql-client.cnf restored_trs < trs.sql
```

Back up `SLIK_STORAGE_DIR` with the database if unexpired SLIK outputs must
survive. After restore, run `migrate status`, start the application, check
`/ready`, and verify a Fincloud login.
