package scanner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
)

// fakeCheckHost stands in for check-host.net. It replays the outcomes observed
// against the live service: a blocked address answers the control nodes but
// nothing from Iran, a down address answers nobody.
type fakeCheckHost struct {
	mu       sync.Mutex
	requests map[string]string // request id -> probed host
	next     int
	blocked  map[string]bool
	down     map[string]bool
	tcpCalls int
}

func newFakeCheckHost(blocked, down []string) *fakeCheckHost {
	f := &fakeCheckHost{
		requests: map[string]string{},
		blocked:  map[string]bool{},
		down:     map[string]bool{},
	}
	for _, b := range blocked {
		f.blocked[b] = true
	}
	for _, d := range down {
		f.down[d] = true
	}
	return f
}

func (f *fakeCheckHost) handler() http.Handler {
	mux := http.NewServeMux()

	start := func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		nodes := r.URL.Query()["node"]

		f.mu.Lock()
		f.next++
		id := fmt.Sprintf("req%d", f.next)
		f.requests[id] = host
		if strings.Contains(r.URL.Path, "check-tcp") {
			f.tcpCalls++
		}
		f.mu.Unlock()

		nodeMeta := map[string][]string{}
		for _, n := range nodes {
			nodeMeta[n] = metaFor(n)
		}
		writeJSON(w, map[string]any{
			"ok": 1, "request_id": id,
			"permanent_link": "https://example.test/report/" + id,
			"nodes":          nodeMeta,
		})
	}
	mux.HandleFunc("/check-tcp", start)
	mux.HandleFunc("/check-traceroute", start)

	mux.HandleFunc("/check-result/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/check-result/")

		f.mu.Lock()
		host := f.requests[id]
		f.mu.Unlock()

		// A traceroute request carries a bare address, a TCP check an address
		// with a port.
		if !strings.Contains(host, ":") {
			writeJSON(w, map[string]any{
				"ir1.node.check-host.net": tracerouteBody(),
			})
			return
		}

		out := map[string]any{}
		for _, n := range allNodes() {
			out[n] = f.outcomeFor(host, n)
		}
		writeJSON(w, out)
	})

	return mux
}

func (f *fakeCheckHost) outcomeFor(host, node string) []map[string]any {
	iranian := strings.HasPrefix(node, "ir")

	switch {
	case f.down[host]:
		// Nothing answers anywhere, control nodes included.
		return []map[string]any{{"error": "Connection timed out"}}
	case f.blocked[host] && iranian:
		return []map[string]any{{"error": "Connection timed out"}}
	default:
		return []map[string]any{{"address": strings.Split(host, ":")[0], "time": 0.0992}}
	}
}

func tracerouteBody() []any {
	return []any{[]any{
		[]any{map[string]any{"host": "185.105.238.193", "query_times": []any{"0.16"}}},
		[]any{map[string]any{"host": "10.233.65.174", "query_times": []any{"0.52", "210.88"}}},
		[]any{map[string]any{"query_times": []any{nil, nil, nil}}},
		[]any{map[string]any{"query_times": []any{nil, nil, nil}}},
	}}
}

func allNodes() []string {
	var out []string
	for i := 1; i <= 8; i++ {
		out = append(out, fmt.Sprintf("ir%d.node.check-host.net", i))
	}
	return append(out, "de1.node.check-host.net", "nl1.node.check-host.net")
}

func metaFor(node string) []string {
	if strings.HasPrefix(node, "ir") {
		n := strings.TrimSuffix(strings.SplitN(node, ".", 2)[0], "")
		return []string{"ir", "Iran", "Tehran", "10.0.0.1", "AS4743" + n[2:]}
	}
	return []string{"de", "Germany", "Frankfurt", "10.0.0.2", "AS24940"}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fakeSplash(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/conf.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/splash/conf" {
			http.NotFound(w, r)
			return
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["client_key"] == "" {
			t.Errorf("client_key was not sent to the splash API")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, string(body))
	}))
}

func testConfig(splashURL, checkHostURL, dbPath string) *config.Config {
	var ir []string
	for i := 1; i <= 8; i++ {
		ir = append(ir, fmt.Sprintf("ir%d.node.check-host.net", i))
	}
	return &config.Config{
		SplashBaseURL: splashURL, SplashClientKey: "k", SplashDeviceID: "d",
		SplashTimeout:    5 * time.Second,
		CheckHostBaseURL: checkHostURL,
		IRNodes:          ir,
		ControlNodes:     []string{"de1.node.check-host.net", "nl1.node.check-host.net"},
		PollInterval:     5 * time.Millisecond,
		ResultTimeout:    2 * time.Second,
		MinIRFail:        6,
		ConfirmRounds:    2,
		ConfirmDelay:     5 * time.Millisecond,
		ScanConcurrency:  6,

		TracerouteEnabled: true,
		TracerouteNodes:   []string{"ir1.node.check-host.net"},
		TracerouteTimeout: 2 * time.Second,

		RetentionDays: 90,
		DBPath:        dbPath,
		Location:      time.UTC,
	}
}

