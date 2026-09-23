package watch_test

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

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/provision"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/watch"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

// ---- fakes ----

// checkHost replays the shape of the real API: a create call returning a
// request id, then a result keyed by node.
type checkHost struct {
	log     *journal
	mu      sync.Mutex
	blocked map[string]bool // host:port -> blocked from Iran
	// open is the exception to blocked: host:port -> node -> still answers.
	// A real block is rarely total, and "partially reachable" is its own case.
	open map[string]map[string]bool
	reqs map[string]string
	n    int
}

func newCheckHost(blocked ...string) *checkHost {
	c := &checkHost{blocked: map[string]bool{}, open: map[string]map[string]bool{}, reqs: map[string]string{}}
	for _, b := range blocked {
		c.blocked[b] = true
	}
	return c
}

// openFrom keeps one node answering for an otherwise blocked endpoint.
func (f *checkHost) openFrom(hostPort, node string) {
	f.mu.Lock()
	if f.open[hostPort] == nil {
		f.open[hostPort] = map[string]bool{}
	}
	f.open[hostPort][node] = true
	f.mu.Unlock()
}

func (f *checkHost) setBlocked(hostPort string, v bool) {
	f.mu.Lock()
	f.blocked[hostPort] = v
	f.mu.Unlock()
}

func (f *checkHost) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	start := func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("r%d", f.n)
		f.reqs[id] = host
		blocked := f.blocked[host]
		f.mu.Unlock()
		verdict := "reachable"
		if blocked {
			verdict = "blocked"
		}
		f.log.add("iran: probed %s (%s)", host, verdict)

		nodes := map[string][]string{}
		for _, n := range r.URL.Query()["node"] {
			nodes[n] = meta(n)
		}
		writeJSON(w, map[string]any{"ok": 1, "request_id": id, "nodes": nodes,
			"permanent_link": "https://example.test/" + id})
	}
	mux.HandleFunc("/check-tcp", start)
	mux.HandleFunc("/check-traceroute", start)
	mux.HandleFunc("/check-result/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/check-result/")
		f.mu.Lock()
		host := f.reqs[id]
		blocked := f.blocked[host]
		open := f.open[host]
		f.mu.Unlock()

		if !strings.Contains(host, ":") { // traceroute
			writeJSON(w, map[string]any{"ir1.node.check-host.net": []any{[]any{
				[]any{map[string]any{"host": "10.233.65.174", "query_times": []any{"0.5"}}},
				[]any{map[string]any{"query_times": []any{nil}}},
			}}})
			return
		}

		out := map[string]any{}
		for _, n := range allNodes() {
			if strings.HasPrefix(n, "ir") && blocked && !open[n] {
				out[n] = []map[string]any{{"error": "Connection timed out"}}
				continue
			}
			out[n] = []map[string]any{{"address": strings.Split(host, ":")[0], "time": 0.1}}
		}
		writeJSON(w, out)
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

type telegramSpy struct {
	mu   sync.Mutex
	sent []string
	log  *journal
}

func (s *telegramSpy) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.sent = append(s.sent, body.Text)
		s.mu.Unlock()
		s.log.add("alert: %s", firstLine(body.Text))
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *telegramSpy) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func (s *telegramSpy) containing(sub string) string {
	for _, m := range s.messages() {
		if strings.Contains(m, sub) {
			return m
		}
	}
	return ""
}

// hetznerSpy owns one address and records any server creation.
type hetznerSpy struct {
	mu      sync.Mutex
	owned   string
	created int
	deleted int
	newIP   string
	// pool, when set, makes the account reserve addresses the way the real one
	// does: handed out in order, including ones a previous server gave back.
	// The address a server reports is whatever was reserved for it.
	pool         []string
	reserved     []string
	releasedIPs  int
	lastReserved string
	// full makes every server creation fail the way a project at its limit
	// does, which is the only condition the floating rescue exists for.
	full bool
	log  *journal
	// floatPool is handed out by the floating-IP endpoint, so a test can make
	// the account offer back an address that is already dead.
	floatPool     []string
	floatsMade    []string
	floatsAssign  []int64
	floatsDeleted int
}

func (h *hetznerSpy) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h.mu.Lock()
			atLimit := h.full
			if !atLimit {
				h.created++
			}
			h.mu.Unlock()
			if atLimit {
				h.log.add("hetzner: create REFUSED (server limit reached)")
				w.WriteHeader(http.StatusForbidden)
				writeJSON(w, map[string]any{"error": map[string]any{
					"code": "resource_limit_exceeded", "message": "server limit reached"}})
				return
			}
			h.log.add("hetzner: server created")
			writeJSON(w, map[string]any{
				"server": srvObj(99, "new-server", h.newIP),
				"action": map[string]any{"id": 5, "status": "running"},
			})
			return
		}
		writeJSON(w, map[string]any{
			"servers": []any{srvObj(1, "edge-1", h.owned)},
			"meta":    map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})
	// Any per-server path: id 99 is the replacement this run created, and id 1
	// is the blocked machine being replaced. Both can be read and both can be
	// handed back.
	mux.HandleFunc("/servers/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			h.mu.Lock()
			h.deleted++
			h.mu.Unlock()
			h.log.add("hetzner: server %s deleted",
				strings.TrimPrefix(r.URL.Path, "/servers/"))
			writeJSON(w, map[string]any{"action": map[string]any{"id": 6, "status": "success"}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/1") {
			writeJSON(w, map[string]any{"server": srvObj(1, "edge-1", h.owned)})
			return
		}
		h.mu.Lock()
		ip := h.newIP
		if h.lastReserved != "" {
			ip = h.lastReserved
		}
		h.mu.Unlock()
		writeJSON(w, map[string]any{"server": srvObj(99, "new-server", ip)})
	})
	mux.HandleFunc("/actions/5", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"action": map[string]any{"id": 5, "status": "success"}})
	})
	mux.HandleFunc("/floating_ips", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]any{"floating_ips": []any{}, "meta": map[string]any{}})
			return
		}
		h.mu.Lock()
		addr := "203.0.113.50"
		if len(h.floatPool) > 0 {
			addr, h.floatPool = h.floatPool[0], h.floatPool[1:]
		}
		h.floatsMade = append(h.floatsMade, addr)
		id := int64(2000 + len(h.floatsMade))
		h.mu.Unlock()
		h.log.add("hetzner: floating address %s created", addr)
		writeJSON(w, map[string]any{"floating_ip": map[string]any{
			"id": id, "ip": addr, "home_location": map[string]any{"name": "hel1"}}})
	})
	mux.HandleFunc("/floating_ips/", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/assign"):
			h.floatsAssign = append(h.floatsAssign, 1)
			h.log.add("hetzner: floating address assigned to the machine")
		case r.Method == http.MethodDelete:
			h.floatsDeleted++
			h.log.add("hetzner: floating address deleted")
		}
		h.mu.Unlock()
		writeJSON(w, map[string]any{"action": map[string]any{"id": 7, "status": "success"}})
	})
	mux.HandleFunc("/primary_ips", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]any{"primary_ips": []any{}, "meta": map[string]any{}})
			return
		}
		h.mu.Lock()
		addr := h.newIP
		if len(h.pool) > 0 {
			addr, h.pool = h.pool[0], h.pool[1:]
		}
		h.reserved = append(h.reserved, addr)
		h.lastReserved = addr
		id := int64(1000 + len(h.reserved))
		h.mu.Unlock()
		h.log.add("hetzner: address %s reserved", addr)
		writeJSON(w, map[string]any{"primary_ip": map[string]any{
			"id": id, "ip": addr, "datacenter": map[string]any{"name": "hel1-dc2"}}})
	})
	mux.HandleFunc("/primary_ips/", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.releasedIPs++
		h.mu.Unlock()
		h.log.add("hetzner: reserved address given back")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/datacenters", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"datacenters": []any{map[string]any{
			"id": 1, "name": "hel1-dc2",
			"location":     map[string]any{"name": "hel1"},
			"server_types": map[string]any{"available": []int64{42}},
		}}, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
	})
	mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"server_types": []any{
			map[string]any{"id": 42, "name": "cpx11"},
		}, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// floatingAddresses is every floating address the account handed out, and
