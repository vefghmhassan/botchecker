package telebot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/xui"
)

// render sends a new message, or edits the one the button came from so a menu
// does not fill the chat with copies of itself.
func (b *Bot) render(ctx context.Context, chat, message int64, text string, kb *notify.Keyboard) error {
	// Whatever the reader just tapped owns this message now; a scan follower
	// still writing to it would overwrite their screen seconds later.
	b.stopLive(chat, message)
	if message != 0 {
		err := b.d.Telegram.EditMessageText(ctx, chat, message, text, kb)
		// An edit that changes nothing is Telegram's way of saying the screen
		// is already correct — tapping the same button twice must not post a
		// duplicate.
		if err == nil || errors.Is(err, notify.ErrNotModified) {
			return nil
		}
		b.d.Log.Debug("could not edit the message, sending a new one", "err", err)
	}
	_, err := b.d.Telegram.SendMessage(ctx, chat, text, kb)
	return err
}

// lang resolves the interface language, shared with the dashboard.
func (b *Bot) lang() i18n.Lang {
	return i18n.Parse(b.d.Settings.Get(settings.UILanguage))
}

// tr is shorthand for a translated string in the current language.
func (b *Bot) tr(key string, args ...any) string { return i18n.T(b.lang(), key, args...) }

// capabilities reports which integrations can currently be used.
func (b *Bot) capabilities() Capabilities {
	return Capabilities{
		Panel:   b.d.Panel != nil && b.d.Panel.Configured(),
		Hetzner: b.d.Hetzner != nil && b.d.Hetzner.Configured(),
		Zex:     b.d.Zex != nil && b.d.Zex.Configured(),
	}
}

// rows loads every endpoint with its ownership, and the panel's node data when
// the panel is configured. The node list is fetched once per render rather
// than per endpoint.
func (b *Bot) rows(ctx context.Context) ([]row, error) {
	statuses, err := b.d.Store.TargetStatuses()
	if err != nil {
		return nil, err
	}

	nodesByAddress := map[string]xui.NodeView{}
	if b.capabilities().Panel {
		if nodes, err := b.d.Panel.Nodes(ctx); err == nil {
			for _, n := range nodes {
				nodesByAddress[n.Address] = n
			}
		} else {
			b.d.Log.Warn("could not read the panel node list", "err", err)
		}
	}

	out := make([]row, 0, len(statuses))
	for _, s := range statuses {
		provider, _, serverName, serverID, abuse, err := b.d.Store.TargetProvider(s.TargetID)
		if err != nil {
			return nil, err
		}
		if provider == "" {
			provider = string(hetzner.OwnedUnknown)
		}
		project, err := b.d.Store.AddressProject(s.Address)
		if err != nil {
			return nil, err
		}

		r := row{
			TargetStatus: s, Provider: provider, ServerName: serverName,
			HZServerID: serverID, AbuseBlocked: abuse, Project: project,
		}
		if n, ok := nodesByAddress[s.Address]; ok {
			r.NodeID, r.NodeEnabled = n.ID, n.Enable
			online := n.OnlineCount
			r.Online = &online
		}
		out = append(out, r)
	}
	return out, nil
}

func (b *Bot) rowByID(ctx context.Context, targetID int64) (row, error) {
	rows, err := b.rows(ctx)
	if err != nil {
		return row{}, err
	}
	for _, r := range rows {
		if r.TargetID == targetID {
			return r, nil
		}
	}
	return row{}, errors.New(i18n.T(i18n.EN, "bot.gone"))
}

// ---- screens ----

func (b *Bot) showMenu(ctx context.Context, chat, message int64) error {
	rows, err := b.rows(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Verdict]++
	}
	lastScan, err := b.d.Store.LatestCompletedScan()
	if err != nil {
		return err
	}
	return b.render(ctx, chat, message,
		menuText(b.lang(), counts, len(rows), lastScan, b.d.Config.Location), mainMenu(b.lang()))
}

