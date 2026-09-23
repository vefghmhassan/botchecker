// Command server runs the botchecker service: it periodically fetches the
// VLESS config list, probes every unique endpoint from inside Iran through
// check-host.net, and serves the results as JSON and as a dashboard.
package main

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/vefgh/botchecker/internal/api"
	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/dashboard"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/inventory"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/provision"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/scheduler"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/telebot"
	"github.com/vefgh/botchecker/internal/watch"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}
	log := newLogger(cfg.LogLevel)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("could not open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// A scan interrupted by a restart would otherwise sit in "running" forever.
	if n, err := st.ReapInterruptedScans(time.Now()); err != nil {
		log.Warn("could not reap interrupted scans", "err", err)
	} else if n > 0 {
		log.Info("marked interrupted scans as failed", "count", n)
	}

	// The background context outlives individual requests so a scan started
	// by an HTTP call keeps running after the client disconnects, and stops
	// only on shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	set, err := settings.New(st.DB(), cfg.EncryptionKey, cfg.SecretKeyPath, settingDefaults(cfg))
	if err != nil {
		log.Error("could not open the settings store", "err", err)
		os.Exit(1)
	}

	sp := splash.NewClient(cfg.SplashBaseURL, cfg.SplashClientKey, cfg.SplashDeviceID, cfg.SplashTimeout)
	ch := checkhost.NewClient(cfg.CheckHostBaseURL, cfg.CheckHostRPSDelay, cfg.PollInterval, cfg.ResultTimeout)
	sc := scanner.New(cfg, sp, ch, st, log)

	tg := notify.NewTelegram(set, cfg.TelegramTimeout)
	panel := xui.New(set, cfg.XUITimeout)
	hz := hetzner.NewRegistry(set, log, cfg.HetznerTimeout, cfg.HetznerInventoryTTL)
	zexClient := zex.New(set, cfg.ZexTimeout)
	sc.SetProviderResolver(hetzner.NewMultiResolver(hz))
	// Read on every scan, so an endpoint can be added from the settings page
	// without a restart.
	extraTargets := func() []splash.Target {
		return splash.ParseExtraTargets(set.Get(settings.ExtraTargets))
	}
	sc.SetExtraTargets(extraTargets)

	// The list of endpoints comes from the panel's complete node list, not from
	// the splash sample the app gets on launch. Every scan used to receive
	// exactly 14 configs whatever the panel held, so servers added to it were
	// never seen and servers removed from it were watched forever.
	inv := inventory.New(sp, st, log)
	inv.SetExtraTargets(extraTargets)
	inv.SetProber(sc)
	inv.SetNotifier(tg)
	sc.SetTargetSource(func(ctx context.Context) ([]splash.Target, int, error) {
		res, err := inv.Sync(ctx)
		return res.Targets, res.Configs, err
	})

	prov := provision.New(cfg, set, st, ch, hz, panel, zexClient, tg, provision.NewNoopHandoff(log), log)
	watcher := watch.New(cfg, set, st, ch, prov, log)

	bot := telebot.New(telebot.Deps{
		Config: cfg, Settings: set, Store: st, Telegram: tg, Scanner: sc,
		Provision: prov, Panel: panel, Hetzner: hz,
		Zex: zexClient, Watcher: watcher, Inventory: inv, Log: log,
	})

	csrf := api.NewCSRF(csrfSecret(cfg), cfg.DashboardUser)

	app := fiber.New(fiber.Config{
		Views:                 dashboard.NewEngine(cfg.Location, func() i18n.Lang { return i18n.Parse(set.Get(settings.UILanguage)) }),
		DisableStartupMessage: true,
		ReadTimeout:           30 * time.Second,
		WriteTimeout:          60 * time.Second,
		AppName:               "botchecker",
	})
	app.Use(recover.New())
	app.Use(api.BasicAuth(cfg.DashboardUser, cfg.DashboardPass))
	app.Use(csrf.Middleware())

	deps := api.Deps{
		Config: cfg, Store: st, Scanner: sc, Settings: set,
		Watcher: watcher, Provision: prov, Hetzner: hz, Panel: panel, Zex: zexClient, Telegram: tg,
		Bot: bot, Inventory: inv,
	}
	api.NewHandler(ctx, deps).Register(app)
	dashboard.NewHandler(dashboard.Deps{
		Config: cfg, Store: st, Scanner: sc, Settings: set,
		Watcher: watcher, Hetzner: hz, Panel: panel, Telegram: tg, CSRF: csrf, Bot: bot,
		Inventory: inv,
	}).Register(app)

	scheduler.New(sc, st, log, cfg.ScanInterval, cfg.ScanInitialDelay, cfg.RetentionDays).Start(ctx)
	watcher.Start(ctx)
	bot.Start(ctx)
	// Read on every tick, so the interval can be changed from the settings page.
	go inv.Run(ctx, func() time.Duration {
		return set.Duration(settings.InventorySyncInterval, 5*time.Minute)
	})

	// A server no config points at is invisible to everything else here: the
	// config list is where addresses come from, so it is never probed, never
	// alerted on, and never replaced — while still being billed and still
	// holding a slot a replacement would need. This looks for those and says so.
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if err := prov.ReportOrphans(ctx); err != nil {
				log.Warn("orphan check failed", "err", err)
			}
			// The mirror image: a machine this service destroyed on purpose
			// whose configs are still being handed to users. It is never acted
			// on — a replacement for an address that no longer exists is the
			// loop the ledger was added to stop — so saying so is all there is.
			if err := prov.ReportBurned(ctx); err != nil {
				log.Warn("retired-address check failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	if !cfg.ControlEnabled() {
		log.Warn("control nodes are disabled",
			"impact", "a server that is simply down cannot be told apart from one blocked in Iran")
	}
	log.Info("botchecker listening",
		"addr", ":"+cfg.AppPort,
		"ir_nodes", len(cfg.IRNodes),
		"control_nodes", len(cfg.ControlNodes),
		"scan_interval", cfg.ScanInterval.String(),
		"watch_interval", set.Duration(settings.WatchInterval, cfg.WatchInterval).String(),
		"telegram", tg.Configured(),
		"telegram_commands", bot.Enabled(),
		"panel", panel.Configured(),
		"hetzner_projects", strings.Join(hz.Slugs(), ","),
		"zex_panel", zexClient.Configured(),
		"provisioning", set.Bool(settings.ProvisionEnabled),
		"dry_run", set.Bool(settings.ProvisionDryRun),
		"db", cfg.DBPath)

	if set.Bool(settings.ProvisionEnabled) && !set.Bool(settings.ProvisionDryRun) {
		log.Warn("automatic provisioning is live",
			"impact", "blocked addresses will create paid Hetzner servers",
			"daily_limit", set.Int(settings.ProvisionMaxPerDay, cfg.ProvisionMaxPerDay))
	}

	errCh := make(chan error, 1)
	go func() { errCh <- app.Listen(":" + cfg.AppPort) }()

	select {
	case err := <-errCh:
		if err != nil {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.ShutdownWithContext(shutdownCtx); err != nil {
			log.Error("shutdown error", "err", err)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

// settingDefaults seeds the settings provider with the values the service
// falls back to when neither the panel nor the environment supplies one.
func settingDefaults(cfg *config.Config) map[string]string {
	return map[string]string{
		settings.HetznerServerType: "cpx11",
		settings.HetznerLocation:   "hel1",
		settings.HetznerNamePrefix: "botchecker-",

		settings.WatchInterval:         cfg.WatchInterval.String(),
		settings.WatchScope:            "all",
		settings.InventorySyncInterval: "5m",
		settings.OutageThreshold:       strconv.Itoa(cfg.OutageThreshold),
		settings.OutageWindow:          cfg.OutageWindow.String(),

		settings.ProvisionEnabled: "false",
		settings.ProvisionDryRun:  "true",
		// A blocked machine means users are offline, so the pacing is set to
		// get them back rather than to be cautious. A replacement now hands the
		// old machine back, so the server count — and the bill — stays flat;
		// the daily cap was sized when a replacement added a server and there
		// was real money at stake in doing it often.
		settings.ProvisionCooldown:    cfg.ProvisionCooldown.String(),
		settings.ProvisionMaxPerDay:   strconv.Itoa(cfg.ProvisionMaxPerDay),
		settings.ProvisionQuietWindow: cfg.ProvisionQuietWindow.String(),
		settings.HetznerLocations:     "FI:hel1,DE:fsn1,DE:nbg1,US:ash,US:hil,SG:sin",
		// Measured on 2026-09-21: a fresh Ashburn address answered from all
		// eight Iranian networks while fresh Helsinki, Nuremberg and Singapore
		// addresses did not. The order is a starting point, not a law — the
		// verification after each build is what actually decides.
		settings.ProvisionLocations:     "ash,hil,nbg1,hel1,sin",
		settings.ProvisionFullBlockOnly: "true",
		settings.ZexBaseURL:             cfg.SplashBaseURL,
	}
}

// csrfSecret derives the form-signing key. It reuses the dashboard password so
// that a rotated password also invalidates outstanding form tokens.
func csrfSecret(cfg *config.Config) []byte {
	sum := sha256.Sum256([]byte("botchecker-csrf|" + cfg.DashboardUser + "|" + cfg.DashboardPass))
	return sum[:]
}
