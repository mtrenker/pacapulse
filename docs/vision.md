# Project vision

PacaPulse is a small, self-hosted analytics service for operators managing
multiple websites and client relationships. It should answer practical traffic
and conversion questions while deliberately avoiding persistent visitor
profiles.

## Intended capabilities

- Manage organizations, sites, users, and read-only client access.
- Report page views, daily visitors, top pages, entry pages, and referrers.
- Track constrained, explicitly configured custom events and goals.
- Import sanitized traffic from reverse-proxy logs.
- Support an optional small browser beacon for SPAs and custom events.
- Authenticate users through OpenID Connect while keeping site grants local.
- Export a site's data in a documented format.
- Apply configurable raw-event and aggregate retention policies.

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
reverse-proxy logs ----> importer ---+
                                     +--> sanitizer --> SQLite --> dashboard
browser beacon --------> collector --+
```

The initial implementation should favor one statically deployable Go service,
a single-writer SQLite database, server-rendered administration pages, and a
small charting dependency. Collection writes may be batched when measurements
justify it, while preserving per-event atomicity and acknowledgement. The
accepted MVP persistence, aggregation, retention, and recovery contracts are
recorded in the [analytics data model](architecture/data-model.md).

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

- Advertising attribution or cross-site identities.
- Session replay, heatmaps, or invasive fingerprinting.
- Arbitrary unbounded event properties.
- Realtime analytics guarantees.
- High-availability embedded storage.
- ClickHouse-scale event ingestion.

## Open decisions

The MVP analytics data model resolves raw-event retention, aggregate structure,
daily-visitor semantics, and SQLite backup/restore boundaries. Remaining open
decisions are:

- Log-import cursor and deduplication behavior.
- Offline bot and optional geographic classification.
- Public API and tracker naming conventions.
