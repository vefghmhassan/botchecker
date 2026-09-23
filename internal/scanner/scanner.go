// Package scanner drives one pass over the configured endpoints: fetch the
// server list, probe each unique address from Iran, classify, and persist.
package scanner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
)

// ErrBusy is returned when a scan is requested while one is already running.
var ErrBusy = errors.New("a scan is already in progress")

const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

type Scanner struct {
	cfg    *config.Config
	splash *splash.Client
	ch     *checkhost.Client
	st     *store.Store
	log    *slog.Logger

	irNodes map[string]bool

	mu       sync.Mutex
	running  bool
	progress Progress
	provider ProviderResolver
	extra    ExtraTargetSource
	source   TargetSource
}

// Progress is a snapshot of the scan in flight.
type Progress struct {
	ScanID    int64     `json:"scan_id"`
	Running   bool      `json:"running"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Phase     string    `json:"phase"`
	Current   string    `json:"current,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func New(cfg *config.Config, sp *splash.Client, ch *checkhost.Client, st *store.Store, log *slog.Logger) *Scanner {
	ir := make(map[string]bool, len(cfg.IRNodes))
	for _, n := range cfg.IRNodes {
		ir[n] = true
	}
	return &Scanner{cfg: cfg, splash: sp, ch: ch, st: st, log: log, irNodes: ir}
}

func (s *Scanner) Progress() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress
}

func (s *Scanner) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Start creates the scan row and runs the scan in the background, returning
// the new scan id immediately. ctx governs the whole background run, so it
// must outlive the request that triggered it.
func (s *Scanner) Start(ctx context.Context, trigger string) (int64, error) {
	return s.start(ctx, trigger, nil)
}

// start runs a scan over the given endpoints, or over the whole fleet when
// given none.
func (s *Scanner) start(ctx context.Context, trigger string, given []splash.Target) (int64, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return 0, ErrBusy
	}

	now := time.Now()
	scanID, err := s.st.CreateScan(trigger, now)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}

	s.running = true
	s.progress = Progress{ScanID: scanID, Running: true, Phase: "fetching config", StartedAt: now}
	s.mu.Unlock()

	go func() {
		blocked, err := s.execute(ctx, scanID, given)

		status := store.ScanCompleted
		if err != nil {
			status = store.ScanFailed
			s.log.Error("scan failed", "scan_id", scanID, "err", err)
		} else {
			s.log.Info("scan finished", "scan_id", scanID, "blocked", blocked)
		}
		if ferr := s.st.FinishScan(scanID, status, blocked, err, time.Now()); ferr != nil {
			s.log.Error("could not finalise scan", "scan_id", scanID, "err", ferr)
		}

		s.mu.Lock()
		s.running = false
		s.progress.Running = false
		s.progress.Phase = status
		s.progress.Current = ""
		s.mu.Unlock()
	}()

	return scanID, nil
}

