package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"

	"github.com/vefgh/botchecker/internal/api"
	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/inventory"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/watch"
	"github.com/vefgh/botchecker/internal/xui"
)

//go:embed templates/*.html
var templateFS embed.FS

// tagPattern strips the HTML that Telegram messages carry, so the dashboard
// shows their text rather than markup.
var tagPattern = regexp.MustCompile(`<[^>]*>`)

// NewEngine builds the template engine. Everything the pages need is compiled
// in: no external stylesheet, script or font is fetched, so the dashboard
// renders correctly even when opened from a network that blocks CDNs.
// LangSource supplies the currently selected interface language. The template
// engine is built once, so the funcs read it per render rather than capturing.
type LangSource func() i18n.Lang

func NewEngine(loc *time.Location, lang LangSource) *html.Engine {
	sub, err := fs.Sub(templateFS, "templates")
	if err != nil {
		panic(err)
	}
	engine := html.NewFileSystem(http.FS(sub), ".html")

	engine.AddFunc("t", func(key string, args ...any) string { return i18n.T(lang(), key, args...) })
	engine.AddFunc("verdictName", func(v string) string { return i18n.T(lang(), "verdict."+v) })
	engine.AddFunc("providerName", func(p string) string { return i18n.T(lang(), "provider."+p) })
	// The stored token is shown when no translation exists, so a reason added
	// later is still readable rather than rendering as the key itself.
	engine.AddFunc("ledgerReason", func(r string) string {
		key := "ledger.reason." + r
		if s := i18n.T(lang(), key); s != key {
			return s
		}
		return r
	})
	engine.AddFunc("num", func(v any) string { return lang().Digits(fmt.Sprint(v)) })
	// ternary keeps a two-branch string choice inline in a template.
	engine.AddFunc("ternary", func(cond bool, yes, no string) string {
		if cond {
			return yes
		}
		return no
	})
	engine.AddFunc("choices", func(key string) []string { return settings.Choices[key] })
	// A language option shows its own name, not its code.
	engine.AddFunc("langName", func(code string) string { return i18n.Parse(code).Name() })
	engine.AddFunc("verdictColor", VerdictColor)
	engine.AddFunc("providerColor", ProviderColor)
	engine.AddFunc("provisionColor", ProvisionColor)
	// Alert bodies are Telegram HTML; the table shows a plain-text preview.
	engine.AddFunc("firstLine", func(s string) string {
		s = tagPattern.ReplaceAllString(s, "")
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				if len(line) > 120 {
					return line[:120] + "…"
				}
				return line
			}
		}
		return ""
	})
	engine.AddFunc("outcomeColor", outcomeColor)
	engine.AddFunc("shortNode", checkhost.ShortName)
	engine.AddFunc("needsConfirm", func(v string) bool {
		return scanner.NeedsConfirmation(scanner.Verdict(v))
	})
	engine.AddFunc("join", func(v []string, sep string) string { return strings.Join(v, sep) })
	engine.AddFunc("intJoin", func(v []int, sep string) string {
		parts := make([]string, len(v))
		for i, n := range v {
			parts[i] = strconv.Itoa(n)
		}
		return strings.Join(parts, sep)
	})
	engine.AddFunc("times", func(v []float64) string {
		parts := make([]string, len(v))
		for i, t := range v {
			parts[i] = fmt.Sprintf("%.2f", t)
		}
		return strings.Join(parts, "  ")
	})
	engine.AddFunc("fmtTime", func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "—"
		}
		return lang().Digits(t.In(loc).Format("2006-01-02 15:04"))
	})
	engine.AddFunc("names", func(n []string) string { return splash.SummariseNames(n, 3) })
	engine.AddFunc("fmtTimeVal", func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return lang().Digits(t.In(loc).Format("2006-01-02 15:04"))
	})
	engine.AddFunc("ago", func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "never"
		}
		return humanAgo(time.Since(*t))
	})
	return engine
}

