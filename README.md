# PacaPulse

Lightweight, privacy-first web analytics for multiple sites.

> [!IMPORTANT]
> PacaPulse is in the planning stage. Its privacy model, architecture, and APIs
> are being designed in public before implementation begins.

## Why PacaPulse?

PacaPulse aims to provide useful website insights without third-party tracking,
persistent visitor identities, or a resource-heavy analytics stack. It is
intended for agencies and operators who manage several websites and need to give
clients access only to their own analytics.

## Principles

- **Privacy by default:** collect the minimum data needed for useful reports.
- **No persistent visitor tracking:** no analytics cookies or local storage.
- **No raw IP retention:** process addresses transiently and discard them.
- **Transparent behavior:** keep collection and aggregation logic inspectable.
- **Self-hosted and lightweight:** target a single small service with embedded
  storage for modest workloads.
- **Explicit tenant boundaries:** users see only organizations and sites they
  are authorized to access.
- **Safe extension points:** support custom events without allowing arbitrary
  personal data collection.

## Initial direction

PacaPulse is expected to combine two optional collection paths:

1. Sanitized page-view aggregation from reverse-proxy access logs.
2. A tiny browser beacon for SPA navigation and explicit custom events.

The first deployment target is a single Go service backed by SQLite, with OIDC
for authentication and application-owned site authorization.

See [the project vision](docs/vision.md) for the current scope and open design
decisions.

## Running the service

The repository currently contains the service baseline and its storage
foundation: a Go binary that loads and validates its configuration, creates and
migrates its SQLite database under an exclusive single-writer lock, serves
Kubernetes-shaped liveness and readiness probes, and shuts down gracefully. It
does not collect or report analytics yet.

```console
$ PACAPULSE_BASE_URL=http://localhost:8080 go run ./cmd/pacapulse
$ curl http://localhost:8080/healthz
{"status":"alive"}
```

The database is created under `./data` by default; `PACAPULSE_DATA_DIR` moves
it. `make` lists the development commands, and `make verify` runs everything a
pull request is checked with. See [the development guide](docs/development.md)
for configuration, the storage lifecycle, the container build, and the local
smoke test.

## Status

Planning and prototyping. Do not use PacaPulse for production analytics yet.

## License

PacaPulse is licensed under the [GNU Affero General Public License v3.0](LICENSE).
This keeps modifications to network-hosted versions available to their users.
