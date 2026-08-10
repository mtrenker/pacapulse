# PacaPulse

Lightweight, privacy-first web analytics for multiple sites.

> [!IMPORTANT]
> PacaPulse is under active MVP development. Its service, SQLite ingestion
> foundation, strict page-view collector, and browser beacon exist, but its
> administration, reports, and production deployment are not complete.

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
- **Bounded collection:** accept only documented page-view fields and reject
  arbitrary properties that could carry personal data.

## MVP direction

The MVP uses a small, transparent browser beacon for ordinary page loads and
explicit SPA navigation. It collects bounded page-view fields without cookies,
browser storage, full URLs, or arbitrary event properties. Reverse-proxy log
importing and custom events are post-MVP possibilities, not initial collection
paths.

The deployment target is one Go service backed by SQLite, with OIDC for
authentication and application-owned site authorization. See the [project
vision](docs/vision.md), [browser collection
protocol](docs/architecture/collection-protocol.md), and [analytics data
model](docs/architecture/data-model.md) for the accepted boundaries.

## Running the service

The repository currently contains the service baseline and its storage
foundation: a Go binary that loads and validates its configuration, creates and
migrates its SQLite database under an exclusive single-writer lock, and can
atomically record sanitized page views through an internal bounded writer. It
serves the privacy-first browser beacon at `GET /beacon/v1.js` and the strict
`POST /api/v1/pageviews` boundary, plus Kubernetes-shaped liveness and
readiness probes, and shuts down gracefully. Site administration, generated
installation snippets, and reports are not implemented yet.

```console
$ export PACAPULSE_VISITOR_SECRET="$(openssl rand -base64 32)"
$ PACAPULSE_BASE_URL=http://localhost:8080 go run ./cmd/pacapulse
$ curl http://localhost:8080/healthz
{"status":"alive"}
$ curl --silent --head http://localhost:8080/beacon/v1.js | head -n 2
HTTP/1.1 200 OK
Cache-Control: public, max-age=3600, stale-while-revalidate=86400
```

The beacon is deliberately transparent and deliberately blockable: it is
served byte for byte from
[`internal/beacon/beacon.js`](internal/beacon/beacon.js), with no build step
in between, so what you read is what a tracked page runs.

The database is created under `./data` by default; `PACAPULSE_DATA_DIR` moves
it. `make` lists the development commands, and `make verify` runs everything a
pull request is checked with. See [the development guide](docs/development.md)
for configuration, the storage lifecycle, the container build, and the local
smoke test.

## Status

MVP implementation in progress. Do not use PacaPulse for production analytics
yet.

## License

PacaPulse is licensed under the [GNU Affero General Public License v3.0](LICENSE).
This keeps modifications to network-hosted versions available to their users.
