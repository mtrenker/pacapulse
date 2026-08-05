-- Reference schema for the PacaPulse v0.1.0 analytics model.
-- Migrations own the production form; this file is an executable architecture artifact.
-- page_size and auto_vacuum must be set before the first table is created.

PRAGMA page_size = 8192;
PRAGMA auto_vacuum = INCREMENTAL;
PRAGMA foreign_keys = ON;

CREATE TABLE schema_migrations (
    version       INTEGER PRIMARY KEY,
    checksum      TEXT NOT NULL CHECK (length(checksum) = 64),
    applied_at_ms INTEGER NOT NULL CHECK (applied_at_ms >= 0)
) STRICT;

CREATE TABLE organizations (
    id            TEXT PRIMARY KEY CHECK (length(id) = 36),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0)
) STRICT, WITHOUT ROWID;

CREATE TABLE sites (
    id                 TEXT PRIMARY KEY CHECK (length(id) = 36),
    organization_id    TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name               TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    collection_key     TEXT NOT NULL UNIQUE CHECK (length(collection_key) BETWEEN 22 AND 64),
    canonical_host     TEXT NOT NULL CHECK (length(CAST(canonical_host AS BLOB)) BETWEEN 1 AND 253),
    collection_enabled INTEGER NOT NULL DEFAULT 1 CHECK (collection_enabled IN (0, 1)),
    collect_dnt        INTEGER NOT NULL DEFAULT 0 CHECK (collect_dnt IN (0, 1)),
    created_at_ms      INTEGER NOT NULL CHECK (created_at_ms >= 0)
) STRICT, WITHOUT ROWID;

CREATE TABLE site_hosts (
    site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    host    TEXT NOT NULL CHECK (length(CAST(host AS BLOB)) BETWEEN 1 AND 253),
    PRIMARY KEY (site_id, host)
) STRICT, WITHOUT ROWID;