// floatingAssigns how many were actually attached to a machine.
func (h *hetznerSpy) floatingAddresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.floatsMade...)
}

func (h *hetznerSpy) floatingAssigns() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.floatsAssign)
}

func (h *hetznerSpy) floatingDeletes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.floatsDeleted
}

// reservedAddresses is every address the account handed out, in order.
func (h *hetznerSpy) reservedAddresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reserved...)
}

// releasedCount is how many reservations were given back. An unassigned primary
// IP is billed, so a refused one that is simply abandoned costs money forever.
func (h *hetznerSpy) releasedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.releasedIPs
}

func (h *hetznerSpy) createdCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.created
}

// deletedCount is how many servers were handed back. A replacement that turns
// out to be filtered has to be destroyed, or it costs money forever and holds
// one of the project's limited server slots.
func (h *hetznerSpy) deletedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deleted
}

func srvObj(id int, name, ip string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "running",
		"public_net":  map[string]any{"ipv4": map[string]any{"ip": ip, "blocked": false}},
		"server_type": map[string]any{"name": "cpx11"},
		"location":    map[string]any{"name": "hel1"},
	}
}

func allNodes() []string {
	var out []string
	for i := 1; i <= 8; i++ {
		out = append(out, fmt.Sprintf("ir%d.node.check-host.net", i))
	}
	return append(out, "de1.node.check-host.net", "nl1.node.check-host.net")
}

func meta(node string) []string {
	if strings.HasPrefix(node, "ir") {
		return []string{"ir", "Iran", "Tehran", "10.0.0.1", "AS47430"}
	}
	return []string{"de", "Germany", "Frankfurt", "10.0.0.2", "AS24940"}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// zexSpy stands in for the Zex admin panel.
type zexSpy struct {
	mu          sync.Mutex
	deactivated []string
	activated   []string
	replaced    []map[string]string
	failReplace bool
	log         *journal
}

func (z *zexSpy) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: "admin_token", Value: "jwt", Expires: time.Now().Add(time.Hour),
		})
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/admin/v2ray/bulk-active", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		z.mu.Lock()
		on := r.FormValue("active") == "true"
		if on {
			z.activated = append(z.activated, r.FormValue("address"))
		} else {
			z.deactivated = append(z.deactivated, r.FormValue("address"))
		}
		z.mu.Unlock()
		if on {
			z.log.add("panel: configs on %s switched ON", r.FormValue("address"))
		} else {
			z.log.add("panel: configs on %s switched OFF", r.FormValue("address"))
		}
		writeJSON(w, map[string]any{"updated": 4})
	})
	mux.HandleFunc("/admin/v2ray/replace-address", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		z.mu.Lock()
		fail := z.failReplace
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.FormValue(k)
		}
		z.replaced = append(z.replaced, form)
		z.mu.Unlock()
		z.log.add("panel: configs moved %s -> %s (activate=%s)",
			form["old_address"], form["new_address"], form["activate"])

		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		writeJSON(w, map[string]any{"updated": 4, "skipped": 0, "ids": []int{1, 2, 3, 4}})
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (z *zexSpy) snapshot() ([]string, []string, []map[string]string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.deactivated...),
		append([]string(nil), z.activated...),
		append([]map[string]string(nil), z.replaced...)
}

// ---- harness ----

type harness struct {
	st  *store.Store
	set *settings.Provider
	w   *watch.Watcher
	ch  *checkHost
	tg  *telegramSpy
	hz  *hetznerSpy
	zex *zexSpy
	ids map[string]int64
	cfg *config.Config
	// log is the ordered record of everything the fakes were asked to do, in
	// the order they were asked. Single facts are checked elsewhere; this is
	// for the sequence.
	log *journal
}

