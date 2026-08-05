# Development

This document covers the service baseline: how to configure, run, test, and
package the PacaPulse binary. It does not describe analytics collection,
storage, authentication, or deployment — none of those exist yet.

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
| `GET /readyz` | Readiness. `200 {"status":"ready"}`, or `503` with `{"status":"draining"}` during termination and `{"status":"unready","failed":["…"]}` when a dependency check fails. |

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

## Process lifecycle

- `SIGINT` and `SIGTERM` start termination.
- Readiness immediately reports `draining` so traffic is routed elsewhere.
- The listener stops accepting connections; in-flight requests get up to
  `PACAPULSE_SHUTDOWN_TIMEOUT` (default 15s) to finish.
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

Measured locally on Linux/amd64 with the stripped binary, idle after answering
health probes:

| Measure | Observed |
| --- | --- |
| Resident memory (`VmRSS`) | ~9.3 MiB |
| Binary size | ~6.5 MiB |

The MVP targets 50–150 MiB idle memory and a 25–50m CPU request. The baseline
sits well inside that, but it has no storage, no templates, and no collection
path yet — this is a starting point for later comparison, not a claim about the
finished service.

## Repository layout

```text
cmd/pacapulse      process entry point: flags, exit codes, signal handling
internal/app       wiring of config, health, router, and server
internal/config    environment loading and fail-fast validation
internal/health    liveness and readiness state, dependency check registry
internal/httpapi   router, probe handlers, privacy-safe request logging
internal/logging   logger construction
internal/server    bounded HTTP server and graceful shutdown
internal/version   build identity
```

The service uses the Go standard library only. Third-party dependencies are
added when they remove concrete complexity, not in anticipation of it.