func runScan(t *testing.T, blocked, down []string) (*store.Store, *fakeCheckHost) {
	t.Helper()

	sp := fakeSplash(t)
	t.Cleanup(sp.Close)

	fake := newFakeCheckHost(blocked, down)
	ch := httptest.NewServer(fake.handler())
	t.Cleanup(ch.Close)

	cfg := testConfig(sp.URL, ch.URL, filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sc := scanner.New(cfg,
		splash.NewClient(cfg.SplashBaseURL, cfg.SplashClientKey, cfg.SplashDeviceID, cfg.SplashTimeout),
		checkhost.NewClient(cfg.CheckHostBaseURL, 0, cfg.PollInterval, cfg.ResultTimeout),
		st, log)

	if _, err := sc.Start(context.Background(), scanner.TriggerManual); err != nil {
		t.Fatalf("start scan: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for sc.Running() {
		if time.Now().After(deadline) {
			t.Fatal("scan did not finish within 30s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return st, fake
}

func TestFullScanClassifiesLiveObservedCases(t *testing.T) {
	// 5.161.158.200:443 was blocked from all eight Iranian networks while
	// Germany and the Netherlands connected. 174.138.13.95:443 stands in for
	// an address that is simply down.
	st, fake := runScan(t,
		[]string{"5.161.158.200:443"},
		[]string{"174.138.13.95:443"})

	scan, err := st.LatestCompletedScan()
	if err != nil || scan == nil {
		t.Fatalf("no completed scan: %v", err)
	}
	if scan.Status != store.ScanCompleted {
		t.Fatalf("scan status = %s, want completed", scan.Status)
	}
	if scan.ConfigCount != 14 || scan.TargetCount != 9 {
		t.Errorf("scan counted %d configs / %d targets, want 14 / 9",
			scan.ConfigCount, scan.TargetCount)
	}
	if scan.BlockedCount != 1 {
		t.Errorf("BlockedCount = %d, want 1", scan.BlockedCount)
	}

	statuses, err := st.TargetStatuses()
	if err != nil {
		t.Fatalf("statuses: %v", err)
	}
	if len(statuses) != 9 {
		t.Fatalf("stored %d endpoints, want 9 unique", len(statuses))
	}

	byKey := map[string]store.TargetStatus{}
	for _, s := range statuses {
		byKey[s.HostPort()] = s
	}

	want := map[string]string{
		"5.161.158.200:443":  "BLOCKED_IR",
		"65.109.216.204:443": "HEALTHY",
		"174.138.13.95:443":  "SERVER_DOWN",
		"2.29.50.112:25245":  "HEALTHY",
	}
	for key, verdict := range want {
		got, ok := byKey[key]
		if !ok {
			t.Errorf("%s missing from results", key)
			continue
		}
		if got.Verdict != verdict {
			t.Errorf("%s verdict = %s, want %s (ir %d/%d up, control %d/%d)",
				key, got.Verdict, verdict, got.IROpen, got.IRTotal, got.ControlOpen, got.ControlTotal)
		}
	}

	// The blocked endpoint carries both config ids that pointed at it, which
	// is what a later replacement step needs.
	if ids := byKey["5.161.158.200:443"].ConfigIDs; len(ids) != 2 {
		t.Errorf("blocked endpoint ConfigIDs = %v, want both 30378 and 30425", ids)
	}

	// Nine endpoints, plus one extra probe to confirm the single blocked one.
	if fake.tcpCalls != 10 {
		t.Errorf("tcp checks = %d, want 10 (9 endpoints + 1 confirmation round)", fake.tcpCalls)
	}
}

func TestFullScanRecordsTransitionsAndTraceroute(t *testing.T) {
	st, _ := runScan(t, []string{"5.161.158.200:443"}, nil)

	events, err := st.AllEvents()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	// Every endpoint is seen for the first time, so each gets one event.
	if len(events) != 9 {
		t.Fatalf("recorded %d transitions, want 9 first sightings", len(events))
	}
	for _, e := range events {
		if e.FromVerdict != "" {
			t.Errorf("%s:%d first event has from=%q, want empty", e.Address, e.Port, e.FromVerdict)
		}
	}

	targetID, err := st.TargetByHostPort("5.161.158.200", 443)
	if err != nil {
		t.Fatalf("lookup blocked target: %v", err)
	}

	tr, err := st.LatestTraceroute(targetID)
	if err != nil || tr == nil {
		t.Fatalf("no traceroute stored for the blocked endpoint: %v", err)
	}
	if tr.LastHop != "10.233.65.174" {
		t.Errorf("LastHop = %q, want 10.233.65.174", tr.LastHop)
	}
	if !tr.LastHopPrivate {
		t.Error("LastHopPrivate = false; a 10.x last hop means the packet never left the operator")
	}

	// A healthy endpoint is not worth a traceroute.
	healthyID, err := st.TargetByHostPort("65.109.216.204", 443)
	if err != nil {
		t.Fatalf("lookup healthy target: %v", err)
	}
	if tr, err := st.LatestTraceroute(healthyID); err == nil && tr != nil {
		t.Error("traceroute was run for a healthy endpoint")
	}
}

func TestSecondScanOnlyRecordsRealChanges(t *testing.T) {
	sp := fakeSplash(t)
	defer sp.Close()

	fake := newFakeCheckHost([]string{"5.161.158.200:443"}, nil)
	ch := httptest.NewServer(fake.handler())
	defer ch.Close()

	cfg := testConfig(sp.URL, ch.URL, filepath.Join(t.TempDir(), "test.db"))
	cfg.TracerouteEnabled = false
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sc := scanner.New(cfg,
		splash.NewClient(cfg.SplashBaseURL, cfg.SplashClientKey, cfg.SplashDeviceID, cfg.SplashTimeout),
		checkhost.NewClient(cfg.CheckHostBaseURL, 0, cfg.PollInterval, cfg.ResultTimeout),
		st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 2; i++ {
		if _, err := sc.Start(context.Background(), scanner.TriggerScheduled); err != nil {
			t.Fatalf("scan %d: %v", i+1, err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for sc.Running() {
			if time.Now().After(deadline) {
				t.Fatal("scan did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	events, err := st.AllEvents()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	// Nothing changed between the two runs, so the transition log must not
	// have grown — that is what keeps the monthly view meaningful.
	if len(events) != 9 {
		t.Fatalf("transitions after two identical scans = %d, want 9", len(events))
	}
}

func TestScanRejectsConcurrentStart(t *testing.T) {
	sp := fakeSplash(t)
	defer sp.Close()
	fake := newFakeCheckHost(nil, nil)
	ch := httptest.NewServer(fake.handler())
	defer ch.Close()

	cfg := testConfig(sp.URL, ch.URL, filepath.Join(t.TempDir(), "test.db"))
	cfg.PollInterval = 50 * time.Millisecond
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sc := scanner.New(cfg,
		splash.NewClient(cfg.SplashBaseURL, cfg.SplashClientKey, cfg.SplashDeviceID, cfg.SplashTimeout),
		checkhost.NewClient(cfg.CheckHostBaseURL, 0, cfg.PollInterval, cfg.ResultTimeout),
		st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := sc.Start(context.Background(), scanner.TriggerManual); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if _, err := sc.Start(context.Background(), scanner.TriggerManual); err != scanner.ErrBusy {
		t.Fatalf("second scan err = %v, want ErrBusy", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for sc.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// A server whose configs have been switched off vanishes from the splash
// response, which is exactly when it stops being monitored. An extra target
// keeps it in the scan.
func TestExtraTargetsAreProbedAlongsideSplash(t *testing.T) {
	sp := fakeSplash(t)
	defer sp.Close()

	fake := newFakeCheckHost([]string{"5.161.128.192:8443"}, nil)
	ch := httptest.NewServer(fake.handler())
	defer ch.Close()

	cfg := testConfig(sp.URL, ch.URL, filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sc := scanner.New(cfg,
		splash.NewClient(cfg.SplashBaseURL, cfg.SplashClientKey, cfg.SplashDeviceID, cfg.SplashTimeout),
		checkhost.NewClient(cfg.CheckHostBaseURL, 0, cfg.PollInterval, cfg.ResultTimeout),
		st, log)
	sc.SetExtraTargets(func() []splash.Target {
		return splash.ParseExtraTargets("5.161.128.192:8443:US")
	})

	if _, err := sc.Start(context.Background(), scanner.TriggerManual); err != nil {
		t.Fatalf("start scan: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for sc.Running() {
		if time.Now().After(deadline) {
			t.Fatal("scan did not finish within 30s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	statuses, err := st.TargetStatuses()
	if err != nil {
		t.Fatalf("statuses: %v", err)
	}

	var found *store.TargetStatus
	for i := range statuses {
		if statuses[i].Address == "5.161.128.192" && statuses[i].Port == 8443 {
			found = &statuses[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the extra endpoint was never probed; got %d targets", len(statuses))
	}
	if found.Verdict != string(scanner.VerdictBlockedIR) {
		t.Errorf("verdict = %q, want BLOCKED_IR", found.Verdict)
	}
	if found.CountryCode != "US" {
		t.Errorf("country = %q, want US", found.CountryCode)
	}
	// The endpoints splash did return must still be there.
	if len(statuses) < 2 {
		t.Errorf("only %d targets: the extra endpoint replaced the splash ones", len(statuses))
	}
}
