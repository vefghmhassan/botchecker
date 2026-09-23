package inventory

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/splash"
)

// TriggerSync marks the scans that probe endpoints a sync has just found.
const TriggerSync = "sync"

// Prober starts probing a given list of endpoints.
type Prober interface {
	StartTargets(ctx context.Context, trigger string, targets []splash.Target) (int64, error)
}

// Notifier delivers a message to the operator.
type Notifier interface {
	Send(ctx context.Context, html string) error
}

// ErrProberBusy means the sync worked but the new endpoints could not be probed
// straight away because a scan is already running. They are not lost: every
// endpoint in the store is re-probed by the next watch round.
var ErrProberBusy = errors.New("a scan is already running; the new endpoints will be probed by the next watch round")

// SetProber lets Update probe what a sync finds.
func (s *Syncer) SetProber(p Prober) { s.prober = p }

// SetNotifier lets Update say what changed.
func (s *Syncer) SetNotifier(n Notifier) { s.notifier = n }

// Update brings the list in line with the panel and looks at whatever is new.
//
// This is what the update button does, and what runs on a timer. The sync on
// its own would make a new server appear on the dashboard with no verdict; the
// probe straight after is what turns "I added ten servers" into ten readings
// from Iran within a minute, rather than at the next full scan.
func (s *Syncer) Update(ctx context.Context) (Result, int64, error) {
	res, err := s.Sync(ctx)
	if err != nil {
		return res, 0, err
	}

	if res.Changed() {
		s.announce(ctx, res)
	}

	fresh := res.Fresh()
	if len(fresh) == 0 || s.prober == nil {
		return res, 0, nil
	}
	scanID, err := s.prober.StartTargets(ctx, TriggerSync, fresh)
	if err != nil {
		s.log.Info("the new endpoints will be probed by the next watch round",
			"count", len(fresh), "reason", err)
		return res, 0, ErrProberBusy
	}
	s.log.Info("probing endpoints the panel just added", "count", len(fresh), "scan_id", scanID)
	return res, scanID, nil
}

// Run syncs on a timer until ctx ends. interval is read on every tick, so
// changing the setting takes effect without a restart.
func (s *Syncer) Run(ctx context.Context, interval func() time.Duration) {
	for {
		if _, _, err := s.Update(ctx); err != nil && !errors.Is(err, ErrProberBusy) {
			s.log.Warn("inventory sync failed", "err", err)
		}

		d := interval()
		if d <= 0 {
			d = 5 * time.Minute
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// announce tells the operator what the panel changed.
//
// Endpoints, not configs: a panel holds thousands of configs on a handful of
// machines, and it is the machines a person needs to know about.
func (s *Syncer) announce(ctx context.Context, res Result) {
	if s.notifier == nil {
		return
	}
	var b strings.Builder
	b.WriteString("🔄 <b>The panel changed</b>\n")
	fmt.Fprintf(&b, "%d endpoint(s) from %d config(s)\n", res.Endpoints, res.Configs)
	section(&b, "🆕 New — probing now", res.New)
	section(&b, "↩️ Back in the panel", res.Returned)
	section(&b, "➖ No longer in the panel", res.Gone)
	if len(res.Gone) > 0 {
		b.WriteString("\n<i>Endpoints that left the panel stop being watched after 15 minutes.</i>")
	}

	if err := s.notifier.Send(ctx, b.String()); err != nil {
		s.log.Warn("could not announce the panel change", "err", err)
	}
}

// section renders one list, capped so a first sync on an empty database cannot
// produce a message too long to read.
func section(b *strings.Builder, title string, keys []string) {
	if len(keys) == 0 {
		return
	}
	const show = 15
	fmt.Fprintf(b, "\n<b>%s</b> (%d)\n", title, len(keys))
	for i, k := range keys {
		if i == show {
			fmt.Fprintf(b, "… and %d more\n", len(keys)-show)
			break
		}
		fmt.Fprintf(b, "<code>%s</code>\n", html.EscapeString(k))
	}
}