func (s *Scanner) execute(ctx context.Context, scanID int64, given []splash.Target) (int, error) {
	// A scan handed a list probes that list — the update button uses this to
	// look at only the endpoints that have just appeared.
	targets, configs := given, 0
	if targets == nil {
		var err error
		if targets, configs, err = s.discover(ctx); err != nil {
			return 0, err
		}
	}

	if err := s.st.SetScanCounts(scanID, configs, len(targets)); err != nil {
		s.log.Warn("could not store scan counts", "err", err)
	}
	s.log.Info("config fetched", "scan_id", scanID, "configs", configs, "unique_targets", len(targets))

	s.setProgress(func(p *Progress) {
		p.Total = len(targets)
		p.Phase = "probing"
	})

	// Endpoints are probed in parallel. Nothing here is CPU work — every
	// endpoint spends its time waiting on check-host — so running them one at a
	// time made a scan take as long as the sum of its parts: 399 seconds for 27
	// endpoints, of which about 8 of every 8.5 per endpoint were spent waiting
	// in a queue. The store serialises its own writes on a single connection,
	// so the only shared state that needs guarding is the counters.
	// Collected per run, not on the Scanner: two scans can overlap — a manual
	// one started while a scheduled one is finishing — and a queue shared
	// between them means one drains the other's work, which is exactly how
	// scan 99 recorded no traceroutes at all.
	var (
		mu      sync.Mutex
		blocked int
		waiting []*deferredConfirm
		traces  []traceJob
	)

	s.eachTarget(ctx, targets, func(ctx context.Context, t splash.Target) {
		res, err := s.firstPass(ctx, scanID, t)
		if err != nil {
			s.log.Warn("target check failed", "target", t.Key(), "err", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if res.confirm != nil {
			waiting = append(waiting, res.confirm)
			return
		}
		if res.trace != nil {
			traces = append(traces, *res.trace)
		}
		if res.verdict == VerdictBlockedIR {
			blocked++
		}
	})
	if ctx.Err() != nil {
		return blocked, ctx.Err()
	}

	// The second opinion. It used to be taken inline, which meant every
	// endpoint that needed one stopped the whole scan for the confirm delay —
	// one endpoint in the measured scan took 70.9 seconds where its neighbours
	// took 8.5. Held until the first pass is done, the delay has usually
	// already elapsed on its own and costs nothing.
	if len(waiting) > 0 {
		s.setProgress(func(p *Progress) { p.Phase = "confirming" })
		s.log.Info("confirming endpoints", "scan_id", scanID, "count", len(waiting))

		s.eachConfirm(ctx, waiting, func(ctx context.Context, d *deferredConfirm) {
			verdict, trace, err := s.confirmPass(ctx, scanID, d)
			if err != nil {
				s.log.Warn("confirmation failed", "target", d.target.Key(), "err", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if trace != nil {
				traces = append(traces, *trace)
			}
			if verdict == VerdictBlockedIR {
				blocked++
			}
		})
	}

	s.traceAll(ctx, traces)

	s.setProgress(func(p *Progress) {
		p.Done = len(targets)
		p.Current = ""
		p.Phase = "probing"
	})
	return blocked, ctx.Err()
}

// concurrency is how many endpoints are probed at once. The limiter decides the
// actual request rate; this only decides how many checks may be in flight
// waiting for check-host to finish them.
func (s *Scanner) concurrency(n int) int {
	want := s.cfg.ScanConcurrency
	if want < 1 {
		want = 1
	}
	if want > n {
		want = n
	}
	return want
}

// eachTarget runs fn over every target with a bounded worker pool, keeping the
// progress counter honest as they finish.
func (s *Scanner) eachTarget(ctx context.Context, targets []splash.Target,
	fn func(context.Context, splash.Target)) {

	var wg sync.WaitGroup
	sem := make(chan struct{}, s.concurrency(len(targets)))

	for _, t := range targets {
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
		go func(t splash.Target) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx, t)
			// Done counts completions, not position: with several in flight
			// there is no single "current" endpoint to report.
			s.setProgress(func(p *Progress) {
				p.Done++
				p.Current = t.Key()
			})
		}(t)
	}
	wg.Wait()
}

// eachConfirm is eachTarget for the second round.
func (s *Scanner) eachConfirm(ctx context.Context, pending []*deferredConfirm,
	fn func(context.Context, *deferredConfirm)) {

	var wg sync.WaitGroup
	sem := make(chan struct{}, s.concurrency(len(pending)))

	for _, d := range pending {
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
		go func(d *deferredConfirm) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx, d)
		}(d)
	}
	wg.Wait()
}

// deferredConfirm is an endpoint whose first reading needs a second opinion,
// held over until the first pass is finished.
//
// The reading itself is carried, not re-derived: the confirm round compares
// against what was actually seen, and the result is written once, after the
// rounds agree or stop agreeing.
type deferredConfirm struct {
	target    splash.Target
	targetID  int64
	first     Verdict
	check     *checkhost.TCPCheck
	assess    Assessment
	probedAt  time.Time
	roundsRun int
}