func (b *Bot) showList(ctx context.Context, chat, message int64, page int, onlyBlocked bool) error {
	all, err := b.rows(ctx)
	if err != nil {
		return err
	}

	rows, title, action := all, "bot.list.servers", ActServerList
	if onlyBlocked {
		rows, title, action = nil, "bot.list.blocked", ActBlocked
		for _, r := range all {
			if r.Verdict == string(scanner.VerdictBlockedIR) {
				rows = append(rows, r)
			}
		}
	}

	pageRows, page, pages := paginate(rows, page)
	return b.render(ctx, chat, message,
		listText(b.lang(), pageRows, page, pages, title),
		listMenu(b.lang(), pageRows, page, pages, action))
}

func (b *Bot) showServer(ctx context.Context, chat, message, targetID int64) error {
	r, err := b.rowByID(ctx, targetID)
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}
	tr, err := b.d.Store.LatestTraceroute(targetID)
	if err != nil {
		b.d.Log.Warn("could not read the traceroute", "err", err)
	}
	return b.render(ctx, chat, message,
		serverText(b.lang(), r, tr, b.d.Config.Location), serverMenu(b.lang(), r, b.capabilities()))
}

func (b *Bot) showProviders(ctx context.Context, chat, message int64) error {
	rows, err := b.rows(ctx)
	if err != nil {
		return err
	}
	return b.render(ctx, chat, message, providersText(b.lang(), rows), backMenu(b.lang()))
}

func (b *Bot) showHistory(ctx context.Context, chat, message int64) error {
	provs, err := b.d.Store.Provisions(10)
	if err != nil {
		return err
	}
	return b.render(ctx, chat, message, historyText(b.lang(), provs, b.d.Config.Location), backMenu(b.lang()))
}

func (b *Bot) showStatus(ctx context.Context, chat, message int64) error {
	rows, err := b.rows(ctx)
	if err != nil {
		return err
	}
	watching := 0
	for _, r := range rows {
		if r.Verdict == string(scanner.VerdictBlockedIR) {
			watching++
		}
	}

	s := ServiceStatus{
		Scanning:    b.d.Scanner != nil && b.d.Scanner.Running(),
		Watching:    watching,
		PanelOn:     b.capabilities().Panel,
		HetznerOn:   b.capabilities().Hetzner,
		ProvisionOn: b.d.Settings.Bool(settings.ProvisionEnabled),
		DryRun:      b.d.Settings.Bool(settings.ProvisionDryRun),
	}
	if b.d.Watcher != nil {
		s.LastWatch = b.d.Watcher.LastRun()
	}
	return b.render(ctx, chat, message, statusText(b.lang(), s, b.d.Config.Location), backMenu(b.lang()))
}

func (b *Bot) startScan(ctx context.Context, chat, message int64) error {
	if b.d.Scanner == nil {
		return b.render(ctx, chat, message, b.tr("bot.noscanner"), backMenu(b.lang()))
	}
	id, err := b.d.Scanner.Start(context.WithoutCancel(ctx), scanner.TriggerManual)
	if errors.Is(err, scanner.ErrBusy) {
		p := b.d.Scanner.Progress()
		return b.render(ctx, chat, message,
			b.tr("bot.scanrunning", p.Phase, p.Done, p.Total), backMenu(b.lang()))
	}
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}
	// The message the reader tapped becomes the progress screen and updates
	// itself, instead of asking them to come back and look again.
	return b.followScan(ctx, chat, message, id)
}

// toggleNotifyOnly needs no confirmation: it only changes what this service
// does on its own, and is trivially reversible.
func (b *Bot) toggleNotifyOnly(ctx context.Context, chat, message, targetID int64) error {
	r, err := b.rowByID(ctx, targetID)
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}
	if err := b.d.Store.SetNotifyOnly(targetID, !r.NotifyOnly); err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}
	return b.showServer(ctx, chat, message, targetID)
}

// ---- confirmation ----

