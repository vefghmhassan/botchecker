package api

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/provision"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

// BotStatus is the part of the Telegram bot the web layer reports on. It is an
// interface so the HTTP packages do not depend on the bot's action layer.
type BotStatus interface {
	Enabled() bool
	Running() bool
	LastUpdate() time.Time
	AllowedIDs() []int64
}

func (h *Handler) registerPhase2(v1 fiber.Router) {
	v1.Get("/watchlist", h.watchlist)
	v1.Get("/outages", h.outages)
	v1.Get("/provisions", h.provisions)
	v1.Get("/provisions/:id", h.provision)
	v1.Post("/provision/:addr/:port", h.triggerProvision)
	v1.Post("/targets/:addr/:port/notify-only", h.setNotifyOnly)
	v1.Get("/countries", h.countries)
	v1.Get("/providers", h.providers)
	v1.Post("/providers/refresh", h.refreshProviders)
	v1.Get("/panel/nodes", h.panelNodes)
	v1.Get("/notifications", h.notifications)
	v1.Get("/bot/actions", h.botActions)
	v1.Get("/settings", h.getSettings)
	v1.Post("/settings", h.postSettings)
	v1.Post("/settings/test/:target", h.testIntegration)
}

// countries shows how each country looks to the replacement logic.
//
// A swap refuses when no country is fully healthy from Iran, and that refusal
// is otherwise silent — there was no way to see which country fell short, or by
// how much, without triggering a replacement.
func (h *Handler) countries(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}

	locations := provision.ParseLocationMap(h.d.Settings.Get(settings.HetznerLocations))
	survey := provision.SurveyCountries(statuses, locations)

	out := make([]fiber.Map, 0, len(survey))
	eligible := 0
	for _, ch := range survey {
		if ch.Eligible() {
			eligible++
		}
		locs := ch.Locations
		if locs == nil {
			// An unmapped country must read as "no locations", not null.
			locs = []string{}
		}
		entry := fiber.Map{
			"country":   ch.Country,
			"healthy":   ch.Healthy,
			"total":     ch.Total,
			"locations": locs,
			"eligible":  ch.Eligible(),
			"reason":    countryReason(ch),
		}
		if !ch.LastChecked.IsZero() {
			entry["last_checked"] = ch.LastChecked
		}
		out = append(out, entry)
	}
	return c.JSON(fiber.Map{"countries": out, "eligible": eligible})
}

// countryReason says in one phrase why a country cannot host a replacement.
func countryReason(ch provision.CountryHealth) string {
	switch {
	case ch.Total == 0:
		return "no addresses"
	case len(ch.Locations) == 0:
		return "no Hetzner location mapped"
	case ch.Healthy < ch.Total:
		return fmt.Sprintf("%d of %d addresses not fully reachable from Iran",
			ch.Total-ch.Healthy, ch.Total)
	default:
		return ""
	}
}

func (h *Handler) watchlist(c *fiber.Ctx) error {
	if h.d.Watcher == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the watch loop is not running")
	}
	entries, err := h.d.Watcher.Watchlist()
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"watching": entries,
		"last_run": h.d.Watcher.LastRun(),
	})
}

func (h *Handler) outages(c *fiber.Ctx) error {
	days := queryInt(c, "days", 7, 1, 365)
	limit := queryInt(c, "limit", 200, 1, 2000)
	rows, err := h.st.Outages(time.Now().AddDate(0, 0, -days), limit)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"days": days, "outages": rows})
}

func (h *Handler) provisions(c *fiber.Ctx) error {
	limit := queryInt(c, "limit", 50, 1, 500)
	rows, err := h.st.Provisions(limit)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"provisions": rows})
}

func (h *Handler) provision(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "provision id must be a number")
	}
	p, err := h.st.GetProvision(id)
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "no such provision")
	}
	if err != nil {
		return err
	}
	return c.JSON(p)
}

// triggerProvision runs the reaction by hand, for testing. Every guard still
// applies, so this cannot be used to bypass the dry-run or the daily cap.
func (h *Handler) triggerProvision(c *fiber.Ctx) error {
	if h.d.Provision == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "provisioning is not wired up")
	}
	address := c.Params("addr")
	if _, err := strconv.Atoi(c.Params("port")); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "port must be a number")
	}

	// The port in the route is kept for compatibility but no longer selects
	// anything: a replacement takes the whole machine, so the whole machine is
	// what gets triggered.
	machine, err := h.st.AddressFor(address)
	if errors.Is(err, store.ErrNoSuchAddress) {
		return fiber.NewError(fiber.StatusNotFound, "no such endpoint")
	}
	if err != nil {
		return err
	}

	count, err := h.st.OutageCountForAddress(address, time.Now().Add(-h.cfg.OutageWindow))
	if err != nil {
		return err
	}

	go func(a store.AddressStatus, n int) {
		_ = h.d.Provision.Trigger(h.bg, a, n)
	}(machine, count)

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"triggered": machine.Address, "ports": machine.PortNumbers(), "outages": count,
	})
}