func outcomeColor(outcome string) string {
	switch outcome {
	case string(checkhost.OutcomeOpen):
		return VerdictColor("HEALTHY")
	case string(checkhost.OutcomeTimeout):
		return VerdictColor("BLOCKED_IR")
	case string(checkhost.OutcomeRefused):
		return VerdictColor("PORT_CLOSED")
	case string(checkhost.OutcomePending):
		return VerdictColor("UNKNOWN")
	default:
		return VerdictColor("SERVER_DOWN")
	}
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// Deps is everything the dashboard reads from.
type Deps struct {
	Config    *config.Config
	Store     *store.Store
	Scanner   *scanner.Scanner
	Settings  *settings.Provider
	Watcher   *watch.Watcher
	Hetzner   *hetzner.Registry
	Panel     *xui.Client
	Telegram  *notify.Telegram
	CSRF      *api.CSRF
	Bot       api.BotStatus
	Inventory *inventory.Syncer
}

type Handler struct {
	cfg *config.Config
	st  *store.Store
	sc  *scanner.Scanner
	d   Deps
}

func NewHandler(d Deps) *Handler {
	return &Handler{cfg: d.Config, st: d.Store, sc: d.Scanner, d: d}
}

func (h *Handler) Register(app *fiber.App) {
	app.Get("/", h.overview)
	app.Post("/inventory/update", h.updateInventory)
	app.Get("/timeline", h.timeline)
	app.Get("/lifetime", h.lifetime)
	app.Get("/asn", h.asn)
	app.Get("/providers", h.providers)
	app.Get("/projects", h.projectsPage)
	app.Post("/projects", h.saveProjects)
	app.Post("/projects/identify", h.identifyProject)
	app.Post("/providers/refresh", h.refreshProviders)
	app.Get("/provisions", h.provisions)
	app.Post("/provisions/release/:addr", h.releaseAddress)
	app.Post("/language", h.setLanguage)
	app.Get("/bot", h.bot)
	app.Get("/settings", h.settingsPage)
	app.Post("/settings", h.saveSettings)
	app.Post("/settings/test", h.testSettings)
	app.Post("/settings/export", h.exportSettings)
	app.Post("/settings/import", h.importSettings)
	app.Get("/target/:addr/:port", h.target)
	app.Post("/target/:addr/:port/notify-only", h.toggleNotifyOnly)
}

// base carries the values the shared layout needs on every page.
// lang resolves the configured interface language.
func (h *Handler) lang() i18n.Lang {
	if h.d.Settings == nil {
		return i18n.EN
	}
	return i18n.Parse(h.d.Settings.Get(settings.UILanguage))
}

func (h *Handler) base(title, nav string) fiber.Map {
	control := "disabled"
	if h.cfg.ControlEnabled() {
		names := make([]string, 0, len(h.cfg.ControlNodes))
		for _, n := range h.cfg.ControlNodes {
			names = append(names, checkhost.ShortName(n))
		}
		control = strings.Join(names, ", ")
	}
	token := ""
	if h.d.CSRF != nil {
		token = h.d.CSRF.Token()
	}
	lang := h.lang()
	return fiber.Map{
		"CSRF":         token,
		"Lang":         lang.Code(),
		"Dir":          lang.Dir(),
		"Languages":    i18n.Langs,
		"Title":        title,
		"Nav":          nav,
		"Refresh":      h.cfg.DashboardRefresh,
		"Scanning":     h.sc.Running(),
		"Now":          time.Now().In(h.cfg.Location).Format("2006-01-02 15:04"),
		"IRNodeCount":  len(h.cfg.IRNodes),
		"ControlLabel": control,
		"TZ":           h.cfg.Location.String(),
		"Legend":       VerdictOrder,
	}
}

func (h *Handler) overview(c *fiber.Ctx) error {
	rows, providerCounts, err := h.providerRows()
	if err != nil {
		return err
	}
	lastScan, err := h.st.LatestCompletedScan()
	if err != nil {
		return err
	}

	// Endpoints the panel no longer hands out are shown apart and left out of
	// every count. Counted, a server deleted last week would still read as
	// "blocked" on the front page, and the numbers would never match the panel.
	var gone []ProviderRow
	live := rows[:0:0]
	for _, r := range rows {
		if r.InPanel() {
			live = append(live, r)
		} else {
			gone = append(gone, r)
		}
	}
	rows = live

	counts := map[string]int{}
	for _, v := range VerdictOrder {
		counts[v] = 0
	}
	blocked, externalBlocked := 0, 0
	for _, r := range rows {
		counts[r.Verdict]++
		if r.Verdict == string(scanner.VerdictBlockedIR) {
			blocked++
			if !r.Replaceable {
				externalBlocked++
			}
		}
	}

	// Outage counts make the watch loop's progress visible on the main page.
	outages := map[int64]int{}
	if h.d.Watcher != nil {
		if entries, err := h.d.Watcher.Watchlist(); err == nil {
			for _, e := range entries {
				outages[e.TargetID] = e.Outages
			}
		}
	}

	data := h.base("Status", "overview")
	data["Targets"] = rows
	data["Counts"] = counts
	data["Total"] = len(rows)
	data["Blocked"] = blocked
	data["ExternalBlocked"] = externalBlocked
	data["ProviderCounts"] = providerCounts
	data["Outages"] = outages
	data["PanelOn"] = h.d.Panel != nil && h.d.Panel.Configured()
	data["LastScan"] = lastScan
	data["Progress"] = h.sc.Progress()
	data["Gone"] = gone
	if h.d.Inventory != nil {
		last := h.d.Inventory.Last()
		data["Sync"] = last
		data["SyncOK"] = !last.At.IsZero() && last.Error == ""
	}
	data["Flash"] = c.Query("msg")
	data["FlashErr"] = c.Query("err")
	return c.Render("overview", data, "layout")
}

// updateInventory is the update button: re-read the panel now, probe whatever
// is new, and come back to the overview saying what changed.
func (h *Handler) updateInventory(c *fiber.Ctx) error {
	if h.d.Inventory == nil {
		return c.Redirect("/?err="+urlEscape("The inventory is not configured."), fiber.StatusSeeOther)
	}
	// Bounded, and detached from the request: the probe it starts must outlive
	// the redirect.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, scanID, err := h.d.Inventory.Update(ctx)
	if err != nil && !errors.Is(err, inventory.ErrProberBusy) {
		return c.Redirect("/?err="+urlEscape(i18n.T(h.lang(), "sync.failed", err.Error())), fiber.StatusSeeOther)
	}
	return c.Redirect("/?msg="+urlEscape(h.syncSummary(res, scanID)), fiber.StatusSeeOther)
}

// syncSummary says what an update found, in the reader's language.
func (h *Handler) syncSummary(res inventory.Result, scanID int64) string {
	l := h.lang()
	msg := i18n.T(l, "sync.done", res.Endpoints, res.Configs)
	switch {
	case res.Changed():
		msg += " " + i18n.T(l, "sync.changed", len(res.New), len(res.Returned), len(res.Gone))
	default:
		msg += " " + i18n.T(l, "sync.nochange")
	}
	if scanID != 0 {
		msg += " " + i18n.T(l, "sync.probing", len(res.Fresh()))
	}
	if !res.Complete {
		msg += " " + i18n.T(l, "sync.partial")
	}
	return msg
}

func (h *Handler) timeline(c *fiber.Ctx) error {
	days := queryInt(c, "days", 30, 1, 365)

	events, err := h.st.AllEvents()
	if err != nil {
		return err
	}
	buckets := store.BuildTimeline(events, days, time.Now(), h.cfg.Location)

	// Only show transitions inside the charted window, newest first.
	cutoff := time.Now().AddDate(0, 0, -days)
	var recent []store.Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].ChangedAt.After(cutoff) {
			recent = append(recent, events[i])
		}
	}

	data := h.base("Timeline", "timeline")
	data["Days"] = days
	data["Chart"] = StackedBarSVG(buckets)
	data["Events"] = recent
	return c.Render("timeline", data, "layout")
}