// askConfirmation turns a destructive tap into a second screen that says
// exactly what will happen, including how many users are connected.
func (b *Bot) askConfirmation(ctx context.Context, chat, message int64, cb Callback, userID, auditID int64) error {
	r, err := b.rowByID(ctx, cb.ID())
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}

	summary, blocker := b.describeAction(cb.Action, r)
	// The reason comes first: a refusal the operator cannot explain is worse
	// than no button at all.
	if blocker != "" {
		return b.render(ctx, chat, message, "🚫 "+blocker, serverMenu(b.lang(), r, b.capabilities()))
	}
	if summary == "" {
		return b.render(ctx, chat, message, b.tr("bot.notavailable"), backMenu(b.lang()))
	}
	// Replacing is the one action whose consequences depend on half a dozen
	// outside services, so its confirmation checks them and lists every change.
	if cb.Action == ActSwapNow {
		return b.askSwapConfirmation(ctx, chat, message, cb, r, userID, auditID)
	}

	token := b.pending.put(pending{
		Action: cb.Action, TargetID: r.TargetID, UserID: userID,
		Summary: summary, AuditID: auditID,
	})

	text := fmt.Sprintf("<b>%s</b>\n\n%s\n\n", b.tr("bot.confirm"), summary)
	if r.Online != nil && *r.Online > 0 {
		text += b.tr("bot.connectednow", *r.Online) + "\n\n"
	}
	text += b.tr("bot.expires")
	return b.render(ctx, chat, message, text, confirmMenu(b.lang(), token))
}

// describeAction returns the confirmation wording, and a reason the action must
// not run at all.
func (b *Bot) describeAction(action string, r row) (summary, blocker string) {
	target := fmt.Sprintf("<code>%s</code>", r.HostPort())

	switch action {
	case ActRequestNew:
		if !r.replaceable() {
			return "", b.tr("bot.act.notmine")
		}
		return b.tr("bot.act.request", target), ""

	case ActNodeDisable:
		return b.tr("bot.act.disable", target), ""

	case ActNodeEnable:
		return b.tr("bot.act.enable", target), ""

	case ActNodeDelete:
		return b.tr("bot.act.delnode", target), ""

	case ActHetznerDel:
		if r.HZServerID == 0 {
			return "", b.tr("bot.act.noserver")
		}
		// Destroying a machine with users on it is never what was meant.
		if r.Online != nil && *r.Online > 0 {
			return "", b.tr("bot.act.draining", *r.Online, r.HostPort())
		}
		return b.tr("bot.act.delserver", "<code>"+r.ServerName+"</code>", r.HZServerID, target), ""

	case ActHide:
		return b.tr("bot.act.hide", target), ""

	case ActShow:
		return b.tr("bot.act.show", target), ""

	case ActSwapNow:
		if b.d.Provision == nil {
			return "", b.tr("bot.act.noprovision")
		}
		// The panel is what moves the configs. Without it a replacement can
		// only be built and reported, which is not what this button says.
		if b.d.Zex == nil || !b.d.Zex.Configured() {
			return "", b.tr("bot.act.nopanel")
		}
		if !r.replaceable() {
			return "", b.tr("bot.act.notreplaceable", target)
		}
		// Says what actually happens, in order, before anything does: the
		// configs go dark first and stay dark if the replacement fails.
		return b.tr("bot.act.swapnow", target, len(r.ConfigIDs)), ""
	}
	return "", ""
}

func (b *Bot) cancelConfirmed(ctx context.Context, chat, message int64, token string, userID int64) error {
	p, ok := b.pending.drop(token, userID)
	if !ok {
		return b.render(ctx, chat, message, b.tr("bot.nothingcancel"), backMenu(b.lang()))
	}
	return b.showServer(ctx, chat, message, p.TargetID)
}

func (b *Bot) runConfirmed(ctx context.Context, chat, message int64, token string, userID, auditID int64) error {
	p, err := b.pending.take(token, userID)
	if err != nil {
		// The store's errors are translation keys, not prose.
		return b.render(ctx, chat, message, "⚠️ "+b.tr(err.Error()), backMenu(b.lang()))
	}

	r, err := b.rowByID(ctx, p.TargetID)
	if err != nil {
		return b.render(ctx, chat, message, fmtErr(err), backMenu(b.lang()))
	}

	// Re-check the blocker at execution time: the situation may have changed
	// between the two taps.
	if _, blocker := b.describeAction(p.Action, r); blocker != "" {
		return b.render(ctx, chat, message, "🚫 "+blocker, serverMenu(b.lang(), r, b.capabilities()))
	}

	result, execErr := b.execute(ctx, p.Action, r)
	if auditID != 0 {
		if ferr := b.d.Store.FinishBotAction(auditID, execErr == nil, execErr, time.Now()); ferr != nil {
			b.d.Log.Warn("could not record the action outcome", "err", ferr)
		}
	}
	if execErr != nil {
		return b.render(ctx, chat, message, fmtErr(execErr), serverMenu(b.lang(), r, b.capabilities()))
	}
	if p.Action == ActSwapNow {
		return b.followSwap(ctx, chat, message, r.TargetID)
	}

	fresh, err := b.rowByID(ctx, p.TargetID)
	if err != nil {
		return b.render(ctx, chat, message, "✅ "+result, backMenu(b.lang()))
	}
	return b.render(ctx, chat, message, "✅ "+result, serverMenu(b.lang(), fresh, b.capabilities()))
}

