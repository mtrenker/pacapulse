package storage

import (
	"context"
	"database/sql"
	"errors"
)

// CollectionSite is the deliberately narrow site configuration exposed to the
// public collector. It contains no organization, display, authentication, or
// reporting data.
type CollectionSite struct {
	ID                string
	CanonicalHost     string
	AllowedHosts      []string
	CollectionEnabled bool
	CollectDNT        bool
}

// ResolveCollectionSite resolves an enabled site by its public collection key.
// Unknown and disabled keys return the same ErrUnknownSite result so the public
// collection boundary cannot distinguish them.
func (d *Database) ResolveCollectionSite(ctx context.Context, collectionKey string) (CollectionSite, error) {
	if !isCanonicalCollectionKey(collectionKey) {
		return CollectionSite{}, ErrUnknownSite
	}
	db, err := d.handle()
	if err != nil {
		return CollectionSite{}, err
	}

	var site CollectionSite
	var collectionEnabled, collectDNT int
	err = db.QueryRowContext(ctx, `SELECT id, canonical_host, collection_enabled, collect_dnt
FROM sites WHERE collection_key = ? AND collection_enabled = 1`, collectionKey).
		Scan(&site.ID, &site.CanonicalHost, &collectionEnabled, &collectDNT)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionSite{}, ErrUnknownSite
	}
	if err != nil {
		return CollectionSite{}, describe(ErrUnavailable, "resolve collection site", "database cannot serve collection configuration")
	}
	site.CollectionEnabled = collectionEnabled == 1
	site.CollectDNT = collectDNT == 1

	rows, err := db.QueryContext(ctx, `SELECT host FROM site_hosts WHERE site_id = ? ORDER BY host`, site.ID)
	if err != nil {
		return CollectionSite{}, describe(ErrUnavailable, "resolve collection site", "database cannot serve collection configuration")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return CollectionSite{}, describe(ErrUnavailable, "resolve collection site", "database cannot serve collection configuration")
		}
		site.AllowedHosts = append(site.AllowedHosts, host)
	}
	if err := rows.Err(); err != nil {
		return CollectionSite{}, describe(ErrUnavailable, "resolve collection site", "database cannot serve collection configuration")
	}
	return site, nil
}

func isCanonicalCollectionKey(value string) bool {
	if len(value) != 35 || value[:3] != "pp_" {
		return false
	}
	for index := 3; index < len(value); index++ {
		character := value[index]
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}
