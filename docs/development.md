# Development

This document covers the service baseline: how to configure, run, test, and
package the PacaPulse binary, and the SQLite database lifecycle it owns. It does
not describe analytics collection, reporting, retention, backup, authentication,
or deployment — none of those exist yet.

## Prerequisites

- Go 1.26.5 or newer. The module targets Go 1.26; CI and the container build
  both pin 1.26.5.
- GNU Make.
- Docker, for the container build and the smoke test only.
- `golangci-lint` v2 for `make lint`.

## Commands

Run `make` on its own for the list. The ones that matter day to day:

| Command | What it does |
| --- | --- |
| `make run` | Start the service locally on `:8080` with text logs |
| `make fmt` | Format the sources in place |
| `make fmt-check` | Fail if anything is not gofmt-clean |
| `make vet` | Run `go vet` |
| `make lint` | Run `golangci-lint` |
| `make test` | Run the tests |
| `make test-race` | Run the tests under the race detector |
| `make cover` | Report per-package coverage |
| `make build` | Build `bin/pacapulse` with the version stamped in |
| `make image` | Build the production container image |
| `make smoke` | Build the image and smoke-test it as a container |
| `make verify` | Everything a pull request is checked with, minus the image |

`make verify` is the gate to run before pushing: it is `fmt-check`, `vet`,
`lint`, and `test-race` in one command, matching the pull-request workflow.

### Starting the service

```console
$ make run
{"time":"...","level":"INFO","msg":"starting pacapulse","version":"dev","shutdown_timeout":"15s"}
{"time":"...","level":"INFO","msg":"http server listening","addr":"[::]:8080"}
```

Or without Make:

```console
$ PACAPULSE_BASE_URL=http://localhost:8080 go run ./cmd/pacapulse
```

## Configuration