// setNotifyOnly marks an endpoint hands-off: alert, but never replace it.
func (h *Handler) setNotifyOnly(c *fiber.Ctx) error {
	addr := c.Params("addr")
	port, err := strconv.Atoi(c.Params("port"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "port must be a number")
	}

	targetID, err := h.st.TargetByHostPort(addr, port)
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "no such endpoint")
	}
	if err != nil {
		return err
	}

	var body struct {
		NotifyOnly *bool `json:"notify_only"`
	}
	if err := c.BodyParser(&body); err != nil || body.NotifyOnly == nil {
		return fiber.NewError(fiber.StatusBadRequest, `expected {"notify_only": true|false}`)
	}
	if err := h.st.SetNotifyOnly(targetID, *body.NotifyOnly); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"address": addr, "port": port, "notify_only": *body.NotifyOnly})
}

// botActions exposes the Telegram audit trail, including rejected attempts.
func (h *Handler) botActions(c *fiber.Ctx) error {
	limit := queryInt(c, "limit", 50, 1, 500)
	rows, err := h.st.BotActions(limit)
	if err != nil {
		return err
	}

	out := fiber.Map{"actions": rows}
	if h.d.Bot != nil {
		out["enabled"] = h.d.Bot.Enabled()
		out["running"] = h.d.Bot.Running()
		out["last_update"] = h.d.Bot.LastUpdate()
		out["allowed_ids"] = h.d.Bot.AllowedIDs()
	}
	if n, err := h.st.UnauthorizedBotAttempts(time.Now().AddDate(0, 0, -7)); err == nil {
		out["unauthorized_last_7d"] = n
	}
	return c.JSON(out)
}

func (h *Handler) providers(c *fiber.Ctx) error {
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}

	out := make([]fiber.Map, 0, len(statuses))
	counts := map[string]int{}
	for _, s := range statuses {
		provider, kind, serverName, serverID, abuse, err := h.st.TargetProvider(s.TargetID)
		if err != nil {
			return err
		}
		if provider == "" {
			provider = string(hetzner.OwnedUnknown)
		}
		counts[provider]++
		out = append(out, fiber.Map{
			"address": s.Address, "port": s.Port, "verdict": s.Verdict,
			"provider": provider, "hz_kind": kind,
			"hz_server_id": serverID, "hz_server_name": serverName,
			"hz_abuse_blocked": abuse,
			"notify_only":      s.NotifyOnly,
			"replaceable":      provider == string(hetzner.OwnedHetzner) && !s.NotifyOnly,
		})
	}
	return c.JSON(fiber.Map{"targets": out, "counts": counts})
}

func (h *Handler) panelNodes(c *fiber.Ctx) error {
	if h.d.Panel == nil || !h.d.Panel.Configured() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the 3x-ui panel is not configured")
	}
	nodes, err := h.d.Panel.Nodes(c.Context())
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(fiber.Map{"nodes": nodes})
}

func (h *Handler) notifications(c *fiber.Ctx) error {
	limit := queryInt(c, "limit", 50, 1, 500)
	rows, err := h.st.Notifications(limit)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"notifications": rows})
}

// ---- settings ----

func (h *Handler) getSettings(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}
	// Views never carry a secret's value, only whether it is set.
	return c.JSON(fiber.Map{"settings": h.d.Settings.Views()})
}

type settingsWrite struct {
	Set   map[string]string `json:"set"`
	Clear []string          `json:"clear"`
}

func (h *Handler) postSettings(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}

	var body settingsWrite
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "expected a JSON body with set/clear")
	}

	for key, value := range body.Set {
		// An empty value means "leave it alone"; clearing is explicit, so a
		// blank form field can never wipe a stored token by accident.
		if strings.TrimSpace(value) == "" {
			continue
		}
		if err := h.d.Settings.Set(key, strings.TrimSpace(value)); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}
	for _, key := range body.Clear {
		if err := h.d.Settings.Clear(key); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}
	return c.JSON(fiber.Map{"settings": h.d.Settings.Views()})
}

