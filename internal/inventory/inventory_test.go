package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
)

// fakePanel behaves like the real vpn-pannel in the three ways that matter:
//
//   - /api/v1/splash/conf hands out a random handful, the way it hands each
//     phone something to connect to
//   - /api/v1/nodes lists every active config, behind a mobile token
//   - /api/v1/auth/no-login issues that token to a device id
//
// Many configs share one endpoint, as in production (4,604 configs on 9
// endpoints), so the tests exercise the grouping too.
type fakePanel struct {
	mu        sync.Mutex
	endpoints []string // "address:port"
	perEP     int      // configs per endpoint

	logins    int
	nodeCalls int
	// failNodes makes /nodes answer with this status instead of the list.
	failNodes int
	// expireOnce makes the next /nodes call refuse the token, as a panel that
	// rotated its signing key would.
	expireOnce bool
	token      string
}

func newFakePanel(n int) *fakePanel {
	f := &fakePanel{perEP: 3, token: "tok-1"}
	for i := 1; i <= n; i++ {
		f.endpoints = append(f.endpoints, fmt.Sprintf("10.0.%d.%d:443", i/250, i%250+1))
	}
	return f
}

func (f *fakePanel) add(eps ...string) {
	f.mu.Lock()
	f.endpoints = append(f.endpoints, eps...)
	f.mu.Unlock()
}

func (f *fakePanel) remove(eps ...string) {
	f.mu.Lock()
	drop := map[string]bool{}
	for _, e := range eps {
		drop[e] = true
	}
	var keep []string
	for _, e := range f.endpoints {
		if !drop[e] {
			keep = append(keep, e)
		}
	}
	f.endpoints = keep
	f.mu.Unlock()
}

func (f *fakePanel) configs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	id := 1
	for _, ep := range f.endpoints {
		host, port, _ := strings.Cut(ep, ":")
		var p int
		fmt.Sscan(port, &p)
		for k := 0; k < f.perEP; k++ {
			out = append(out, map[string]any{
				"ID": id, "Name": fmt.Sprintf("cfg-%d", id), "Address": host, "Port": p,
				"Protocol": "vless", "CountryCode": "FI", "IsActive": true,
				"RawLink": "vless://x@" + ep,
			})
			id++
		}
	}
	return out
}

func (f *fakePanel) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/auth/no-login", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DeviceID string `json:"device_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.DeviceID == "" {
			http.Error(w, "device_id required", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.logins++
		tok := f.token
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
	})

	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nodeCalls++
		fail, expire, tok := f.failNodes, f.expireOnce, f.token
		if expire {
			f.expireOnce = false
			f.token = "tok-2" // the next login gets a new one
		}
		f.mu.Unlock()

		if r.Header.Get("Authorization") != "Bearer "+tok || expire {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if fail != 0 {
			http.Error(w, "boom", fail)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": f.configs()})
	})

	mux.HandleFunc("/api/v1/splash/conf", func(w http.ResponseWriter, r *http.Request) {
		all := f.configs()
		rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		if len(all) > 4 {
			all = all[:4]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"no_ads_list": all})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakePanel) counts() (logins, nodeCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.nodeCalls
}

type probeSpy struct {
	mu    sync.Mutex
	calls [][]string
	busy  bool
}

func (p *probeSpy) StartTargets(_ context.Context, trigger string, targets []splash.Target) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy {
		return 0, fmt.Errorf("busy")
	}
	var keys []string
	for _, t := range targets {
		keys = append(keys, t.Key())
	}
	sort.Strings(keys)
	p.calls = append(p.calls, keys)
	return int64(len(p.calls)), nil
}

type notifySpy struct {
	mu   sync.Mutex
	sent []string
}

func (n *notifySpy) Send(_ context.Context, html string) error {
	n.mu.Lock()
	n.sent = append(n.sent, html)
	n.mu.Unlock()
	return nil
}

type rig struct {
	panel *fakePanel
	st    *store.Store
	sync  *Syncer
	probe *probeSpy
	tg    *notifySpy
}

