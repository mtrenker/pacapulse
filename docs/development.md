# Development

This document covers how to configure, run, test, and package the PacaPulse
binary, the SQLite lifecycle, the strict public page-view collector, the
browser beacon it serves, and the internal page-view storage contract.
Reporting, retention, backup, authentication, and deployment do not exist yet.

## Prerequisites

- Go 1.26.5 or newer. The module targets Go 1.26; CI and the container build
  both pin 1.26.5.
- GNU Make.
- Node 24, for `make test-beacon` — which `make verify` runs. The version is
  pinned in `.node-version` and CI installs exactly that one. Node is a
  validation tool: it runs the browser asset's test suite and measures its
  Brotli size, which Go's standard library cannot. It never enters the binary,
  the container image, or the runtime, and the repository has no
  `package.json`, no npm dependencies, and no lockfile. Point the Makefile at
  a specific interpreter with `make test-beacon NODE=/path/to/node`.
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
| `make test-beacon` | Run the browser asset's Node tests and its size gate |
| `make cover` | Report per-package coverage |
| `make build` | Build `bin/pacapulse` with the version stamped in |
| `make image` | Build the production container image |
| `make smoke` | Build the image and smoke-test it as a container |
| `make verify` | Everything a pull request is checked with, minus the image |

`make verify` is the gate to run before pushing: it is `fmt-check`, `vet`,
`lint`, `test-race`, and `test-beacon` in one command, matching the
pull-request workflow.

### Starting the service

```console
$ make run
{"time":"...","level":"INFO","msg":"starting pacapulse","version":"dev","shutdown_timeout":"15s"}
{"time":"...","level":"INFO","msg":"http server listening","addr":"[::]:8080"}
```

Or without Make:

```console
$ PACAPULSE_VISITOR_SECRET="$(openssl rand -base64 32)" \
  PACAPULSE_BASE_URL=http://localhost:8080 go run ./cmd/pacapulse
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
| `PACAPULSE_VISITOR_SECRET` | yes | — | Strict RFC 4648 standard base64 that decodes to at least 32 random bytes. Used only in memory for transient source limiting and daily visitor estimates. |
| `PACAPULSE_TRUSTED_PROXY_CIDRS` | no | blank | Comma-separated trusted proxy CIDR prefixes. Blank trusts no forwarding proxy; malformed lists prevent startup. |

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
$ PACAPULSE_VISITOR_SECRET="$(openssl rand -base64 32)" go run ./cmd/pacapulse
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
| `GET /beacon/v1.js` | The public browser asset. Cacheable, validated, cross-origin readable, and offered as identity or gzip. `HEAD` works; every other method is an empty `405`. |
| `POST /api/v1/pageviews` | Strict browser page-view collection. Every response is empty and non-cacheable; accepted CORS is reflected only after site and origin approval. |

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

## The browser beacon

`internal/beacon/beacon.js` is the public asset served at `GET /beacon/v1.js`.
It is embedded with `go:embed` and served **byte for byte**: there is no build
step, bundler, minifier, or template between the file in the repository and
the script a tracked page executes, and a test compares the served body with
the file on disk to keep it that way. That is what makes the beacon a
reviewable privacy boundary instead of a build artifact — and it is why the
asset is a classic script written in plain ES5-shaped JavaScript rather than
something that would need compiling.

The normative behaviour is in
[the collection protocol](architecture/collection-protocol.md). What follows is
the reasoning that does not fit inside the asset's size budget.

### Installing it

```html
<script
  defer
  referrerpolicy="no-referrer"
  src="https://pulse.example.com/beacon/v1.js"
  data-site="pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ">
