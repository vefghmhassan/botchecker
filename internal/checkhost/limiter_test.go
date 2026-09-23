package checkhost

import (
	"context"
	"testing"
	"time"
)

// The old limiter held a fixed two-second gap that nobody had measured, and it
// capped the whole service at 30 requests a minute. These lock in the rule that
// replaced it: go briskly, obey a refusal immediately, and only speed back up
// after the API has been quiet.

func TestARefusalSlowsTheLimiterDown(t *testing.T) {
	l := newLimiter(400 * time.Millisecond)
	before := l.rate().Interval

	l.throttled()

	after := l.rate()
	if after.Interval <= before {
		t.Fatalf("a 429 did not slow anything down: %v then %v", before, after.Interval)
	}
	if after.Interval != 2*before {
		t.Errorf("interval = %v, want double %v", after.Interval, before)
	}
	if after.Throttles != 1 || after.LastThrottle == nil {
		t.Errorf("the refusal was not recorded: %+v", after)
	}
}

func TestBackingOffStopsAtTheCeiling(t *testing.T) {
	l := newLimiter(400 * time.Millisecond)
	// However angry check-host gets, the client must not wander off into
	// intervals so long that a scan never finishes.
	for i := 0; i < 20; i++ {
		l.throttled()
	}
	if got := l.rate().Interval; got != limiterCeiling {
		t.Fatalf("interval = %v, want the ceiling %v", got, limiterCeiling)
	}
}

func TestQuietTimeEasesTheIntervalBackDownButNeverBelowTheFloor(t *testing.T) {
	l := newLimiter(400 * time.Millisecond)
	l.throttled()
	raised := l.rate().Interval

	// Pretend the quiet period has already passed, repeatedly.
	for i := 0; i < 50; i++ {
		l.mu.Lock()
		l.lastRecover = time.Now().Add(-2 * l.recoverAfter)
		l.maybeRecover(time.Now())
		l.mu.Unlock()
	}

	got := l.rate().Interval
	if got >= raised {
		t.Fatalf("the interval never recovered: raised to %v, still %v", raised, got)
	}
	// The floor exists so a recovering limiter cannot end up hammering the API
	// harder than it was ever allowed to.
	if got < limiterFloor {
		t.Fatalf("interval %v went below the floor %v", got, limiterFloor)
	}
}

func TestRecoveryWaitsForTheQuietPeriod(t *testing.T) {
	l := newLimiter(400 * time.Millisecond)
	l.throttled()
	raised := l.rate().Interval

	// No time has passed, so nothing should change: speeding up again straight
	// after a refusal would just provoke the next one.
	l.mu.Lock()
	l.maybeRecover(time.Now())
	l.mu.Unlock()

	if got := l.rate().Interval; got != raised {
		t.Fatalf("interval moved during the quiet period: %v then %v", raised, got)
	}
}

func TestABurstOfProbesLeavesTogether(t *testing.T) {
	// Six parallel probes must not be strung out over six intervals before the
	// first result can even start arriving.
	l := newLimiter(time.Second)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < limiterBurst; i++ {
		if err := l.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("the burst was serialised: %d requests took %v", limiterBurst, elapsed)
	}

	// And the one after the burst does wait, or there would be no limit at all.
	start = time.Now()
	if err := l.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("the request after the burst was not spaced: %v", elapsed)
	}
}

func TestACancelledContextDoesNotBlock(t *testing.T) {
	l := newLimiter(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < limiterBurst; i++ {
		_ = l.wait(ctx)
	}
	cancel()

	done := make(chan error, 1)
	go func() { done <- l.wait(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want the context error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait ignored a cancelled context")
	}
}