func newRig(t *testing.T, endpoints int) *rig {
	t.Helper()
	panel := newFakePanel(endpoints)
	srv := panel.server(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "inv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	client := splash.NewClient(srv.URL, "key", "botchecker-device", 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(client, st, log)
	probe, tg := &probeSpy{}, &notifySpy{}
	s.SetProber(probe)
	s.SetNotifier(tg)
	return &rig{panel: panel, st: st, sync: s, probe: probe, tg: tg}
}

func endpoints(n, from int) []string {
	var out []string
	for i := from; i < from+n; i++ {
		out = append(out, fmt.Sprintf("10.9.0.%d:443", i))
	}
	return out
}

// ---- the complaint itself ----

// Every scan received exactly 14 configs, whatever the panel held, because the
// splash endpoint hands out a random handful. Ten servers were added and never
// appeared. This is that situation, with the whole list available.
func TestTheWholeFleetIsSeenNotASample(t *testing.T) {
	r := newRig(t, 20)

	res, err := r.sync.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceNodes || !res.Complete {
		t.Fatalf("read from %q (complete=%v), want the complete node list", res.Source, res.Complete)
	}
	if res.Endpoints != 20 {
		t.Fatalf("saw %d endpoints, want all 20 — a sample would have shown about 4", res.Endpoints)
	}
	if res.Configs != 60 {
		t.Errorf("configs = %d, want 60 (three per endpoint)", res.Configs)
	}

	statuses, _ := r.st.TargetStatuses()
	if len(statuses) != 20 {
		t.Fatalf("%d endpoints recorded, want 20", len(statuses))
	}
}

func TestTenNewServersAppearAndAreProbedAtOnce(t *testing.T) {
	r := newRig(t, 10)
	ctx := context.Background()

	if _, _, err := r.sync.Update(ctx); err != nil {
		t.Fatal(err)
	}
	// The first sync on an empty store finds everything new. Clear the spies
	// so the assertion is about the ten added afterwards.
	r.probe.calls, r.tg.sent = nil, nil

	added := endpoints(10, 1)
	r.panel.add(added...)

	res, scanID, err := r.sync.Update(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(res.New, added) {
		t.Fatalf("new = %v\nwant %v", res.New, added)
	}
	if len(res.Gone) != 0 || len(res.Returned) != 0 {
		t.Errorf("unexpected changes: gone=%v returned=%v", res.Gone, res.Returned)
	}

	// Probed straight away — exactly the ten, not the whole fleet.
	if scanID == 0 || len(r.probe.calls) != 1 {
		t.Fatalf("the new servers were not probed: scan %d, calls %v", scanID, r.probe.calls)
	}
	if !equal(r.probe.calls[0], added) {
		t.Fatalf("probed %v, want only the ten new ones", r.probe.calls[0])
	}

	// And the operator was told, naming them.
	if len(r.tg.sent) != 1 {
		t.Fatalf("want one announcement, got %d", len(r.tg.sent))
	}
	for _, ep := range added {
		if !strings.Contains(r.tg.sent[0], ep) {
			t.Errorf("the announcement does not name %s", ep)
		}
	}
}

func TestAnUnchangedPanelIsQuiet(t *testing.T) {
	r := newRig(t, 5)
	ctx := context.Background()
	if _, _, err := r.sync.Update(ctx); err != nil {
		t.Fatal(err)
	}
	r.probe.calls, r.tg.sent = nil, nil

	// Every five minutes, forever: this must cost nothing but the read.
	for i := 0; i < 3; i++ {
		res, scanID, err := r.sync.Update(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Changed() || scanID != 0 {
			t.Fatalf("round %d reported a change on an unchanged panel: %+v", i, res)
		}
	}
	if len(r.probe.calls) != 0 || len(r.tg.sent) != 0 {
		t.Fatalf("an unchanged panel caused probes %v or messages %d", r.probe.calls, len(r.tg.sent))
	}
}

// ---- servers leaving the panel ----

func TestADeletedServerIsMarkedGoneAndLeftOutOfEverything(t *testing.T) {
	r := newRig(t, 6)
	ctx := context.Background()
	if _, err := r.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	gone := []string{"10.0.0.2:443", "10.0.0.4:443"}
	r.panel.remove(gone...)

	res, err := r.sync.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(res.Gone, gone) {
		t.Fatalf("gone = %v, want %v", res.Gone, gone)
	}

	statuses, _ := r.st.TargetStatuses()
	var inPanel, missing int
	for _, s := range statuses {
		if s.InPanel() {
			inPanel++
		} else {
			missing++
			if s.Active {
				t.Errorf("%s left the panel but is still marked active", s.HostPort())
			}
		}
	}
	// Kept, not deleted: the history of a removed server is still worth reading.
	if inPanel != 4 || missing != 2 {
		t.Fatalf("in panel %d, missing %d — want 4 and 2", inPanel, missing)
	}
}

func TestAServerThatComesBackIsReturnedNotNew(t *testing.T) {
	r := newRig(t, 3)
	ctx := context.Background()
	if _, err := r.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	ep := "10.0.0.2:443"
	r.panel.remove(ep)
	if _, err := r.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r.panel.add(ep)

	res, err := r.sync.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Configs switched off during a replacement and on again must not be
	// announced as a new server.
	if len(res.New) != 0 {
		t.Errorf("a returning server was reported as new: %v", res.New)
	}
	if !equal(res.Returned, []string{ep}) {
		t.Fatalf("returned = %v, want [%s]", res.Returned, ep)
	}
	statuses, _ := r.st.TargetStatuses()
	for _, s := range statuses {
		if s.HostPort() == ep && !s.InPanel() {
			t.Fatal("the missing marker was not cleared when it came back")
		}
	}
}

// ---- the two ways a read can lie ----

// If the complete list cannot be read the sample is used — but a sample leaves
// out almost everything by design, so it must never be allowed to say that
// anything has gone. Allowed to, it would mark nearly the whole fleet deleted.
func TestAFallbackToTheSampleNeverRemovesAnything(t *testing.T) {
	r := newRig(t, 12)
	ctx := context.Background()
	if _, err := r.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	r.panel.mu.Lock()
	r.panel.failNodes = http.StatusInternalServerError
	r.panel.mu.Unlock()

	res, err := r.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("the sample should have been used: %v", err)
	}
	if res.Source != SourceSample || res.Complete {
		t.Fatalf("source %q complete %v — want an incomplete sample", res.Source, res.Complete)
	}
	if res.FallbackReason == "" {
		t.Error("the fallback was not explained")
	}
	if len(res.Gone) != 0 {
		t.Fatalf("a sample marked %d servers gone: %v", len(res.Gone), res.Gone)
	}
	statuses, _ := r.st.TargetStatuses()
	for _, s := range statuses {
		if !s.InPanel() {
			t.Fatalf("%s was marked gone on the strength of a sample", s.HostPort())
		}
	}
}

func TestAnEmptyListIsTreatedAsABrokenRead(t *testing.T) {
	r := newRig(t, 8)
	ctx := context.Background()
	if _, err := r.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// A panel restarting, a database not yet loaded: 200 with nothing in it.
	r.panel.mu.Lock()
	r.panel.endpoints = nil
	r.panel.mu.Unlock()

	res, err := r.sync.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || len(res.Gone) != 0 {
		t.Fatalf("an empty list wiped the fleet: complete=%v gone=%d", res.Complete, len(res.Gone))
	}
	statuses, _ := r.st.TargetStatuses()
	for _, s := range statuses {
		if !s.InPanel() {
			t.Fatalf("%s was marked gone because the panel returned nothing", s.HostPort())
		}
	}
}

// ---- the token ----

func TestTheTokenIsReusedAcrossSyncs(t *testing.T) {
	r := newRig(t, 3)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := r.sync.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// The token lasts thirty days. Logging in on every sync would create a
	// login every five minutes on the production panel for no reason.
	if logins, calls := r.panel.counts(); logins != 1 || calls != 5 {
		t.Fatalf("logins %d, node calls %d — want 1 and 5", logins, calls)
	}
}

func TestARefusedTokenIsRenewedOnce(t *testing.T) {
	r := newRig(t, 3)
	ctx := context.Background()
	if _, err := r.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	r.panel.mu.Lock()
	r.panel.expireOnce = true
	r.panel.mu.Unlock()

	res, err := r.sync.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete {
		t.Fatalf("a rotated token dropped the sync to a sample: %s", res.FallbackReason)
	}
	if logins, _ := r.panel.counts(); logins != 2 {
		t.Fatalf("logins = %d, want exactly one renewal", logins)
	}
}

// ---- endpoints watched by hand ----

func TestManuallyWatchedEndpointsAreNeverMarkedGone(t *testing.T) {
	r := newRig(t, 3)
	manual := splash.Target{Address: "203.0.113.9", Port: 8443, Manual: true}
	r.sync.SetExtraTargets(func() []splash.Target { return []splash.Target{manual} })

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		res, err := r.sync.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// They are watched precisely because the panel does not list them.
		for _, g := range res.Gone {
			if g == manual.Key() {
				t.Fatalf("a manually watched endpoint was marked gone")
			}
		}
	}
}

// ---- when probing cannot start ----

func TestABusyScannerDoesNotLoseTheNewServers(t *testing.T) {
	r := newRig(t, 2)
	ctx := context.Background()
	if _, _, err := r.sync.Update(ctx); err != nil {
		t.Fatal(err)
	}
	r.probe.busy = true
	r.panel.add(endpoints(3, 50)...)

	res, _, err := r.sync.Update(ctx)
	if err != ErrProberBusy {
		t.Fatalf("err = %v, want ErrProberBusy", err)
	}
	// Recorded regardless, so the next watch round — which probes everything
	// in the store — picks them up within two minutes.
	if len(res.New) != 3 {
		t.Fatalf("new = %v", res.New)
	}
	statuses, _ := r.st.TargetStatuses()
	if len(statuses) != 5 {
		t.Fatalf("%d endpoints recorded, want 5 — the new ones were lost", len(statuses))
	}
}

func equal(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, ",") == strings.Join(b, ",")
}