// execute performs a confirmed action and returns what to tell the operator.
func (b *Bot) execute(ctx context.Context, action string, r row) (string, error) {
	switch action {
	case ActSwapNow:
		return b.startSwap(ctx, r)

	case ActRequestNew:
		machine, err := b.d.Store.AddressFor(r.Address)
		if err != nil {
			return "", err
		}
		count, err := b.d.Store.OutageCountForAddress(r.Address, time.Now().Add(-b.d.Config.OutageWindow))
		if err != nil {
			return "", err
		}
		go func(a store.AddressStatus, n int) {
			if err := b.d.Provision.Trigger(context.WithoutCancel(ctx), a, n); err != nil {
				b.d.Log.Warn("provisioning requested from telegram failed", "err", err)
			}
		}(machine, count)
		return b.tr("bot.done.request", r.Address), nil

	case ActNodeDisable, ActNodeEnable:
		enable := action == ActNodeEnable
		if err := b.d.Panel.SetNodeEnabled(ctx, r.NodeID, enable); err != nil {
			return "", err
		}
		if enable {
			return b.tr("bot.done.enabled"), nil
		}
		return b.tr("bot.done.disabled"), nil

	case ActNodeDelete:
		if err := b.d.Panel.DeleteNode(ctx, r.NodeID); err != nil {
			return "", err
		}
		return b.tr("bot.done.delnode"), nil

	case ActHetznerDel:
		// Three checks stand between a tap and a destroyed machine, because a
		// Hetzner server id is only meaningful inside its own project: the same
		// number names a different server in every other account.
		//
		// 1. The project must be known. Falling back to "the first configured
		//    project" would send this id somewhere it was never meant to go.
		if r.Project == "" {
			return "", errors.New(b.tr("bot.err.noproject"))
		}
		cli, ok := b.d.Hetzner.Client(r.Project)
		if !ok {
			return "", errors.New(b.tr("bot.err.noproject"))
		}
		// 2. The id must still name the address the operator is looking at. A
		//    stale hz_server_id — recorded before a rebuild, or after a server
		//    was replaced outside this service — would otherwise delete
		//    whatever now holds that id.
		live, err := cli.Server(ctx, r.HZServerID)
		if err != nil {
			return "", err
		}
		if live.Address != r.Address {
			return "", fmt.Errorf(b.tr("bot.err.servermoved"), r.HZServerID, live.Address, r.Address)
		}
		// 3. Only now.
		if err := cli.DeleteServer(ctx, r.HZServerID); err != nil {
			return "", err
		}
		b.d.Hetzner.Invalidate(r.Project)
		// Hetzner returns a deleted server's IP to its location's pool, so the
		// next replacement built there can be handed this address back. Record
		// it, or the service will buy what was just thrown away.
		if err := b.d.Store.BurnAddress(store.LedgerEntry{
			Address:  r.Address,
			Reason:   store.BurnDeletedByOperator,
			Verdict:  r.Verdict,
			Project:  r.Project,
			ServerID: r.HZServerID,
			Note:     "deleted from the bot",
		}); err != nil {
			b.d.Log.Error("could not record the deleted address in the ledger",
				"address", r.Address, "err", err)
		}
		return b.tr("bot.done.delserver", r.ServerName, r.HZServerID), nil

	case ActHide, ActShow:
		show := action == ActShow
		n, err := b.d.Zex.BulkActive(ctx, r.Address, show)
		if err != nil {
			return "", err
		}
		if show {
			return b.tr("bot.done.show", n), nil
		}
		return b.tr("bot.done.hide", n), nil
	}
	return "", errors.New("unknown action")
}
