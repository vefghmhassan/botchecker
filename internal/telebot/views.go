package telebot

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
)

// pageSize keeps a listing well inside Telegram's 4096-character message limit
// even when every row carries a long config name.
const pageSize = 8

// verdictEmoji gives each verdict a glyph that reads at a glance on a phone.
func verdictEmoji(verdict string) string {
	switch verdict {
	case string(scanner.VerdictHealthy):
		return "🟢"
	case string(scanner.VerdictBlockedIR):
		return "🔴"
	case string(scanner.VerdictPartialBlock):
		return "🟠"
	case string(scanner.VerdictServerDown):
		return "⚫"
	case string(scanner.VerdictPortClosed):
		return "🟣"
	default:
		return "⚪"
	}
}

func providerEmoji(provider string) string {
	switch provider {
	case string(hetzner.OwnedHetzner):
		return "☁️"
	case string(hetzner.OwnedExternal):
		return "🌐"
	default:
		return "❔"
	}
}

// row is one endpoint with everything the bot needs to render and act on it.
type row struct {
	store.TargetStatus
	Provider   string
	ServerName string
	HZServerID int64
	// Project is the Hetzner account that holds this machine. Without it a
	// server id cannot be acted on safely.
	Project      string
	AbuseBlocked bool
	Online       *int
	// NodeID is the 3x-ui node serving this address, 0 when the panel does
	// not know it.
	NodeID      int
	NodeEnabled bool
}

func (r row) replaceable() bool {
	return r.Provider == string(hetzner.OwnedHetzner) && !r.NotifyOnly
}

// menuText is the landing screen.
func menuText(l i18n.Lang, counts map[string]int, total int, lastScan *store.Scan, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", i18n.T(l, "bot.title"))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.summary",
		"🟢", counts[string(scanner.VerdictHealthy)],
		"🔴", counts[string(scanner.VerdictBlockedIR)],
		"🟠", counts[string(scanner.VerdictPartialBlock)],
		"⚫", counts[string(scanner.VerdictServerDown)]))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.total", total))

	if lastScan != nil && lastScan.FinishedAt != nil {
		fmt.Fprintf(&b, "\n%s", i18n.T(l, "bot.lastscan",
			lastScan.ID, lastScan.FinishedAt.In(loc).Format("2006-01-02 15:04")))
	} else {
		b.WriteString("\n" + i18n.T(l, "bot.noscan"))
	}
	return l.Digits(b.String())
}