// testIntegration verifies one set of credentials without changing anything.
func (h *Handler) testIntegration(c *fiber.Ctx) error {
	ctx, cancel := contextWithTimeout(c, 20*time.Second)
	defer cancel()

	switch c.Params("target") {
	case settings.GroupTelegram:
		if h.d.Telegram == nil || !h.d.Telegram.Configured() {
			return fiber.NewError(fiber.StatusBadRequest, "set a bot token and chat id first")
		}
		if err := h.d.Telegram.Send(ctx, "✅ botchecker test message — alerts are working."); err != nil {
			return fiber.NewError(fiber.StatusBadGateway, err.Error())
		}
		return c.JSON(fiber.Map{"ok": true, "detail": "test message sent"})

	case settings.GroupXUI:
		if h.d.Panel == nil || !h.d.Panel.Configured() {
			return fiber.NewError(fiber.StatusBadRequest, "set the panel URL and API token first")
		}
		n, err := h.d.Panel.Ping(ctx)
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, err.Error())
		}
		return c.JSON(fiber.Map{"ok": true, "detail": strconv.Itoa(n) + " node(s) visible"})

	case settings.GroupHetzner:
		if h.d.Hetzner == nil || !h.d.Hetzner.Configured() {
			return fiber.NewError(fiber.StatusBadRequest, "set a Hetzner token first")
		}
		// Every project is reported, working or not. A token that silently
		// stopped working is exactly what this button exists to surface, and a
		// single aggregate would hide it behind the projects that still answer.
		invs, errs := h.d.Hetzner.InventoryAll(ctx)
		var lines []string
		for _, p := range h.d.Hetzner.Projects() {
			switch {
			case errs[p.Slug] != nil:
				lines = append(lines, fmt.Sprintf("%s: %v", p.Slug, errs[p.Slug]))
			case invs[p.Slug] != nil:
				inv := invs[p.Slug]
				detail := fmt.Sprintf("%s: %d server(s), %d address(es)",
					p.Slug, inv.Servers, inv.Len())
				if ok, why := p.Usable(); !ok {
					detail += " — cannot build: " + why
				}
				lines = append(lines, detail)
			}
		}
		if len(lines) == 0 {
			return fiber.NewError(fiber.StatusBadGateway, "no project could be read")
		}
		return c.JSON(fiber.Map{
			"ok":       len(errs) == 0,
			"detail":   strings.Join(lines, " · "),
			"projects": len(h.d.Hetzner.Projects()),
		})
	case settings.GroupZex:
		if h.d.Zex == nil || !h.d.Zex.Configured() {
			return fiber.NewError(fiber.StatusBadRequest, "set the panel URL, admin email and password first")
		}
		// A login is the whole question: the admin routes accept the JWT it
		// returns, so anything that logs in can also swap addresses.
		if err := h.d.Zex.Ping(ctx); err != nil {
			return fiber.NewError(fiber.StatusBadGateway, err.Error())
		}
		return c.JSON(fiber.Map{"ok": true, "detail": "signed in to the panel"})
	}
	return fiber.NewError(fiber.StatusBadRequest, "unknown target: use telegram, xui, hetzner or zex")
}

// refreshProviders re-reads the Hetzner inventory and re-tags every known
// endpoint. Ownership otherwise only updates during a full scan, which takes
// minutes — far too slow after simply swapping a token.
func (h *Handler) refreshProviders(c *fiber.Ctx) error {
	if h.d.Hetzner == nil || !h.d.Hetzner.Configured() {
		return fiber.NewError(fiber.StatusBadRequest, "no Hetzner project is configured")
	}

	ctx, cancel := contextWithTimeout(c, 60*time.Second)
	defer cancel()

	h.d.Hetzner.InvalidateAll()
	invs, errs := h.d.Hetzner.InventoryAll(ctx)

	// Nothing is written when any project could not be read. A partial view
	// would mark every address in the unreachable project as external, and an
	// external address is one this service will never replace — so a momentary
	// API failure would quietly switch off automatic replacement for a whole
	// account. Retrying is cheap; recovering from that is not.
	if len(errs) > 0 {
		names := make([]string, 0, len(errs))
		for slug, err := range errs {
			names = append(names, fmt.Sprintf("%s: %v", slug, err))
		}
		sort.Strings(names)
		return fiber.NewError(fiber.StatusBadGateway,
			"nothing was changed — could not read "+strings.Join(names, "; "))
	}

	machines, err := h.st.AddressStatuses()
	if err != nil {
		return err
	}
	addresses := make([]string, 0, len(machines))
	for _, m := range machines {
		addresses = append(addresses, m.Address)
	}

	assignments, conflicts := hetzner.Reconcile(addresses, invs, errs)

	now := time.Now()
	counts := map[string]int{}
	byProject := map[string]int{}
	for _, a := range assignments {
		counts[string(a.Ownership)]++
		if a.Project != "" {
			byProject[a.Project]++
		}
		if err := h.st.SetAddressProvider(a.Address, string(a.Ownership), a.Project,
			a.Resource.Kind, a.Resource.ServerID, a.Resource.ServerName,
			a.Resource.AbuseBlocked, now); err != nil {
			return err
		}
	}

	out := fiber.Map{
		"refreshed": len(assignments),
		"counts":    counts,
		"projects":  byProject,
	}
	if len(conflicts) > 0 {
		out["conflicts"] = conflicts
	}
	return c.JSON(out)
}
