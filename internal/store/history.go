package store

import (
	"sort"
	"time"
)

// DayBucket is the verdict mix across all endpoints on one calendar day.
type DayBucket struct {
	Date   time.Time      `json:"date"`
	Counts map[string]int `json:"counts"`
	Total  int            `json:"total"`
}

// Count is a template-friendly lookup that tolerates a missing key.
func (d DayBucket) Count(verdict string) int { return d.Counts[verdict] }

// BuildTimeline reconstructs the daily verdict mix from the transition log.
//
// Because events record only changes, an endpoint's verdict on a given day is
// the last transition at or before the end of that day. Endpoints that had not
// been seen yet simply do not count towards that day.
func BuildTimeline(events []Event, days int, now time.Time, loc *time.Location) []DayBucket {
	if days < 1 {
		days = 1
	}
	if loc == nil {
		loc = time.UTC
	}

	byTarget := map[int64][]Event{}
	for _, e := range events {
		byTarget[e.TargetID] = append(byTarget[e.TargetID], e)
	}
	for id := range byTarget {
		evs := byTarget[id]
		sort.Slice(evs, func(i, j int) bool { return evs[i].ChangedAt.Before(evs[j].ChangedAt) })
	}

	local := now.In(loc)
	firstDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	out := make([]DayBucket, 0, days)
	for i := 0; i < days; i++ {
		day := firstDay.AddDate(0, 0, i)
		dayEnd := day.AddDate(0, 0, 1)

		bucket := DayBucket{Date: day, Counts: map[string]int{}}
		for _, evs := range byTarget {
			verdict := ""
			for _, e := range evs {
				if e.ChangedAt.Before(dayEnd) {
					verdict = e.ToVerdict
					continue
				}
				break
			}
			if verdict != "" {
				bucket.Counts[verdict]++
				bucket.Total++
			}
		}
		out = append(out, bucket)
	}
	return out
}

// LifetimeSpan is one uninterrupted healthy run for an endpoint: how long it
// worked from Iran before it was blocked.
type LifetimeSpan struct {
	TargetID int64      `json:"target_id"`
	Address  string     `json:"address"`
	Port     int        `json:"port"`
	Start    time.Time  `json:"start"`
	End      *time.Time `json:"end,omitempty"`
	Duration float64    `json:"duration_days"`
	Ended    bool       `json:"ended"`
}

// LifetimeSummary is the set of runs plus aggregates over the completed ones.
type LifetimeSummary struct {
	Spans     []LifetimeSpan `json:"spans"`
	Completed int            `json:"completed"`
	Ongoing   int            `json:"ongoing"`
	AvgDays   float64        `json:"avg_days"`
	MedDays   float64        `json:"median_days"`
	MinDays   float64        `json:"min_days"`
	MaxDays   float64        `json:"max_days"`
}

const (
	verdictHealthy   = "HEALTHY"
	verdictBlockedIR = "BLOCKED_IR"
)

// BuildLifetimes measures, per endpoint, how long each healthy run lasted
// before the address was blocked from Iran. A run that has not ended yet is
// reported as ongoing and measured up to now, but is excluded from the
// averages so an address that is still alive does not drag them down.
func BuildLifetimes(events []Event, now time.Time) LifetimeSummary {
	byTarget := map[int64][]Event{}
	for _, e := range events {
		byTarget[e.TargetID] = append(byTarget[e.TargetID], e)
	}

	var spans []LifetimeSpan
	for id, evs := range byTarget {
		sort.Slice(evs, func(i, j int) bool { return evs[i].ChangedAt.Before(evs[j].ChangedAt) })

		var open *LifetimeSpan
		for _, e := range evs {
			switch e.ToVerdict {
			case verdictHealthy:
				if open == nil {
					open = &LifetimeSpan{
						TargetID: id, Address: e.Address, Port: e.Port, Start: e.ChangedAt,
					}
				}
			case verdictBlockedIR:
				if open != nil {
					end := e.ChangedAt
					open.End = &end
					open.Ended = true
					open.Duration = end.Sub(open.Start).Hours() / 24
					spans = append(spans, *open)
					open = nil
				}
			}
		}
		if open != nil {
			open.Duration = now.Sub(open.Start).Hours() / 24
			spans = append(spans, *open)
		}
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Duration != spans[j].Duration {
			return spans[i].Duration > spans[j].Duration
		}
		return spans[i].Address < spans[j].Address
	})

	sum := LifetimeSummary{Spans: spans}
	var completed []float64
	for _, s := range spans {
		if s.Ended {
			completed = append(completed, s.Duration)
		} else {
			sum.Ongoing++
		}
	}
	sum.Completed = len(completed)
	if len(completed) == 0 {
		return sum
	}

	sort.Float64s(completed)
	total := 0.0
	for _, d := range completed {
		total += d
	}
	sum.AvgDays = total / float64(len(completed))
	sum.MinDays = completed[0]
	sum.MaxDays = completed[len(completed)-1]

	mid := len(completed) / 2
	if len(completed)%2 == 1 {
		sum.MedDays = completed[mid]
	} else {
		sum.MedDays = (completed[mid-1] + completed[mid]) / 2
	}
	return sum
}
