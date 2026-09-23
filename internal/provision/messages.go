package provision

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/vefgh/botchecker/internal/store"
)

// Alert kinds, also used as the notification record's kind.
const (
	KindBlocked         = "blocked"
	KindBlockedExternal = "blocked_external"
	KindProvisioned     = "provisioned"
	KindVerified        = "verified"
	KindQuieted         = "quieted"
	KindQuietFailed     = "quiet_failed"
	KindSwapped         = "swapped"
	KindSwapFailed      = "swap_failed"
	KindRetired         = "retired"
	KindOrphan          = "orphan"
	KindBurned          = "burned_address"
	KindFloated         = "floated"
	KindFloatManual     = "float_manual"
	KindTest            = "test"
)

// context gathered for one alert.
type alertContext struct {
	Target store.TargetStatus
	// Ports is every port the machine serves. One alert covers the whole
	// machine, so the reader needs to know what "this address" actually means
	// — a replacement takes all of these with it.
	Ports       []int
	OutageCount int
	Window      string
	Online      *int
	Provider    string
	ServerName  string
	AbuseBlock  bool
	NotifyOnly  bool
	Traceroute  *store.TracerouteRow
}

// blockedMessage is the alert for an address that can be replaced.
func blockedMessage(c alertContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 <b>Blocked from Iran</b>\n<code>%s</code>%s\n\n",
		esc(c.Target.Address), portSuffix(c.Ports))
	writeCommon(&b, c)

	if c.ServerName != "" {
		fmt.Fprintf(&b, "Hetzner server: <code>%s</code>\n", esc(c.ServerName))
	}
	if c.AbuseBlock {
		b.WriteString("\n⚠️ <b>Hetzner has blocked this IP for abuse.</b> " +
			"A new server will not fix that — check the abuse notice first.\n")
	}
	if c.NotifyOnly {
		b.WriteString("\n🔒 This endpoint is marked <b>notify only</b> — " +
			"no replacement will be created for it.\n")
	}
	return b.String()
}

// externalMessage is the alert for an address outside the Hetzner project.
// It must be unmistakable: nothing will happen automatically.
func externalMessage(c alertContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 <b>Blocked from Iran</b>\n<code>%s</code>%s\n\n",
		esc(c.Target.Address), portSuffix(c.Ports))
	writeCommon(&b, c)

	b.WriteString("\n🛠 <b>This address is not in your Hetzner project.</b>\n" +
		"It cannot be replaced automatically — create the replacement on whichever " +
		"provider hosts it and swap it in by hand.\n")
	if c.NotifyOnly {
		b.WriteString("\n🔒 It is also marked <b>notify only</b>.\n")
	}
	return b.String()
}

func writeCommon(b *strings.Builder, c alertContext) {
	// A manual trigger has no outage history behind it, so saying "seen
	// blocked 0 times" would just read as a contradiction.
	if c.OutageCount > 0 {
		fmt.Fprintf(b, "Seen blocked %d times in %s.\n", c.OutageCount, c.Window)
	} else {
		b.WriteString("Triggered manually.\n")
	}

	if len(c.Target.Names) > 0 {
		fmt.Fprintf(b, "Config: %s\n", esc(strings.Join(c.Target.Names, ", ")))
	}
	if len(c.Target.ConfigIDs) > 0 {
		ids := make([]string, len(c.Target.ConfigIDs))
		for i, id := range c.Target.ConfigIDs {
			ids[i] = strconv.Itoa(id)
		}
		fmt.Fprintf(b, "Config IDs: %s\n", strings.Join(ids, ", "))
	}

	fmt.Fprintf(b, "Iran: %d/%d probes connected · control: %d/%d\n",
		c.Target.IROpen, c.Target.IRTotal, c.Target.ControlOpen, c.Target.ControlTotal)

	if c.Online != nil {
		fmt.Fprintf(b, "👥 <b>%d user(s) online</b> on this server right now.\n", *c.Online)
	} else {
		b.WriteString("👥 Online count unavailable (3x-ui panel not configured).\n")
	}

	if c.Traceroute != nil && c.Traceroute.LastHop != "" {
		fmt.Fprintf(b, "Packets die at <code>%s</code> after %d silent hops",
			esc(c.Traceroute.LastHop), c.Traceroute.DeadHops)
		if c.Traceroute.LastHopPrivate {
			b.WriteString(" — inside the operator's own network, so the filtering is domestic")
		}
		b.WriteString(".\n")
	}
}

