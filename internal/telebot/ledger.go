package telebot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/store"
)

// The ledger screens. An address ends up here when this service destroyed it —
// retired during a replacement, discarded for coming back filtered, or deleted
// from this bot — and it exists so that address is never bought back.
//
// Releasing is a real decision, so it is two taps with a screen in between that
// says what was recorded and why, rather than a button on a list.

// showLedger lists the addresses that are barred, newest first.
func (b *Bot) showLedger(ctx context.Context, chat, message int64) error {
	entries, err := b.d.Store.BurnedAddresses()
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}

	l := b.lang()
	var active []store.LedgerEntry
	for _, e := range entries {
		if e.Active() {
			active = append(active, e)
		}
	}
	if len(active) == 0 {
		return b.render(ctx, chat, message, b.tr("bot.ledger.empty"), backMenu(l))
	}

	var text strings.Builder
	fmt.Fprintf(&text, "<b>%s</b>\n\n%s\n\n", b.tr("bot.ledger.title"), b.tr("bot.ledger.blurb"))

	kb := &notify.Keyboard{}
	for _, e := range active {
		fmt.Fprintf(&text, "<code>%s</code> — %s, %s\n",
			html.EscapeString(e.Address), html.EscapeString(b.ledgerReason(e.Reason)),
			html.EscapeString(e.RecordedAt.In(b.d.Config.Location).Format("2006-01-02 15:04")))
		kb.Rows = append(kb.Rows, []notify.Button{
			btn("⚰️ "+e.Address, ActLedgerItem, e.Address),
		})
	}
	kb.Rows = append(kb.Rows, []notify.Button{btn(b.tr("bot.menu.home"), ActMenu, nil)})
	return b.render(ctx, chat, message, text.String(), kb)
}

// showLedgerItem is the screen that has to be read before an address can be let
// back in. It says what was recorded, when, and what releasing actually means.
func (b *Bot) showLedgerItem(ctx context.Context, chat, message int64, address string) error {
	e, err := b.d.Store.LedgerEntry(address)
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}

	l := b.lang()
	var text strings.Builder
	fmt.Fprintf(&text, "<b>%s</b>\n<code>%s</code>\n\n", b.tr("bot.ledger.title"), html.EscapeString(e.Address))
	fmt.Fprintf(&text, "%s: %s\n", b.tr("bot.ledger.reason"), html.EscapeString(b.ledgerReason(e.Reason)))
	fmt.Fprintf(&text, "%s: %s\n", b.tr("bot.ledger.when"),
		html.EscapeString(e.RecordedAt.In(b.d.Config.Location).Format("2006-01-02 15:04")))
	if e.Verdict != "" {
		fmt.Fprintf(&text, "%s: %s\n", b.tr("bot.ledger.verdict"), html.EscapeString(e.Verdict))
	}
	if e.Project != "" {
		fmt.Fprintf(&text, "%s: <code>%s</code>\n", b.tr("bot.ledger.project"), html.EscapeString(e.Project))
	}
	if e.Note != "" {
		fmt.Fprintf(&text, "\n<i>%s</i>\n", html.EscapeString(e.Note))
	}

	kb := &notify.Keyboard{}
	if e.Active() {
		text.WriteString("\n" + b.tr("bot.ledger.releasewarn"))
		kb.Rows = append(kb.Rows, []notify.Button{
			btn(i18n.T(l, "bot.ledger.release"), ActRelease, e.Address),
		})
	} else if e.ReleasedAt != nil {
		fmt.Fprintf(&text, "\n%s\n", b.tr("bot.ledger.released",
			e.ReleasedAt.In(b.d.Config.Location).Format("2006-01-02 15:04")))
	}
	kb.Rows = append(kb.Rows, []notify.Button{
		btn(i18n.T(l, "bot.ledger.back"), ActLedger, nil),
		btn(i18n.T(l, "bot.menu.home"), ActMenu, nil),
	})
	return b.render(ctx, chat, message, text.String(), kb)
}

// releaseAddress lets an address be used again.
func (b *Bot) releaseAddress(ctx context.Context, chat, message int64, address string) error {
	if err := b.d.Store.ReleaseAddress(address, time.Now()); err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}
	b.d.Log.Info("address released from the ledger", "address", address)
	return b.showLedgerItem(ctx, chat, message, address)
}

// ledgerReason renders a reason in the reader's language, falling back to the
// stored token so an unknown one is still readable rather than blank.
func (b *Bot) ledgerReason(reason string) string {
	if s := b.tr("ledger.reason." + reason); s != "" && !strings.HasPrefix(s, "ledger.reason.") {
		return s
	}
	return reason
}
