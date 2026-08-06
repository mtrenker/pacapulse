package storage

import (
	"context"
	"errors"
	"testing"
)

func TestResolveCollectionSiteReturnsOnlyEnabledCollectorPolicy(t *testing.T) {
	db := newStore(t)
	exec(t, db, "UPDATE sites SET collection_key = ?, canonical_host = ?, collect_dnt = 1 WHERE id = ?", "pp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "shop.example", testSiteID)
	exec(t, db, "INSERT INTO site_hosts (site_id, host) VALUES (?, ?), (?, ?)", testSiteID, "shop.example", testSiteID, "www.shop.example")

	site, err := db.ResolveCollectionSite(context.Background(), "pp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("ResolveCollectionSite() error = %v", err)
	}
	if site.ID != testSiteID || site.CanonicalHost != "shop.example" || !site.CollectionEnabled || !site.CollectDNT {
		t.Errorf("site = %+v", site)
	}
	if len(site.AllowedHosts) != 2 || site.AllowedHosts[1] != "www.shop.example" {
		t.Errorf("hosts = %v", site.AllowedHosts)
	}

	exec(t, db, "UPDATE sites SET collection_enabled = 0 WHERE id = ?", testSiteID)
	for _, key := range []string{"pp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "pp_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "not-canonical"} {
		if _, err := db.ResolveCollectionSite(context.Background(), key); !errors.Is(err, ErrUnknownSite) {
			t.Errorf("key resolution error = %v, want ErrUnknownSite", err)
		}
	}
}
