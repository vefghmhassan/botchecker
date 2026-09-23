package telebot

import (
	"fmt"

	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/scanner"
)

func btn(text, action string, arg any) notify.Button {
	return notify.Button{Text: text, Data: Encode(action, arg)}
}

// mainMenu is the landing keyboard.
func mainMenu(l i18n.Lang) *notify.Keyboard {
	return &notify.Keyboard{Rows: [][]notify.Button{
		{btn(i18n.T(l, "bot.menu.servers"), ActServerList, 1), btn(i18n.T(l, "bot.menu.blocked"), ActBlocked, 1)},
		{btn(i18n.T(l, "bot.menu.providers"), ActProviders, nil), btn(i18n.T(l, "bot.menu.history"), ActHistory, nil)},
		{btn(i18n.T(l, "bot.menu.sync"), ActSync, nil), btn(i18n.T(l, "bot.menu.scan"), ActScan, nil)},
		{btn(i18n.T(l, "bot.menu.status"), ActStatus, nil)},
		{btn(i18n.T(l, "bot.menu.ledger"), ActLedger, nil)},
	}}
}

// listMenu turns a page of endpoints into one button each, plus paging.
func listMenu(l i18n.Lang, rows []row, page, pages int, listAction string) *notify.Keyboard {
	kb := &notify.Keyboard{}
	for _, r := range rows {
		label := fmt.Sprintf("%s %s", verdictEmoji(r.Verdict), r.HostPort())
		kb.Rows = append(kb.Rows, []notify.Button{btn(label, ActServer, r.TargetID)})
	}

	if pages > 1 {
		var nav []notify.Button
		if page > 1 {
			nav = append(nav, btn("◀", listAction, page-1))
		}
		nav = append(nav, btn(fmt.Sprintf("%d/%d", page, pages), ActNoop, nil))
		if page < pages {
			nav = append(nav, btn("▶", listAction, page+1))
		}
		kb.Rows = append(kb.Rows, nav)
	}

	kb.Rows = append(kb.Rows, []notify.Button{btn(i18n.T(l, "bot.menu.home"), ActMenu, nil)})
	return kb
}

// serverMenu builds the per-endpoint controls.
//
// Buttons are built from what is actually possible: an action that would always
// fail is not shown at all, so the menu never offers something it cannot do.
func serverMenu(l i18n.Lang, r row, caps Capabilities) *notify.Keyboard {
	kb := &notify.Keyboard{}

	notifyLabel := i18n.T(l, "bot.btn.notifyoff")
	if r.NotifyOnly {
		notifyLabel = i18n.T(l, "bot.btn.notifyon")
	}
	first := []notify.Button{btn(notifyLabel, ActNotifyOnly, r.TargetID)}
	if r.replaceable() && r.Verdict == string(scanner.VerdictBlockedIR) {
		first = append(first, btn(i18n.T(l, "bot.btn.request"), ActRequestNew, r.TargetID))
	}
	kb.Rows = append(kb.Rows, first)

	// Offered for anything replaceable that is not working, not only for a full
	// block. A machine that is simply down had no button at all before, which
	// is exactly the case where a person most wants to act by hand.
	if caps.Zex && r.replaceable() && r.Verdict != string(scanner.VerdictHealthy) {
		kb.Rows = append(kb.Rows, []notify.Button{
			btn(i18n.T(l, "bot.btn.swapnow"), ActSwapNow, r.TargetID),
		})
	}

	if caps.Panel && r.NodeID != 0 {
		var second []notify.Button
		if r.NodeEnabled {
			second = append(second, btn(i18n.T(l, "bot.btn.disable"), ActNodeDisable, r.TargetID))
		} else {
			second = append(second, btn(i18n.T(l, "bot.btn.enable"), ActNodeEnable, r.TargetID))
		}
		second = append(second, btn(i18n.T(l, "bot.btn.delnode"), ActNodeDelete, r.TargetID))
		kb.Rows = append(kb.Rows, second)
	}

	// Hiding is only offered when the panel is reachable, and its opposite is
	// offered alongside so a quieted address can be brought back.
	var third []notify.Button
	if caps.Zex {
		third = append(third,
			btn(i18n.T(l, "bot.btn.hide"), ActHide, r.TargetID),
			btn(i18n.T(l, "bot.btn.show"), ActShow, r.TargetID))
	}
	if caps.Hetzner && r.HZServerID != 0 {
		third = append(third, btn(i18n.T(l, "bot.btn.delserver"), ActHetznerDel, r.TargetID))
	}
	if len(third) > 0 {
		kb.Rows = append(kb.Rows, third)
	}

	kb.Rows = append(kb.Rows, []notify.Button{
		btn(i18n.T(l, "bot.menu.back"), ActServerList, 1), btn(i18n.T(l, "bot.menu.home"), ActMenu, nil),
	})
	return kb
}

// confirmMenu is the second step in front of anything destructive.
func confirmMenu(l i18n.Lang, token string) *notify.Keyboard {
	return &notify.Keyboard{Rows: [][]notify.Button{
		{btn(i18n.T(l, "bot.btn.yes"), ActConfirm, token), btn(i18n.T(l, "bot.btn.cancel"), ActCancel, token)},
	}}
}

// backMenu is the footer for screens with no actions of their own.
// liveMenu is the keyboard on a message that is following a running scan.
// Going home is the way to stop following, so the reader is never stuck
// watching a screen that keeps redrawing itself.
func liveMenu(l i18n.Lang) *notify.Keyboard {
	return &notify.Keyboard{Rows: [][]notify.Button{
		{btn(i18n.T(l, "bot.live.stopwatching"), ActMenu, nil)},
	}}
}

func backMenu(l i18n.Lang) *notify.Keyboard {
	return &notify.Keyboard{Rows: [][]notify.Button{{btn(i18n.T(l, "bot.menu.home"), ActMenu, nil)}}}
}

// Capabilities says which integrations are usable right now.
type Capabilities struct {
	Panel   bool
	Hetzner bool
	Zex     bool
}