// passResult is what one endpoint produced in the first pass: either a settled
// verdict, or a second opinion still owed — and, when it came back blocked, the
// traceroute that is owed on it.
type passResult struct {
	verdict Verdict
	confirm *deferredConfirm
	trace   *traceJob
}

// firstPass probes one endpoint once and records the ownership.
//
// A verdict that needs confirming is handed back rather than waited on. The
// old code slept the confirm delay right here, which stopped every other
// endpoint in the scan for thirty seconds at a time.
func (s *Scanner) firstPass(ctx context.Context, scanID int64, t splash.Target) (passResult, error) {
	now := time.Now()
	targetID, err := s.st.UpsertTarget(t, now)
	if err != nil {
		return passResult{}, err
	}

	// Ownership is refreshed on every full scan so an address moved between
	// providers is reclassified rather than staying stale.
	if r := s.resolver(); r != nil {
		info := r.Resolve(ctx, t.Address)
		// Written for the whole machine: ownership is a property of the
		// address, and one write keeps both ports of a dual-port machine from
		// ever disagreeing about which project holds it.
		if err := s.st.SetAddressProvider(t.Address, info.Provider, info.Project, info.Kind,
			info.ServerID, info.ServerName, info.AbuseBlocked, now); err != nil {
			s.log.Warn("could not record the provider", "target", t.Key(), "err", err)
		}
	}

	check, assess, err := s.probe(ctx, t)
	if err != nil {
		return passResult{}, err
	}

	if NeedsConfirmation(assess.Verdict) && s.cfg.ConfirmRounds > 1 {
		return passResult{confirm: &deferredConfirm{
			target: t, targetID: targetID, first: assess.Verdict,
			check: check, assess: assess, probedAt: time.Now(), roundsRun: 1,
		}}, nil
	}

	trace, err := s.record(ctx, scanID, t, targetID, check, assess, true)
	if err != nil {
		return passResult{}, err
	}
	return passResult{verdict: assess.Verdict, trace: trace}, nil
}

// confirmPass takes the second opinion on an endpoint the first pass flagged.
//
// The delay between observations is preserved, but only the part that has not
// already passed: the point was ever to look twice at different moments, not to
// spend thirty seconds doing nothing.
func (s *Scanner) confirmPass(ctx context.Context, scanID int64, d *deferredConfirm) (Verdict, *traceJob, error) {
	confirmed := true
	check, assess := d.check, d.assess

	for round := d.roundsRun + 1; round <= s.cfg.ConfirmRounds; round++ {
		if wait := s.cfg.ConfirmDelay - time.Since(d.probedAt); wait > 0 {
			if err := sleep(ctx, wait); err != nil {
				return VerdictUnknown, nil, err
			}
		}

		nextCheck, nextAssess, err := s.probe(ctx, d.target)
		if err != nil {
			s.log.Warn("confirmation round failed, keeping first result",
				"target", d.target.Key(), "err", err)
			confirmed = false
			break
		}
		d.probedAt = time.Now()
		check, assess = nextCheck, nextAssess

		if assess.Verdict != d.first {
			// The two rounds disagree: the newer reading wins but is flagged
			// unconfirmed so a flapping node cannot masquerade as a block.
			confirmed = false
			break
		}
	}

	trace, err := s.record(ctx, scanID, d.target, d.targetID, check, assess, confirmed)
	if err != nil {
		return VerdictUnknown, nil, err
	}
	return assess.Verdict, trace, nil
}

