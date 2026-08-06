# Project vision

PacaPulse is a small, self-hosted analytics service for operators managing
multiple websites and client relationships. It should answer practical traffic
and conversion questions while deliberately avoiding persistent visitor
profiles.

## MVP capabilities

- Manage organizations, sites, users, and read-only client access.
- Install a small browser beacon for ordinary page loads and explicit SPA
  navigation.
- Report page views, daily visitors, daily trends, top pages, external referrer
  hosts, and bounded campaign fields for 7, 30, and 90 days.
- Authenticate users through OpenID Connect while keeping site grants local.
- Apply the accepted 30-day event, roughly 48-hour visitor-key, and 13-month
  aggregate retention policy.
- Delete a site's configuration, grants, events, visitor state, and aggregates.

Reverse-proxy importing, custom events and goals, entry-page/session semantics,
and public export are possible later capabilities rather than MVP scope.

## Privacy boundaries

The intended defaults are:

- Do not use analytics cookies or browser storage.
- Do not persist raw IP addresses or full user-agent strings.
- Remove URL query strings except for explicitly allowlisted campaign fields.
- Store external referrer hosts rather than full referrer URLs.
- Scope ephemeral visitor identifiers to a site and short time period.
- Reject unknown event properties, enforce size limits, and document that event
  values must not contain personal information.
- Respect Global Privacy Control, with Do Not Track behavior configurable.
- Make deletion by site or organization straightforward and auditable.

Privacy-focused defaults do not by themselves determine the legal requirements
for every deployment. Operators remain responsible for their notices, lawful
basis, retention choices, and agreements with clients.

## Initial architecture

```text
browser beacon --> privacy gate and sanitizer --> SQLite --> dashboard
```

The initial implementation favors one statically deployable Go service, a
single-writer SQLite database, server-rendered administration pages, and a small
charting dependency. Collection writes may be batched only when measurements
justify it and per-event atomicity and acknowledgement remain intact. The
accepted public transport is recorded in the [browser collection
protocol](architecture/collection-protocol.md); persistence, aggregation,
retention, and recovery are recorded in the [analytics data
model](architecture/data-model.md).

Authentication belongs to an external OIDC provider such as Authentik. PacaPulse
should key a login by its OIDC issuer and stable subject, retaining only the
profile information needed for its local organization and site grants.

## Initial resource goals

These are design targets rather than guarantees:

- 50–150 MiB idle memory.
- 25–50m Kubernetes CPU request.
- One application pod.
- One 2–5 GiB persistent volume for modest traffic.
- No required PostgreSQL, ClickHouse, Redis, or message broker.

## Explicit non-goals for the first release

- Reverse-proxy log importing.
- Custom events, goals, session or entry-page semantics, and public export.
- Advertising attribution or cross-site identities.
- Session replay, heatmaps, or invasive fingerprinting.
- Arbitrary unbounded event properties.
- Realtime analytics guarantees.
- High-availability embedded storage.
- ClickHouse-scale event ingestion.

## Deferred decisions

The MVP analytics data model and browser collection protocol resolve the
storage, visitor, retention, recovery, public naming, payload, origin, privacy,
and retry boundaries needed by the first release. Reverse-proxy cursor
semantics, offline bot or geographic classification, custom events, and public
reporting/export APIs are deliberately deferred until after the MVP.