func provisionedMessage(replaces string, s *createdInfo) string {
	return fmt.Sprintf("🆕 <b>Replacement server created</b>\n"+
		"New address: <code>%s</code>\nReplaces: <code>%s</code>\n"+
		"Type: %s · Location: %s · Hetzner id: %d\n\nVerifying reachability from Iran…",
		esc(s.Address), esc(replaces), esc(s.ServerType), esc(s.Location), s.ServerID)
}

// verifiedMessage reports the probe of a replacement. perPort carries each
// port's own verdict, because "healthy on 443, blocked on 8443" is a broken
// clone rather than filtering and the operator cannot tell the two apart from
// an aggregate.
func verifiedMessage(replaces, newAddr, perPort, verdict string, healthy bool) string {
	if healthy {
		return fmt.Sprintf("✅ <b>New server reachable from Iran</b>\n"+
			"<code>%s</code> replaces <code>%s</code>\n"+
			"Ports: <code>%s</code>\n\n"+
			"It is <b>not</b> wired in automatically — swap it in when you are ready.",
			esc(newAddr), esc(replaces), esc(perPort))
	}
	return fmt.Sprintf("⚠️ <b>New server is not reachable from Iran</b>\n"+
		"<code>%s</code> came back <code>%s</code>.\nPorts: <code>%s</code>\n\n"+
		"No further server was created — a fresh address can be blocked on arrival, "+
		"and retrying automatically would just spend money.",
		esc(newAddr), esc(verdict), esc(perPort))
}

// retiredMessage reports that the blocked machine has been handed back.
func retiredMessage(address, serverName string, serverID int64, project string) string {
	return fmt.Sprintf("🗑 <b>Blocked machine handed back</b>\n<code>%s</code>\n\n"+
		"%s (id %d) in project <code>%s</code> answered from no Iranian network and its "+
		"configs were already switched off, so it was serving nobody. Deleting it frees "+
		"the slot the replacement needs.",
		esc(address), esc(serverName), serverID, esc(project))
}

// burnedMessage reports a machine that is dead on purpose but still being
// handed to users.
//
// This is the "tell me, do not touch it" case. The address was retired,
// discarded or deleted deliberately, so replacing it would buy a machine to
// stand in for one that no longer exists — but its configs are still in the
// panel, so somebody is being given an address that answers nothing. Only the
// operator can decide what those configs should point at instead.
func burnedMessage(address string, e store.LedgerEntry, configs int, ports []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚰️ <b>Retired machine still in the app</b>\n<code>%s</code>\n\n", esc(address))
	fmt.Fprintf(&b, "Recorded as <b>%s</b> on %s", esc(e.Reason),
		esc(e.RecordedAt.UTC().Format("2006-01-02 15:04 UTC")))
	if e.Verdict != "" {
		fmt.Fprintf(&b, " after reading %s", esc(e.Verdict))
	}
	b.WriteString(".\n")
	if e.Note != "" {
		fmt.Fprintf(&b, "<i>%s</i>\n", esc(e.Note))
	}
	b.WriteString("\n")

	if configs > 0 {
		fmt.Fprintf(&b, "<b>%d config(s)</b> on port(s) %s still point at it, so users are "+
			"being handed an address that answers nothing.\n\n", configs, esc(joinInts(ports)))
	}
	b.WriteString("I have not touched it. No replacement will be built for this address " +
		"while it is in the ledger — release it from the bot if it should be used again.")
	return b.String()
}

// floatedMessage reports a machine rescued with a new address instead of a new
// server.
func floatedMessage(oldAddr, newAddr string, serverID int64, configs int) string {
	return fmt.Sprintf("🎈 <b>Rescued with a floating address</b>\n"+
		"<code>%s</code> → <code>%s</code>\n\n"+
		"No project had room for a new server, so server %d kept its disk and took a new "+
		"address instead. It answered from Iran, and %d config(s) moved across.",
		esc(oldAddr), esc(newAddr), serverID, configs)
}

