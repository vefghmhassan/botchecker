// Package watch re-probes addresses that are already known to be blocked,
// much more often than the full scan, so that a persistent block is confirmed
// within minutes instead of hours.
package watch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/provision"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

// TriggerScan is the scan trigger recorded for watch rounds, so they can be
// told apart from full scans in the history.
const TriggerScan = "watch"

// missingGrace is how long an endpoint that has left the panel keeps being
// watched. Long enough to ride out one bad sync, short enough that a deleted
// server stops costing probes within a quarter of an hour.
const missingGrace = 15 * time.Minute

// Watch scopes.
const (
	// ScopeAll re-probes every endpoint, so a healthy address that gets
	// filtered is caught within one watch interval.
	ScopeAll = "all"
	// ScopeBlocked re-probes only the already-blocked addresses, which is
	// cheaper but leaves new blocks to the next full scan.
	ScopeBlocked = "blocked"
)

type Watcher struct {
	cfg  *config.Config
	set  *settings.Provider
	st   *store.Store
	ch   *checkhost.Client
	prov *provision.Manager
	log  *slog.Logger

	irNodes map[string]bool

	mu      sync.Mutex
	running bool
	last    time.Time
}

func New(cfg *config.Config, set *settings.Provider, st *store.Store, ch *checkhost.Client,
	prov *provision.Manager, log *slog.Logger) *Watcher {

	ir := make(map[string]bool, len(cfg.IRNodes))
	for _, n := range cfg.IRNodes {
		ir[n] = true
	}
	return &Watcher{cfg: cfg, set: set, st: st, ch: ch, prov: prov, log: log, irNodes: ir}
}

// Start runs the watch loop until ctx is done.
func (w *Watcher) Start(ctx context.Context) {
	go func() {
		for {
			interval := w.interval()
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Error("watch round failed", "err", err)
			}
		}
	}()
}

// interval is read each round so a change made in the dashboard takes effect
// without a restart.
func (w *Watcher) interval() time.Duration {
	return w.set.Duration(settings.WatchInterval, w.cfg.WatchInterval)
}

func (w *Watcher) threshold() int {
	return w.set.Int(settings.OutageThreshold, w.cfg.OutageThreshold)
}

func (w *Watcher) window() time.Duration {
	return w.set.Duration(settings.OutageWindow, w.cfg.OutageWindow)
}

// Watchlist is the set of endpoints being re-probed, with their outage counts.
type Entry struct {
	store.TargetStatus
	Outages   int    `json:"outages"`
	Threshold int    `json:"threshold"`
	Window    string `json:"window"`
}

// scope reports whether every endpoint is re-probed or only the blocked ones.
func (w *Watcher) scope() string {
	if strings.EqualFold(w.set.Get(settings.WatchScope), ScopeBlocked) {
		return ScopeBlocked
	}
	return ScopeAll
}

// Watchlist returns the endpoints currently under watch.
//
// By default that is every known endpoint, not just the blocked ones: a healthy
// address that gets filtered would otherwise go unnoticed until the next full
// scan, which is far less frequent. Only the outage counting and the alert
// threshold are specific to blocked addresses.
func (w *Watcher) Watchlist() ([]Entry, error) {
	targets, err := w.st.TargetStatuses()
	if err != nil {
		return nil, err
	}

	blockedOnly := w.scope() == ScopeBlocked
	since := time.Now().Add(-w.window())

	var out []Entry
	for _, t := range targets {
		if blockedOnly && t.Verdict != string(scanner.VerdictBlockedIR) {
			continue
		}
		// Gone from the panel: nobody is being handed this endpoint, so
		// probing it costs check-host calls and can only produce alerts about
		// a server that no longer matters. Before the inventory sync existed,
		// a deleted server stayed on this list forever.
		//
		// The grace period is there because one bad read of the panel — a
		// restart, a half-loaded list — would otherwise switch monitoring off
		// for everything it missed until the next sync put it back.
		if t.MissingSince != nil && time.Since(*t.MissingSince) > missingGrace {
			continue
		}
		n, err := w.st.OutageCount(t.TargetID, since)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{
			TargetStatus: t, Outages: n,
			Threshold: w.threshold(), Window: w.window().String(),
		})
	}
	return out, nil
}