func newHarness(t *testing.T, ch *checkHost, ownedAddress, newIP string) *harness {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	set, err := settings.New(st.DB(), "", filepath.Join(dir, "k"), map[string]string{
		settings.ProvisionEnabled:   "false",
		settings.ProvisionDryRun:    "true",
		settings.HetznerServerType:  "cpx11",
		settings.HetznerLocation:    "hel1",
		settings.HetznerNamePrefix:  "bc-",
		settings.ProvisionCooldown:  "6h",
		settings.ProvisionMaxPerDay: "3",
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	jrnl := &journal{}
	ch.log = jrnl
	tg := &telegramSpy{log: jrnl}
	hz := &hetznerSpy{owned: ownedAddress, newIP: newIP, log: jrnl}

	var ir []string
	for i := 1; i <= 8; i++ {
		ir = append(ir, fmt.Sprintf("ir%d.node.check-host.net", i))
	}
	cfg := &config.Config{
		IRNodes:      ir,
		ControlNodes: []string{"de1.node.check-host.net", "nl1.node.check-host.net"},
		MinIRFail:    6, PollInterval: time.Millisecond, ResultTimeout: 2 * time.Second,
		WatchInterval: time.Hour, OutageThreshold: 3, OutageWindow: 30 * time.Minute,
		ProvisionCooldown: 6 * time.Hour, ProvisionMaxPerDay: 3,
		ProvisionBootWait: time.Millisecond, ProvisionVerifyRounds: 1,
		ProvisionVerifyDelay: time.Millisecond, ProvisionActionTimeout: 5 * time.Second,
		TracerouteEnabled: false, Location: time.UTC,
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	chClient := checkhost.NewClient(ch.server(t).URL, 0, time.Millisecond, 2*time.Second)
	hzClient := hetzner.NewRegistry(set, log, 5*time.Second, time.Millisecond).WithBaseURL(hz.server(t).URL)
	tgClient := notify.NewTelegram(set, 5*time.Second).WithBaseURL(tg.server(t).URL)

	if err := set.Set(settings.HetznerToken, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := set.Set(settings.TelegramBotToken, "1:a"); err != nil {
		t.Fatal(err)
	}
	if err := set.Set(settings.TelegramChatID, "42"); err != nil {
		t.Fatal(err)
	}

	// The panel is deliberately left unconfigured so the alert reports the
	// online count as unavailable rather than inventing a zero.
	panelClient := xui.New(set, time.Second)

	zx := &zexSpy{log: jrnl}
	zexClient := zex.New(set, 5*time.Second).WithBaseURL(zx.server(t).URL)

	prov := provision.New(cfg, set, st, chClient, hzClient, panelClient, zexClient, tgClient,
		provision.NewNoopHandoff(log), log)
	w := watch.New(cfg, set, st, chClient, prov, log)

	return &harness{st: st, set: set, w: w, ch: ch, tg: tg, hz: hz, zex: zx,
		ids: map[string]int64{}, cfg: cfg, log: jrnl}
}

// seedBlocked registers an endpoint already known to be blocked, which is what
// puts it on the watchlist.
func (h *harness) seedBlocked(t *testing.T, address string, port int) int64 {
	t.Helper()
	tgt := splash.Target{Address: address, Port: port, ConfigIDs: []int{1}, Names: []string{"cfg"}, Active: true}
	id, err := h.st.UpsertTarget(tgt, time.Now())
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	scanID, err := h.st.CreateScan("manual", time.Now())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := h.st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: id, Verdict: "BLOCKED_IR",
		IRTimeout: 8, IRTotal: 8, ControlOpen: 2, ControlTotal: 2, CheckedAt: time.Now(),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if _, err := h.st.RecordVerdict(id, "BLOCKED_IR", scanID, time.Now()); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if err := h.st.FinishScan(scanID, store.ScanCompleted, 1, nil, time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// Ownership as a real scan records it, project included. Without the
	// project nothing may act on the machine — a server id is only meaningful
	// inside the account that issued it.
	if err := h.st.SetAddressProvider(address, "hetzner", "default", "server",
		1, "edge-1", false, time.Now()); err != nil {
		t.Fatalf("provider: %v", err)
	}

	h.ids[fmt.Sprintf("%s:%d", address, port)] = id
	return id
}

// seedHealthy registers an endpoint that is currently working.
func (h *harness) seedHealthy(t *testing.T, address string, port int) int64 {
	t.Helper()
	tgt := splash.Target{Address: address, Port: port, ConfigIDs: []int{1}, Names: []string{"cfg"}, Active: true}
	id, err := h.st.UpsertTarget(tgt, time.Now())
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	scanID, err := h.st.CreateScan("manual", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: id, Verdict: "HEALTHY",
		IROpen: 8, IRTotal: 8, ControlOpen: 2, ControlTotal: 2, CheckedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.RecordVerdict(id, "HEALTHY", scanID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.FinishScan(scanID, store.ScanCompleted, 0, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	h.ids[address] = id
	return id
}

// enableSwap configures the Zex panel so the full replacement path runs.
func (h *harness) enableSwap(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		settings.ZexAdminEmail:          "admin@example.com",
		settings.ZexAdminPassword:       "pw",
		settings.HetznerLocations:       "DE:fsn1,FI:hel1",
		settings.ProvisionEnabled:       "true",
		settings.ProvisionDryRun:        "false",
		settings.HetznerSnapshotID:      "12345",
		settings.ProvisionQuietWindow:   "1ms",
		settings.ProvisionFullBlockOnly: "true",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) rounds(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := h.w.RunOnce(context.Background()); err != nil {
			t.Fatalf("watch round %d: %v", i+1, err)
		}
	}
}

// ---- tests ----

const (
	ownedAddr    = "5.161.158.200"
	externalAddr = "174.138.13.95"
	freshIP      = "88.99.1.2"
)

func TestThresholdNeedsThreeOutages(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	id := h.seedBlocked(t, ownedAddr, 443)

	h.rounds(t, 2)
	if got := h.countOutages(t, id); got != 2 {
		t.Fatalf("outages = %d, want 2", got)
	}
	if len(h.tg.messages()) != 0 {
		t.Fatalf("alert sent after only 2 outages: %v", h.tg.messages())
	}

	h.rounds(t, 1)
	if h.tg.containing("Blocked from Iran") == "" {
		t.Fatalf("no alert after the third outage: %v", h.tg.messages())
	}
	// The counter resets so the same window cannot fire twice.
	if got := h.countOutages(t, id); got != 0 {
		t.Errorf("outages after firing = %d, want 0", got)
	}
}

func TestRecoveryClearsTheCounter(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	id := h.seedBlocked(t, ownedAddr, 443)

	h.rounds(t, 2)
	ch.setBlocked(ownedAddr+":443", false)
	h.rounds(t, 1)

	if got := h.countOutages(t, id); got != 0 {
		t.Fatalf("outages after recovery = %d, want 0", got)
	}
	if len(h.tg.messages()) != 0 {
		t.Errorf("an alert was sent for an endpoint that recovered: %v", h.tg.messages())
	}

	// It stays on the watchlist — every endpoint is watched now — but with a
	// clean slate, so the next block starts counting from zero.
	list, err := h.w.Watchlist()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("watchlist = %d entries, want the endpoint still watched", len(list))
	}
	if list[0].Outages != 0 {
		t.Errorf("outages = %d, want 0 after recovery", list[0].Outages)
	}
}

// A healthy endpoint that gets filtered must be caught by the watch loop, not
// left until the next full scan half an hour later.
func TestHealthyEndpointIsWatchedToo(t *testing.T) {
	ch := newCheckHost() // nothing blocked to begin with
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedHealthy(t, ownedAddr, 443)

	list, err := h.w.Watchlist()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("watchlist = %d, want a healthy endpoint to be watched", len(list))
	}

	// It gets blocked between rounds.
	ch.setBlocked(ownedAddr+":443", true)
	h.rounds(t, 1)

	if got := h.countOutages(t, h.ids[ownedAddr]); got != 1 {
		t.Fatalf("outages = %d, want the new block recorded on the first round", got)
	}
}

// With the scope narrowed the old behaviour comes back.
func TestBlockedScopeWatchesOnlyBlocked(t *testing.T) {
	ch := newCheckHost()
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedHealthy(t, ownedAddr, 443)

	if err := h.set.Set(settings.WatchScope, "blocked"); err != nil {
		t.Fatal(err)
	}
	list, err := h.w.Watchlist()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("watchlist = %d, want only blocked endpoints in this scope", len(list))
	}
}

// The real config has more blocked addresses outside Hetzner than inside it.
// Those must produce a clearly different alert, not silence.
func TestExternalAddressIsReportedNotProvisioned(t *testing.T) {
	ch := newCheckHost(externalAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, externalAddr, 443)

	if err := h.set.Set(settings.ProvisionEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if err := h.set.Set(settings.ProvisionDryRun, "false"); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	msg := h.tg.containing("not in your Hetzner project")
	if msg == "" {
		t.Fatalf("no external alert was sent: %v", h.tg.messages())
	}
	if !strings.Contains(msg, externalAddr) {
		t.Errorf("the alert does not name the address: %s", msg)
	}
	if h.hz.createdCount() != 0 {
		t.Errorf("a server was created for an address outside the project")
	}

	provs, err := h.st.Provisions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 || provs[0].Status != store.ProvExternal {
		t.Fatalf("provision record = %+v, want status %s", provs, store.ProvExternal)
	}
}

func TestDryRunAlertsWithoutCreatingAServer(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)

	if err := h.set.Set(settings.ProvisionEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	h.rounds(t, 3)

	if h.tg.containing("Blocked from Iran") == "" {
		t.Fatalf("no alert: %v", h.tg.messages())
	}
	if h.hz.createdCount() != 0 {
		t.Fatal("a server was created while dry run was on")
	}

	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvDryRun {
		t.Fatalf("status = %+v, want %s", provs, store.ProvDryRun)
	}
}

func TestProvisioningOffMeansNoServer(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)

	h.rounds(t, 3) // provisioning disabled by default

	if h.hz.createdCount() != 0 {
		t.Fatal("a server was created while provisioning was switched off")
	}
	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvSkipped {
		t.Fatalf("status = %+v, want %s", provs, store.ProvSkipped)
	}
}

func TestFullProvisioningCreatesAndVerifies(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443") // the new IP is reachable
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)

	for k, v := range map[string]string{
		settings.ProvisionEnabled:  "true",
		settings.ProvisionDryRun:   "false",
		settings.HetznerSnapshotID: "12345",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	h.rounds(t, 3)

	if h.hz.createdCount() != 1 {
		t.Fatalf("servers created = %d, want 1", h.hz.createdCount())
	}
	if h.tg.containing("Replacement server created") == "" {
		t.Errorf("no creation alert: %v", h.tg.messages())
	}
	if h.tg.containing("New server reachable from Iran") == "" {
		t.Errorf("no verification alert: %v", h.tg.messages())
	}

	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 {
		t.Fatalf("provisions = %d, want 1", len(provs))
	}
	p := provs[0]
	if p.Status != store.ProvVerified {
		t.Errorf("status = %s, want %s", p.Status, store.ProvVerified)
	}
	if p.NewAddress != freshIP {
		t.Errorf("new address = %q, want %s", p.NewAddress, freshIP)
	}
	if p.VerifyVerdict != "HEALTHY" {
		t.Errorf("verify verdict = %q, want HEALTHY", p.VerifyVerdict)
	}
	// The hook is intentionally empty, so the record must say a human still
	// has to wire the address in.
	if p.HandoffStatus != store.HandoffPending {
		t.Errorf("handoff = %q, want %s", p.HandoffStatus, store.HandoffPending)
	}
}

func TestNewAddressBornBlockedStopsInsteadOfRetrying(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", freshIP+":443") // the replacement is blocked too
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)

	for k, v := range map[string]string{
		settings.ProvisionEnabled:  "true",
		settings.ProvisionDryRun:   "false",
		settings.HetznerSnapshotID: "12345",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	h.rounds(t, 3)

	if h.hz.createdCount() != 1 {
		t.Fatalf("servers created = %d; retrying automatically would burn money", h.hz.createdCount())
	}
	if h.tg.containing("not reachable from Iran") == "" {
		t.Errorf("no failure alert: %v", h.tg.messages())
	}

	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvVerifyFail {
		t.Fatalf("status = %+v, want %s", provs, store.ProvVerifyFail)
	}
}

func TestCooldownBlocksASecondRun(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)

	for k, v := range map[string]string{
		settings.ProvisionEnabled:  "true",
		settings.ProvisionDryRun:   "false",
		settings.HetznerSnapshotID: "12345",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	h.rounds(t, 3) // first trigger
	h.rounds(t, 3) // second trigger, inside the cooldown

	if h.hz.createdCount() != 1 {
		t.Fatalf("servers created = %d, want 1 — the cooldown did not hold", h.hz.createdCount())
	}
	provs, _ := h.st.Provisions(10)
	if len(provs) != 2 {
		t.Fatalf("provisions = %d, want 2 records", len(provs))
	}
	if provs[0].Status != store.ProvSkipped {
		t.Errorf("second run status = %s, want %s", provs[0].Status, store.ProvSkipped)
	}
}

// An endpoint the operator marked hands-off must still alert, but must never
// cause a server to be created — "tell me it broke, leave it alone".
func TestNotifyOnlyAlertsButNeverProvisions(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	id := h.seedBlocked(t, ownedAddr, 443)

	if err := h.st.SetNotifyOnly(id, true); err != nil {
		t.Fatalf("mark notify-only: %v", err)
	}
	for k, v := range map[string]string{
		settings.ProvisionEnabled:  "true",
		settings.ProvisionDryRun:   "false",
		settings.HetznerSnapshotID: "12345",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	h.rounds(t, 3)

	msg := h.tg.containing("Blocked from Iran")
	if msg == "" {
		t.Fatalf("no alert was sent for a notify-only endpoint: %v", h.tg.messages())
	}
	if !strings.Contains(msg, "notify only") {
		t.Errorf("the alert does not say the endpoint is hands-off:\n%s", msg)
	}
	if h.hz.createdCount() != 0 {
		t.Fatal("a server was created for an endpoint marked notify-only")
	}

	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvNotifyOnly {
		t.Fatalf("status = %+v, want %s", provs, store.ProvNotifyOnly)
	}
}

// Clearing the flag must let provisioning work again.
func TestNotifyOnlyCanBeTurnedOff(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	id := h.seedBlocked(t, ownedAddr, 443)

	if err := h.st.SetNotifyOnly(id, true); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetNotifyOnly(id, false); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		settings.ProvisionEnabled:  "true",
		settings.ProvisionDryRun:   "false",
		settings.HetznerSnapshotID: "12345",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	h.rounds(t, 3)

	if h.hz.createdCount() != 1 {
		t.Fatalf("servers created = %d, want 1 once the flag is cleared", h.hz.createdCount())
	}
}

func (h *harness) countOutages(t *testing.T, targetID int64) int {
	t.Helper()
	n, err := h.st.OutageCount(targetID, time.Now().Add(-h.cfg.OutageWindow))
	if err != nil {
		t.Fatalf("count outages: %v", err)
	}
	return n
}

// ---- the full replacement cycle ----

// The whole point of this phase: a replacement is built somewhere healthy,
// verified from Iran, and every config is moved onto it in one call — without
// the configs ever being taken down first.
func TestFullSwapMovesConfigsToTheNewAddress(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443") // the new IP is fine
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	// A healthy German address makes DE an eligible country to build in.
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)

	h.rounds(t, 3)

	deactivated, activated, replaced := h.zex.snapshot()
	// Nothing is switched off on the way. Measured on the one replacement that
	// ran end to end in production, doing so cost 389 seconds of user outage
	// where 327 of them were spent building and verifying with the configs
	// already pointing at nothing. The address being replaced answers from no
	// Iranian network, so taking its configs down early took nothing away from
	// anyone and only lengthened the hole.
	if len(deactivated) != 0 {
		t.Fatalf("configs were taken down before the replacement existed: %v", deactivated)
	}
	if len(replaced) != 1 {
		t.Fatalf("replace calls = %d, want 1", len(replaced))
	}

	got := replaced[0]
	if got["old_address"] != ownedAddr || got["new_address"] != freshIP {
		t.Errorf("swap was %s → %s, want %s → %s",
			got["old_address"], got["new_address"], ownedAddr, freshIP)
	}
	if got["country"] != "DE" {
		t.Errorf("country = %q, want the healthy country DE", got["country"])
	}
	if got["activate"] != "true" {
		t.Errorf("activate = %q, want the configs switched back on", got["activate"])
	}
	// Reactivation happens through the replace call, not a separate one.
	if len(activated) != 0 {
		t.Errorf("unexpected separate activation calls: %v", activated)
	}

	// And no "taken out of circulation" alert either, because nothing was.
	if msg := h.tg.containing("Configs taken out of circulation"); msg != "" {
		t.Errorf("an outage was announced that never happened: %s", msg)
	}
	if h.tg.containing("Replaced") == "" {
		t.Errorf("no alert when the swap completed: %v", h.tg.messages())
	}

	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvSwapped {
		t.Fatalf("status = %+v, want %s", provs, store.ProvSwapped)
	}
}

// If the replacement is itself blocked, the configs must stay off. Handing out
// a config that is known not to work is worse than handing out none.
func TestConfigsStayOffWhenTheNewAddressIsAlsoBlocked(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", freshIP+":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)

	h.rounds(t, 3)

	deactivated, activated, replaced := h.zex.snapshot()
	if len(deactivated) != 1 {
		t.Fatalf("deactivated = %v, want the address taken out", deactivated)
	}
	if len(replaced) != 0 {
		t.Fatal("configs were moved onto an address that is itself blocked")
	}
	if len(activated) != 0 {
		t.Fatal("configs were switched back on despite the failure")
	}

	if h.tg.containing("still switched off") == "" {
		t.Errorf("the failure alert does not say the configs are off: %v", h.tg.messages())
	}
	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvFailed {
		t.Fatalf("status = %+v, want %s", provs, store.ProvFailed)
	}
}

// No healthy country is no longer a reason to refuse. Measurement on
// 2026-09-21 showed a fresh address in a fully blocked region answering from
// all eight Iranian networks, so the region says nothing and the probe of the
// new address decides.
func TestBuildsEvenWhenNoCountryLooksHealthy(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443") // the fresh address answers
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443) // the only address, and it is blocked
	h.enableSwap(t)

	h.rounds(t, 3)

	if h.hz.createdCount() != 1 {
		t.Fatalf("servers created = %d, want 1 — a blocked region must not veto the attempt",
			h.hz.createdCount())
	}
	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 1 {
		t.Fatalf("configs moved = %d, want 1", len(replaced))
	}
	// One deletion, and it is the blocked machine being replaced — not the
	// working replacement that was just built.
	if h.hz.deletedCount() != 1 {
		t.Fatalf("%d machines handed back, want 1 (the blocked one)", h.hz.deletedCount())
	}
}

// A replacement that is itself filtered costs money and holds a server slot for
// nothing, so it is handed straight back.
func TestFilteredReplacementIsDeleted(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", freshIP+":443") // the replacement is filtered too
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.rounds(t, 3)

	if h.hz.createdCount() == 0 {
		t.Fatal("nothing was built")
	}
	if h.hz.deletedCount() != h.hz.createdCount() {
		t.Fatalf("built %d, deleted %d — a filtered server was left running",
			h.hz.createdCount(), h.hz.deletedCount())
	}
	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 0 {
		t.Fatal("configs were moved onto an address that is itself blocked")
	}
	_, activated, _ := h.zex.snapshot()
	if len(activated) != 0 {
		t.Fatal("configs were switched back on despite the failure")
	}
}

// "Only the ones Iran cannot reach at all." A partially reachable address still
// carries users on whichever network works; replacing it would cut them off.
func TestPartiallyReachableAddressIsLeftAlone(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", freshIP+":443")
	ch.openFrom(ownedAddr+":443", "ir1.node.check-host.net")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.rounds(t, 3)

	if h.hz.createdCount() != 0 {
		t.Fatalf("a server was built for an address Iran can still partly reach (%d)",
			h.hz.createdCount())
	}
	deactivated, _, replaced := h.zex.snapshot()
	if len(deactivated) != 0 || len(replaced) != 0 {
		t.Fatalf("configs were touched: deactivated=%v replaced=%v", deactivated, replaced)
	}
}

// A panel failure at the final step must not be silent: the server exists and
// the configs are still off.
func TestPanelFailureDuringSwapIsReported(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)
	h.zex.failReplace = true

	h.rounds(t, 3)

	if h.hz.createdCount() != 1 {
		t.Fatalf("servers created = %d, want 1", h.hz.createdCount())
	}
	if h.tg.containing("Replacement failed") == "" {
		t.Errorf("the panel failure was not reported: %v", h.tg.messages())
	}
	provs, _ := h.st.Provisions(10)
	if len(provs) != 1 || provs[0].Status != store.ProvFailed {
		t.Fatalf("status = %+v, want %s", provs, store.ProvFailed)
	}
}

// Dry run must reach no further than the alert.
func TestDryRunTouchesNothing(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)
	if err := h.set.Set(settings.ProvisionDryRun, "true"); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	deactivated, _, replaced := h.zex.snapshot()
	if len(deactivated) != 0 || len(replaced) != 0 {
		t.Fatalf("dry run changed the panel: deactivated=%v replaced=%v", deactivated, replaced)
	}
	if h.hz.createdCount() != 0 {
		t.Fatal("dry run created a server")
	}
}