// floatManualMessage asks for the one command this service cannot run itself.
//
// A floating IP is routed to the server but not answered by it until the
// address is on the interface, and without an SSH key there is no way to put it
// there. Asking is better than assigning an address that silently answers
// nothing.
func floatManualMessage(address, newAddr string, serverID int64, script string) string {
	return fmt.Sprintf("🎈 <b>Floating address waiting for one command</b>\n"+
		"<code>%s</code> → <code>%s</code>\n\n"+
		"The address is assigned to server %d, but Hetzner will not answer on it until it "+
		"is added to the interface, and no SSH key is configured for me to do it.\n\n"+
		"Run this on the server, then press the button:\n<pre>%s</pre>",
		esc(address), esc(newAddr), serverID, esc(script))
}

// joinInts renders a port list for a sentence.
func joinInts(ns []int) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

// orphanMessage reports a server nothing points at. Deliberately not acted on:
// a machine missing from the config list may be mid-setup, may be serving
// something this service does not know about, or may simply be forgotten — and
// only the operator can tell which.
func orphanMessage(rows []orphanRow) string {
	var b strings.Builder
	b.WriteString("🔎 <b>Servers not in the app</b>\n\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "<code>%s</code> — %s (id %d, %s)\n",
			esc(r.Address), esc(r.ServerName), r.ServerID, esc(r.Project))
	}
	b.WriteString("\nNo config points at these, so the app never hands them out. " +
		"<b>Nothing was touched</b> — they may be mid-setup or serving something else. " +
		"Check them and delete them if they really are dead weight; each one costs money " +
		"and holds a slot a replacement would need.")
	return b.String()
}

// orphanRow is one server with no config behind it.
type orphanRow struct {
	Address    string
	ServerName string
	ServerID   int64
	Project    string
}

type createdInfo struct {
	ServerID   int64
	Address    string
	ServerType string
	Location   string
	// Project is which Hetzner account created it, carried so a later delete
	// reaches the same account.
	Project string
}

func esc(s string) string { return html.EscapeString(s) }

// quietedMessage reports that the broken configs have stopped being served.
// quietedMessage reports configs being withdrawn. This only happens when no
// replacement could be produced: while one is on its way the configs stay up,
// because the address they point at is already unreachable from Iran and taking
// them down early only adds to how long the user is without anything.
func quietedMessage(hostPort string, configs int) string {
	return fmt.Sprintf("🔇 <b>Configs taken out of circulation</b>\n"+
		"<code>%s</code>\n\n"+
		"%d config(s) are no longer handed to the app, because no replacement "+
		"could be built for this machine. They stay off until one is.",
		esc(hostPort), configs)
}

func quietFailedMessage(hostPort string, cause error) string {
	return fmt.Sprintf("⚠️ <b>Could not take the configs out of circulation</b>\n"+
		"<code>%s</code>\n\n%s\n\nThe app is still being handed a blocked config — "+
		"disable it in the panel by hand.",
		esc(hostPort), esc(cause.Error()))
}

// swappedMessage is the one that says users are being served again.
func swappedMessage(oldHostPort, newAddress, country string, created *createdInfo, result *replaceOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ <b>Replaced</b>\n<code>%s</code> → <code>%s</code>\n\n",
		esc(oldHostPort), esc(newAddress))
	fmt.Fprintf(&b, "%d config(s) moved and re-enabled", result.Updated)
	if result.Skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", result.Skipped)
	}
	b.WriteString(".\n")
	fmt.Fprintf(&b, "New server in %s", esc(country))
	if created != nil {
		fmt.Fprintf(&b, " · %s · Hetzner id %d", esc(created.Location), created.ServerID)
	}
	b.WriteString("\n")

	for _, e := range result.Errors {
		fmt.Fprintf(&b, "\n⚠️ %s", esc(e))
	}
	return b.String()
}

// swapFailedMessage says exactly what is still broken and what was left off.
func swapFailedMessage(hostPort string, configs int, cause error) string {
	return fmt.Sprintf("🛑 <b>Replacement failed</b>\n<code>%s</code>\n\n%s\n\n"+
		"%d config(s) are still switched off, on purpose — handing out a config that "+
		"is known not to work is worse than handing out none. Re-enable them in the "+
		"panel once you have a working address.",
		esc(hostPort), esc(cause.Error()), configs)
}

// replaceOutcome mirrors the panel's reply without importing it here.
type replaceOutcome struct {
	Updated int
	Skipped int
	Errors  []string
}

// portSuffix renders the ports a machine serves, so an alert about an address
// says plainly how much moves with it.
func portSuffix(ports []int) string {
	switch len(ports) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(":%d", ports[0])
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return " (ports " + strings.Join(parts, ", ") + ")"
}
