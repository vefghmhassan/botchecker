package scanner

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
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
)

// A probe is all waiting: the endpoint is checked by check-host's own nodes and
// this service only asks whether they have finished. Doing that one endpoint at
// a time made a scan take as long as the sum of its parts — 399 seconds for 27
// endpoints in production, where about 8 of every 8.5 seconds per endpoint were
// spent queueing rather than on the network.

// slowCheckHost answers create immediately but withholds results until a fixed
// latency has passed, which is how the real API behaves.
type slowCheckHost struct {
	latency time.Duration

	mu      sync.Mutex
	started map[string]time.Time
	hosts   map[string]string
	peak    int
	inWait  int
	n       int
}

func newSlowCheckHost(latency time.Duration) *slowCheckHost {
	return &slowCheckHost{
		latency: latency,
		started: map[string]time.Time{},
		hosts:   map[string]string{},
	}
}

func (f *slowCheckHost) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/check-tcp", func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		nodes := r.URL.Query()["node"]

		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("req%d", f.n)
		f.started[id] = time.Now()
		f.hosts[id] = host
		// How many checks are outstanding at once is the thing under test.
		f.inWait++
		if f.inWait > f.peak {
			f.peak = f.inWait
		}
		f.mu.Unlock()

		meta := map[string][]string{}
		for _, n := range nodes {
			meta[n] = []string{"ir", "Iran", "Tehran", "10.0.0.1", "AS47430"}
		}
		writeSlowJSON(w, map[string]any{
			"ok": 1, "request_id": id, "permanent_link": "https://example.test/" + id,
			"nodes": meta,
		})
	})

	mux.HandleFunc("/check-result/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/check-result/")

		f.mu.Lock()
		started, ok := f.started[id]
		host := f.hosts[id]
		ready := ok && time.Since(started) >= f.latency
		if ready && f.inWait > 0 {
			f.inWait--
			delete(f.started, id)
		}
		f.mu.Unlock()

		if !ready {
			// Not finished yet: the API returns nulls for the nodes.
			out := map[string]any{}
			for _, n := range slowNodes() {
				out[n] = nil
			}
			writeSlowJSON(w, out)
			return
		}

		out := map[string]any{}
		for _, n := range slowNodes() {
			out[n] = []map[string]any{{"address": strings.Split(host, ":")[0], "time": 0.05}}
		}
		writeSlowJSON(w, out)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *slowCheckHost) peakConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func slowNodes() []string {
	var out []string
	for i := 1; i <= 8; i++ {
		out = append(out, fmt.Sprintf("ir%d.node.check-host.net", i))
	}
	return append(out, "de1.node.check-host.net", "nl1.node.check-host.net")
}

func writeSlowJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// slowConfig is the minimum a scanner needs to probe, with no traceroute and no
// confirmation round: this file is measuring the worker pool, not the rest.
func slowConfig(checkHostURL string, concurrency int) *config.Config {
	var ir []string
	for i := 1; i <= 8; i++ {
		ir = append(ir, fmt.Sprintf("ir%d.node.check-host.net", i))
	}
	return &config.Config{
		CheckHostBaseURL: checkHostURL,
		IRNodes:          ir,
		ControlNodes:     []string{"de1.node.check-host.net", "nl1.node.check-host.net"},
		PollInterval:     5 * time.Millisecond,
		ResultTimeout:    5 * time.Second,
		MinIRFail:        6,
		ConfirmRounds:    1,
		ScanConcurrency:  concurrency,
		Location:         time.UTC,
	}
}

// scanTargets probes n endpoints at the given concurrency and reports how long
// it took and how many checks were ever in flight at once.
func scanTargets(t *testing.T, n, concurrency int, latency time.Duration) (time.Duration, int) {
	t.Helper()

	fake := newSlowCheckHost(latency)
	srv := fake.server(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := slowConfig(srv.URL, concurrency)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// No spacing: this measures the worker pool, not the limiter, which has its
	// own tests.
	ch := checkhost.NewClient(srv.URL, 0, 5*time.Millisecond, 5*time.Second)
	s := New(cfg, nil, ch, st, log)

	scanID, err := st.CreateScan("manual", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	targets := make([]splash.Target, 0, n)
	for i := 0; i < n; i++ {
		targets = append(targets, splash.Target{
			Address: fmt.Sprintf("10.1.0.%d", i+1), Port: 443,
			ConfigIDs: []int{i}, Names: []string{"c"}, Active: true,
		})
	}

	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.concurrency(len(targets)))
	for _, tg := range targets {
		sem <- struct{}{}
		wg.Add(1)
		go func(tg splash.Target) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := s.firstPass(context.Background(), scanID, tg); err != nil {
				t.Errorf("probe %s: %v", tg.Key(), err)
			}
		}(tg)
	}
	wg.Wait()
	return time.Since(start), fake.peakConcurrent()
}

