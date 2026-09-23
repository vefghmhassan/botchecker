package hetzner

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/settings"

	_ "modernc.org/sqlite"
)

func testSettings(t *testing.T, token string) *settings.Provider {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value BLOB,
		secret INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	p, err := settings.New(db, "", filepath.Join(dir, "k"), nil)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if token != "" {
		if err := p.Set(settings.HetznerToken, token); err != nil {
			t.Fatalf("set token: %v", err)
		}
	}
	return p
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAPI serves the three listings the inventory reads, with the servers list
// split across two pages so pagination is exercised.
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want a bearer token", got)
		}
		page := r.URL.Query().Get("page")
		switch page {
		case "", "1":
			writeJSON(w, map[string]any{
				"servers": []any{server(1, "edge-1", "5.161.158.200", false)},
				"meta":    map[string]any{"pagination": map[string]any{"next_page": 2}},
			})
		default:
			writeJSON(w, map[string]any{
				"servers": []any{server(2, "edge-2", "65.109.216.204", true)},
				"meta":    map[string]any{"pagination": map[string]any{"next_page": nil}},
			})
		}
	})

	mux.HandleFunc("/primary_ips", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"primary_ips": []any{map[string]any{
				"id": 10, "ip": "1.2.3.4", "type": "ipv4", "blocked": false,
				"assignee_id": 1, "assignee_type": "server",
			}},
			"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})

	mux.HandleFunc("/floating_ips", func(w http.ResponseWriter, r *http.Request) {
		srv := int64(2)
		writeJSON(w, map[string]any{
			"floating_ips": []any{map[string]any{
				"id": 20, "ip": "9.9.9.9", "type": "ipv4", "blocked": true, "server": srv,
			}},
			"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func server(id int, name, ip string, blocked bool) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "running",
		"public_net":  map[string]any{"ipv4": map[string]any{"ip": ip, "blocked": blocked}},
		"server_type": map[string]any{"name": "cpx11"},
		"location":    map[string]any{"name": "hel1"},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestInventoryCoversAllThreeAddressKinds(t *testing.T) {
	api := fakeAPI(t)
	c := New(testSettings(t, "test-token"), discardLogger(), 5*time.Second, time.Minute).WithBaseURL(api.URL)

	inv, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	cases := []struct {
		address string
		kind    string
		owner   Ownership
	}{
		// Page one and page two, so pagination is proven.
		{"5.161.158.200", "server", OwnedHetzner},
		{"65.109.216.204", "server", OwnedHetzner},
		{"1.2.3.4", "primary_ip", OwnedHetzner},
		{"9.9.9.9", "floating_ip", OwnedHetzner},
		// Not in the project at all: DigitalOcean in the real config.
		{"174.138.13.95", "", OwnedExternal},
	}
	for _, tc := range cases {
		if got := inv.Ownership(tc.address); got != tc.owner {
			t.Errorf("Ownership(%s) = %s, want %s", tc.address, got, tc.owner)
		}
		if tc.kind == "" {
			continue
		}
		res, ok := inv.Lookup(tc.address)
		if !ok {
			t.Errorf("%s missing from the inventory", tc.address)
			continue
		}
		if res.Kind != tc.kind {
			t.Errorf("%s kind = %q, want %q", tc.address, res.Kind, tc.kind)
		}
	}

	if inv.Servers != 2 {
		t.Errorf("Servers = %d, want 2 across both pages", inv.Servers)
	}
}

func TestInventoryCarriesAbuseBlockAndServerName(t *testing.T) {
	api := fakeAPI(t)
	c := New(testSettings(t, "test-token"), discardLogger(), 5*time.Second, time.Minute).WithBaseURL(api.URL)

	inv, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	// Hetzner's own abuse block is a different problem from Iranian
	// filtering, so it has to survive into the record.
	blocked, _ := inv.Lookup("65.109.216.204")
	if !blocked.AbuseBlocked {
		t.Error("AbuseBlocked = false, want true")
	}
	if blocked.ServerName != "edge-2" {
		t.Errorf("ServerName = %q, want edge-2", blocked.ServerName)
	}

	// A floating IP inherits the name of the server it is attached to.
	floating, _ := inv.Lookup("9.9.9.9")
	if floating.ServerName != "edge-2" {
		t.Errorf("floating ip ServerName = %q, want edge-2", floating.ServerName)
	}
}

func TestInventoryIsCached(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	for _, path := range []string{"/servers", "/primary_ips", "/floating_ips"} {
		key := path[1:]
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if key == "servers" {
				calls++
			}
			writeJSON(w, map[string]any{key: []any{}, "meta": map[string]any{}})
		})
	}
	api := httptest.NewServer(mux)
	defer api.Close()

	c := New(testSettings(t, "test-token"), discardLogger(), 5*time.Second, time.Minute).WithBaseURL(api.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.Inventory(context.Background()); err != nil {
			t.Fatalf("Inventory: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("servers was fetched %d times, want 1 — the cache is not working", calls)
	}

	c.InvalidateInventory()
	if _, err := c.Inventory(context.Background()); err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if calls != 2 {
		t.Errorf("after invalidation calls = %d, want 2", calls)
	}
}

// Without a token nothing can be said about an address. Reporting it as
// external would tell the operator their own server is somebody else's.
func TestResolverWithoutTokenSaysUnknown(t *testing.T) {
	c := New(testSettings(t, ""), discardLogger(), time.Second, time.Minute)
	r := NewResolver(c)

	info := r.Resolve(context.Background(), "5.161.158.200")
	if info.Provider != string(OwnedUnknown) {
		t.Fatalf("Provider = %s, want unknown", info.Provider)
	}
}

func TestResolverClassifies(t *testing.T) {
	api := fakeAPI(t)
	c := New(testSettings(t, "test-token"), discardLogger(), 5*time.Second, time.Minute).WithBaseURL(api.URL)
	r := NewResolver(c)

	mine := r.Resolve(context.Background(), "5.161.158.200")
	if mine.Provider != string(OwnedHetzner) || mine.ServerName != "edge-1" {
		t.Errorf("own address resolved to %+v", mine)
	}

	theirs := r.Resolve(context.Background(), "2.29.60.163")
	if theirs.Provider != string(OwnedExternal) {
		t.Errorf("foreign address resolved to %s, want external", theirs.Provider)
	}
}

func TestNormaliseStripsPrefixLength(t *testing.T) {
	if got := normalise("2a01:4f9::/64"); got != "2a01:4f9::" {
		t.Errorf("normalise = %q, want the address without the prefix", got)
	}
}
