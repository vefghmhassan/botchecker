// Package inventory keeps the list of endpoints in step with the panel.
//
// Finding out which servers exist and finding out whether Iran can reach them
// are different jobs with very different costs. The first is one HTTP call. The
// second is dozens of check-host probes. They used to be done together, once
// per full scan, from the app's splash endpoint — which returns a random handful
// of configs rather than the fleet — so a server added to the panel could go
// unseen indefinitely, and one deleted from it was monitored forever.
//
// Here they are separate. A sync reads the complete list, adds what is new,
// marks what has gone, and says what changed. Probing stays where it was.
package inventory

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
)

// Source is where the list came from.
const (
	// SourceNodes is the panel's complete list of active configs.
	SourceNodes = "nodes"
	// SourceSample is the app's splash endpoint: a random handful. Used only
	// when the complete list cannot be read, and never trusted to say that
	// anything has gone.
	SourceSample = "sample"
)

// Result is what one sync found.
type Result struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	// Complete is true only when the whole list was read. A sync that fell
	// back to the sample adds what it saw but removes nothing.
	Complete  bool `json:"complete"`
	Configs   int  `json:"configs"`
	Endpoints int  `json:"endpoints"`

	// New are endpoints never seen before. Returned were marked gone and are
	// listed again. Gone are listed no longer.
	New      []string `json:"new,omitempty"`
	Returned []string `json:"returned,omitempty"`
	Gone     []string `json:"gone,omitempty"`

	// FallbackReason says why the complete list could not be used.
	FallbackReason string `json:"fallback_reason,omitempty"`
	Error          string `json:"error,omitempty"`

	// Targets is the endpoint list the sync produced, for a caller that wants
	// to probe it. Not serialised: it is the working set, not a report.
	Targets []splash.Target `json:"-"`
}

// Changed reports whether anything worth telling a person about happened.
func (r Result) Changed() bool {
	return len(r.New) > 0 || len(r.Returned) > 0 || len(r.Gone) > 0
}

// Fresh is what should be probed straight away: endpoints nobody has looked at
// yet, and ones that have just come back.
func (r Result) Fresh() []splash.Target {
	want := map[string]bool{}
	for _, k := range r.New {
		want[k] = true
	}
	for _, k := range r.Returned {
		want[k] = true
	}
	var out []splash.Target
	for _, t := range r.Targets {
		if want[t.Key()] {
			out = append(out, t)
		}
	}
	return out
}

// Lister is the part of the splash client a sync needs.
type Lister interface {
	Nodes(ctx context.Context) ([]splash.ServerConfig, error)
	Fetch(ctx context.Context) (*splash.Response, error)
}

// Syncer reads the panel and updates the endpoint list.
type Syncer struct {
	src      Lister
	st       *store.Store
	log      *slog.Logger
	extra    func() []splash.Target
	prober   Prober
	notifier Notifier

	mu   sync.Mutex // serialises syncs, so two can never race on the diff
	last Result
	lmu  sync.RWMutex
}

func New(src Lister, st *store.Store, log *slog.Logger) *Syncer {
	return &Syncer{src: src, st: st, log: log}
}

// SetExtraTargets supplies endpoints watched by hand. They are merged in on
// every sync and never marked gone, since the panel not listing them is the
// reason they are watched by hand at all.
func (s *Syncer) SetExtraTargets(fn func() []splash.Target) { s.extra = fn }

// Last is the most recent sync, for the dashboard.
func (s *Syncer) Last() Result {
	s.lmu.RLock()
	defer s.lmu.RUnlock()
	return s.last
}

// Sync reads the panel once and brings the endpoint list in line with it.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := Result{At: time.Now()}
	defer func() {
		s.lmu.Lock()
		s.last = res
		s.lmu.Unlock()
	}()

	configs, err := s.src.Nodes(ctx)
	if err == nil {
		res.Source, res.Complete = SourceNodes, true
	} else {
		// A sample is worse than the list but far better than nothing: it
		// still adds whatever it happens to show. What it can never do is say
		// that something has gone, because it leaves out almost everything by
		// design.
		res.FallbackReason = err.Error()
		s.log.Warn("could not read the complete node list, falling back to the splash sample",
			"err", err, "impact", "new servers may be missed and deleted ones are not removed")

		resp, ferr := s.src.Fetch(ctx)
		if ferr != nil {
			res.Error = ferr.Error()
			return res, ferr
		}
		configs = resp.Configs()
		res.Source = SourceSample
	}

	targets := splash.TargetsFrom(configs)
	manual := map[string]bool{}
	if s.extra != nil {
		extra := s.extra()
		for _, t := range extra {
			manual[t.Key()] = true
		}
		targets = splash.MergeTargets(targets, extra)
	}
	res.Configs, res.Endpoints, res.Targets = len(configs), len(targets), targets

	known, err := s.st.KnownEndpoints()
	if err != nil {
		res.Error = err.Error()
		return res, err
	}

	now := time.Now()
	present := make(map[string]bool, len(targets))
	var returned []string
	for _, t := range targets {
		key := t.Key()
		present[key] = true
		missing, seen := known[key]
		switch {
		case !seen:
			res.New = append(res.New, key)
		case missing:
			returned = append(returned, key)
		}
		if _, err := s.st.UpsertTarget(t, now); err != nil {
			s.log.Warn("could not record an endpoint", "endpoint", key, "err", err)
		}
	}
	if err := s.st.MarkPresent(returned); err != nil {
		s.log.Warn("could not clear the missing marker", "err", err)
	}
	res.Returned = returned

	// A complete list with nothing in it is almost certainly a broken read — a
	// panel restarting, a database not yet loaded — rather than a fleet that
	// was deleted in one go. Acting on it would mark every server gone at
	// once, and the dashboard would go blank. So it is reported and nothing
	// is removed; a real mass deletion shows up on the next sync that sees
	// the panel with configs in it again.
	if res.Complete && len(present) == len(manual) && len(known) > 0 {
		res.Complete = false
		res.FallbackReason = "the panel listed no endpoints at all; nothing was marked gone"
		s.log.Warn("the panel returned an empty node list, not removing anything",
			"known", len(known))
	}

	if res.Complete {
		gone, err := s.st.MarkMissing(present, manual, now)
		if err != nil {
			s.log.Warn("could not mark endpoints gone from the panel", "err", err)
		}
		res.Gone = gone
	}

	sort.Strings(res.New)
	sort.Strings(res.Returned)
	sort.Strings(res.Gone)

	s.log.Info("inventory synced",
		"source", res.Source, "configs", res.Configs, "endpoints", res.Endpoints,
		"new", len(res.New), "returned", len(res.Returned), "gone", len(res.Gone))
	return res, nil
}