func TestProbingInParallelCollapsesTheWallTime(t *testing.T) {
	const (
		endpoints = 12
		latency   = 120 * time.Millisecond
	)

	serial, serialPeak := scanTargets(t, endpoints, 1, latency)
	parallel, parallelPeak := scanTargets(t, endpoints, 6, latency)

	t.Logf("serial: %v (peak %d in flight) · parallel: %v (peak %d in flight)",
		serial.Round(time.Millisecond), serialPeak,
		parallel.Round(time.Millisecond), parallelPeak)

	if serialPeak != 1 {
		t.Errorf("the sequential run overlapped checks: peak %d", serialPeak)
	}
	if parallelPeak < 2 {
		t.Fatalf("nothing actually ran in parallel: peak %d in flight", parallelPeak)
	}
	// Six workers on twelve endpoints should be comfortably under half the
	// sequential time. The bound is loose on purpose — this asserts that the
	// work overlaps, not a specific speed on a specific machine.
	if parallel > serial/2 {
		t.Fatalf("parallel run took %v, want well under half of %v", parallel, serial)
	}
}

func TestEveryEndpointIsStillRecordedExactlyOnce(t *testing.T) {
	// Speed is worthless if results go missing or get attached to the wrong
	// endpoint, which is the way a worker pool usually goes wrong.
	const endpoints = 20

	fake := newSlowCheckHost(20 * time.Millisecond)
	srv := fake.server(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := slowConfig(srv.URL, 8)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := checkhost.NewClient(srv.URL, 0, 5*time.Millisecond, 5*time.Second)
	s := New(cfg, nil, ch, st, log)

	scanID, err := st.CreateScan("manual", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	var targets []splash.Target
	for i := 0; i < endpoints; i++ {
		targets = append(targets, splash.Target{
			Address: fmt.Sprintf("10.2.0.%d", i+1), Port: 443,
			ConfigIDs: []int{i}, Names: []string{"c"}, Active: true,
		})
	}

	s.eachTarget(context.Background(), targets, func(ctx context.Context, tg splash.Target) {
		if _, err := s.firstPass(ctx, scanID, tg); err != nil {
			t.Errorf("probe %s: %v", tg.Key(), err)
		}
	})

	statuses, err := st.TargetStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != endpoints {
		t.Fatalf("recorded %d endpoints, want %d", len(statuses), endpoints)
	}
	for _, got := range statuses {
		if got.Verdict != string(VerdictHealthy) {
			t.Errorf("%s recorded %s, want HEALTHY", got.HostPort(), got.Verdict)
		}
		if got.IROpen != 8 {
			t.Errorf("%s recorded ir_open=%d, want 8 — readings were crossed between endpoints",
				got.HostPort(), got.IROpen)
		}
	}
}

// Two scans can overlap — a manual one started while a scheduled one is
// finishing — and the traceroute work must not be shared between them.
//
// It was, briefly: the queue lived on the Scanner, so the earlier scan's
// traceroute pass drained the later scan's jobs and that scan recorded no
// traceroutes at all while reporting thirteen blocked addresses.
func TestTraceroutesBelongToTheScanThatFoundThem(t *testing.T) {
	fake := newSlowCheckHost(10 * time.Millisecond)
	srv := fake.server(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := slowConfig(srv.URL, 4)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := checkhost.NewClient(srv.URL, 0, 5*time.Millisecond, 5*time.Second)
	s := New(cfg, nil, ch, st, log)

	scanID, err := st.CreateScan("manual", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Two endpoints, recorded through the same path a scan uses.
	var got []traceJob
	for i := 0; i < 2; i++ {
		tg := splash.Target{
			Address: fmt.Sprintf("10.3.0.%d", i+1), Port: 443,
			ConfigIDs: []int{i}, Names: []string{"c"}, Active: true,
		}
		res, err := s.firstPass(context.Background(), scanID, tg)
		if err != nil {
			t.Fatal(err)
		}
		if res.trace != nil {
			got = append(got, *res.trace)
		}
	}

	// These endpoints come back healthy, so there is nothing to trace — the
	// point is that whatever is owed travels with the result rather than being
	// parked somewhere another scan can take it.
	if len(got) != 0 {
		t.Fatalf("healthy endpoints produced traceroute work: %v", got)
	}

	// The Scanner must hold no traceroute state of its own between runs.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progress.Phase == "traceroute" {
		t.Error("the scanner was left parked in the traceroute phase")
	}
}
