package telebot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/scanner"
)

// A scan takes minutes. Rather than telling the reader to come back, the
// message that started it is edited in place until the scan finishes.
const (
	// livePoll is how often progress is re-read. Edits are only sent when the
	// text actually changed, so this costs nothing while a slow probe runs.
	livePoll = 3 * time.Second
	// liveMaxAge stops a follower whose scan never finishes, so a wedged scan
	// cannot leak a goroutine for the life of the process.
	liveMaxAge = 45 * time.Minute
)

// liveKey identifies the one message a follower owns.
type liveKey struct{ chat, message int64 }

// stopLive cancels any follower writing to this message.
//
// Every screen goes through render, so navigating away from a running scan
// hands the message back to whatever the reader tapped instead of letting the
// follower overwrite it a second later.
func (b *Bot) stopLive(chat, message int64) {
	b.mu.Lock()
	cancel, ok := b.live[liveKey{chat, message}]
	if ok {
		delete(b.live, liveKey{chat, message})
	}
	b.mu.Unlock()
	if ok {
		cancel()
	}
}

// followScan edits the message in place until the scan ends.
//
// It writes with the Telegram client directly rather than through render:
// render stops the follower for the message it draws, which would make the
// first update cancel the very goroutine sending it.
func (b *Bot) followScan(ctx context.Context, chat, message, scanID int64) error {
	if message == 0 {
		// Nothing to edit — a command rather than a button press.
		return b.render(ctx, chat, message, b.tr("bot.scanstarted", scanID), backMenu(b.lang()))
	}

	// Replaces any follower already on this message, so a second tap does not
	// leave two goroutines writing to the same screen.
	b.stopLive(chat, message)

	// Detached from the request: the reader's tap is long answered by the time
	// the scan ends.
	base := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(base, liveMaxAge)
	key := liveKey{chat, message}
	b.mu.Lock()
	if b.live == nil {
		b.live = map[liveKey]context.CancelFunc{}
	}
	b.live[key] = cancel
	b.mu.Unlock()

	// The first frame is drawn before returning, so the reader sees the scan
	// take hold on the tap rather than up to livePoll later.
	last := b.liveText(scanID, b.d.Scanner.Progress())
	_ = b.d.Telegram.EditMessageText(runCtx, chat, message, last, liveMenu(b.lang()))

	go func() {
		defer func() {
			b.forgetLive(key)
			cancel()
		}()

		ticker := time.NewTicker(livePoll)
		defer ticker.Stop()

		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}

			p := b.d.Scanner.Progress()
			if !p.Running || p.ScanID != scanID {
				// Finished. Deregister first: showMenu goes through render,
				// which stops the follower owning this message — and that is
				// this goroutine, whose context the final draw still needs.
				b.forgetLive(key)

				drawCtx, drawCancel := context.WithTimeout(base, 15*time.Second)
				if err := b.showMenu(drawCtx, chat, message); err != nil {
					b.d.Log.Debug("could not draw the finished scan", "err", err)
				}
				drawCancel()
				return
			}

			text := b.liveText(scanID, p)
			if text == last {
				continue
			}
			if err := b.d.Telegram.EditMessageText(runCtx, chat, message, text, liveMenu(b.lang())); err != nil {
				b.d.Log.Debug("could not update the scan progress", "err", err)
				continue
			}
			last = text
		}
	}()
	return nil
}

// liveText renders one frame of the running scan.
//
// It is given the progress rather than reading it: the caller has already
// decided, from one snapshot, that the scan is still running, and a second read
// here could describe a different moment — or a scan that just finished.
func (b *Bot) liveText(scanID int64, p scanner.Progress) string {
	l := b.lang()

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n\n", i18n.T(l, "bot.live.title", l.Digits(fmt.Sprint(scanID))))
	fmt.Fprintf(&sb, "%s\n", progressBar(p))
	fmt.Fprintf(&sb, "%s\n", i18n.T(l, "bot.live.counted",
		l.Digits(fmt.Sprint(p.Done)), l.Digits(fmt.Sprint(p.Total))))
	if p.Phase != "" {
		fmt.Fprintf(&sb, "%s\n", i18n.T(l, "bot.live.phase", phaseLabel(l, p.Phase)))
	}
	if p.Current != "" {
		fmt.Fprintf(&sb, "%s\n", i18n.T(l, "bot.live.current", p.Current))
	}
	if !p.StartedAt.IsZero() {
		elapsed := time.Since(p.StartedAt).Truncate(time.Second)
		fmt.Fprintf(&sb, "%s", i18n.T(l, "bot.live.elapsed", l.Digits(elapsed.String())))
	}
	return sb.String()
}

// progressBar draws the ten-slot bar. A scan spends most of its time on the
// endpoint in flight, so the bar moves in visible steps rather than smoothly.
func progressBar(p scanner.Progress) string {
	const slots = 10
	filled := 0
	if p.Total > 0 {
		filled = p.Done * slots / p.Total
	}
	if filled > slots {
		filled = slots
	}
	pct := 0
	if p.Total > 0 {
		pct = p.Done * 100 / p.Total
	}
	return fmt.Sprintf("%s%s  %d%%",
		strings.Repeat("█", filled), strings.Repeat("░", slots-filled), pct)
}

// phaseLabel translates the scanner's phase, falling back to the raw value so
// a phase added later still shows something.
func phaseLabel(l i18n.Lang, phase string) string {
	if key := "bot.phase." + phase; i18n.Has(key) {
		return i18n.T(l, key)
	}
	return phase
}

// forgetLive drops the follower's registration without cancelling it, so the
// goroutine can still finish the work it is in the middle of.
func (b *Bot) forgetLive(key liveKey) {
	b.mu.Lock()
	delete(b.live, key)
	b.mu.Unlock()
}
