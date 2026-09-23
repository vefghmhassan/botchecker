package checkhost

import (
	"context"
	"sync"
	"time"
)

// limiter spaces outbound requests, and learns how fast it is allowed to go.
//
// check-host.net does not publish its rate limits, and the create response's
// "limits" field is parsed but never actually sent by the live API — so there
// is nothing to read the ceiling from. The old client picked 2 seconds and
// stayed there, which capped the whole service at 30 requests a minute: a scan
// of 27 endpoints took 399 seconds, and roughly 8 of every 8.5 seconds per
// endpoint were spent waiting in this queue rather than on the network.
//
// So the interval is learned instead of guessed. It starts brisk, doubles on
// every 429, and creeps back toward the floor once the API has been quiet for a
// while. A ceiling that moves is better than a number nobody measured: it finds
// whatever check-host actually allows, and it backs off on its own if that
// changes.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time

	min, max time.Duration
	// burst lets a few requests go out back to back, so a handful of parallel
	// probes all start immediately rather than being strung out.
	burst int

	lastThrottle time.Time
	throttles    int
	// recoverAfter is how long the API must stay quiet before the interval is
	// allowed to shrink again.
	recoverAfter time.Duration
	lastRecover  time.Time
}

// limiterBounds are deliberately wide: the floor is fast enough that the
// network, not this queue, is the slow part, and the ceiling is slower than the
// fixed 2 seconds this replaced, so a genuinely angry API can still be obeyed.
const (
	limiterFloor   = 200 * time.Millisecond
	limiterStart   = 400 * time.Millisecond
	limiterCeiling = 5 * time.Second
	limiterBurst   = 4
	limiterRecover = 90 * time.Second
)

func newLimiter(interval time.Duration) *limiter {
	// A caller that asks for no spacing at all gets none; the tests rely on it.
	if interval <= 0 {
		return &limiter{}
	}
	start := interval
	if start > limiterCeiling {
		start = limiterCeiling
	}
	return &limiter{
		interval:     start,
		min:          limiterFloor,
		max:          limiterCeiling,
		burst:        limiterBurst,
		recoverAfter: limiterRecover,
		lastRecover:  time.Now(),
	}
}

// wait blocks until the next request is allowed, or ctx is done.
func (l *limiter) wait(ctx context.Context) error {
	if l == nil || l.interval <= 0 {
		return ctx.Err()
	}

	l.mu.Lock()
	now := time.Now()
	l.maybeRecover(now)

	// Every request takes the next slot and pushes the queue out by one
	// interval. Allowance accumulates while idle, but only up to burst
	// intervals — so a handful of parallel probes leave together and the rest
	// are spaced, rather than six probes being strung out over six intervals
	// before the first result can even start arriving.
	if earliest := now.Add(-time.Duration(l.burst-1) * l.interval); l.next.Before(earliest) {
		l.next = earliest
	}
	slot := l.next
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// throttled is called when check-host answers 429. The interval doubles and the
// next slot is pushed out, so the requests already queued behind this one do
// not arrive at the old rate.
func (l *limiter) throttled() {
	if l == nil || l.interval <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.interval *= 2
	if l.interval > l.max {
		l.interval = l.max
	}
	now := time.Now()
	l.lastThrottle = now
	l.lastRecover = now
	l.throttles++
	// The accumulated allowance is spent too: a 429 means it is already gone,
	// so the next request waits a full interval rather than going straight out.
	if pushed := now.Add(l.interval); pushed.After(l.next) {
		l.next = pushed
	}
}

// maybeRecover eases the interval back toward the floor after a quiet spell.
// Caller holds the lock.
func (l *limiter) maybeRecover(now time.Time) {
	if l.interval <= l.min || now.Sub(l.lastRecover) < l.recoverAfter {
		return
	}
	l.lastRecover = now
	// Twenty percent at a time: fast enough to matter within a few rounds,
	// slow enough that it does not immediately re-provoke the limit it just
	// backed away from.
	l.interval = time.Duration(float64(l.interval) * 0.8)
	if l.interval < l.min {
		l.interval = l.min
	}
}

// Rate is what the limiter has settled on, for the dashboard and the status
// endpoint. An operator asking "why is the scan slow" should be able to see
// whether it is us or check-host.
type Rate struct {
	Interval     time.Duration `json:"interval"`
	Floor        time.Duration `json:"floor"`
	Ceiling      time.Duration `json:"ceiling"`
	Burst        int           `json:"burst"`
	Throttles    int           `json:"throttles"`
	LastThrottle *time.Time    `json:"last_throttle,omitempty"`
}

func (l *limiter) rate() Rate {
	if l == nil {
		return Rate{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := Rate{
		Interval: l.interval, Floor: l.min, Ceiling: l.max,
		Burst: l.burst, Throttles: l.throttles,
	}
	if !l.lastThrottle.IsZero() {
		t := l.lastThrottle
		r.LastThrottle = &t
	}
	return r
}
