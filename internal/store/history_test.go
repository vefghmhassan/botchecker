package store

import (
	"math"
	"testing"
	"time"
)

func ev(targetID int64, addr string, to string, day int, base time.Time) Event {
	return Event{
		TargetID:  targetID,
		Address:   addr,
		Port:      443,
		ToVerdict: to,
		ChangedAt: base.AddDate(0, 0, day),
	}
}

func TestBuildTimelineCarriesVerdictForward(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	base := now.AddDate(0, 0, -9) // ten-day window starts here

	events := []Event{
		ev(1, "1.1.1.1", "HEALTHY", 0, base),
		ev(1, "1.1.1.1", "BLOCKED_IR", 5, base),
		ev(2, "2.2.2.2", "HEALTHY", 0, base),
	}

	days := BuildTimeline(events, 10, now, time.UTC)
	if len(days) != 10 {
		t.Fatalf("got %d days, want 10", len(days))
	}

	// Day 0: both healthy. A verdict persists until the next transition, so
	// days with no event still report the carried-forward state.
	if got := days[0].Count("HEALTHY"); got != 2 {
		t.Errorf("day 0 HEALTHY = %d, want 2", got)
	}
	if got := days[4].Count("HEALTHY"); got != 2 {
		t.Errorf("day 4 HEALTHY = %d, want 2 (no event that day)", got)
	}
	// Day 5 onwards: target 1 is blocked.
	if got := days[5].Count("BLOCKED_IR"); got != 1 {
		t.Errorf("day 5 BLOCKED_IR = %d, want 1", got)
	}
	if got := days[9].Count("BLOCKED_IR"); got != 1 {
		t.Errorf("day 9 BLOCKED_IR = %d, want 1", got)
	}
	if got := days[9].Count("HEALTHY"); got != 1 {
		t.Errorf("day 9 HEALTHY = %d, want 1", got)
	}
}

func TestBuildTimelineIgnoresTargetsNotYetSeen(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	base := now.AddDate(0, 0, -9)

	// The endpoint first appears on day 6, so earlier days must not count it.
	events := []Event{ev(1, "1.1.1.1", "HEALTHY", 6, base)}

	days := BuildTimeline(events, 10, now, time.UTC)
	if days[0].Total != 0 {
		t.Errorf("day 0 total = %d, want 0 before the endpoint existed", days[0].Total)
	}
	if days[6].Total != 1 {
		t.Errorf("day 6 total = %d, want 1", days[6].Total)
	}
}

func TestBuildTimelineHandlesNoEvents(t *testing.T) {
	days := BuildTimeline(nil, 30, time.Now(), time.UTC)
	if len(days) != 30 {
		t.Fatalf("got %d days, want 30", len(days))
	}
	for i, d := range days {
		if d.Total != 0 {
			t.Fatalf("day %d total = %d, want 0", i, d.Total)
		}
	}
}

func TestBuildLifetimesMeasuresHealthyRuns(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	base := now.AddDate(0, 0, -30)

	events := []Event{
		// Ran healthy for 10 days, then was blocked.
		ev(1, "1.1.1.1", "HEALTHY", 0, base),
		ev(1, "1.1.1.1", "BLOCKED_IR", 10, base),
		// Ran healthy for 4 days, then was blocked.
		ev(2, "2.2.2.2", "HEALTHY", 2, base),
		ev(2, "2.2.2.2", "BLOCKED_IR", 6, base),
		// Still healthy: measured up to now but excluded from the averages.
		ev(3, "3.3.3.3", "HEALTHY", 5, base),
	}

	sum := BuildLifetimes(events, now)

	if sum.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", sum.Completed)
	}
	if sum.Ongoing != 1 {
		t.Errorf("Ongoing = %d, want 1", sum.Ongoing)
	}
	if !closeTo(sum.MinDays, 4) || !closeTo(sum.MaxDays, 10) {
		t.Errorf("min/max = %.2f/%.2f, want 4/10", sum.MinDays, sum.MaxDays)
	}
	if !closeTo(sum.AvgDays, 7) {
		t.Errorf("AvgDays = %.2f, want 7", sum.AvgDays)
	}
	if !closeTo(sum.MedDays, 7) {
		t.Errorf("MedDays = %.2f, want 7", sum.MedDays)
	}

	// Spans are ordered longest first, with the ongoing run measured to now.
	if len(sum.Spans) != 3 {
		t.Fatalf("Spans = %d, want 3", len(sum.Spans))
	}
	if sum.Spans[0].Address != "3.3.3.3" || sum.Spans[0].Ended {
		t.Errorf("longest span = %+v, want the ongoing 3.3.3.3 run", sum.Spans[0])
	}
	if !closeTo(sum.Spans[0].Duration, 25) {
		t.Errorf("ongoing duration = %.2f days, want 25", sum.Spans[0].Duration)
	}
}

func TestBuildLifetimesHandlesRecovery(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	base := now.AddDate(0, 0, -30)

	// Blocked, then healthy again, then blocked again: two separate runs.
	events := []Event{
		ev(1, "1.1.1.1", "HEALTHY", 0, base),
		ev(1, "1.1.1.1", "BLOCKED_IR", 3, base),
		ev(1, "1.1.1.1", "HEALTHY", 10, base),
		ev(1, "1.1.1.1", "BLOCKED_IR", 18, base),
	}

	sum := BuildLifetimes(events, now)
	if sum.Completed != 2 {
		t.Fatalf("Completed = %d, want 2 separate healthy runs", sum.Completed)
	}
	if !closeTo(sum.MinDays, 3) || !closeTo(sum.MaxDays, 8) {
		t.Errorf("min/max = %.2f/%.2f, want 3/8", sum.MinDays, sum.MaxDays)
	}
}

func TestBuildLifetimesWithNoBlocks(t *testing.T) {
	now := time.Now()
	sum := BuildLifetimes([]Event{ev(1, "1.1.1.1", "HEALTHY", 0, now.AddDate(0, 0, -5))}, now)

	if sum.Completed != 0 || sum.Ongoing != 1 {
		t.Fatalf("Completed/Ongoing = %d/%d, want 0/1", sum.Completed, sum.Ongoing)
	}
	if sum.AvgDays != 0 {
		t.Errorf("AvgDays = %v, want 0 when nothing has been blocked yet", sum.AvgDays)
	}
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 0.01 }