// LastRun reports when the watch loop last completed a round.
func (w *Watcher) LastRun() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// RunOnce probes every watched endpoint once.
func (w *Watcher) RunOnce(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		w.log.Warn("skipping watch round", "reason", "previous round still running")
		return nil
	}
	w.running = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running = false
		w.last = time.Now()
		w.mu.Unlock()
	}()

	watched, err := w.Watchlist()
	if err != nil {
		return err
	}
	if len(watched) == 0 {
		return nil
	}

	w.log.Info("watch round starting", "endpoints", len(watched))

	// Watch rounds are recorded as their own scan so the probe readings keep
	// their foreign keys and stay visible on the endpoint page.
	scanID, err := w.st.CreateScan(TriggerScan, time.Now())
	if err != nil {
		return err
	}
	if err := w.st.SetScanCounts(scanID, 0, len(watched)); err != nil {
		w.log.Warn("could not record watch scan counts", "err", err)
	}

	// Probing is parallel; acting on the result is not.
	//
	// A round used to take about five minutes against an eight-minute interval,
	// which meant a blocked machine needed roughly twenty-four minutes to reach
	// the outage threshold before a replacement could even start. None of that
	// time was ours: every endpoint was waiting on check-host, one after the
	// other.
	//
	// Firing stays sequential and happens after the whole round, because a
	// trigger buys servers and moves configs — it must not run several times at
	// once, and it must see the finished evidence rather than a half-counted
	// window.
	var (
		mu      sync.Mutex
		blocked int
		crossed []string
		seen    = map[string]bool{}
	)

	var wg sync.WaitGroup
	sem := make(chan struct{}, w.concurrency(len(watched)))

	for _, entry := range watched {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(entry Entry) {
			defer wg.Done()
			defer func() { <-sem }()

			isBlocked, address := w.probe(ctx, scanID, entry)
			mu.Lock()
			defer mu.Unlock()
			if isBlocked {
				blocked++
			}
			// Deduplicated by address: both ports of one machine crossing in
			// the same round is one machine to replace, not two.
			if address != "" && !seen[address] {
				seen[address] = true
				crossed = append(crossed, address)
			}
		}(entry)
	}
	wg.Wait()

	if err := w.st.FinishScan(scanID, store.ScanCompleted, blocked, nil, time.Now()); err != nil {
		w.log.Warn("could not finish the watch scan", "err", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, address := range crossed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.fire(ctx, address, w.outageCount(address))
	}
	return nil
}

// concurrency is how many endpoints are probed at once.
func (w *Watcher) concurrency(n int) int {
	want := w.cfg.ScanConcurrency
	if want < 1 {
		want = 1
	}
	if want > n {
		want = n
	}
	return want
}

// outageCount re-reads the window for one machine, so the number reported to
// the provisioning manager is the finished count rather than whatever it was
// when the port happened to cross.
func (w *Watcher) outageCount(address string) int {
	n, err := w.st.OutageCountForAddress(address, time.Now().Add(-w.window()))
	if err != nil {
		w.log.Warn("could not count outages", "address", address, "err", err)
	}
	return n
}