Configuration comes from the environment only. It is validated at startup and
the process refuses to run if anything is wrong, reporting every problem at
once.

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `PACAPULSE_BASE_URL` | yes | — | Public origin the service is reachable at. Absolute `http`/`https` URL with no credentials, query, or fragment. A trailing slash is trimmed. |
| `PACAPULSE_ADDR` | no | `:8080` | TCP listen address in `host:port` form. Port `0` selects an ephemeral port. |
| `PACAPULSE_DATA_DIR` | no | `./data` | Directory the service keeps its SQLite database and advisory lockfile in. Created with mode `0700` when absent. Only the directory is configurable — see [Storage](#storage). |
| `PACAPULSE_LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. |
| `PACAPULSE_LOG_FORMAT` | no | `json` | One of `json`, `text`. |
| `PACAPULSE_SHUTDOWN_TIMEOUT` | no | `15s` | Grace period for in-flight requests after a termination signal. Must be positive and at most `25s`, which keeps it below the Kubernetes default 30s termination grace period. |

An unset variable and a variable set to whitespace are treated the same way, so
an empty ConfigMap key falls back to the default instead of failing.

`PACAPULSE_BASE_URL` is required now — before anything depends on it — so that
every environment is forced to declare its public origin once, rather than
discovering the requirement when beacon snippets and OIDC redirect URIs arrive.

The HTTP server's read, write, header, and idle timeouts are fixed constants in
`internal/config`. They exist to cap what a single connection can hold, and no
workload has yet justified making them tunable.

### Diagnostics never echo values

A configuration error names the variable and the expectation, never the value:

```console
$ go run ./cmd/pacapulse
pacapulse: invalid configuration: PACAPULSE_BASE_URL is required and must be an absolute http or https URL, for example https://pulse.example.com
```

This is a deliberate contract, covered by tests in `internal/config` and
`cmd/pacapulse`. When a later issue adds a secret — an OIDC client secret, a
setup token — it inherits the guarantee only if validation keeps reporting
names rather than contents.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness. `200 {"status":"alive"}` for as long as the process is serving. Never checks dependencies. |
| `GET /readyz` | Readiness. `200 {"status":"ready"}`, or `503` with `{"status":"draining"}` during termination and `{"status":"unready","failed":["…"]}` when a dependency check fails. The database reports itself as `database`. |

The split matters operationally: a failing dependency must take the instance out
of the load balancer, not make Kubernetes restart an otherwise healthy process.
Liveness therefore stays `200` while readiness fails.

Readiness responses list the *names* of failing checks and nothing else. The
underlying error — which can carry a hostname or connection string — goes to the
log, which is operator-only.

Later issues register their dependency checks on the readiness registry
(`app.Health().Register(...)`) without touching the router or the server.

## Logging

Logs are structured, written to stdout, and describe the process rather than its
traffic. The rule, enforced by tests in `internal/httpapi` and `internal/app`:

- Request headers, query strings, request bodies, cookies, client addresses, and
  configured values must never be passed to a logger.
- The per-request record contains the method, the **matched route pattern**, the
  status, and the duration. The route pattern comes from the routing table, so
  no visitor-supplied string reaches the log; a request that matches no route is
  recorded as `unmatched`.
- Successful requests log at `debug`, so Kubernetes probe traffic stays out of
  the default log. Responses of `500` and above log at `error`.
- `net/http`'s own transport error log — which can embed a peer address — is
  routed at `debug` for the same reason.

## Storage

PacaPulse owns one SQLite database, written by one process. The accepted
contracts it implements — schema, page size, migrations, single-owner
enforcement, failure behaviour — are in
[the analytics data model](architecture/data-model.md).

### The data directory

`PACAPULSE_DATA_DIR` names a directory, not a database. The file names beneath
it belong to the application and no SQLite DSN is accepted from the environment,
so a deployment cannot point the service at shared or unexpected storage:

```text
<PACAPULSE_DATA_DIR>/pacapulse.db      the analytics database
<PACAPULSE_DATA_DIR>/pacapulse.db-wal  the write-ahead log
<PACAPULSE_DATA_DIR>/pacapulse.db-shm  the WAL index
<PACAPULSE_DATA_DIR>/pacapulse.lock    the exclusive advisory lockfile
```

A directory the service creates gets mode `0700` and the database `0600`; SQLite
copies the database's mode onto the WAL files, so nothing on the volume is
group- or world-readable. An existing directory's permissions are left alone —
on a mounted volume they belong to the platform.

The container image sets `PACAPULSE_DATA_DIR=/data` and deliberately does not
create it. A container started without a volume mounted there fails on startup
rather than writing analytics into a layer that disappears with it.

### Startup

1. Create the data directory if it is missing.
2. Take an exclusive `flock` on the lockfile without blocking, and hold it until
   the process exits.
3. Create the database file with owner-only permissions if it is missing.
4. Require `PRAGMA quick_check` to return `ok`, before anything is written to a
   file that may not be a database.
5. Set `page_size` and `auto_vacuum` — which SQLite only honours while the file
   has no tables — then `journal_mode`.
6. Verify every required setting by asking SQLite what it is actually doing.
7. Bootstrap the migration ledger, validate every applied migration's checksum,
   refuse a schema newer than the binary, and apply what is pending.
8. Verify the settings again, then run `PRAGMA foreign_key_check` and the four
   explicit aggregate/visitor orphan checks.

Shutdown closes the database and releases the lock inside the termination grace
period, so the volume is free before the process exits.

### The single writer

A Kubernetes `ReadWriteOnce` volume is node-scoped; it is not a process-level
lock. The advisory lockfile is, and every writing entry point — today the
server, later the restore-finalization command and any maintenance tool — must
hold it for its whole lifetime. A second process against the same directory is
refused at startup:

```console
$ pacapulse
{"level":"ERROR","msg":"pacapulse failed to start","error":"another process owns the database lock: acquire lock: resource temporarily unavailable"}
```

### Migrations

Migrations live in `internal/storage/migrations` and are embedded in the binary,
so a build always carries the schema it expects. They are forward-only: there
are no down-migrations, and recovery from an incompatible change is a restore,
not an improvised reversal.

- The file name is `NNNN_description.sql`, with versions consecutive from `0001`.
- `schema_migrations` records the integer version, the SHA-256 of the file's
  exact bytes, and a UTC timestamp the application assigns.
- Each migration runs in one transaction with its ledger row, so a failure
  leaves the previous committed schema and ledger untouched.
- Editing an applied migration is caught by its checksum and makes that database
  refuse to serve. Change the schema by adding a migration.

`docs/architecture/data-model.sql` is the accepted reference schema and is never
executed by the service. A test applies it to a scratch database and compares
every resulting object against the migrated one, so the two cannot drift apart.

### When the database is unusable

Two failures stop the process, because there is nothing to run without them: an
unusable data directory, and a lock another process owns. Both exit `1`.

Everything else — content that is not a database, a failed integrity check, a
checksum mismatch, a schema newer than the binary, a failed migration, a failed
post-migration check — keeps the process alive so it can be diagnosed. Liveness
stays `200`, readiness reports `{"status":"unready","failed":["database"]}`, and
the reason is remembered rather than retried: PacaPulse does not attempt
automatic repair.

Storage diagnostics name the failure and the stage that produced it, never the
path, the SQL, or any stored value:

```text
database failed its integrity check: integrity check: file is not a database (26)
data directory is unusable: create data directory: mkdir: permission denied
```

### Inspecting a database

Inspect a **copy**, not the live database. The running service holds the
exclusive lock, and the data model requires that no second process writes the
volume — an editing session against a live database is how a single-writer store
loses its guarantees.

```console
$ cp data/pacapulse.db /tmp/inspect.db          # service stopped, or a backup
$ sqlite3 /tmp/inspect.db 'PRAGMA quick_check;'
$ sqlite3 /tmp/inspect.db 'PRAGMA page_size; PRAGMA auto_vacuum; PRAGMA journal_mode;'
$ sqlite3 /tmp/inspect.db 'SELECT version, checksum, applied_at_ms FROM schema_migrations;'
$ sqlite3 /tmp/inspect.db '.schema daily_site_stats'
```

The service reports the version it migrated to at startup, which is the cheaper
answer to "what schema is running?":

```console
{"level":"INFO","msg":"database ready","schema_version":1}
```

## Process lifecycle

- `SIGINT` and `SIGTERM` start termination.
- Readiness immediately reports `draining` so traffic is routed elsewhere.
- The listener stops accepting connections; in-flight requests get up to
  `PACAPULSE_SHUTDOWN_TIMEOUT` (default 15s) to finish.
- The database is closed and the advisory lock released once draining ends.
- The process exits `0` once draining completes. If the grace period expires
  first, remaining connections are closed and the process exits `1` with the
  reason logged, so an unclean termination is visible rather than silent.

Exit codes:

| Code | Meaning |
| --- | --- |
| `0` | Clean shutdown |
| `1` | Runtime failure, including an exceeded shutdown grace period |
| `2` | Configuration or usage error; the process never started |

## Container

```console
$ make image                 # build ghcr.io-shaped image locally as pacapulse:dev
$ make smoke                 # build it and run the full container smoke test
$ IMAGE=ghcr.io/mtrenker/pacapulse:<sha> BUILD=0 scripts/smoke-test.sh
$ docker run --rm -v "$PWD/data:/data" -p 8080:8080 \
    -e PACAPULSE_BASE_URL=http://localhost:8080 pacapulse:dev
```

The build is multi-stage. The build stage is `golang:1.26.5-alpine`, pinned by
digest; the runtime stage is `gcr.io/distroless/static-debian12:nonroot`, also
pinned by digest. The binary is built with `CGO_ENABLED=0` and `-trimpath`, and
the commit SHA is stamped in so a running container can report the source it was
built from with `pacapulse -version`.

The runtime image has no shell, no package manager, and no Go toolchain, and
runs as `nonroot` (uid 65532). Because there is no shell or `curl`, the OCI
`HEALTHCHECK` is the service binary probing its own readiness endpoint over
loopback:

```console
$ docker run --rm pacapulse:dev -health-check   # exit 0 healthy, 1 unhealthy
```

`scripts/smoke-test.sh` checks the properties the image is supposed to
guarantee, not just that it starts:

- the image config runs as a non-root user and its entrypoint is the service
  binary;
- the exported filesystem contains the binary and no Go toolchain;
- both health endpoints answer over a published port;
- the OCI health check reaches `healthy`;
- missing configuration exits `2` naming the variable, and an invalid value is
  reported without being echoed;
- a writable mounted volume receives a migrated database, whose data directory
  is mode `700` and whose files are owner-only — read back through `docker cp`,
  because the host user is not the container's user and cannot open them;
- a second container against the same volume is refused the exclusive lock and
  exits `1` without naming the path;
- a clean shutdown leaves no write-ahead log, so the database was closed rather
  than abandoned;
- container logs contain no configuration values or request metadata;
- `docker stop` (SIGTERM) produces exit code `0` inside the grace period.

## Continuous integration

Two workflows, split so that untrusted code never runs with credentials that can
publish:

- `.github/workflows/ci.yml` runs on pull requests and on pushes to `main`. It
  runs formatting, `go vet`, `golangci-lint`, race-enabled tests, and a
  container build plus smoke test. It uses the `pull_request` event, so a fork
  contribution runs with a read-only token and no access to secrets, and it
  never pushes an image.
- `.github/workflows/publish.yml` runs only on pushes to `main`, which cannot be
  triggered by a fork. It is the only workflow granted `packages: write`.

Images are published as `ghcr.io/mtrenker/pacapulse:sha-<full commit sha>` and
by digest. No mutable tag such as `latest` is published, so a deployment
reference can never silently change meaning.

## Baseline resource usage

Measured locally on Linux/amd64 with the stripped binary, idle after migrating a
fresh database and answering health probes:

| Measure | Baseline | With storage |
| --- | --- | --- |
| Resident memory (`VmRSS`) | ~9.3 MiB | ~12.2 MiB |
| Binary size | ~6.5 MiB | ~10.4 MiB |
| Migrated empty database | — | ~248 KiB |

The pure-Go SQLite translation is what the binary grew by; it is the price of a
statically linked database on a distroless base, and it is paid once rather than
per site.

The MVP targets 50–150 MiB idle memory and a 25–50m CPU request. The service
sits well inside that, but it has no templates and no collection path yet, and
the page cache, writer queue, and result sizes that will dominate a loaded
process do not exist. This is a starting point for later comparison, not a claim
about the finished service.

## Repository layout

```text
cmd/pacapulse      process entry point: flags, exit codes, signal handling
internal/app       wiring of config, storage, health, router, and server
internal/config    environment loading and fail-fast validation
internal/health    liveness and readiness state, dependency check registry
internal/httpapi   router, probe handlers, privacy-safe request logging
internal/logging   logger construction
internal/server    bounded HTTP server and graceful shutdown
internal/storage   SQLite lifecycle: lock, settings, migrations, integrity
internal/version   build identity
```

Third-party dependencies are added when they remove concrete complexity, not in
anticipation of it. There is one: `modernc.org/sqlite`, a pure-Go translation of
SQLite. It is what lets the production binary stay `CGO_ENABLED=0` and static on
a distroless base while embedding a real database; a cgo driver would trade that
for a libc-bearing runtime image.
