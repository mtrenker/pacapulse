# MVP analytics data model

Status: accepted architecture decision for [issue #2](https://github.com/mtrenker/pacapulse/issues/2) and the [`v0.1.0` MVP](https://github.com/mtrenker/pacapulse/issues/1).

This document defines the SQLite persistence, visitor-estimation, aggregation, retention, migration, backup, and recovery contracts for the first useful PacaPulse analytics loop. It deliberately optimizes for understandable traffic trends rather than exact identification. The public request, browser behavior, origin policy, and privacy gate are defined by the [MVP browser collection protocol](collection-protocol.md).

The executable reference DDL is in [`data-model.sql`](data-model.sql). Production schema changes must still be delivered as ordered migrations; the reference file is an architecture artifact, not a migration runner.

## Decisions at a glance

- Use one Go service, one SQLite database, one application replica, and one serialized writer.
- Persist sanitized page-view events for 30 days, but never persist their IP address, full user agent, query string, full referrer URL, or visitor key.
- Derive a full 32-byte HMAC from a transient canonical IP and coarse browser/device class. Scope it cryptographically to one site and UTC day.
- Keep visitor-deduplication rows for only the current and previous UTC day (at most about 48 hours).
- Update four explicit daily aggregate families—site, page, external referrer, and campaign—in the same transaction as the event ledger.
- Bound attacker-controlled aggregate cardinality with fixed per-site/day limits and explicit overflow rows.
- Retain daily aggregates for 13 calendar months.
- Run data retention inside the application. Use a Kubernetes CronJob only to schedule an application-owned, consistent SQLite snapshot outside the live PVC.
- Restore into a fresh PVC and replay the external deletion journal. Never overwrite the failed volume during recovery.
- Use forward-only, checksummed startup migrations before the service becomes ready.

## Product meaning

A PacaPulse visitor is a **site- and day-scoped estimate**, not a person, browser profile, or session. The same person can be counted more than once after changing network or device. People using the same public address and coarse browser/device class can be combined. These limitations are expected and acceptable because the product answers trend questions rather than identity questions.

Counts for page, referrer, and campaign dimensions are not additive. One daily visitor may appear under several pages, referrers, or campaigns while counting only once in the site total. Product copy must use **daily visitors** and must not claim unique people across a multi-day range.

## Privacy and retention classification

| Data | Examples | Persistence | Retention |
| --- | --- | --- | --- |
| Site configuration | Random site ID, public collection key, name, canonical host, DNT policy | SQLite | Until site or organization deletion |
| Sanitized event | Server receive time, path, external referrer host, bounded UTM values | SQLite | Rolling 30 days |
| Transient request input | Client IP, full user-agent header, GPC/DNT headers | Memory only | One request |
| Daily visitor key | Site/day-scoped HMAC | Deduplication tables only | Current and previous UTC day |
| Daily aggregates | Page views and daily visitors by known dimensions | SQLite | Rolling 13 calendar months |
| Deletion audit | Random site ID, actor's internal identity ID, time, row counts | SQLite | 13 calendar months |
| Backup | Encrypted SQLite snapshot and encrypted manifest | Dedicated object storage | At most 7 days |

Paths and campaign values can themselves contain personal information if a site operator puts it in a URL. PacaPulse strips queries and bounds all values, but it cannot reliably infer whether an arbitrary path segment or campaign name is personal data. Installation guidance must prohibit personal information in paths and UTM values. Future configurable rewrite rules may improve this; guessing with an unreliable PII detector is not part of the MVP.

## Collection gate and canonicalization

Sanitization happens before a write transaction. Rejected requests create no event, visitor key, aggregate, or request-body log.

The [browser collection protocol](collection-protocol.md#server-processing-order) owns the collector's normative gate and rate-limit order. This storage contract requires only that sanitization and privacy gates complete before the write transaction: rejected or suppressed requests create no event, visitor key, aggregate, or request-body log. The collector captures one server instant for both `received_at_ms` and UTC day, derives the optional visitor key from transient trustworthy inputs, and then executes the event transaction.

The public collection key is an identifier, not a credential. Origin validation and deployment rate limits reduce accidental or unsophisticated pollution but cannot make a public browser collector immune to forged requests. Reports are insights, not an auditable billing source.

### Event identity and time

The beacon generates one random RFC 4122 UUIDv4 per page view without using cookies or browser storage. It reuses that ID only when retrying the same request. The collector stores its canonical lowercase text form and treats `(site_id, event_id)` as the idempotency key. A duplicate is a successful no-op even if its supplied body differs; the first accepted event wins.

`received_at_ms` and `day` come from the server clock. The browser does not choose analytics time. A delayed request belongs to the UTC day on which the server accepts it, so there is no backdated-event or aggregate-reopening path in the MVP.

### Paths

- Take the browser URL's pathname only.
- Require a valid path beginning with `/`; use `/` for the home page.
- Reject literal `?` and `#` characters and cap the canonical UTF-8 representation at 512 bytes.
- Require complete two-hex-digit percent escapes, uppercase their hex digits, and do not decode the escaped octet.
- Do not merge `/pricing` and `/pricing/`; preserve repeated and trailing slashes because a site may assign them different semantics.
- Never send or store the query or fragment.

### Referrers

The beacon parses an HTTP(S) `document.referrer` in the browser and sends only its hostname. A full referrer URL therefore never crosses the collection boundary. The collector never treats the beacon request's HTTP `Referer` header as the external referrer; the beacon suppresses that header and, without suppression, it would identify the tracked page rather than its external source.

- Accept only the payload's hostname field.
- Normalize it to lowercase IDNA ASCII, remove a final dot, and omit its port.
- Discard invalid referrers and hosts matching the site's configured host list.
- Store no full URL, scheme, path, query, fragment, or user information.
- Create referrer aggregate and visitor rows only for a valid external hostname.

The dashboard may derive **direct page views** as site page views minus external-referrer page views. It must not claim a distinct direct-visitor count because daily visitors can use both direct and referred paths.

### Campaigns

Recognize only the exact lowercase URL parameter names:

- `utm_source`
- `utm_medium`
- `utm_campaign`

The beacon sends them as explicit campaign fields rather than forwarding a URL query. For each value:

- require valid UTF-8;
- normalize Unicode to NFC, trim surrounding Unicode whitespace, and apply locale-independent Unicode lowercase conversion;
- discard that campaign member if it exceeds 100 UTF-8 bytes after normalization, while retaining the page view;
- safely render it as untrusted text.

Missing members become empty strings. A duplicated allowlisted query name is omitted as ambiguous. Create campaign aggregate and visitor rows only when at least one member is non-empty. Discard `utm_term`, `utm_content`, advertising click identifiers, unknown query fields, and all other query values.

Campaign attribution applies only to the page view carrying those fields. The MVP does not persist attribution across pages, sessions, or days.

## Daily visitor derivation

The deployment provides one stable random master secret containing at least 32 decoded bytes through a base64-encoded Kubernetes Secret value. It never enters SQLite, logs, metrics, backups, or issue evidence.

Use HMAC-SHA256 and an unambiguous length-prefixed binary encoding:

```text
day_key = HMAC-SHA256(
  master_secret,
  encode("pacapulse/visitor-day/v1", site_id, YYYY-MM-DD)
)

visitor_key = HMAC-SHA256(
  day_key,
  encode(canonical_ip_bytes, browser_family, device_class)
)
```

`encode` prefixes every component with a fixed-width unsigned length, preventing concatenation collisions. Store the complete 32-byte `visitor_key`; truncation saves little and adds no useful property at this scale.

### Client IP

- Trust `X-Forwarded-For` only when the immediate peer is a configured trusted ingress proxy. Ignore `Forwarded`, `X-Real-IP`, and all forwarding headers from an untrusted immediate peer.
- Parse every chain member with an IP-address type rather than string manipulation, walk right to left past configured trusted proxies, and use the first untrusted address. A direct unproxied request uses its actual peer address.
- Unmap IPv4-mapped IPv6 addresses and use the canonical 4- or 16-byte representation.
- Use the full address transiently. Prefix truncation would merge too many unrelated visitors while the persisted output is already secret-keyed and day/site scoped.
- If no trustworthy valid client IP is available, accept and count the page view but do not increment any visitor count.

Collector ingress access logging must be disabled, and application logs must not record forwarding headers, remote addresses, raw headers, or request bodies.

### Coarse user-agent class

Classify in memory into stable, bounded categories:

- Browser: `chromium`, `edge`, `firefox`, `safari`, or `other`.
- Device: `desktop`, `mobile`, `tablet`, or `other`.

Do not include browser/OS versions, device models, full user-agent text, or geography. A missing or unrecognized user agent uses `other`; it does not suppress a visitor estimate when the IP is trustworthy. Bot classification is outside the MVP.

The string `visitor-day/v1` versions the semantics. A classifier or input change requires a version change and should deploy at a UTC boundary, because changing it during a day can count returning visitors again. Rotate the master secret at a UTC boundary for the same reason.

A database-only compromise without the Kubernetes secret yields opaque, mutually unrelated site/day keys. This is not a claim of irreversible anonymization: an attacker holding both the database and master secret can brute-force the small IPv4 and coarse-agent input space and correlate recovered addresses. Secret isolation and the roughly 48-hour key retention are therefore material privacy controls. Restoring the database does not require the visitor secret for historical reports because persisted aggregates are authoritative; the secret is needed only to continue same-day deduplication.

## Schema

[`data-model.sql`](data-model.sql) defines the accepted reference schema using SQLite `STRICT` tables, explicit constraints, foreign keys, and known dimensions. Production migrations may add auth-owned tables, but must preserve these contracts.

UUIDs, checksums, collection keys, internal identity IDs, and `YYYY-MM-DD` values are server-generated canonical ASCII. Character and byte lengths therefore coincide for those columns. The SQL day constraint checks canonical shape; the application creates the calendar-valid value from one captured UTC server instant.

### Configuration boundary

The reference schema includes only the organization and site fields needed by analytics:

- immutable random UUID IDs;
- a random public collection key that can be rotated independently;
- canonical and allowed hosts;
- enabled/disabled collection;
- the default-off `collect_dnt` policy.

OIDC identities, sessions, instance ownership, and site grants belong to the authentication/authorization implementation. Grant rows must reference sites with `ON DELETE CASCADE`, and the deletion workflow must include their count in the audit record.

### Sanitized events

`page_view_events` is a 30-day diagnostic ledger. It intentionally has no visitor column. This allows page-view aggregates to be recomputed during the event window, but **visitor aggregates cannot be reconstructed after their short-lived deduplication rows expire**. Recovery of visitor counts therefore depends on verified backups.

### Daily aggregates

Four explicit table families avoid a generic dimension abstraction:

- `daily_site_stats`
- `daily_page_stats`
- `daily_referrer_stats`
- `daily_campaign_stats`

Every row has an integer row ID, site, ISO UTC day, page-view count, and daily-visitor count. Dimension tables add only their named canonical dimensions. Integer IDs keep long dimension text out of the much larger visitor-deduplication keys.

Attacker-controlled aggregate cardinality is bounded per site and UTC day:

- 2,000 non-overflow paths;
- 500 non-overflow external referrer hosts;
- 500 non-overflow campaign tuples.

An already-known dimension continues using its row. Once a non-overflow limit is reached, each new value maps to that family's one additional reserved overflow row for the day. The event ledger still retains the accepted canonical value for 30 days, while the 13-month report shows the aggregate dimension as **Other**. A visitor encountering several overflow values counts once in that family's Other row. The site total has no attacker-controlled dimension.

These fixed MVP limits are deliberately not operator-configurable. They bound storage to at most 3,004 aggregate rows per active site/day: one site row, 3,000 non-overflow dimension rows, and three overflow rows. Selection is arrival-ordered, so after a limit is reached a later high-traffic dimension remains inside Other for that day. The dashboard must show the Other row and its share whenever it is nonzero; sustained legitimate overflow means the site's cardinality exceeds the MVP reporting envelope, while unexpected overflow can indicate abuse.

Known tables produce readable SQL, independent cardinality inspection, and obvious retention/deletion behavior. There is no JSON dimension key, dynamic schema, or hash whose meaning must be reconstructed.

### Visitor-deduplication tables

Four matching tables retain the 32-byte daily visitor key:

- `daily_site_visitors`
- `daily_page_visitors`
- `daily_referrer_visitors`
- `daily_campaign_visitors`

Each table stores only an aggregate `stat_id` and 32-byte visitor key. Its composite primary key answers whether that visitor was already counted in the exact site/day/dimension represented by the aggregate row. Foreign-key deletion of an aggregate also removes its temporary visitor keys. Keeping dimension text in the aggregate row once—rather than in every visitor primary key—bounds deduplication-row size and avoids SQLite overflow pages for ordinary keys.

## Event transaction

Use one `BEGIN IMMEDIATE` transaction per accepted event. A later performance change may batch several accepted events in one writer transaction only if it preserves the same per-event atomicity and acknowledgement semantics.

Pseudocode:

```text
INSERT page_view_events ... ON CONFLICT DO NOTHING RETURNING 1
if no row returned:
    COMMIT and report accepted duplicate

GET OR CREATE the daily_site_stats row and add one page view

GET the exact daily_page_stats row
if absent and the day's path limit is not reached:
    CREATE the exact row
if absent and the limit is reached:
    GET OR CREATE the day's page overflow row
add one page view and retain its stat_id

repeat the same bounded resolution for an external referrer and campaign tuple

if visitor_key exists:
    INSERT daily_site_visitors(stat_id, visitor_key) ON CONFLICT DO NOTHING
    if inserted: increment the referenced daily_site_stats.visitors

    INSERT daily_page_visitors(stat_id, visitor_key) ON CONFLICT DO NOTHING
    if inserted: increment the referenced daily_page_stats.visitors

    repeat for each applicable referrer and campaign stat_id

COMMIT
```

Any validation, SQL, count-update, or commit failure returns an error and records no part of the event. Do not acknowledge success before commit. A bounded writer queue applies backpressure; when it is full, reject work rather than silently dropping an acknowledged event.

The application owns one serialized writer. A small read pool may query WAL snapshots, but no second process or application replica may write the database. Every SQLite connection—including reads, migrations, maintenance, backup, and repair tooling—must execute and verify `PRAGMA foreign_keys = ON`; relying on the reference schema connection is insufficient.

## Report calculations

For a selected completed-day interval `[start_day, end_day]`:

```sql
SELECT sum(page_views), sum(visitors)
FROM daily_site_stats
WHERE site_id = ? AND day BETWEEN ? AND ?;
```

The dimension reports use equally explicit queries:

```sql
SELECT path, overflow,
       sum(page_views) AS page_views,
       sum(visitors) AS daily_visitors
FROM daily_page_stats
WHERE site_id = ? AND day BETWEEN ? AND ?
GROUP BY path, overflow
ORDER BY page_views DESC, overflow DESC, path ASC
LIMIT ?;

SELECT referrer_host, overflow,
       sum(page_views) AS page_views,
       sum(visitors) AS daily_visitors
FROM daily_referrer_stats
WHERE site_id = ? AND day BETWEEN ? AND ?
GROUP BY referrer_host, overflow
ORDER BY page_views DESC, overflow DESC, referrer_host ASC
LIMIT ?;

SELECT campaign_source,
       campaign_medium,
       campaign_name,
       overflow,
       sum(page_views) AS page_views,
       sum(visitors) AS daily_visitors
FROM daily_campaign_stats
WHERE site_id = ? AND day BETWEEN ? AND ?
GROUP BY campaign_source, campaign_medium, campaign_name, overflow
ORDER BY page_views DESC,
         overflow DESC,
         campaign_source ASC,
         campaign_medium ASC,
         campaign_name ASC
LIMIT ?;
```

The daily trend uses completed UTC bounds supplied by the application:

```sql
SELECT day, page_views, visitors
FROM daily_site_stats
WHERE site_id = ? AND day BETWEEN ? AND ?
ORDER BY day ASC;
```

The application inserts zero-value days in the response rather than storing empty aggregate rows. For a requested `N`-day comparison, it computes `current_end = yesterday UTC`, `current_start = current_end - (N - 1) days`, `previous_end = current_start - 1 day`, and `previous_start = previous_end - (N - 1) days`, then runs:

```sql
SELECT 'current' AS period,
       coalesce(sum(page_views), 0) AS page_views,
       coalesce(sum(visitors), 0) AS daily_visitors
FROM daily_site_stats
WHERE site_id = ? AND day BETWEEN ? AND ?
UNION ALL
SELECT 'previous' AS period,
       coalesce(sum(page_views), 0) AS page_views,
       coalesce(sum(visitors), 0) AS daily_visitors
FROM daily_site_stats
WHERE site_id = ? AND day BETWEEN ? AND ?;
```

Parameters are `(site_id, current_start, current_end, site_id, previous_start, previous_end)`. Percentage change is calculated in the application with an explicit no-baseline state when the previous value is zero.

The report layer maps rows with `overflow = 1` to a localized **Other** label and never exposes the reserved empty dimension value as ordinary content.

All report queries require `site_id`; repository APIs must not expose an unscoped report method. Unique indexes begin with `(site_id, day)`, so bounded report scans do not require a second report-index family. Separate `(day, id)` indexes support global retention. Before implementation is accepted, representative `EXPLAIN QUERY PLAN` fixtures must confirm the intended indexes.

Summing daily visitor counts produces visitor-days, which the UI calls **daily visitors**. The same person appearing on seven days can contribute seven. Summed page/referrer/campaign visitor counts can exceed the site count because those dimensions overlap.

The 7-, 30-, and 90-day comparison windows use completed UTC days so both periods cover equal durations. A current-day value may be shown separately and clearly marked **partial**, but it is excluded from percentage comparison. No realtime guarantee is implied.

## Retention and maintenance

Retention belongs to the live application because it changes related records transactionally. Run it before initial readiness, then periodically from the single service.

Compute cutoffs in Go using UTC calendar operations rather than SQLite's ambient clock:

- Events: delete `received_at_ms` earlier than the current instant minus 30 days.
- Aggregates: delete `day` earlier than the UTC calendar date 13 months before maintenance.
- Visitor deduplication: keep the current and immediately previous UTC day; delete older days.
- Deletion audits: delete records older than 13 months.

Delete events and audits through their retention indexes in bounded primary-key batches. Delete aggregate rows through their `(day, id)` indexes; foreign-key cascades remove matching visitor keys. For the 48-hour visitor cleanup, select old aggregate IDs through the same day indexes and delete matching visitor rows in bounded batches without deleting the retained aggregates. Yield between writer transactions.

After a successful retention pass, run `PRAGMA wal_checkpoint(TRUNCATE)` and bounded incremental vacuum work. `secure_delete` clears deleted content in the main database pages, while the truncating checkpoint removes older page images from the WAL. Neither operation promises physical erasure from the storage provider's block snapshots; the backup and infrastructure retention policies bound those copies.

A maintenance error must be visible in privacy-safe operational status and retried; it must never advance a recorded success watermark. Startup retention may delay readiness rather than serve indefinitely with already-expired data.

Set and verify these connection/runtime pragmas deliberately:

```text
page_size = 8192
foreign_keys = ON
journal_mode = WAL
wal_autocheckpoint = 1000 pages
journal_size_limit = 64 MiB
auto_vacuum = INCREMENTAL
secure_delete = ON
synchronous = FULL
busy_timeout = 5000ms
```

`page_size` and `auto_vacuum` must be set before migration 1 creates a table. For an index B-tree, SQLite's approximate maximum local payload is `((page_size - 12) * 64 / 255) - 23`: about 1,002 bytes at 4 KiB and 2,030 bytes at 8 KiB. The maximum bounded event record is about 1,155 bytes before ordinary record-header variation, so the 8 KiB page avoids the predictable 4 KiB overflow-page cliff. Future column additions must recheck this bound rather than assuming the page size itself is the local-payload limit. Migration 1 and every restore assert `page_size = 8192` and `auto_vacuum = 2`; silently running with another configuration is an error because changing either later requires a full `VACUUM` and roughly a second database's free space. Bounded incremental vacuuming then returns free tail pages without routine full-vacuum downtime. Internal free pages can remain allocated for reuse, so file size need not fall after every pass.

## Database failure behavior

Before migrations, startup opens the database and requires `PRAGMA quick_check` to return `ok`. A non-database file, failed integrity check, applied-checksum mismatch, or newer schema keeps readiness false while liveness remains available for diagnosis. The application does not attempt automatic repair.

At runtime:

- `SQLITE_BUSY` after the configured timeout rejects that collection request with a retryable unavailable response and commits no partial data.
- `SQLITE_FULL` stops collection with a retryable unavailable response, marks ingestion degraded, and keeps already-readable reports available. It must not trigger a tight retry/log loop.
- `SQLITE_CORRUPT`, `SQLITE_NOTADB`, or persistent I/O errors stop analytics reads and writes, make readiness false, preserve liveness, and require the restore runbook.

A live-database snapshot depends on SQLite being readable. When corruption prevents startup, PacaPulse cannot create a final backup from that PVC; recovery starts from the newest previously verified external backup while preserving the failed volume.

## Site and organization deletion

Site deletion is owner-only. Before changing SQLite, the application writes an authenticated, encrypted deletion marker containing only the random site ID and request timestamp to the external backup control plane. Markers are append-only and retained for 14 days—longer than the seven-day backup window—so restoring a pre-deletion backup can mechanically reapply every later deletion. If marker persistence fails, deletion fails visibly before the live database changes. A marker written before a later database failure is never revoked: it remains an authoritative privacy-forward deletion request. The application retries the SQLite deletion to completion and exposes outstanding marker-written/database-present site IDs in restricted operational status until reconciled; restore replay remains unconditional.

SQLite's single writer then serializes this transaction against ingestion:

1. Count affected event, aggregate, visitor, and grant rows for the audit record.
2. Delete the site. Foreign-key cascades remove hosts, events, aggregates, deduplication rows, and auth-owned grants.
3. Insert `site_deletion_audit` using only the random site ID, actor's random internal identity ID, timestamp, and row counts.
4. Commit.
5. Checkpoint and truncate the WAL, then run bounded incremental vacuum maintenance.

A collection transaction committed before deletion is removed by the cascade. One that waits until after deletion cannot satisfy the site foreign key. Product and campaign content, hostnames, paths, OIDC subjects, emails, and site names do not enter the deletion audit.

Deleting an organization must invoke the same site-deletion workflow for each site rather than relying on an unaudited raw cascade.

Deletion removes data immediately from the active database. Encrypted backups can retain a point-in-time copy for at most seven days. The UI and documentation must state this limit. Restore replays every external marker newer than the selected backup before serving traffic, preventing point-in-time recovery from silently resurrecting a deleted site.

## Migrations

- Embed ordered SQL migrations in the application binary.
- Record each integer version, SHA-256 checksum, and application-assigned UTC timestamp in `schema_migrations`.
- Apply pending migrations before readiness using the one writer, after the startup integrity and database-configuration checks.
- Refuse to start when an applied checksum differs or the database version is newer than the binary understands.
- Use a transaction for every migration SQLite permits. A failure leaves the service unready and retains the previous committed version.
- Treat migrations as forward-only. Prefer additive expand/contract changes that permit application rollback.
- Require a verified pre-migration backup before a non-additive or long-running migration. When backward compatibility is impossible, recovery means restoring that backup, not running an improvised down-migration.
- Keep `AUTOINCREMENT` on all four aggregate IDs so a stale visitor reference can never become valid again through rowid reuse.
- Never rebuild a `daily_*_stats` table without rebuilding its matching visitor table in the same reviewed migration. SQLite's 12-step rebuild requires `foreign_keys = OFF` before its transaction; afterward, restore and verify `foreign_keys = ON`, run `PRAGMA foreign_key_check`, and run explicit left-join orphan checks for all four stat/visitor pairs before commit or readiness.

Startup and every retention pass also run the four orphan checks. They fail if any visitor `stat_id` lacks its aggregate row; `foreign_key_check` alone cannot detect an orphan that was accidentally re-pointed by reused IDs in an older database.

## Single-owner enforcement

Kubernetes uses a one-replica `Recreate` Deployment, but `ReadWriteOnce` is node-scoped and is not itself a single-process guarantee. Prefer `ReadWriteOncePod` when the target cluster and CSI driver support it. In every environment, every writing entry point—the serving application, restore-finalization command, migration tool, and any future maintenance CLI—must hold the same exclusive advisory PVC lockfile for its full lifetime and refuse to run when that lock is held. Operators must never mount the live database into a debug/job pod or force-delete a terminating PacaPulse pod until the old process and volume attachment are confirmed gone.

## Backup contract

A daily Kubernetes CronJob schedules an application-owned backup operation but never mounts or copies the live PVC. The exact internal authentication, network policy, encryption tooling, and object-storage provider belong to the deployment follow-up; they must keep the operation off the public Ingress and keep decryption credentials out of the running application where practical.

The running application:

1. Executes `VACUUM INTO` on a dedicated read connection to create a compact, consistent point-in-time database in a size-bounded `emptyDir`, not the database PVC. Unlike a stepped online backup from another connection, this snapshot does not repeatedly restart under sustained WAL writes. Run it during the expected low-write window with a bounded duration, while monitoring the live WAL; abort the snapshot before WAL growth exceeds 128 MiB.
2. Opens the copy separately, verifies `page_size = 8192` and `auto_vacuum = 2`, and requires `PRAGMA quick_check` to return `ok`.
3. Creates a manifest containing the schema version, application version, UTC creation time, plaintext database size, and SHA-256 checksum.
4. Packages and encrypts the database and manifest with a reviewed streaming scheme.
5. Uploads the encrypted artifact over TLS to dedicated storage outside the SQLite PVC and Terraform-state boundary.
6. Reports success only after upload completion is confirmed.
7. Removes the local temporary copy regardless of outcome.

`journal_size_limit` bounds normal post-checkpoint WAL retention, but cannot truncate frames needed by the snapshot's active reader. Backup duration and the 128 MiB live-WAL abort threshold are therefore separate safety controls; operational status must measure and alert on WAL bytes independently from the main database.

External lifecycle policy is the authoritative seven-day maximum backup retention. Logs and metrics may expose last attempt/success times, duration, byte count, and broad failure category, but not credentials, encryption material, object names, database content, or request data.

The CronJob failing means the backup failed. Operational status must make a stale last-success time visible. A backup that was not integrity-checked and durably uploaded is not successful.

## Restore contract

Restore is a deliberate maintenance procedure, never an automatic CronJob action:

1. Obtain explicit production-operation authorization and pause ArgoCD reconciliation long enough to keep PacaPulse stopped.
2. Preserve the failed PVC and its metadata for investigation; do not overwrite it.
3. Provision a fresh PVC.
4. Download the newest suitable encrypted backup, decrypt it using separately controlled restore credentials, verify its manifest checksum, and require full `PRAGMA integrity_check` success.
5. Confirm the recorded schema and application versions are compatible.
6. While the public Deployment remains stopped, run an offline restore-finalization command that first acquires the same exclusive PVC lock against the new volume: apply compatible forward migrations, run retention and orphan checks, fetch and authenticate the external deletion journal, and mechanically delete every site whose marker is newer than the backup.
7. Start PacaPulse against the finalized PVC and wait for readiness.
8. Verify owner login, site/grant boundaries, aggregate date ranges, and a controlled collection request.
9. Resume ArgoCD reconciliation.
10. Create and verify a fresh post-restore backup.

Operational documentation must record the recovery point and recovery-time observations without publishing object names, credentials, user data, or raw analytics content.

## Capacity and resource estimate

Exact bytes depend on text lengths, B-tree occupancy, visitor reuse, and dimension cardinality. Capacity must be modeled as three different lifetimes rather than one per-event multiplier:

- The 30-day event ledger stores one bounded event plus its primary key, receive-time index, and site/day index. Typical rows are expected around 0.25–0.9 KiB; the 512-byte path and three 100-byte campaign bounds define the benchmark maximum.
- The roughly 48-hour deduplication window stores two mandatory and up to two optional compact `(stat_id, 32-byte visitor_key)` rows per visitor/dimension encounter. It does not scale with the full 30-day ledger.
- The 13-month aggregates store dimension text once per site/day row. Fixed overflow limits cap each site at 2,000 non-overflow path, 500 referrer, and 500 campaign rows per day even under forged input. With every dimension at its maximum byte length, measured planning size is about 3.25 MiB per site/day or 1.25 GiB per site across 13 months.

| Scenario | Total page views/month | Approximate active analytics storage | Interpretation |
| --- | ---: | ---: | --- |
| Low | 10,000 | under 25 MiB | Several low-traffic sites |
| Expected | 250,000 | 100–350 MiB | Modest multi-site operation |
| Upper modest | 2,000,000 | 0.8–3.0 GiB | Near the intended 5 GiB PVC envelope |
| Maximum aggregate cardinality | Traffic-independent | 1.25 GiB per site for 13 months | Three continuously abused sites exceed the 70% alert; four approach the 5 GiB PVC |

These are design estimates, not guarantees. The maximum-cardinality case intentionally identifies a violated resource target: the 5 GiB MVP volume is not safe for three or more sites that continuously exhaust every dimension cap. Such sustained overflow is an operational incident, not supported steady state; alerting and per-site request controls must intervene before the PVC threshold. Implementation benchmarks must report the event/index, temporary deduplication, and aggregate components separately using short and maximum-byte dimensions, low and high visitor reuse, overflow traffic, and retention cleanup. Alert before the 5 GiB PVC reaches 70% and 85% usage. Cardinality abuse is bounded jointly by byte limits, fixed overflow thresholds, request limits, per-site rate limits, and operational visibility.

Each accepted event executes one ledger insert, two to four aggregate page-view upserts, two to four visitor inserts, and zero to four visitor-count updates. At MVP volume, a serialized writer and one transaction per event favor correctness. If measurements require batching, acknowledgements must still wait for durable commit and failures must remain atomic per accepted event.

The application should be measured against the MVP's approximately 50–150 MiB idle memory and 25–50m Kubernetes CPU request goals. SQLite page cache, writer queue, backup buffer, and report result sizes must be explicitly bounded. Backups use `emptyDir` so a database-sized temporary copy does not consume PVC headroom.

## Required validation fixtures

Implementation tests must cover at least:

1. Two views by one visitor on one page: two page views, one site visitor, one page visitor.
2. One visitor on two pages: one site visitor and one visitor in each page row; page visitor sums are intentionally non-additive.
3. Two visitors sharing a page, referrer, and campaign tuple.
4. One visitor using two campaigns or referrers on the same day.
5. The same transient inputs on two sites and two UTC days, producing four unrelated HMACs.
6. UTC rollover and a request delayed across midnight, using server receive time.
7. Duplicate event ID with identical and differing payloads, both leaving the first accepted event as the only count.
8. GPC and default DNT suppression creating no rows.
9. Missing trusted IP creating page-view counts but no visitor counts.
10. Missing/unknown user agent using the coarse `other` class.
11. External `document.referrer` payloads plus internal, invalid, Unicode/IDNA, and full-path values; prove the HTTP `Referer` header is not used as the analytics referrer.
12. UTM case/whitespace normalization, partial tuples, overlong values, unknown parameters, and safe rendering.
13. Transaction failure at each write stage with no partial count.
14. Retention boundaries for events, aggregates, visitors, and deletion audits.
15. Site deletion removing all content and leaving only the minimal audit row.
16. Migration checksum mismatch, newer schema, failed migration, verified pre-migration backup gate, and a stats/visitor table rebuild that preserves IDs and leaves no orphans.
17. `VACUUM INTO` during sustained writes, a realistically sized WAL ceiling/abort, quick check, encrypted upload failure, full restore integrity check, and post-restore retention.
18. Deleting a site, restoring a pre-deletion backup, replaying the external marker, and proving the site remains absent; also cover marker success followed by database-deletion failure and reconciliation.
19. Startup corruption/NOTADB, runtime FULL/BUSY, stale retention, foreign-keys-disabled connection rejection, orphan detection, and exclusive-lock contention for both the server and restore-finalization command.
20. Exact cardinality limits, overflow visitor deduplication, and a visible Other share for later high-traffic dimensions.
21. Report authorization proving every query is site-scoped before SQL execution.

## Rejected alternatives

### Aggregate-only persistence

Rejected for the MVP because a short sanitized ledger makes page-view diagnosis and recomputation possible. The ledger remains bounded and contains no visitor key.

### Raw-event-only reporting

Rejected because repeated distinct-count queries would require retaining daily visitor identifiers for the full 30-day event window, weaken the privacy lifecycle, and make 13-month reporting expensive.

### Visitor keys in sanitized events

Rejected because diagnostics do not need them. Keeping them only in deduplication tables allows removal after the day-closing grace period.

### Multi-day or rolling-window visitor identifiers

Rejected even though they would enable a range-unique visitor metric. Retaining a stable key for 7, 30, or 90 days would make behavior linkable across days, materially increase the impact of a database-and-secret compromise, and contradict the product's deliberate day boundary. PacaPulse accepts visitor-days and transparent trend estimates instead of retaining the identifier needed to approximate unique people across a report range.

### Generic dimension tables or JSON keys

Rejected because the MVP has four known dimensions. Explicit tables are easier to inspect, constrain, retain, delete, and explain.

### Truncated client network prefix

Rejected because `/24` or `/64` grouping would merge too many unrelated visitors. The full address is transient and the stored HMAC is already secret-keyed and site/day scoped.

### Full user agent in the visitor derivation

Rejected because browser/device versions add fingerprinting precision that trend reporting does not need. Stable coarse classes are sufficient.

### Client-provided event time and session attribution

Rejected because they introduce offline/late-event reopening, clock trust, and persistent behavior linking. The MVP uses server receive time and no sessions.

### Direct database copying from a backup CronJob

Rejected because copying a live SQLite file is not a guaranteed consistent backup and mounting the same `ReadWriteOnce` PVC from a second pod can fail across nodes. The live owner creates a consistent `VACUUM INTO` snapshot outside the PVC.

### Down-migrations and in-place restore

Rejected because destructive rollback paths are difficult to prove and overwriting the failed volume destroys recovery evidence. Forward compatibility plus fresh-PVC restore is safer and more explainable.

## Follow-up implementation outcomes

The architecture supports these independently deliverable outcomes after review:

1. Implement migrations, the serialized SQLite writer, event transaction, and deterministic repository queries.
2. Implement site configuration, collector validation, transient visitor derivation, and privacy controls.
3. Implement retention, secure site deletion, and maintenance observability.
4. Implement site-scoped 7/30/90-day reports and completed-period comparisons.
5. Implement the private backup endpoint, encrypted object-store upload, CronJob, and restore runbook.

Only the first executable downstream wave should be published after this decision is reviewed; these outcomes are not authorization to create all follow-up issues at once.