// probe checks one endpoint, records what it saw, and reports whether it came
// back blocked and — when the machine has crossed the outage threshold — which
// address needs acting on.
//
// It deliberately does not act. Buying a server takes minutes and must happen
// once per machine, so the caller collects the crossings and works through them
// after the round.
func (w *Watcher) probe(ctx context.Context, scanID int64, entry Entry) (blocked bool, crossed string) {
	target := entry.TargetStatus

	check, err := w.ch.CheckTCP(ctx, target.HostPort(), w.cfg.AllNodes())
	if err != nil {
		w.log.Warn("watch probe failed", "target", target.HostPort(), "err", err)
		return false, ""
	}

	assess := scanner.Classify(check.Results, w.irNodes, w.cfg.MinIRFail, w.cfg.ControlEnabled())
	now := time.Now()

	resultID, err := w.st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: target.TargetID, Verdict: string(assess.Verdict),
		IROpen: assess.IROpen, IRTimeout: assess.IRTimeout, IRRefused: assess.IRRefused,
		IRTotal: assess.IRTotal, ControlOpen: assess.ControlOpen, ControlTotal: assess.ControlTotal,
		Confirmed: false, PermanentLink: check.PermanentLink, CheckedAt: now,
	})
	if err != nil {
		w.log.Warn("could not store the watch result", "target", target.HostPort(), "err", err)
		return false, ""
	}
	if err := w.st.InsertNodeResults(resultID, check.Results); err != nil {
		w.log.Warn("could not store watch node results", "err", err)
	}
	if _, err := w.st.RecordVerdict(target.TargetID, string(assess.Verdict), scanID, now); err != nil {
		w.log.Warn("could not record the watch verdict", "err", err)
	}

	if assess.Verdict != scanner.VerdictBlockedIR {
		// One good reading clears the history: a slow drip of unrelated
		// failures over days must not add up to a trigger.
		if err := w.st.ClearOutages(target.TargetID); err != nil {
			w.log.Warn("could not clear outages", "err", err)
		}
		w.log.Info("watched endpoint recovered", "target", target.HostPort(), "verdict", assess.Verdict)
		return false, ""
	}

	if err := w.st.RecordOutage(target.TargetID, resultID, scanID, now); err != nil {
		w.log.Warn("could not record the outage", "err", err)
		return true, ""
	}

	// Counted across the machine, not this port. A per-IP block takes every
	// port down at the same moment, so counting per port means each port
	// separately reaches the threshold and separately asks for a replacement of
	// the same machine.
	count, err := w.st.OutageCountForAddress(target.Address, now.Add(-w.window()))
	if err != nil {
		w.log.Warn("could not count outages", "err", err)
		return true, ""
	}

	threshold := w.threshold()
	w.log.Info("watched endpoint still blocked",
		"target", target.HostPort(), "address_outages", count, "threshold", threshold)

	if count >= threshold {
		return true, target.Address
	}
	return true, ""
}

// fire hands the whole machine to the provisioning manager and resets the
// counter so the same window cannot trigger twice.
//
// The counter is cleared after Trigger returns, and not at all when the manager
// was busy. Clearing first threw the evidence away on a busy run, and the
// machine then had to accumulate the whole window again from zero before it
// could ask a second time. The watch loop is single-threaded, so nothing can
// record an outage while Trigger runs.
func (w *Watcher) fire(ctx context.Context, address string, count int) {
	addr, err := w.st.AddressFor(address)
	if err != nil {
		w.log.Error("could not load the machine", "address", address, "err", err)
		return
	}

	w.log.Info("outage threshold reached",
		"address", address, "ports", addr.PortNumbers(), "outages", count)

	// A machine this service destroyed on purpose keeps failing forever, so it
	// keeps reaching the threshold. The manager knows what to do with it and
	// reports it once a day, but there is no reason to wake it every round: the
	// counter is spent here and the loop moves on.
	if burned, _, err := w.st.IsBurned(address); err != nil {
		w.log.Warn("could not read the address ledger", "address", address, "err", err)
	} else if burned {
		w.log.Info("machine is in the ledger, leaving it alone", "address", address)
		if err := w.st.ClearOutagesForAddress(address); err != nil {
			w.log.Warn("could not reset the outage counter", "err", err)
		}
		return
	}

	if err := w.prov.Trigger(ctx, addr, count); err != nil {
		if errors.Is(err, provision.ErrBusy) {
			w.log.Warn("provisioning is busy, will retry next window", "address", address)
			return
		}
		w.log.Error("provisioning failed", "address", address, "err", err)
	}

	// Every port's evidence is spent: the decision was made for all of them.
	// Left alone, the sibling port crosses its own threshold on the next round
	// and asks again for a machine that is already being dealt with.
	if err := w.st.ClearOutagesForAddress(address); err != nil {
		w.log.Warn("could not reset the outage counter", "err", err)
	}
}