// record persists one endpoint's reading, and traces where packets stop when
// the address turns out to be blocked.
func (s *Scanner) record(ctx context.Context, scanID int64, t splash.Target, targetID int64,
	check *checkhost.TCPCheck, assess Assessment, confirmed bool) (trace *traceJob, err error) {

	// One timestamp for both the result and the transition: the traceroute
	// that follows can take minutes, and an endpoint must not appear to have
	// been blocked later than it was last checked.
	checkedAt := time.Now()
	resultID, err := s.st.InsertResult(store.ResultRecord{
		ScanID:        scanID,
		TargetID:      targetID,
		Verdict:       string(assess.Verdict),
		IROpen:        assess.IROpen,
		IRTimeout:     assess.IRTimeout,
		IRRefused:     assess.IRRefused,
		IRTotal:       assess.IRTotal,
		ControlOpen:   assess.ControlOpen,
		ControlTotal:  assess.ControlTotal,
		Confirmed:     confirmed,
		PermanentLink: check.PermanentLink,
		CheckedAt:     checkedAt,
	})
	if err != nil {
		return nil, err
	}
	if err := s.st.InsertNodeResults(resultID, check.Results); err != nil {
		s.log.Warn("could not store node results", "target", t.Key(), "err", err)
	}

	if assess.Verdict == VerdictBlockedIR && s.cfg.TracerouteEnabled {
		// Handed back, not run here. A traceroute takes up to 150 seconds and
		// tells us where packets stop — useful, but it is an annotation on a
		// verdict that has already been decided. Running it inline held one of
		// the probe workers for the whole time, so seven blocked addresses
		// turned a scan whose probing finished in seconds into one that took
		// 137.
		trace = &traceJob{target: t, resultID: resultID}
	}

	if _, err := s.st.RecordVerdict(targetID, string(assess.Verdict), scanID, checkedAt); err != nil {
		s.log.Warn("could not record verdict transition", "target", t.Key(), "err", err)
	}
	return trace, nil
}

// traceJob is one blocked endpoint waiting to be traced.
type traceJob struct {
	target   splash.Target
	resultID int64
}

// traceAll records where packets stop for every blocked address found.
//
// They all run together: a traceroute is pure waiting on check-host's own
// probes, so the pass costs about as long as the slowest single trace rather
// than the sum of them.
func (s *Scanner) traceAll(ctx context.Context, jobs []traceJob) {
	if len(jobs) == 0 {
		return
	}
	s.setProgress(func(p *Progress) { p.Phase = "traceroute" })
	defer s.setProgress(func(p *Progress) { p.Phase = "probing" })

	s.log.Info("tracing blocked addresses", "count", len(jobs))

	var wg sync.WaitGroup
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		wg.Add(1)
		go func(job traceJob) {
			defer wg.Done()
			s.runTraceroute(ctx, job.target, job.resultID)
		}(job)
	}
	wg.Wait()
}

func (s *Scanner) probe(ctx context.Context, t splash.Target) (*checkhost.TCPCheck, Assessment, error) {
	check, err := s.ch.CheckTCP(ctx, t.Key(), s.cfg.AllNodes())
	if err != nil {
		return nil, Assessment{}, err
	}
	a := Classify(check.Results, s.irNodes, s.cfg.MinIRFail, s.cfg.ControlEnabled())
	return check, a, nil
}

// runTraceroute records where packets stop. A last responding hop on a private
// address means they never left the operator's own network.
func (s *Scanner) runTraceroute(ctx context.Context, t splash.Target, resultID int64) {
	s.setProgress(func(p *Progress) { p.Phase = "traceroute" })
	defer s.setProgress(func(p *Progress) { p.Phase = "probing" })

	traces, err := s.ch.Traceroute(ctx, t.Address, s.cfg.TracerouteNodes, s.cfg.TracerouteTimeout)
	if err != nil {
		s.log.Warn("traceroute failed", "target", t.Key(), "err", err)
		return
	}
	for _, tr := range traces {
		if err := s.st.InsertTraceroute(resultID, tr, time.Now()); err != nil {
			s.log.Warn("could not store traceroute", "target", t.Key(), "err", err)
		}
	}
}

func (s *Scanner) setProgress(fn func(*Progress)) {
	s.mu.Lock()
	fn(&s.progress)
	s.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