</script>
```

Everything configurable comes from that tag. The collection key is the
`data-site` attribute, and the collector origin is derived from the resolved
`src` — the asset never hard-codes a hostname, so a self-hosted installation
works without a rebuild. A missing `document.currentScript`, a malformed key,
or an unusable `src` stops the script silently before it installs anything.

A site whose stored `collect_dnt` policy is enabled additionally receives
`data-collect-dnt="true"`. Only that exact string is an override, and all it
does is add `"dnt": true` to the body; the server's own policy still decides.

Tracked sites that use CSP need both directives:

```http
Content-Security-Policy: script-src 'self' https://pulse.example.com; connect-src 'self' https://pulse.example.com
```

The asset uses no inline script, no `eval`, no `new Function`, and no dynamic
import, so it is compatible with a policy that forbids all of those. A site
using `'strict-dynamic'` must give the beacon tag its own nonce or load it
from an already trusted script: a host allowlist alone is ignored there.

Explicit SPA route changes call `window.pacapulse?.("pageview")` after the
router has committed and `location` reflects the new route. The beacon
deliberately patches no history API, router, link, or observer.

### Why the privacy gate runs first

GPC and default DNT suppression happen before the script generates a UUID,
reads `location.pathname`, `location.search`, or `document.referrer`, or
touches the network. A gate that only skipped the `fetch` would still have
built a page-view record in a document that asked not to be measured, and the
ordering would be invisible to a reviewer. The Node tests prove the ordering
rather than the outcome: suppressed cases run with each of those reads
replaced by a throwing mock, so a gate that ran one step too late fails
loudly.

The same gate re-runs for prerender activation, bfcache restoration, and every
explicit SPA call, because a visitor can change the setting mid-document.

### Why a generic failure is never retried

The beacon retries exactly once, and only for its own five-second timeout or a
CORS-visible `429`/`503`. Every silent rejection path — unknown key, disabled
site, rejected origin, early body or media-type refusal, instance-level rate
limiting — reaches the browser as an opaque `TypeError` that reveals nothing
about which one it was. Retrying those would double exactly the traffic the
collector already decided not to accept, and would invalidate the abuse-rate
calculations the fixed token buckets are sized against. The timeout is
recognised by the script's own flag rather than by an `AbortError` name,
because browsers also use that name for aborts the script did not cause.

### The size budget

The protocol caps the asset at 12 KiB uncompressed and 4 KiB compressed, and
`make verify` measures the exact embedded bytes three ways:

| Representation | Budget | Measured by |
| --- | --- | --- |
| Source | 12 KiB | `internal/beacon` Go test and the Node size test |
| gzip (level 9) | 4 KiB | `internal/beacon` Go test and the Node size test |
| Brotli (quality 11) | 4 KiB | the Node size test |

Brotli is measured in Node because Go's standard library has no Brotli
encoder. The service itself only offers identity and gzip, which is what
ordinary `Accept-Encoding` negotiation needs; the Brotli figure is a budget
check on the asset, not a promise about the wire.

The budget is tight enough that prose has to earn its place in the file. When
a comment explains something a reader needs but the budget cannot hold, it
belongs in this document instead — the size test prints the exact remaining
headroom on every run.

### Serving it

`internal/beacon` computes both representations and both validators once at
construction; the asset cannot change while the process runs, so compressing
per request would only spend CPU on a response that caches for an hour.

```text
Content-Type: application/javascript; charset=utf-8
Cache-Control: public, max-age=3600, stale-while-revalidate=86400
ETag: <content validator>
Vary: Accept-Encoding
X-Content-Type-Options: nosniff
Cross-Origin-Resource-Policy: cross-origin
```

The two representations carry **different** strong validators. They differ
byte for byte, so sharing one is how a shared cache ends up answering a gzip
request with identity bytes labelled as compressed. Negotiation follows RFC
9110: identity is acceptable unless `identity;q=0` or a `*;q=0` without a more
specific entry removes it, gzip is only sent when it was asked for by name or
through a positive wildcard, quality values decide ties in the client's
favour, and a request that accepts nothing this handler can produce gets an
empty `406`.

### Testing it

The browser tests are `node --test` files under `internal/beacon/jstest/`,
using only Node's built-in runtime — `node:test`, `node:assert`, `node:vm`,
and `node:zlib`. There is no test framework, no DOM library, and no npm
dependency of any kind.

`harness.mjs` builds a fake window and evaluates the **real asset** in a
`node:vm` context. Everything the beacon can observe is deterministic: a fake
clock instead of timers, a counted and controllable Web Crypto, and captured
`fetch` calls a test resolves or rejects by hand. Because every privacy-
sensitive read is counted, a test can assert what did *not* happen.

`extraction.test.mjs` is generated from
`docs/architecture/collection-protocol-fixtures.json` — the same file the Go
collector tests consume. Adding a `browser_extraction` case to that file adds
a case here without editing the test, which is the point: a case the two
implementations disagree about has to be resolved in the protocol rather than
in one side's test file.

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

### Recording a page view

`storage.Database.RecordPageView` is the only way anything is written to the
analytics tables. It is an internal Go API, not an endpoint: the strict public
collector calls it only after origin, privacy, transient-input, limiting, and
canonicalization gates have produced this sanitized shape.

```go
result, err := store.RecordPageView(ctx, storage.PageView{
    SiteID:         siteID,          // canonical lowercase UUIDv4
    EventID:        eventID,         // canonical lowercase UUIDv4, the idempotency key
    ReceivedAt:     receivedAt,      // one server instant
    Path:           "/pricing",      // ≤512 UTF-8 bytes, leading '/', no '?' or '#'
    ReferrerHost:   "news.example.com", // optional, 1–253 bytes
    CampaignSource: "newsletter",    // optional utm_*, ≤100 bytes each
    CampaignMedium: "email",
    CampaignName:   "spring-launch",
    VisitorKey:     key,             // optional, exactly 32 bytes
})
```

The input is **already sanitized**. There is no field for a client address, a
user-agent header, a full referrer URL, or a query string, so deciding what a
request means stays in the collector and only bounded canonical values reach
SQLite. The zero value of every optional field means absent.

Both identifiers must be canonical lowercase **UUIDv4** — 8-4-4-4-12 lowercase
hex with the version nibble `4` and the variant nibble in `8`–`b`. The text form
is the idempotency key, so an upper-case or brace-wrapped spelling of the same
UUID would be a second event; and the data model specifies random v4, so the nil
UUID and a v1 carrying a MAC address and a timestamp are refused rather than
stored as if they were random.

Two things the caller deliberately does not control:

- **The aggregate day.** It is derived from `ReceivedAt` in UTC inside the
  package, so a row can never be filed under a day its own timestamp does not
  fall in. A request delayed across midnight belongs to the day the server
  accepted it; there is no backdating path.
- **The cardinality limits.** 2,000 paths, 500 referrer hosts, and 500 campaign
  tuples per site and UTC day, fixed. A value the day already knows keeps its
  exact row; a new one past the limit is counted in that family's single
  reserved overflow row, which reports as **Other**. This bounds a site/day to
  3,004 aggregate rows even under forged input.

The whole page view is validated before a transaction is opened, so a rejected
one has nothing to undo. Everything after that is one `BEGIN IMMEDIATE`
transaction — ledger row, site and page totals, referrer and campaign totals
when present, visitor-deduplication rows when a visitor key is — and the call
returns only after it commits. **A successful return means the measurement is
durable; a failed one means no part of it was kept.** Tests prove this by
injecting a failure at each stage and comparing every table's counts.

Repeating a `(SiteID, EventID)` pair is a success whose result reports
`Duplicate`. The first accepted delivery stands, nothing is counted again, and a
repeat carrying a different payload is discarded rather than merged.

| Error | Meaning | Retryable |
| --- | --- | --- |
| `ErrInvalidPageView` | The input does not satisfy the contract | no |
| `ErrUnknownSite` | The site does not exist, or was deleted while queued | no |
| `ErrQueueFull` | The writer already has all the work it accepts | yes |
| `ErrUnavailable` | SQLite is busy, or the volume is full | yes |
| `ErrClosed`, `ErrUninitialized` | The store is not accepting writes | no |
| `ErrWrite`, `ErrIntegrity` | The write failed and nothing was kept | no |
| `ctx.Err()` | The caller gave up | — |

Like every other diagnostic in the package, these name the field, the stage, or
the condition and never the value: no site or event identifier, path, referrer,
campaign value, visitor key, SQL text, or database path appears in an error a
caller or the log can see.

SQLite itself is the one source that does not obey that rule — its message for a
failed statement names the table, the column, and the constraint. An ingestion
error therefore reports the numeric result code instead (`sqlite result code
3091`), which is enough to look the condition up and names nothing. The startup
failure a database is refused for is reduced the same way: `RecordPageView`
returns the bare class, while the stage and SQLite's own words stay in readiness
and the startup log, where an operator reads them.

`VisitorKey` is copied before the page view is queued. It is the caller's
buffer, and a collection request that times out while queued is free to reuse
it; sharing the slice would be both a data race and a way for one visitor's
deduplication identity to become another's.

### The writer queue and shutdown

Every write is serialized through one worker goroutine that owns the transaction
path, so SQLite's single writer is enforced inside the process as well as
between processes by the advisory lock. Callers hand work to a **queue bounded
at 256** page views.

The bound is fixed rather than tunable, for the same reason the cardinality
limits are. A full queue holds well under a megabyte of bounded records, and
drains in roughly a second at one immediate transaction per event with
`synchronous = FULL` — about as long as a collection request stays useful. A
deeper queue would only accept work whose caller has already given up. When it
is full, `RecordPageView` returns `ErrQueueFull` **immediately**: the data model
requires backpressure rather than an unbounded wait or a silently dropped
acknowledgement. The case that makes this visible is a busy writer, where a
single attempt can wait out the 5s `busy_timeout` while the queue fills behind
it.

A caller whose context is cancelled while queued is abandoned without being
executed, and gets its context error. Once the worker has started a transaction
it runs to completion under the worker's own context: interrupting SQLite
mid-statement costs the pool its one connection, and an event that has started
is cheaper to finish than to tear down.

`Close` is the shutdown path, and its order is what makes it safe:

1. New page views are refused with `ErrClosed`.
2. The worker is told to stop and waited for, so the transaction already in
   flight reaches its commit or its rollback.
3. Everything still queued is answered with `ErrClosed`. None of it was
   executed, so none of it is acknowledged.
4. The database is closed and the advisory lock released.

No caller is left waiting on a writer that has stopped, no goroutine outlives
`Close`, and nothing uncommitted is ever reported as a success.

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
    -e PACAPULSE_BASE_URL=http://localhost:8080 \
    -e PACAPULSE_VISITOR_SECRET="$(openssl rand -base64 32)" pacapulse:dev
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
- `GET /beacon/v1.js` returns the embedded asset with every normative header,
  the same bytes under gzip negotiation, a `304` for its own validator, and a
  `405` for `POST`;
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
  runs formatting, `go vet`, `golangci-lint`, race-enabled tests, the browser
  beacon's Node tests and size gate, and a container build plus smoke test. It
  uses the `pull_request` event, so a fork
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

The MVP targets 50–150 MiB idle memory and a 25–50m CPU request. The figures
above predate the public collector and describe an idle process with no
templates; the limiter maps, page cache, and report result sizes that will
dominate a loaded service are not represented. The writer queue is bounded
at 256 bounded records, so it contributes well under a megabyte at saturation.
This is a starting point for later comparison, not a claim about the finished
service.

## Repository layout

```text
cmd/pacapulse      process entry point: flags, exit codes, signal handling
internal/app       wiring of config, storage, collector, beacon, health, router, server
internal/beacon    the embedded browser asset, its serving contract, and its tests
internal/collector strict page-view validation, privacy, transient HMACs, and limits
internal/config    environment loading and fail-fast validation
internal/health    liveness and readiness state, dependency check registry
internal/httpapi   route registration, probe handlers, privacy-safe request logging
internal/logging   logger construction
internal/server    bounded HTTP server and graceful shutdown
internal/storage   SQLite lifecycle and the serialized page-view write path
internal/version   build identity
```

Third-party dependencies are added when they remove concrete complexity, not in
anticipation of it. There is one: `modernc.org/sqlite`, a pure-Go translation of
SQLite. It is what lets the production binary stay `CGO_ENABLED=0` and static on
a distroless base while embedding a real database; a cgo driver would trade that
for a libc-bearing runtime image.