func (h *Handler) lifetime(c *fiber.Ctx) error {
	events, err := h.st.AllEvents()
	if err != nil {
		return err
	}
	summary := store.BuildLifetimes(events, time.Now())

	data := h.base("Lifetime", "lifetime")
	data["Summary"] = summary
	data["Chart"] = LifetimeBarsSVG(summary.Spans)
	return c.Render("lifetime", data, "layout")
}

func (h *Handler) asn(c *fiber.Ctx) error {
	days := queryInt(c, "days", 30, 1, 365)
	stats, err := h.st.ASNBreakdown(time.Now().AddDate(0, 0, -days), h.cfg.IRNodes)
	if err != nil {
		return err
	}

	data := h.base("Operators", "asn")
	data["Days"] = days
	data["Stats"] = stats
	data["Chart"] = PercentBarsSVG(stats)
	return c.Render("asn", data, "layout")
}

func (h *Handler) target(c *fiber.Ctx) error {
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

	results, err := h.st.ResultsForTarget(targetID, 5)
	if err != nil {
		return err
	}
	allEvents, err := h.st.AllEvents()
	if err != nil {
		return err
	}
	statuses, err := h.st.TargetStatuses()
	if err != nil {
		return err
	}

	var events []store.Event
	for i := len(allEvents) - 1; i >= 0; i-- {
		if allEvents[i].TargetID == targetID {
			events = append(events, allEvents[i])
		}
	}
	var status *store.TargetStatus
	for i := range statuses {
		if statuses[i].TargetID == targetID {
			status = &statuses[i]
			break
		}
	}

	data := h.base(addr, "")
	data["Address"] = addr
	data["Port"] = port
	data["Status"] = status
	data["Events"] = events
	data["Results"] = results

	if tr, err := h.st.LatestTraceroute(targetID); err == nil && tr != nil {
		var hops []checkhost.Hop
		if err := json.Unmarshal([]byte(tr.HopsJSON), &hops); err == nil {
			data["Hops"] = hops
		}
		data["Traceroute"] = tr
	}
	return c.Render("target", data, "layout")
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

// setLanguage switches the interface language for everyone, including the bot.
func (h *Handler) setLanguage(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return c.Redirect("/", fiber.StatusSeeOther)
	}
	if err := h.d.Settings.Set(settings.UILanguage, string(i18n.Parse(c.FormValue("lang")))); err != nil {
		return err
	}

	// Return to the page the switch was pressed on.
	back := c.Get(fiber.HeaderReferer)
	if back == "" || !strings.HasPrefix(back, "/") {
		back = "/"
	}
	return c.Redirect(back, fiber.StatusSeeOther)
}
