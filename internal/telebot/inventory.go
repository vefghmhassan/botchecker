package telebot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/vefgh/botchecker/internal/inventory"
)

// updateInventory is the bot's update button: read the panel now, and probe
// whatever it has that this service has never looked at.
//
// When the sync finds new endpoints and starts probing them, the message turns
// into the live progress screen — the same one a scan uses — so the reader
// watches the new servers get their verdicts instead of being told to check
// back later.
func (b *Bot) updateInventory(ctx context.Context, chat, message int64) error {
	l := b.lang()
	if b.d.Inventory == nil {
		return b.render(ctx, chat, message, b.tr("bot.noinventory"), backMenu(l))
	}

	res, scanID, err := b.d.Inventory.Update(context.WithoutCancel(ctx))
	if err != nil && !errors.Is(err, inventory.ErrProberBusy) {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(l))
	}

	text := inventoryText(b, res)
	if scanID != 0 {
		// Say what was found first; the follower then takes the message over.
		_ = b.render(ctx, chat, message, text, liveMenu(l))
		return b.followScan(ctx, chat, message, scanID)
	}
	if errors.Is(err, inventory.ErrProberBusy) {
		text += "\n\n" + b.tr("bot.sync.busy")
	}
	return b.render(ctx, chat, message, text, backMenu(l))
}

// inventoryText is what one sync found, for a chat.
func inventoryText(b *Bot, res inventory.Result) string {
	var s strings.Builder
	fmt.Fprintf(&s, "<b>%s</b>\n", b.tr("bot.sync.title"))
	s.WriteString(b.tr("bot.sync.read", res.Endpoints, res.Configs) + "\n")
	if !res.Complete {
		s.WriteString("⚠️ " + b.tr("bot.sync.sample") + "\n")
	}
	list := func(title string, keys []string) {
		if len(keys) == 0 {
			return
		}
		fmt.Fprintf(&s, "\n<b>%s</b> (%d)\n", title, len(keys))
		for i, k := range keys {
			if i == 15 {
				fmt.Fprintf(&s, "… +%d\n", len(keys)-15)
				break
			}
			fmt.Fprintf(&s, "<code>%s</code>\n", html.EscapeString(k))
		}
	}
	list("🆕 "+b.tr("bot.sync.new"), res.New)
	list("↩️ "+b.tr("bot.sync.back"), res.Returned)
	list("➖ "+b.tr("bot.sync.gone"), res.Gone)
	if !res.Changed() {
		s.WriteString("\n" + b.tr("bot.sync.nochange"))
	}
	return s.String()
}
