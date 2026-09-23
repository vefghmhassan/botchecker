package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

// ProviderRow is one endpoint with its ownership, for the providers page and
// the provider column on the overview.
type ProviderRow struct {
	store.TargetStatus
	Provider     string
	Kind         string
	ServerID     int64
	ServerName   string
	AbuseBlocked bool
	Replaceable  bool
}

// providerRows attaches ownership to every endpoint.
func (h *Handler) providerRows() ([]ProviderRow, map[string]int, error) {
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return nil, nil, err
	}

	counts := map[string]int{}
	rows := make([]ProviderRow, 0, len(statuses))
	for _, s := range statuses {
		provider, kind, name, id, abuse, err := h.st.TargetProvider(s.TargetID)
		if err != nil {
			return nil, nil, err
		}
		if provider == "" {
			provider = string(hetzner.OwnedUnknown)
		}
		counts[provider]++
		rows = append(rows, ProviderRow{
			TargetStatus: s, Provider: provider, Kind: kind,
			ServerID: id, ServerName: name, AbuseBlocked: abuse,
			Replaceable: provider == string(hetzner.OwnedHetzner) && !s.NotifyOnly,
		})
	}
	return rows, counts, nil
}

func (h *Handler) bot(c *fiber.Ctx) error {
	actions, err := h.st.BotActions(60)
	if err != nil {
		return err
	}
	unauthorized, err := h.st.UnauthorizedBotAttempts(time.Now().AddDate(0, 0, -7))
	if err != nil {
		return err
	}

	data := h.base("Bot", "bot")
	data["Actions"] = actions
	data["Unauthorized"] = unauthorized
	if h.d.Bot != nil {
		data["Enabled"] = h.d.Bot.Enabled()
		data["BotRunning"] = h.d.Bot.Running()
		data["LastUpdate"] = h.d.Bot.LastUpdate()
		data["AllowedIDs"] = h.d.Bot.AllowedIDs()
	}
	return c.Render("bot", data, "layout")
}

func (h *Handler) providers(c *fiber.Ctx) error {
	rows, counts, err := h.providerRows()
	if err != nil {
		return err
	}

	data := h.base("Providers", "providers")
	data["Rows"] = rows
	data["Counts"] = counts
	data["HetznerConfigured"] = h.d.Hetzner != nil && h.d.Hetzner.Configured()
	return c.Render("providers", data, "layout")
}

// toggleNotifyOnly flips the hands-off flag from the endpoint page.
func (h *Handler) toggleNotifyOnly(c *fiber.Ctx) error {
	addr := c.Params("addr")
	port, err := strconv.Atoi(c.Params("port"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "port must be a number")
	}
	targetID, err := h.st.TargetByHostPort(addr, port)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "no such endpoint")
	}
	if err := h.st.SetNotifyOnly(targetID, c.FormValue("notify_only") != ""); err != nil {
		return err
	}
	return c.Redirect("/target/"+addr+"/"+strconv.Itoa(port), fiber.StatusSeeOther)
}

// refreshProviders re-tags every endpoint from the dashboard, so a freshly
// entered token takes effect without waiting for the next full scan.
func (h *Handler) refreshProviders(c *fiber.Ctx) error {
	if h.d.Hetzner == nil || !h.d.Hetzner.Configured() {
		return c.Redirect("/providers", fiber.StatusSeeOther)
	}

	ctx, cancel := contextWithTimeout(c, 60*time.Second)
	defer cancel()

	h.d.Hetzner.InvalidateAll()
	invs, errs := h.d.Hetzner.InventoryAll(ctx)

	// A project that could not be read makes this a partial view, and a partial
	// view must not be written: every address in the missing project would be
	// relabelled external, which is the label that stops it ever being
	// replaced automatically.
	if len(errs) > 0 {
		names := make([]string, 0, len(errs))
		for slug := range errs {
			names = append(names, slug)
		}
		sort.Strings(names)
		return fiber.NewError(fiber.StatusBadGateway,
			"nothing was changed — could not read project(s): "+strings.Join(names, ", "))
	}

	machines, err := h.st.AddressStatuses()
	if err != nil {
		return err
	}
	addresses := make([]string, 0, len(machines))
	for _, m := range machines {
		addresses = append(addresses, m.Address)
	}

	assignments, _ := hetzner.Reconcile(addresses, invs, errs)
	now := time.Now()
	for _, a := range assignments {
		if err := h.st.SetAddressProvider(a.Address, string(a.Ownership), a.Project,
			a.Resource.Kind, a.Resource.ServerID, a.Resource.ServerName,
			a.Resource.AbuseBlocked, now); err != nil {
			return err
		}
	}
	return c.Redirect("/providers", fiber.StatusSeeOther)
}

func (h *Handler) provisions(c *fiber.Ctx) error {
	rows, err := h.st.Provisions(100)
	if err != nil {
		return err
	}
	notes, err := h.st.Notifications(50)
	if err != nil {
		return err
	}

	// The ledger belongs on this page rather than its own: it is the record of
	// what replacements destroyed, and it is read for the same reason the rows
	// above are — "what happened to this machine, and why is nothing being done
	// about it".
	ledger, err := h.st.BurnedAddresses()
	if err != nil {
		return err
	}

	data := h.base("Provisions", "provisions")
	data["Provisions"] = rows
	data["Notifications"] = notes
	data["Ledger"] = ledger
	data["Enabled"] = h.settingBool(settings.ProvisionEnabled)
	data["DryRun"] = h.settingBool(settings.ProvisionDryRun)
	return c.Render("provisions", data, "layout")
}

