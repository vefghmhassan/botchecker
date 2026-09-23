package telebot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/inventory"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/provision"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

// pollTimeout is the long-poll wait. Telegram holds the request open until an
// update arrives or this elapses.
const pollTimeout = 30 * time.Second

// Watcher is the part of the watch loop the bot reports on.
type Watcher interface {
	LastRun() time.Time
}

// Deps is everything the bot reads from and acts through.
type Deps struct {
	Config    *config.Config
	Settings  *settings.Provider
	Store     *store.Store
	Telegram  *notify.Telegram
	Scanner   *scanner.Scanner
	Provision *provision.Manager
	Panel     *xui.Client
	Hetzner   *hetzner.Registry
	Zex       *zex.Client
	Watcher   Watcher
	Inventory *inventory.Syncer
	Log       *slog.Logger
}

type Bot struct {
	d       Deps
	pending *confirmations

	mu       sync.Mutex
	offset   int64
	lastSeen time.Time
	running  bool
	// live holds the in-place updater for each message that is following a
	// running scan, so navigating away can stop it.
	live map[liveKey]context.CancelFunc
}

func New(d Deps) *Bot {
	return &Bot{d: d, pending: newConfirmations()}
}

// Enabled reports whether the command loop should run.
func (b *Bot) Enabled() bool {
	return b.d.Settings.Bool(settings.TelegramBotEnable) && b.d.Telegram.HasToken()
}

// Running reports whether the loop is polling.
func (b *Bot) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// LastUpdate is when an update was last received, for the dashboard.
func (b *Bot) LastUpdate() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSeen
}

// AllowedIDs are the Telegram user ids permitted to command the bot. An empty
// result means nobody may, and the loop refuses to start.
func (b *Bot) AllowedIDs() []int64 {
	raw := b.d.Settings.Get(settings.TelegramAllowedID)
	if strings.TrimSpace(raw) == "" {
		// Falling back to the alert chat means the owner does not have to
		// configure the same id twice.
		raw = b.d.Settings.Get(settings.TelegramChatID)
	}

	var out []int64
	for _, part := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id != 0 {
			out = append(out, id)
		}
	}
	return out
}

func (b *Bot) allowed(userID int64) bool {
	for _, id := range b.AllowedIDs() {
		if id == userID {
			return true
		}
	}
	return false
}

// Start runs the command loop until ctx is done. It is a no-op when the bot is
// not enabled, and refuses to start without an allow list.
func (b *Bot) Start(ctx context.Context) {
	if !b.Enabled() {
		b.d.Log.Info("telegram command loop disabled", "reason", "telegram.bot_enabled is off or no token is set")
		return
	}
	if len(b.AllowedIDs()) == 0 {
		b.d.Log.Warn("telegram command loop not started",
			"reason", "no allowed ids configured",
			"impact", "a bot that accepts commands from anyone could delete servers")
		return
	}

	b.d.Log.Info("telegram command loop starting", "allowed_ids", len(b.AllowedIDs()))
	go b.loop(ctx)
}

func (b *Bot) loop(ctx context.Context) {
	b.setRunning(true)
	defer b.setRunning(false)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := b.d.Telegram.GetUpdates(ctx, b.nextOffset(), pollTimeout)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			if ctx.Err() != nil {
				return
			}
			continue
		case errors.Is(err, notify.ErrConflict):
			// Two processes cannot poll the same bot. Say so plainly rather
			// than retrying in a tight loop.
			b.d.Log.Error("another process is already polling this bot",
				"fix", "run only one botchecker instance with telegram.bot_enabled on")
			if sleep(ctx, 30*time.Second) != nil {
				return
			}
			continue
		case err != nil:
			b.d.Log.Warn("could not fetch telegram updates", "err", err, "retry_in", backoff)
			if sleep(ctx, backoff) != nil {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			b.advanceOffset(u.UpdateID)
			b.handle(ctx, u)
		}
	}
}

func (b *Bot) handle(ctx context.Context, u notify.Update) {
	sender := u.Sender()
	if sender == nil {
		return
	}

	b.mu.Lock()
	b.lastSeen = time.Now()
	b.mu.Unlock()

	action, detail := describe(u)

	if !b.allowed(sender.ID) {
		// Deliberately silent: replying would confirm the bot exists to
		// someone who should not be talking to it. The attempt is recorded so
		// it is visible in the dashboard.
		b.d.Log.Warn("ignoring telegram update from an unauthorized id",
			"telegram_id", sender.ID, "user", sender.Label(), "action", action)
		if _, err := b.d.Store.RecordBotAction(sender.ID, sender.Label(), false,
			action, 0, detail, time.Now()); err != nil {
			b.d.Log.Warn("could not record the unauthorized attempt", "err", err)
		}
		return
	}

	auditID, err := b.d.Store.RecordBotAction(sender.ID, sender.Label(), true, action, 0, detail, time.Now())
	if err != nil {
		b.d.Log.Warn("could not record the bot action", "err", err)
	}

	var handleErr error
	switch {
	case u.CallbackQuery != nil:
		handleErr = b.onCallback(ctx, u.CallbackQuery, auditID)
	case u.Message != nil:
		handleErr = b.onCommand(ctx, u.Message)
	}

	if auditID != 0 {
		if ferr := b.d.Store.FinishBotAction(auditID, handleErr == nil, handleErr, time.Now()); ferr != nil {
			b.d.Log.Warn("could not finish the bot action record", "err", ferr)
		}
	}
	if handleErr != nil {
		b.d.Log.Warn("telegram update failed", "action", action, "err", handleErr)
	}
}

