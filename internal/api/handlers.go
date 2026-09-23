package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/inventory"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/provision"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/watch"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

// Deps is everything the API reads from. The phase-two fields may be nil in
// tests that only exercise the scanning endpoints.
type Deps struct {
	Config    *config.Config
	Store     *store.Store
	Scanner   *scanner.Scanner
	Settings  *settings.Provider
	Watcher   *watch.Watcher
	Provision *provision.Manager
	Hetzner   *hetzner.Registry
	Panel     *xui.Client
	Zex       *zex.Client
	Telegram  *notify.Telegram
	Bot       BotStatus
	// Inventory re-reads the endpoint list from the panel on demand.
	Inventory *inventory.Syncer
}

type Handler struct {
	cfg *config.Config
	st  *store.Store
	sc  *scanner.Scanner
	d   Deps
	// bg outlives the HTTP request that starts a scan, so a scan is not
	// cancelled the moment the client disconnects.
	bg context.Context
}

func NewHandler(bg context.Context, d Deps) *Handler {
	return &Handler{cfg: d.Config, st: d.Store, sc: d.Scanner, d: d, bg: bg}
}

// Register mounts the JSON API. Auth is applied by the caller.
func (h *Handler) Register(app *fiber.App) {
	app.Get("/healthz", h.health)

	v1 := app.Group("/api/v1")
	v1.Post("/scan", h.startScan)
	v1.Post("/inventory/update", h.updateInventory)
	v1.Get("/inventory", h.inventoryState)
	v1.Get("/scan/latest", h.latestScan)
	v1.Get("/scan/:id", h.getScan)
	v1.Get("/scans", h.listScans)
	v1.Get("/blocked", h.blocked)
	v1.Get("/targets", h.targets)
	v1.Get("/targets/:addr/:port", h.targetDetail)
	v1.Get("/stats/timeline", h.statsTimeline)
	v1.Get("/stats/lifetime", h.statsLifetime)
	v1.Get("/stats/asn", h.statsASN)

	h.registerPhase2(v1)
}

func (h *Handler) health(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok", "scanning": h.sc.Running()})
}

func (h *Handler) startScan(c *fiber.Ctx) error {
	id, err := h.sc.Start(h.bg, scanner.TriggerManual)
	if errors.Is(err, scanner.ErrBusy) {
		p := h.sc.Progress()
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":    "a scan is already in progress",
			"progress": p,
		})
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"scan_id": id})
}

func (h *Handler) getScan(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "scan id must be a number")
	}

	scan, err := h.st.GetScan(id)
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "no such scan")
	}
	if err != nil {
		return err
	}

	out := fiber.Map{"scan": scan}
	if p := h.sc.Progress(); p.ScanID == id {
		out["progress"] = p
	}
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}
	out["results"] = statuses
	return c.JSON(out)
}

func (h *Handler) latestScan(c *fiber.Ctx) error {
	scan, err := h.st.LatestCompletedScan()
	if err != nil {
		return err
	}
	if scan == nil {
		return fiber.NewError(fiber.StatusNotFound, "no completed scan yet")
	}
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"scan": scan, "results": statuses})
}

func (h *Handler) listScans(c *fiber.Ctx) error {
	limit := queryInt(c, "limit", 50, 1, 500)
	scans, err := h.st.ListScans(limit)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"scans": scans})
}

// blocked is the primary output: the endpoints that need replacing, with the
// config ids that point at them.
func (h *Handler) blocked(c *fiber.Ctx) error {
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}

	out := []fiber.Map{}
	for _, s := range statuses {
		if s.Verdict != string(scanner.VerdictBlockedIR) {
			continue
		}
		// Ownership decides whether anything can be done automatically, so it
		// travels with every blocked endpoint rather than only on its own page.
		provider, _, serverName, _, abuse, err := h.st.TargetProvider(s.TargetID)
		if err != nil {
			return err
		}
		if provider == "" {
			provider = string(hetzner.OwnedUnknown)
		}

		entry := fiber.Map{
			"address":          s.Address,
			"port":             s.Port,
			"host_port":        s.HostPort(),
			"config_ids":       s.ConfigIDs,
			"names":            s.Names,
			"country_code":     s.CountryCode,
			"protocol":         s.Protocol,
			"ir_timeout":       s.IRTimeout,
			"ir_total":         s.IRTotal,
			"control_open":     s.ControlOpen,
			"confirmed":        s.Confirmed,
			"checked_at":       s.CheckedAt,
			"blocked_since":    s.SinceAt,
			"provider":         provider,
			"hz_server":        serverName,
			"hz_abuse_blocked": abuse,
			"notify_only":      s.NotifyOnly,
			"replaceable":      provider == string(hetzner.OwnedHetzner) && !s.NotifyOnly,
		}
		if tr, err := h.st.LatestTraceroute(s.TargetID); err == nil && tr != nil {
			entry["traceroute"] = fiber.Map{
				"node":             checkhost.ShortName(tr.Node),
				"last_hop":         tr.LastHop,
				"last_hop_private": tr.LastHopPrivate,
				"dead_hops":        tr.DeadHops,
			}
		}
		out = append(out, entry)
	}
	return c.JSON(fiber.Map{"blocked": out, "count": len(out)})
}

func (h *Handler) targets(c *fiber.Ctx) error {
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"targets": statuses})
}

func (h *Handler) targetDetail(c *fiber.Ctx) error {
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

	limit := queryInt(c, "limit", 20, 1, 200)
	results, err := h.st.ResultsForTarget(targetID, limit)
	if err != nil {
		return err
	}
	events, err := h.st.AllEvents()
	if err != nil {
		return err
	}

	var mine []store.Event
	for _, e := range events {
		if e.TargetID == targetID {
			mine = append(mine, e)
		}
	}

	// Hops are stored encoded; decode them so the response is one document.
	type detail struct {
		store.ResultDetail
		Hops map[string][]checkhost.Hop `json:"hops,omitempty"`
	}
	out := make([]detail, 0, len(results))
	for _, r := range results {
		d := detail{ResultDetail: r}
		for _, tr := range r.Traceroutes {
			var hops []checkhost.Hop
			if err := json.Unmarshal([]byte(tr.HopsJSON), &hops); err == nil {
				if d.Hops == nil {
					d.Hops = map[string][]checkhost.Hop{}
				}
				d.Hops[checkhost.ShortName(tr.Node)] = hops
			}
		}
		out = append(out, d)
	}

	return c.JSON(fiber.Map{
		"address": addr, "port": port,
		"events": mine, "results": out,
	})
}

func (h *Handler) statsTimeline(c *fiber.Ctx) error {
	days := queryInt(c, "days", 30, 1, 365)
	events, err := h.st.AllEvents()
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"days":     days,
		"timeline": store.BuildTimeline(events, days, time.Now(), h.cfg.Location),
	})
}

func (h *Handler) statsLifetime(c *fiber.Ctx) error {
	events, err := h.st.AllEvents()
	if err != nil {
		return err
	}
	return c.JSON(store.BuildLifetimes(events, time.Now()))
}

func (h *Handler) statsASN(c *fiber.Ctx) error {
	days := queryInt(c, "days", 30, 1, 365)
	since := time.Now().AddDate(0, 0, -days)
	stats, err := h.st.ASNBreakdown(since, h.cfg.IRNodes)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"days": days, "asn": stats})
}

func queryInt(c *fiber.Ctx, name string, def, min, max int) int {
	v, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
