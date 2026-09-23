package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

// These cover the failure that started this: on 2026-09-21 three replacements
// were handed an address this service had itself thrown away, one of them twice
// in five hours. Each round cost a server slot and up to twelve minutes of
// probing before the address was recognised and discarded again.

func testManager(t *testing.T, apiURL string) (*Manager, *store.Store, *settings.Provider) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	set, err := settings.New(st.DB(), "", filepath.Join(dir, "k"), map[string]string{
		settings.HetznerProjects: "proxy|snapshot=1|type=cpx32|locations=hel1",
		settings.HetznerToken:    "tok",
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if err := set.Set(settings.HetznerTokens, "proxy=tok"); err != nil {
		t.Fatalf("tokens: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hz := hetzner.NewRegistry(set, log, 5*time.Second, time.Minute).WithBaseURL(apiURL)

	cfg := &config.Config{}
	// An unconfigured Telegram, not a nil one: send() records every alert in
	// the store whether or not it could be delivered, and that record is what
	// these tests read.
	m := &Manager{cfg: cfg, set: set, st: st, hz: hz, log: log,
		tg:    notify.NewTelegram(set, time.Second),
		panel: xui.New(set, time.Second),
		zex:   zex.New(set, time.Second)}
	return m, st, set
}

// ipPool is a stub that hands out addresses in a fixed order, the way Hetzner
// returns a deleted server's IP to the next machine built in that location.
type ipPool struct {
	mu       sync.Mutex
	addrs    []string
	handed   []string
	released []int64
	next     int64
}

func (p *ipPool) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/datacenters", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"datacenters": []map[string]any{{
				"id": 1, "name": "hel1-dc2",
				"location":     map[string]any{"name": "hel1"},
				"server_types": map[string]any{"available": []int64{42}},
			}},
			"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})
	mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server_types": []map[string]any{{"id": 42, "name": "cpx32"}},
			"meta":         map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})
	mux.HandleFunc("/primary_ips", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.next++
		addr := "203.0.113.99"
		if len(p.addrs) > 0 {
			addr, p.addrs = p.addrs[0], p.addrs[1:]
		}
		p.handed = append(p.handed, addr)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"primary_ip": map[string]any{"id": p.next, "ip": addr,
				"datacenter": map[string]any{"name": "hel1-dc2"}},
		})
	})
	mux.HandleFunc("/primary_ips/", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		_, _ = fmt.Sscan(strings.TrimPrefix(r.URL.Path, "/primary_ips/"), &id)
		p.mu.Lock()
		p.released = append(p.released, id)
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func candidate() buildCandidate {
	return buildCandidate{Project: "proxy", Location: "hel1", ServerType: "cpx32", SnapshotID: "1"}
}

func TestARecycledAddressIsRefusedAndAnotherAskedFor(t *testing.T) {
	// The pool offers back two addresses this service already killed before
	// producing a clean one.
	pool := &ipPool{addrs: []string{"5.161.158.200", "2.29.50.112", "65.109.183.208"}}
	srv := pool.server(t)
	m, st, _ := testManager(t, srv.URL)

	for _, a := range []string{"5.161.158.200", "2.29.50.112"} {
		if err := st.BurnAddress(store.LedgerEntry{Address: a, Reason: store.BurnRetired}); err != nil {
			t.Fatal(err)
		}
	}

	cli := projectClient(t, m)
	ip, dc, err := m.reserveCleanIP(context.Background(), cli, candidate())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if ip.Address != "65.109.183.208" {
		t.Fatalf("a dead address was accepted: %s", ip.Address)
	}
	if dc != "hel1-dc2" {
		t.Fatalf("datacenter not carried through: %q", dc)
	}
	// Both refusals must be given back. An unassigned primary IP is billed, so
	// leaving them would quietly cost money forever.
	if len(pool.released) != 2 {
		t.Fatalf("want 2 rejected addresses released, got %d", len(pool.released))
	}
}

func TestReservingStopsAfterTheConfiguredNumberOfAttempts(t *testing.T) {
	// A pool that only ever returns a dead address must not be hammered.
	pool := &ipPool{}
	for i := 0; i < 50; i++ {
		pool.addrs = append(pool.addrs, "5.161.158.200")
	}
	srv := pool.server(t)
	m, st, set := testManager(t, srv.URL)

	if err := st.BurnAddress(store.LedgerEntry{
		Address: "5.161.158.200", Reason: store.BurnDiscarded}); err != nil {
		t.Fatal(err)
	}
	if err := set.Set(settings.ProvisionIPAttempts, "3"); err != nil {
		t.Fatal(err)
	}

	cli := projectClient(t, m)
	_, _, err := m.reserveCleanIP(context.Background(), cli, candidate())
	if err == nil {
		t.Fatal("expected to give up on this location")
	}
	if !strings.Contains(err.Error(), ErrNoCleanAddress.Error()) {
		t.Fatalf("the caller cannot tell to move on: %v", err)
	}
	if len(pool.handed) != 3 {
		t.Fatalf("want exactly 3 attempts, got %d", len(pool.handed))
	}
	if len(pool.released) != 3 {
		t.Fatalf("every rejected address must be released, got %d", len(pool.released))
	}
}

func TestAnAddressAlreadyMonitoredAndBrokenIsAlsoRefused(t *testing.T) {
	// Not in the ledger, but the config list already points at it and it reads
	// as down. Handing it out as a replacement would move users from one dead
	// address to the same dead address.
	pool := &ipPool{addrs: []string{"2.29.60.163", "65.109.183.208"}}
	srv := pool.server(t)
	m, st, _ := testManager(t, srv.URL)

	seedBrokenTarget(t, st, "2.29.60.163", 443, "SERVER_DOWN")

	cli := projectClient(t, m)
	ip, _, err := m.reserveCleanIP(context.Background(), cli, candidate())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if ip.Address != "65.109.183.208" {
		t.Fatalf("a monitored broken address was accepted: %s", ip.Address)
	}
}

func TestAHealthyMonitoredAddressIsNotTreatedAsDirty(t *testing.T) {
	// The guard must not be so eager that it refuses everything: an address
	// reading HEALTHY is not a reason to throw a reservation away.
	pool := &ipPool{addrs: []string{"65.109.183.208"}}
	srv := pool.server(t)
	m, st, _ := testManager(t, srv.URL)

	seedBrokenTarget(t, st, "65.109.183.208", 443, "HEALTHY")

	cli := projectClient(t, m)
	ip, _, err := m.reserveCleanIP(context.Background(), cli, candidate())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if ip.Address != "65.109.183.208" {
		t.Fatalf("a healthy address was refused: %s", ip.Address)
	}
}

func TestDirtyAddressExplainsItself(t *testing.T) {
	m, st, _ := testManager(t, "http://127.0.0.1:1")
	if err := st.BurnAddress(store.LedgerEntry{
		Address: "5.161.158.200", Reason: store.BurnRetired, Verdict: "BLOCKED_IR"}); err != nil {
		t.Fatal(err)
	}
	why := m.dirtyAddress("5.161.158.200")
	// The reason is logged and shown, so it has to name what happened rather
	// than just say no.
	for _, want := range []string{"ledger", "retired", "BLOCKED_IR"} {
		if !strings.Contains(why, want) {
			t.Errorf("reason %q does not mention %q", why, want)
		}
	}
	if m.dirtyAddress("198.51.100.1") != "" {
		t.Error("an unknown address must be usable")
	}
}

// seedBrokenTarget puts an address in the config list with a recorded reading,
// the way a real scan would.
func seedBrokenTarget(t *testing.T, st *store.Store, address string, port int, verdict string) {
	t.Helper()
	now := time.Now()
	id, err := st.UpsertTarget(splash.Target{
		Address: address, Port: port, CountryCode: "FI",
		ConfigIDs: []int{1}, Names: []string{"n"}, Active: true,
	}, now)
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	scanID, err := st.CreateScan("manual", now)
	if err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	open := 0
	if verdict == "HEALTHY" {
		open = 8
	}
	if _, err := st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: id, Verdict: verdict,
		IROpen: open, IRTotal: 8, ControlOpen: 2, ControlTotal: 2, CheckedAt: now,
	}); err != nil {
		t.Fatalf("seed result: %v", err)
	}
}

// projectClient fails the test rather than handing back a nil client, which
// otherwise panics deep inside the HTTP layer and hides what went wrong.
func projectClient(t *testing.T, m *Manager) *hetzner.Client {
	t.Helper()
	cli, ok := m.hz.Client("proxy")
	if !ok || cli == nil {
		t.Fatal("no Hetzner client for the test project — check the tokens setting")
	}
	return cli
}
