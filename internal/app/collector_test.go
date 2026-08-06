package app_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/storage"
)

func TestCollectorThroughRealRouterAndStorage(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, map[string]string{config.EnvDataDir: dir, config.EnvLogLevel: "debug"})

	// Site administration is deliberately outside this issue. Seed the accepted
	// schema directly, then release it before the service takes ownership.
	seed, err := storage.Open(storage.Options{Dir: dir})
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	if err := seed.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close() error = %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, storage.DatabaseFileName))
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;
INSERT INTO organizations (id, name, created_at_ms) VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'Fixture', 0);
INSERT INTO sites (id, organization_id, name, collection_key, canonical_host, created_at_ms)
VALUES ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'Fixture', 'pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ', 'shop.example', 0);
INSERT INTO site_hosts (site_id, host) VALUES ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'shop.example');`); err != nil {
		_ = db.Close()
		t.Fatalf("seed site: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	var logs bytes.Buffer
	_, base, result, cancel := startApp(t, cfg, &logs)
	body := `{"v":1,"site":"pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ","event_id":"550e8400-e29b-41d4-a716-446655440000","path":"/pricing","utm_source":" NEWSLETTER "}`
	for delivery := 0; delivery < 2; delivery++ {
		req, err := http.NewRequest(http.MethodPost, base+httpapi.PageViewsPath, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Origin", "https://shop.example")
		req.Header.Set("User-Agent", "Mozilla/5.0 X11 Chrome/120 identifying-agent")
		req.Header.Set("X-Forwarded-For", "203.0.113.77")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("delivery %d: %v", delivery+1, err)
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read response: %v", readErr)
		}
		if resp.StatusCode != http.StatusNoContent || len(responseBody) != 0 {
			t.Errorf("delivery %d response = %d %q", delivery+1, resp.StatusCode, responseBody)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "https://shop.example" || resp.Header.Get("Vary") != "Origin" {
			t.Errorf("delivery %d CORS headers = %v", delivery+1, resp.Header)
		}
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	inspect, err := sql.Open("sqlite", filepath.Join(dir, storage.DatabaseFileName))
	if err != nil {
		t.Fatalf("open inspection database: %v", err)
	}
	defer func() { _ = inspect.Close() }()
	var events, views, visitors int
	if err := inspect.QueryRow(`SELECT
(SELECT count(*) FROM page_view_events),
(SELECT page_views FROM daily_site_stats),
(SELECT visitors FROM daily_site_stats)`).Scan(&events, &views, &visitors); err != nil {
		t.Fatalf("inspect collection: %v", err)
	}
	if events != 1 || views != 1 || visitors != 1 {
		t.Errorf("stored event/views/visitors = %d/%d/%d, want 1/1/1", events, views, visitors)
	}
	var visitorHex string
	if err := inspect.QueryRow(`SELECT hex(visitor_key) FROM daily_site_visitors`).Scan(&visitorHex); err != nil {
		t.Fatalf("read visitor fixture: %v", err)
	}
	for _, forbidden := range []string{
		body, "550e8400", "/pricing", "NEWSLETTER", "Chrome/120", "identifying-agent",
		"pp_q3D9", "https://shop.example", "203.0.113.77", visitorHex,
		cfg.DataDir, storage.DatabaseFileName, "INSERT INTO", "page_view_events",
	} {
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("collector log contains %q:\n%s", forbidden, logs.String())
		}
	}
}