// describe names an update for the audit trail without storing its whole body.
func describe(u notify.Update) (action, detail string) {
	switch {
	case u.CallbackQuery != nil:
		cb, err := Decode(u.CallbackQuery.Data)
		if err != nil {
			return "callback", u.CallbackQuery.Data
		}
		return "callback:" + cb.Action, cb.Arg
	case u.Message != nil:
		text := strings.TrimSpace(u.Message.Text)
		if strings.HasPrefix(text, "/") {
			return "command", strings.Fields(text)[0]
		}
		return "message", ""
	default:
		return "unknown", ""
	}
}

func (b *Bot) onCommand(ctx context.Context, msg *notify.Message) error {
	chat := msg.Chat.ID
	switch strings.Fields(strings.TrimSpace(msg.Text) + " ")[0] {
	case "/start", "/menu":
		return b.showMenu(ctx, chat, 0)
	case "/status":
		return b.showStatus(ctx, chat, 0)
	case "/blocked":
		return b.showList(ctx, chat, 0, 1, true)
	case "/servers":
		return b.showList(ctx, chat, 0, 1, false)
	case "/scan":
		return b.startScan(ctx, chat, 0)
	case "/help":
		_, err := b.d.Telegram.SendMessage(ctx, chat, b.helpText(), mainMenu(b.lang()))
		return err
	default:
		return b.showMenu(ctx, chat, 0)
	}
}

func (b *Bot) helpText() string {
	return "<b>" + b.tr("bot.title") + "</b>\n\n" + b.tr("bot.help")
}

func (b *Bot) onCallback(ctx context.Context, q *notify.CallbackQuery, auditID int64) error {
	cb, err := Decode(q.Data)
	if err != nil {
		return b.d.Telegram.AnswerCallback(ctx, q.ID, b.tr("bot.oldbutton"), false)
	}

	var chat, message int64
	if q.Message != nil {
		message = q.Message.MessageID
		if q.Message.Chat != nil {
			chat = q.Message.Chat.ID
		}
	}

	// Acknowledge immediately so the button stops spinning while we work.
	if err := b.d.Telegram.AnswerCallback(ctx, q.ID, "", false); err != nil {
		b.d.Log.Debug("could not answer the callback", "err", err)
	}

	switch cb.Action {
	case ActNoop:
		return nil
	case ActMenu:
		return b.showMenu(ctx, chat, message)
	case ActServerList:
		return b.showList(ctx, chat, message, cb.Page(), false)
	case ActBlocked:
		return b.showList(ctx, chat, message, cb.Page(), true)
	case ActServer:
		return b.showServer(ctx, chat, message, cb.ID())
	case ActProviders:
		return b.showProviders(ctx, chat, message)
	case ActHistory:
		return b.showHistory(ctx, chat, message)
	case ActLedger:
		return b.showLedger(ctx, chat, message)
	case ActLedgerItem:
		return b.showLedgerItem(ctx, chat, message, cb.Arg)
	case ActRelease:
		return b.releaseAddress(ctx, chat, message, cb.Arg)
	case ActStatus:
		return b.showStatus(ctx, chat, message)
	case ActScan:
		return b.startScan(ctx, chat, message)
	case ActSync:
		return b.updateInventory(ctx, chat, message)
	case ActNotifyOnly:
		return b.toggleNotifyOnly(ctx, chat, message, cb.ID())
	case ActConfirm:
		return b.runConfirmed(ctx, chat, message, cb.Arg, q.From.ID, auditID)
	case ActCancel:
		return b.cancelConfirmed(ctx, chat, message, cb.Arg, q.From.ID)
	default:
		return b.askConfirmation(ctx, chat, message, cb, q.From.ID, auditID)
	}
}

// ---- offset bookkeeping ----

func (b *Bot) nextOffset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.offset
}

func (b *Bot) advanceOffset(updateID int64) {
	b.mu.Lock()
	if updateID >= b.offset {
		b.offset = updateID + 1
	}
	b.mu.Unlock()
}

func (b *Bot) setRunning(v bool) {
	b.mu.Lock()
	b.running = v
	b.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func fmtErr(err error) string { return fmt.Sprintf("⚠️ %s", err.Error()) }