CREATE TABLE page_view_events (
    site_id          TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    event_id         TEXT NOT NULL CHECK (length(event_id) = 36),
    received_at_ms   INTEGER NOT NULL CHECK (received_at_ms >= 0),
    day              TEXT NOT NULL CHECK (
        day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
    ),
    path             TEXT NOT NULL CHECK (
        length(CAST(path AS BLOB)) BETWEEN 1 AND 512
        AND substr(path, 1, 1) = '/'
        AND instr(path, '?') = 0
        AND instr(path, '#') = 0
    ),
    referrer_host    TEXT CHECK (
        referrer_host IS NULL
        OR length(CAST(referrer_host AS BLOB)) BETWEEN 1 AND 253
    ),
    campaign_source  TEXT NOT NULL DEFAULT '' CHECK (
        length(CAST(campaign_source AS BLOB)) <= 100
    ),
    campaign_medium  TEXT NOT NULL DEFAULT '' CHECK (
        length(CAST(campaign_medium AS BLOB)) <= 100
    ),
    campaign_name    TEXT NOT NULL DEFAULT '' CHECK (
        length(CAST(campaign_name AS BLOB)) <= 100
    ),
    PRIMARY KEY (site_id, event_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX page_view_events_retention_idx
    ON page_view_events (received_at_ms);
CREATE INDEX page_view_events_site_day_idx
    ON page_view_events (site_id, day);

CREATE TABLE daily_site_stats (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id    TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    day        TEXT NOT NULL CHECK (
        day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
    ),
    page_views INTEGER NOT NULL DEFAULT 0 CHECK (page_views >= 0),
    visitors   INTEGER NOT NULL DEFAULT 0 CHECK (visitors >= 0),
    UNIQUE (site_id, day)
) STRICT;

CREATE TABLE daily_page_stats (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id    TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    day        TEXT NOT NULL CHECK (
        day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
    ),
    path       TEXT NOT NULL,
    overflow   INTEGER NOT NULL DEFAULT 0 CHECK (overflow IN (0, 1)),
    page_views INTEGER NOT NULL DEFAULT 0 CHECK (page_views >= 0),
    visitors   INTEGER NOT NULL DEFAULT 0 CHECK (visitors >= 0),
    CHECK (
        (overflow = 1 AND path = '')
        OR (
            overflow = 0
            AND length(CAST(path AS BLOB)) BETWEEN 1 AND 512
            AND substr(path, 1, 1) = '/'
            AND instr(path, '?') = 0
            AND instr(path, '#') = 0
        )
    ),
    UNIQUE (site_id, day, path)
) STRICT;

CREATE TABLE daily_referrer_stats (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id       TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    day           TEXT NOT NULL CHECK (
        day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
    ),
    referrer_host TEXT NOT NULL,
    overflow      INTEGER NOT NULL DEFAULT 0 CHECK (overflow IN (0, 1)),
    page_views    INTEGER NOT NULL DEFAULT 0 CHECK (page_views >= 0),
    visitors      INTEGER NOT NULL DEFAULT 0 CHECK (visitors >= 0),
    CHECK (
        (overflow = 1 AND referrer_host = '')
        OR (
            overflow = 0
            AND length(CAST(referrer_host AS BLOB)) BETWEEN 1 AND 253
        )
    ),
    UNIQUE (site_id, day, referrer_host)
) STRICT;

CREATE TABLE daily_campaign_stats (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id         TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    day             TEXT NOT NULL CHECK (
        day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
    ),
    campaign_source TEXT NOT NULL,
    campaign_medium TEXT NOT NULL,
    campaign_name   TEXT NOT NULL,
    overflow        INTEGER NOT NULL DEFAULT 0 CHECK (overflow IN (0, 1)),
    page_views      INTEGER NOT NULL DEFAULT 0 CHECK (page_views >= 0),
    visitors        INTEGER NOT NULL DEFAULT 0 CHECK (visitors >= 0),
    CHECK (
        (
            overflow = 1
            AND campaign_source = ''
            AND campaign_medium = ''
            AND campaign_name = ''
        )
        OR (
            overflow = 0
            AND (campaign_source <> '' OR campaign_medium <> '' OR campaign_name <> '')
            AND length(CAST(campaign_source AS BLOB)) <= 100
            AND length(CAST(campaign_medium AS BLOB)) <= 100
            AND length(CAST(campaign_name AS BLOB)) <= 100
        )
    ),
    UNIQUE (site_id, day, campaign_source, campaign_medium, campaign_name)
) STRICT;

CREATE INDEX daily_site_stats_retention_idx
    ON daily_site_stats (day, id);
CREATE INDEX daily_page_stats_retention_idx
    ON daily_page_stats (day, id);
CREATE INDEX daily_referrer_stats_retention_idx
    ON daily_referrer_stats (day, id);
CREATE INDEX daily_campaign_stats_retention_idx
    ON daily_campaign_stats (day, id);

CREATE TABLE daily_site_visitors (
    stat_id     INTEGER NOT NULL REFERENCES daily_site_stats(id) ON DELETE CASCADE,
    visitor_key BLOB NOT NULL CHECK (length(visitor_key) = 32),
    PRIMARY KEY (stat_id, visitor_key)
) STRICT, WITHOUT ROWID;

CREATE TABLE daily_page_visitors (
    stat_id     INTEGER NOT NULL REFERENCES daily_page_stats(id) ON DELETE CASCADE,
    visitor_key BLOB NOT NULL CHECK (length(visitor_key) = 32),
    PRIMARY KEY (stat_id, visitor_key)
) STRICT, WITHOUT ROWID;

CREATE TABLE daily_referrer_visitors (
    stat_id     INTEGER NOT NULL REFERENCES daily_referrer_stats(id) ON DELETE CASCADE,
    visitor_key BLOB NOT NULL CHECK (length(visitor_key) = 32),
    PRIMARY KEY (stat_id, visitor_key)
) STRICT, WITHOUT ROWID;

CREATE TABLE daily_campaign_visitors (
    stat_id     INTEGER NOT NULL REFERENCES daily_campaign_stats(id) ON DELETE CASCADE,
    visitor_key BLOB NOT NULL CHECK (length(visitor_key) = 32),
    PRIMARY KEY (stat_id, visitor_key)
) STRICT, WITHOUT ROWID;

CREATE TABLE site_deletion_audit (
    id                     INTEGER PRIMARY KEY,
    site_id                TEXT NOT NULL CHECK (length(site_id) = 36),
    actor_identity_id      TEXT NOT NULL CHECK (length(actor_identity_id) BETWEEN 1 AND 200),
    deleted_at_ms          INTEGER NOT NULL CHECK (deleted_at_ms >= 0),
    event_rows_deleted     INTEGER NOT NULL CHECK (event_rows_deleted >= 0),
    aggregate_rows_deleted INTEGER NOT NULL CHECK (aggregate_rows_deleted >= 0),
    visitor_rows_deleted   INTEGER NOT NULL CHECK (visitor_rows_deleted >= 0),
    grant_rows_deleted     INTEGER NOT NULL CHECK (grant_rows_deleted >= 0)
) STRICT;

CREATE INDEX site_deletion_audit_deleted_at_idx
    ON site_deletion_audit (deleted_at_ms);