// releaseAddress lets a burned address be used again.
//
// Not a destructive action — the worst case is one wasted build, which the
// check after creation catches anyway — so it needs no second screen. What it
// does need is to be possible at all: an address barred forever with no way
// back would eventually exhaust the pool.
func (h *Handler) releaseAddress(c *fiber.Ctx) error {
	address := c.Params("addr")
	if err := h.st.ReleaseAddress(address, time.Now()); err != nil {
		return err
	}
	return c.Redirect("/provisions", fiber.StatusSeeOther)
}

// group bundles one settings form for the template.
type group struct {
	Name     string
	Title    string
	Blurb    string
	Testable bool
	Settings []settings.View
}

// group titles come from the translation table, keyed by group name.

func (h *Handler) settingsPage(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}

	lang := h.lang()
	var groups []group
	for _, name := range settings.GroupsInOrder {
		groups = append(groups, group{
			Name:     name,
			Title:    i18n.T(lang, "group."+name+".title"),
			Blurb:    i18n.T(lang, "group."+name+".blurb"),
			Testable: name == settings.GroupTelegram || name == settings.GroupXUI || name == settings.GroupHetzner,
			Settings: h.d.Settings.ViewsByGroup(name),
		})
	}

	data := h.base("Settings", "settings")
	data["Groups"] = groups
	data["Flash"] = c.Query("msg")
	data["FlashErr"] = c.Query("err")
	return c.Render("settings", data, "layout")
}

// saveSettings applies one form. A blank field leaves the stored value alone;
// clearing is an explicit checkbox, so submitting the form cannot silently
// wipe a token the operator could not see in the first place.
func (h *Handler) saveSettings(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}

	groupName := c.FormValue("group")
	for _, view := range h.d.Settings.ViewsByGroup(groupName) {
		if c.FormValue("clear__"+view.Key) != "" {
			if err := h.d.Settings.Clear(view.Key); err != nil {
				return redirectSettings(c, "", err.Error())
			}
			continue
		}

		value := strings.TrimSpace(c.FormValue(view.Key))
		if view.Kind == settings.KindBool {
			// An unchecked box submits nothing, which for a switch means off.
			value = "false"
			if c.FormValue(view.Key) != "" {
				value = "true"
			}
		} else if value == "" {
			// A blank field means "leave it alone"; clearing is explicit.
			continue
		}

		if err := h.d.Settings.Set(view.Key, value); err != nil {
			return redirectSettings(c, "", err.Error())
		}
	}
	return redirectSettings(c, "Saved.", "")
}

// testSettings verifies one integration and reports back on the same page.
func (h *Handler) testSettings(c *fiber.Ctx) error {
	ctx, cancel := contextWithTimeout(c, 20*time.Second)
	defer cancel()

	switch c.FormValue("group") {
	case settings.GroupTelegram:
		if h.d.Telegram == nil || !h.d.Telegram.Configured() {
			return redirectSettings(c, "", "Set a bot token and chat id first.")
		}
		if err := h.d.Telegram.Send(ctx, "✅ botchecker test message — alerts are working."); err != nil {
			return redirectSettings(c, "", err.Error())
		}
		return redirectSettings(c, "Test message sent to Telegram.", "")

	case settings.GroupXUI:
		if h.d.Panel == nil || !h.d.Panel.Configured() {
			return redirectSettings(c, "", "Set the panel URL and API token first.")
		}
		n, err := h.d.Panel.Ping(ctx)
		if err != nil {
			return redirectSettings(c, "", err.Error())
		}
		return redirectSettings(c, plural(n, "node")+" visible in the panel.", "")

	case settings.GroupHetzner:
		if h.d.Hetzner == nil || !h.d.Hetzner.Configured() {
			return redirectSettings(c, "", "Set a Hetzner token first.")
		}
		h.d.Hetzner.InvalidateAll()
		invs, errs := h.d.Hetzner.InventoryAll(ctx)
		var lines []string
		for _, p := range h.d.Hetzner.Projects() {
			switch {
			case errs[p.Slug] != nil:
				lines = append(lines, p.Slug+": "+errs[p.Slug].Error())
			case invs[p.Slug] != nil:
				detail := p.Slug + ": " + plural(invs[p.Slug].Servers, "server") +
					", " + plural(invs[p.Slug].Len(), "address")
				if ok, why := p.Usable(); !ok {
					detail += " — cannot build: " + why
				}
				lines = append(lines, detail)
			}
		}
		if len(errs) > 0 {
			return redirectSettings(c, "", strings.Join(lines, " · "))
		}
		return redirectSettings(c, strings.Join(lines, " · "), "")
	}
	return redirectSettings(c, "", "Unknown section.")
}

func redirectSettings(c *fiber.Ctx, msg, errMsg string) error {
	q := "/settings"
	switch {
	case errMsg != "":
		q += "?err=" + urlEscape(errMsg)
	case msg != "":
		q += "?msg=" + urlEscape(msg)
	}
	return c.Redirect(q, fiber.StatusSeeOther)
}

func (h *Handler) settingBool(key string) bool {
	return h.d.Settings != nil && h.d.Settings.Bool(key)
}

func plural(n int, word string) string {
	s := itoa(n) + " " + word
	if n != 1 {
		if strings.HasSuffix(word, "s") {
			return s + "es"
		}
		return s + "s"
	}
	return s
}