// seedHealthyIn registers a working endpoint in a given country, which is what
// makes that country eligible to build in.
func (h *harness) seedHealthyIn(t *testing.T, address string, port int, country string) int64 {
	t.Helper()
	id := h.seedHealthy(t, address, port)
	if err := h.st.SetTargetProvider(id, "hetzner", "server", 1, "edge", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().Exec(`UPDATE targets SET country_code = ? WHERE id = ?`, country, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A machine that serves two ports must come out of a replacement with both
// ports intact.
//
// This is the regression test for a real incident: the swap sent the triggering
// port as new_port, the panel matches on address alone, and 200 live configs
// that lived on 8443 were rewritten to 443 — unverified, unrecorded, and
// discovered only by reading the panel's database afterwards.
func TestSwapNeverRewritesAConfigsPort(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", ownedAddr+":8443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedBlocked(t, ownedAddr, 8443)
	h.enableSwap(t)

	h.rounds(t, 3)

	_, _, replaced := h.zex.snapshot()
	if len(replaced) == 0 {
		t.Fatal("nothing was replaced")
	}
	for _, form := range replaced {
		if v, ok := form["new_port"]; ok {
			t.Fatalf("new_port=%q was sent: every config on %s would be forced onto that port, "+
				"including the ones serving 8443", v, ownedAddr)
		}
		if form["old_address"] != ownedAddr {
			t.Errorf("old_address = %q, want %q", form["old_address"], ownedAddr)
		}
	}
}

// A machine with two ports is one machine. It gets one alert, one server and
// one swap — not two of each because two counters crossed the threshold.
func TestOneSwapPerMachineNotPerPort(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", ownedAddr+":8443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedBlocked(t, ownedAddr, 8443)
	h.enableSwap(t)

	h.rounds(t, 4)

	deactivated, _, replaced := h.zex.snapshot()
	if len(replaced) != 1 {
		t.Fatalf("configs were moved %d times, want 1 — the machine was replaced once per port",
			len(replaced))
	}
	if len(deactivated) != 0 {
		t.Errorf("configs were taken down on the way: %v", deactivated)
	}
	if n := h.hz.createdCount(); n != 1 {
		t.Errorf("bought %d servers for one machine, want 1", n)
	}
}

// Once a machine has been replaced, its other port must not start the whole
// thing again. The cooldown is keyed on the address for exactly this reason:
// keyed on the port, the sibling had no history to be suppressed by.
func TestSiblingPortCannotStartASecondSwap(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", ownedAddr+":8443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedBlocked(t, ownedAddr, 8443)
	h.enableSwap(t)

	h.rounds(t, 8)

	if n := h.hz.createdCount(); n != 1 {
		t.Fatalf("bought %d servers, want 1 — the sibling port triggered again", n)
	}
	provs, _ := h.st.Provisions(20)
	swapped := 0
	for _, p := range provs {
		if p.Status == store.ProvSwapped {
			swapped++
		}
	}
	if swapped != 1 {
		t.Fatalf("%d swaps recorded, want 1", swapped)
	}
}

// One port blocked while the other still answers is not a blocked machine. The
// users reaching it on the working port would lose it for nothing.
func TestMachineWithOneWorkingPortIsLeftAlone(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", ownedAddr+":8443")
	ch.openFrom(ownedAddr+":8443", "ir1.node.check-host.net")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedBlocked(t, ownedAddr, 8443)
	h.enableSwap(t)

	h.rounds(t, 4)

	if n := h.hz.createdCount(); n != 0 {
		t.Fatalf("bought %d servers for a machine Iran can still reach", n)
	}
	deactivated, _, replaced := h.zex.snapshot()
	if len(deactivated) != 0 || len(replaced) != 0 {
		t.Fatalf("configs were touched: hidden=%v moved=%v", deactivated, replaced)
	}
}

// A replacement must answer on every port the old machine served. Healthy on
// 443 and dead on 8443 is a broken clone, not filtering, and moving the 8443
// configs onto it would break them.
func TestReplacementMustAnswerOnEveryPort(t *testing.T) {
	ch := newCheckHost(ownedAddr+":443", ownedAddr+":8443", freshIP+":8443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedBlocked(t, ownedAddr, 8443)
	h.enableSwap(t)

	h.rounds(t, 4)

	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 0 {
		t.Fatal("configs were moved onto a server that does not answer on 8443")
	}
	if h.hz.deletedCount() != h.hz.createdCount() {
		t.Errorf("built %d, deleted %d — a half-working server was left running",
			h.hz.createdCount(), h.hz.deletedCount())
	}
}

// A blocked machine is dead weight: it answers from nowhere in Iran, its
// configs are off, and it holds a slot in a project with a hard server limit.
// Replacing it means handing it back, not keeping it alongside the new one.
func TestTheBlockedMachineIsHandedBack(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.rounds(t, 3)

	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 1 {
		t.Fatalf("configs moved %d times, want 1", len(replaced))
	}
	if h.hz.deletedCount() != 1 {
		t.Fatalf("%d machines handed back, want 1 — the blocked one is still running and "+
			"still holding its slot", h.hz.deletedCount())
	}
}

// Handing back a machine is destructive, so it must not happen while the
// replacement could simply have been built beside it.
func TestNothingIsHandedBackWhenTheReplacementFails(t *testing.T) {
	// The fresh address is blocked too, so no replacement is ever usable.
	ch := newCheckHost(ownedAddr+":443", freshIP+":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.rounds(t, 3)

	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 0 {
		t.Fatal("configs were moved onto an address that is itself blocked")
	}
	// Every server deleted here is one this run created and rejected; the old
	// machine is not among them, because no working replacement was found.
	if h.hz.deletedCount() != h.hz.createdCount() {
		t.Fatalf("created %d, deleted %d — the old machine was handed back even though "+
			"nothing replaced it", h.hz.createdCount(), h.hz.deletedCount())
	}
}

// Handing a machine back means sending a server id to a Hetzner account. The id
// only means something inside the account that issued it, so with no project
// recorded there is no safe account to send it to — and the machine is kept
// rather than a guess being made.
func TestTheOldMachineIsKeptWhenItsProjectIsUnknown(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	// Ownership as an older version recorded it: no project.
	if err := h.st.SetAddressProvider(ownedAddr, "hetzner", "", "server",
		1, "edge-1", false, time.Now()); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 1 {
		t.Fatalf("the replacement itself did not happen (%d moves)", len(replaced))
	}
	if h.hz.deletedCount() != 0 {
		t.Fatalf("%d machine(s) were handed back without knowing which account owns them",
			h.hz.deletedCount())
	}
}

// ---- the address ledger ----

// On 2026-09-21 the service built three replacements that were handed an
// address it had itself destroyed earlier the same day, one of them twice five
// hours apart. Hetzner returns a deleted server's IP to its location's pool, and
// nothing here remembered what had been deleted, so the same dead address was
// bought, booted and probed for minutes before being thrown away again — each
// round costing one of the project's five server slots.

func TestABurnedAddressNeverReachesAServer(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	// The machine was handed back during an earlier replacement, but its
	// configs still point at it, so it keeps failing and keeps reaching the
	// threshold.
	if err := h.st.BurnAddress(store.LedgerEntry{
		Address: ownedAddr, Reason: store.BurnRetired, Verdict: "BLOCKED_IR",
	}); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 6)

	if n := h.hz.createdCount(); n != 0 {
		t.Fatalf("%d server(s) were bought to replace a machine that no longer exists", n)
	}
	// And no config was touched: quieting them would take users off an address
	// that is already dead, for a replacement that is never coming.
	off, on, _ := h.zex.snapshot()
	if len(off) != 0 || len(on) != 0 {
		t.Fatalf("the panel was touched: off=%v on=%v", off, on)
	}
}

func TestABurnedAddressStopsFillingTheProvisionTable(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	if err := h.st.BurnAddress(store.LedgerEntry{
		Address: ownedAddr, Reason: store.BurnRetired,
	}); err != nil {
		t.Fatal(err)
	}

	// Nine rounds is three crossings of the threshold. Before the ledger, each
	// one wrote a provision row that could never succeed.
	h.rounds(t, 9)

	provs, err := h.st.Provisions(20)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 0 {
		t.Fatalf("a destroyed address is still asking to be replaced: %d rows", len(provs))
	}
}

func TestReleasingABurnedAddressLetsTheReplacementRunAgain(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	if err := h.st.BurnAddress(store.LedgerEntry{
		Address: ownedAddr, Reason: store.BurnRetired,
	}); err != nil {
		t.Fatal(err)
	}
	h.rounds(t, 3)
	if h.hz.createdCount() != 0 {
		t.Fatal("the ledger did not hold")
	}

	// The operator looked and decided it is usable after all. The button has to
	// actually change what happens, or it is decoration.
	if err := h.st.ReleaseAddress(ownedAddr, time.Now()); err != nil {
		t.Fatal(err)
	}
	h.rounds(t, 3)

	if h.hz.createdCount() == 0 {
		t.Fatal("releasing the address changed nothing")
	}
}

// A replacement that completes must record the machine it handed back, or the
// very next build in that location can be given the address straight back.
func TestACompletedReplacementRecordsTheOldAddress(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.rounds(t, 3)

	if h.hz.createdCount() == 0 {
		t.Fatal("no replacement was built, so there is nothing to assert about")
	}
	burned, entry, err := h.st.IsBurned(ownedAddr)
	if err != nil {
		t.Fatal(err)
	}
	if !burned {
		t.Fatalf("the replaced address was not recorded, so it can be bought back")
	}
	if entry.Reason != store.BurnRetired {
		t.Errorf("reason = %q, want %q", entry.Reason, store.BurnRetired)
	}
	// The address that replaced it must obviously stay usable.
	if b, _, _ := h.st.IsBurned(freshIP); b {
		t.Error("the working replacement address was burned")
	}
}

// The exact failure from 2026-09-21, reproduced: the account hands back an
// address this service had already destroyed. Before the ledger, that address
// was bought, booted, and probed from Iran for minutes before being recognised
// and thrown away — and then offered again on the next attempt.
func TestARecycledAddressIsRefusedBeforeAnyServerIsBought(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	// The pool offers back a machine we destroyed earlier today, then a clean
	// one. check-host answers for the clean address, not the dead one.
	h.hz.pool = []string{"2.29.50.112", freshIP}
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)
	// Only the location the stub account actually has a datacenter in, so the
	// reservation is exercised rather than skipped for somewhere it cannot
	// build.
	if err := h.set.Set(settings.ProvisionLocations, "hel1"); err != nil {
		t.Fatal(err)
	}

	if err := h.st.BurnAddress(store.LedgerEntry{
		Address: "2.29.50.112", Reason: store.BurnDiscarded, Verdict: "BLOCKED_IR",
	}); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	got := h.hz.reservedAddresses()
	if len(got) < 2 || got[0] != "2.29.50.112" || got[1] != freshIP {
		t.Fatalf("the pool was not exercised as expected: %v", got)
	}
	// One server, built on the clean address. The recycled one never got that
	// far, so no slot was burned and no probe was spent on it.
	if n := h.hz.createdCount(); n != 1 {
		t.Fatalf("servers created = %d, want 1", n)
	}
	if h.hz.releasedCount() != 1 {
		t.Fatalf("the refused reservation was not given back (%d released)", h.hz.releasedCount())
	}

	// And the configs landed on the clean address, not the dead one.
	_, on, replaced := h.zex.snapshot()
	if len(on) == 0 && len(replaced) == 0 {
		t.Fatal("no configs were moved at all")
	}
	for _, r := range replaced {
		if r["newAddress"] == "2.29.50.112" {
			t.Fatalf("configs were moved onto a dead address: %v", r)
		}
	}
}

// ---- the floating rescue ----

// Every project here is capped at five servers and on 2026-09-21 all of them
// were full, so a blocked machine's configs were switched off with nothing to
// move them to. What is wrong is the address, not the machine: its disk, certs
// and configs are all fine, and a new address needs no slot at all.

func TestAFullAccountFallsBackToANewAddressOnTheSameMachine(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.hz.full = true // nothing can be built
	h.hz.floatPool = []string{"203.0.113.50"}
	ch.openFrom("203.0.113.50:443", "")
	if err := h.set.Set(settings.ProvisionFloatOnFailure, "true"); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	if h.hz.createdCount() != 0 {
		t.Fatal("a server was created even though the account is full")
	}
	if got := h.hz.floatingAddresses(); len(got) != 1 || got[0] != "203.0.113.50" {
		t.Fatalf("no floating address was reserved: %v", got)
	}
	if h.hz.floatingAssigns() != 1 {
		t.Fatalf("the floating address was never attached to the machine")
	}

	// No SSH key is configured, so the address cannot be put on the machine's
	// interface and nothing can be concluded about it yet. The right outcome is
	// to hand back the address and ask for the one command — not to move
	// configs onto something that answers nothing, and not to blame the address
	// for a command that never ran.
	if h.tg.containing("ip addr add") == "" {
		t.Fatalf("the operator was not told what to run: %v", h.tg.messages())
	}
	if h.hz.floatingDeletes() == 0 {
		t.Fatal("the unusable floating address was left assigned and billed")
	}
	if burned, _, _ := h.st.IsBurned("203.0.113.50"); burned {
		t.Fatal("a good address was condemned for a command that was never run")
	}
}

func TestTheFloatingRescueIsNotTriedWhenItIsSwitchedOff(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.hz.full = true
	if err := h.set.Set(settings.ProvisionFloatOnFailure, "false"); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	if n := len(h.hz.floatingAddresses()); n != 0 {
		t.Fatalf("a setting that is off was ignored: %d floating addresses made", n)
	}
	// And the failure is still reported, rather than silently doing nothing.
	if h.tg.containing("could not") == "" && h.tg.containing("failed") == "" {
		t.Fatalf("the failure was not reported: %v", h.tg.messages())
	}
}

func TestARecycledFloatingAddressIsRefusedToo(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.hz.full = true
	// The floating pool is a pool like any other: it can hand back something
	// already known to be dead.
	h.hz.floatPool = []string{"2.29.50.112", "203.0.113.50"}
	ch.openFrom("203.0.113.50:443", "")
	if err := h.set.Set(settings.ProvisionFloatOnFailure, "true"); err != nil {
		t.Fatal(err)
	}
	if err := h.st.BurnAddress(store.LedgerEntry{
		Address: "2.29.50.112", Reason: store.BurnDiscarded}); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	got := h.hz.floatingAddresses()
	if len(got) != 2 || got[0] != "2.29.50.112" || got[1] != "203.0.113.50" {
		t.Fatalf("the pool was not exercised as expected: %v", got)
	}
	// The dead one must have been given back: a floating IP is billed while it
	// exists, and it must never end up attached to anything.
	if h.hz.floatingDeletes() == 0 {
		t.Fatal("the refused floating address was not released")
	}
	if h.hz.floatingAssigns() > 1 {
		t.Fatalf("a dead floating address was attached to the machine")
	}
}

// ---- how long the user is actually offline ----

// Measured on the one replacement that ran end to end in production
// (provision 29): the configs went down at second 5 and did not come back
// until second 389, and 327 of those seconds were spent building and verifying
// a replacement while users held configs that pointed at nothing.
//
// Switching off early bought nothing. The address being replaced answers from
// no Iranian network, so its configs were already dead; taking them down first
// only turned a one-call cutover into a six-and-a-half-minute hole. This is the
// test that stops that coming back.
func TestTheConfigsAreNeverDarkWhileAReplacementIsOnTheWay(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)

	h.rounds(t, 3)

	deactivated, _, replaced := h.zex.snapshot()
	if len(replaced) != 1 {
		t.Fatalf("the swap did not complete: %v", replaced)
	}
	if len(deactivated) != 0 {
		t.Fatalf("BulkActive(_, false) was called %d time(s) on the success path: %v",
			len(deactivated), deactivated)
	}
}

// The safety property that switching off early was there for is kept, on the
// path where it actually applies: with no replacement coming, a config that is
// known not to work must not stay in circulation.
func TestConfigsAreWithdrawnWhenNothingCanBeBuilt(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)

	h.hz.full = true // every project at its server limit
	if err := h.set.Set(settings.ProvisionFloatOnFailure, "false"); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	deactivated, _, replaced := h.zex.snapshot()
	if len(replaced) != 0 {
		t.Fatalf("nothing could be built, so nothing should have been moved: %v", replaced)
	}
	if len(deactivated) != 1 || deactivated[0] != ownedAddr {
		t.Fatalf("a dead config was left in circulation: deactivated = %v", deactivated)
	}
	if h.tg.containing("Configs taken out of circulation") == "" {
		t.Errorf("the withdrawal was not announced: %v", h.tg.messages())
	}
}

// ---- endpoints that left the panel ----

// Before the inventory sync, a server deleted from the panel stayed on the
// watchlist forever, costing probes and raising alerts about a machine nobody
// was being handed.
func TestAServerGoneFromThePanelStopsBeingWatched(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")

	// Marked gone well past the grace period.
	long := time.Now().Add(-time.Hour)
	if _, err := h.st.MarkMissing(map[string]bool{"1.2.3.4:443": true}, nil, long); err != nil {
		t.Fatal(err)
	}

	list, err := h.w.Watchlist()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if e.Address == ownedAddr {
			t.Fatalf("%s left the panel an hour ago and is still being watched", ownedAddr)
		}
	}
	if len(list) != 1 {
		t.Fatalf("watchlist = %d entries, want only the one still in the panel", len(list))
	}
}

// One bad read of the panel — a restart, a half-loaded list — must not switch
// monitoring off for everything it missed.
func TestAJustMissingServerIsStillWatchedDuringTheGracePeriod(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)

	if _, err := h.st.MarkMissing(map[string]bool{}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	list, err := h.w.Watchlist()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Address != ownedAddr {
		t.Fatalf("a server missing for seconds was dropped from the watchlist: %+v", list)
	}
}
