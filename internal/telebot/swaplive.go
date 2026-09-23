package telebot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/provision"
)

// The "replace now" button, end to end: a plan shown before anything is
// touched, then the message that asked for it follows the run step by step
// until it ends.

// planTimeout bounds the pre-run check. It pings the panel, check-host and
// every Hetzner project, and a hung one must not leave the screen spinning.
const planTimeout = 30 * time.Second

// manualOverride is what pressing the button waives; see provision.Override.
var manualOverride = provision.Override{Cooldown: true, FullBlock: true, Manual: true}

// askSwapConfirmation checks everything the run depends on and lists the
// changes it would make, and only offers the confirm button when nothing
// stands in the way.
func (b *Bot) askSwapConfirmation(ctx context.Context, chat, message int64, cb Callback, r row, userID, auditID int64) error {
	// The checks take a few seconds; say so rather than leave the old screen.
	if message != 0 {
		_ = b.d.Telegram.EditMessageText(ctx, chat, message, b.tr("bot.plan.checking", r.Address), nil)
	}

	machine, err := b.d.Store.AddressFor(r.Address)
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), serverMenu(b.lang(), r, b.capabilities()))
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	plan := b.d.Provision.Plan(pctx, machine, manualOverride)
	cancel()

	text := planText(b.lang(), plan)
	if plan.Blocked {
		return b.render(ctx, chat, message, text+"\n\n"+b.tr("bot.plan.blocked"),
			serverMenu(b.lang(), r, b.capabilities()))
	}

	token := b.pending.put(pending{
		Action: cb.Action, TargetID: r.TargetID, UserID: userID,
		Summary: text, AuditID: auditID,
	})
	if r.Online != nil && *r.Online > 0 {
		text += "\n\n" + b.tr("bot.connectednow", *r.Online)
	}
	text += "\n\n" + b.tr("bot.expires")
	return b.render(ctx, chat, message, text, confirmMenu(b.lang(), token))
}

// startSwap begins the run; the caller then hands the message to followSwap.
func (b *Bot) startSwap(ctx context.Context, r row) (string, error) {
	machine, err := b.d.Store.AddressFor(r.Address)
	if err != nil {
		return "", err
	}
	count, err := b.d.Store.OutageCountForAddress(r.Address, time.Now().Add(-b.d.Config.OutageWindow))
	if err != nil {
		return "", err
	}
	if err := b.d.Provision.TriggerAsync(ctx, machine, count, manualOverride); err != nil {
		return "", err
	}
	return b.tr("bot.done.swapnow", r.Address), nil
}

// followSwap edits the message in place until the replacement ends, then
// leaves the full record of what was done on it.
func (b *Bot) followSwap(ctx context.Context, chat, message int64, targetID int64) error {
	if message == 0 {
		return nil
	}
	b.stopLive(chat, message)

	base := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(base, liveMaxAge)
	key := liveKey{chat, message}
	b.mu.Lock()
	if b.live == nil {
		b.live = map[liveKey]context.CancelFunc{}
	}
	b.live[key] = cancel
	b.mu.Unlock()

	last := progressText(b.lang(), b.d.Provision.Progress(), time.Now())
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

			p := b.d.Provision.Progress()
			text := progressText(b.lang(), p, time.Now())
			if !p.Running {
				// The last frame is the record of the run, so it stays, with
				// the server's buttons under it rather than a live keyboard.
				b.forgetLive(key)
				drawCtx, drawCancel := context.WithTimeout(base, 15*time.Second)
				kb := backMenu(b.lang())
				if r, err := b.rowByID(drawCtx, targetID); err == nil {
					kb = serverMenu(b.lang(), r, b.capabilities())
				}
				if err := b.d.Telegram.EditMessageText(drawCtx, chat, message, text, kb); err != nil {
					b.d.Log.Debug("could not draw the finished replacement", "err", err)
				}
				drawCancel()
				return
			}
			if text == last {
				continue
			}
			if err := b.d.Telegram.EditMessageText(runCtx, chat, message, text, liveMenu(b.lang())); err != nil {
				b.d.Log.Debug("could not update the replacement progress", "err", err)
				continue
			}
			last = text
		}
	}()
	return nil
}

// planText renders the checks and the changes.
func planText(l i18n.Lang, p provision.Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b>\n\n", i18n.T(l, "bot.plan.title", html.EscapeString(p.Address)))

	fmt.Fprintf(&sb, "<b>%s</b>\n", i18n.T(l, "bot.plan.checks"))
	for _, c := range p.Checks {
		icon := "✅"
		switch c.Level {
		case provision.PlanWarn:
			icon = "⚠️"
		case provision.PlanBlock:
			icon = "🚫"
		}
		fmt.Fprintf(&sb, "%s %s\n", icon, line(l, c.Key, c.Args))
	}

	fmt.Fprintf(&sb, "\n<b>%s</b>\n", i18n.T(l, "bot.plan.changes"))
	for i, s := range p.Steps {
		fmt.Fprintf(&sb, "%s. %s\n", l.Digits(fmt.Sprint(i+1)), line(l, s.Key, s.Args))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// progressText renders one frame of the run, or its final record.
func progressText(l i18n.Lang, p provision.Progress, now time.Time) string {
	var sb strings.Builder

	switch {
	case p.Running:
		fmt.Fprintf(&sb, "<b>%s</b>\n", i18n.T(l, "bot.prog.running", html.EscapeString(p.Address)))
	case p.Err != "":
		fmt.Fprintf(&sb, "<b>%s</b>\n", i18n.T(l, "bot.prog.failed", html.EscapeString(p.Address)))
	case p.NewAddress != "":
		fmt.Fprintf(&sb, "<b>%s</b>\n", i18n.T(l, "bot.prog.done",
			html.EscapeString(p.Address), html.EscapeString(p.NewAddress)))
	default:
		fmt.Fprintf(&sb, "<b>%s</b>\n", i18n.T(l, "bot.prog.stopped", html.EscapeString(p.Address)))
	}

	end := now
	if !p.Running && !p.Finished.IsZero() {
		end = p.Finished
	}
	if !p.Started.IsZero() {
		fmt.Fprintf(&sb, "%s\n", i18n.T(l, "bot.live.elapsed",
			l.Digits(end.Sub(p.Started).Truncate(time.Second).String())))
	}
	sb.WriteString("\n")

	for _, s := range p.Steps {
		offset := s.At.Sub(p.Started).Truncate(time.Second)
		fmt.Fprintf(&sb, "<code>%s</code> %s\n", l.Digits(clock(offset)), line(l, s.Key, s.Args))
	}
	if p.Running {
		sb.WriteString("⏳\n")
	}
	if p.Err != "" {
		fmt.Fprintf(&sb, "\n🛑 %s\n", html.EscapeString(p.Err))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// line translates one plan or progress line. String arguments come from
// Hetzner, the panel and error messages, so they are escaped before they reach
// Telegram's HTML parser.
func line(l i18n.Lang, key string, args []any) string {
	safe := make([]any, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			safe[i] = html.EscapeString(s)
		} else {
			safe[i] = a
		}
	}
	return i18n.T(l, key, safe...)
}

// clock renders an offset from the start of the run as m:ss.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