// listText renders one page of endpoints. The buttons carry the detail links;
// the text is a compact overview so the page reads without tapping.
func listText(l i18n.Lang, rows []row, page, pages int, titleKey string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>", html.EscapeString(i18n.T(l, titleKey)))
	if pages > 1 {
		fmt.Fprintf(&b, "  ·  %s", i18n.T(l, "bot.list.page", page, pages))
	}
	b.WriteString("\n\n")

	if len(rows) == 0 {
		b.WriteString(i18n.T(l, "bot.list.empty"))
		return b.String()
	}
	for _, r := range rows {
		// The address stays in ASCII so it reads correctly inside Persian text.
		fmt.Fprintf(&b, "%s <code>%s</code> %s\n",
			verdictEmoji(r.Verdict), html.EscapeString(r.HostPort()), providerEmoji(r.Provider))
		fmt.Fprintf(&b, "    %s · %s %d/%d",
			i18n.T(l, "verdict."+r.Verdict), i18n.T(l, "th.iran"), r.IROpen, r.IRTotal)
		if r.NotifyOnly {
			fmt.Fprintf(&b, " · 🔒 %s", i18n.T(l, "providers.notifyonly"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// serverText is the detail screen for one endpoint.
func serverText(l i18n.Lang, r row, tr *store.TracerouteRow, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b><code>%s</code></b>\n\n", verdictEmoji(r.Verdict), html.EscapeString(r.HostPort()))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.verdictline", "<b>"+i18n.T(l, "verdict."+r.Verdict)+"</b>"))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.iranline", r.IROpen, r.IRTotal, r.ControlOpen, r.ControlTotal))

	if r.Online != nil {
		fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.online", *r.Online))
	} else {
		b.WriteString(i18n.T(l, "bot.noonline") + "\n")
	}

	fmt.Fprintf(&b, "%s %s", providerEmoji(r.Provider),
		i18n.T(l, "bot.providerline", i18n.T(l, "provider."+r.Provider)))
	if r.ServerName != "" {
		fmt.Fprintf(&b, " · <code>%s</code>", html.EscapeString(r.ServerName))
	}
	b.WriteString("\n")

	if r.AbuseBlocked {
		b.WriteString(i18n.T(l, "bot.abuse") + "\n")
	}
	if r.NotifyOnly {
		b.WriteString(i18n.T(l, "bot.notifyonly") + "\n")
	}
	if len(r.Names) > 0 {
		fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.configline", html.EscapeString(splash.SummariseNames(r.Names, 5))))
	}
	if r.SinceAt != nil {
		fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.sinceline", r.SinceAt.In(loc).Format("2006-01-02 15:04")))
	}
	if tr != nil && tr.LastHop != "" {
		fmt.Fprintf(&b, "\n%s", i18n.T(l, "bot.dieat",
			"<code>"+html.EscapeString(tr.LastHop)+"</code>", tr.DeadHops))
		if tr.LastHopPrivate {
			b.WriteString(i18n.T(l, "bot.insideop"))
		}
		b.WriteString(".")
	}
	return b.String()
}

// providersText answers "which of these could I replace automatically".
func providersText(l i18n.Lang, rows []row) string {
	counts := map[string]int{}
	blockedExternal := 0
	for _, r := range rows {
		counts[r.Provider]++
		if r.Verdict == string(scanner.VerdictBlockedIR) && r.Provider != string(hetzner.OwnedHetzner) {
			blockedExternal++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", i18n.T(l, "bot.providers"))
	fmt.Fprintf(&b, "☁️ %s\n", i18n.T(l, "bot.inproject", counts[string(hetzner.OwnedHetzner)]))
	fmt.Fprintf(&b, "🌐 %s\n", i18n.T(l, "bot.elsewhere", counts[string(hetzner.OwnedExternal)]))
	fmt.Fprintf(&b, "❔ %s\n", i18n.T(l, "bot.unknownprov", counts[string(hetzner.OwnedUnknown)]))

	if blockedExternal > 0 {
		fmt.Fprintf(&b, "\n%s", i18n.T(l, "bot.externalwarn", blockedExternal))
	}
	return l.Digits(b.String())
}

// historyText lists recent reactions to a block.
func historyText(l i18n.Lang, provs []store.Provision, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", i18n.T(l, "bot.history"))
	if len(provs) == 0 {
		b.WriteString(i18n.T(l, "bot.nohistory"))
		return b.String()
	}
	for _, p := range provs {
		fmt.Fprintf(&b, "<code>%s:%d</code> · %s\n", html.EscapeString(p.Address), p.Port, p.Status)
		fmt.Fprintf(&b, "    %s", p.TriggeredAt.In(loc).Format("01-02 15:04"))
		if p.NewAddress != "" {
			fmt.Fprintf(&b, " → <code>%s</code>", html.EscapeString(p.NewAddress))
		}
		if p.VerifyVerdict != "" {
			fmt.Fprintf(&b, " (%s)", p.VerifyVerdict)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// statusText summarises how the service itself is configured and running.
func statusText(l i18n.Lang, s ServiceStatus, loc *time.Location) string {
	on := func(v bool) string {
		if v {
			return i18n.T(l, "bot.on")
		}
		return i18n.T(l, "bot.off")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", i18n.T(l, "bot.status"))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.scanningnow", on(s.Scanning)))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.watching", s.Watching))
	if !s.LastWatch.IsZero() {
		fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.lastwatch", s.LastWatch.In(loc).Format("15:04")))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.panel", on(s.PanelOn)))
	fmt.Fprintf(&b, "%s\n", i18n.T(l, "bot.hetzner", on(s.HetznerOn)))
	fmt.Fprintf(&b, "%s", i18n.T(l, "bot.provisioning", on(s.ProvisionOn)))
	if s.DryRun {
		b.WriteString(i18n.T(l, "bot.dryrunnote"))
	}
	return l.Digits(b.String())
}

// ServiceStatus is the snapshot the status screen renders.
type ServiceStatus struct {
	Scanning    bool
	Watching    int
	LastWatch   time.Time
	PanelOn     bool
	HetznerOn   bool
	ProvisionOn bool
	DryRun      bool
}

func paginate(rows []row, page int) ([]row, int, int) {
	pages := (len(rows) + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}

	start := (page - 1) * pageSize
	if start >= len(rows) {
		return nil, page, pages
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], page, pages
}
