package dashboard_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/api"
	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/dashboard"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/xui"
)

const (
	testUser = "admin"
	testPass = "s3cret"
)

// seed builds a store holding one blocked endpoint and one healthy one, with
// history far enough back that the timeline and lifetime views have something
// to draw.
func seed(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "dash.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now()
	scanID, err := st.CreateScan("manual", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("create scan: %v", err)
	}

	blocked := splash.Target{
		Address: "5.161.158.200", Port: 443, CountryCode: "US", Protocol: "vless",
		ConfigIDs: []int{30378, 30425}, Names: []string{"ash-one-C-A-12", "ash-one-C-A-59"},
		Active: true,
	}
	healthy := splash.Target{
		Address: "65.109.216.204", Port: 443, CountryCode: "FI", Protocol: "vless",
		ConfigIDs: []int{26990}, Names: []string{"Helk--new29"}, Active: true,
	}

	blockedID, err := st.UpsertTarget(blocked, now)
	if err != nil {
		t.Fatalf("upsert blocked: %v", err)
	}
	healthyID, err := st.UpsertTarget(healthy, now)
	if err != nil {
		t.Fatalf("upsert healthy: %v", err)
	}

	// The blocked address worked for eight days before it went dark.
	insertEvent(t, st, blockedID, "", "HEALTHY", now.AddDate(0, 0, -10), scanID)
	insertEvent(t, st, blockedID, "HEALTHY", "BLOCKED_IR", now.AddDate(0, 0, -2), scanID)
	insertEvent(t, st, healthyID, "", "HEALTHY", now.AddDate(0, 0, -12), scanID)

	blockedResult, err := st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: blockedID, Verdict: "BLOCKED_IR",
		IRTimeout: 8, IRTotal: 8, ControlOpen: 2, ControlTotal: 2,
		Confirmed: true, PermanentLink: "https://example.test/r/1", CheckedAt: now,
	})
	if err != nil {
		t.Fatalf("insert blocked result: %v", err)
	}
	if _, err := st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: healthyID, Verdict: "HEALTHY",
		IROpen: 8, IRTotal: 8, ControlOpen: 2, ControlTotal: 2,
		Confirmed: true, CheckedAt: now,
	}); err != nil {
		t.Fatalf("insert healthy result: %v", err)
	}

	if err := st.InsertNodeResults(blockedResult, []checkhost.NodeResult{
		{NodeMeta: checkhost.NodeMeta{Node: "ir1.node.check-host.net", CountryCode: "ir", City: "Tehran", ASN: "AS47430"},
			Outcome: checkhost.OutcomeTimeout, Detail: "Connection timed out"},
		{NodeMeta: checkhost.NodeMeta{Node: "ir2.node.check-host.net", CountryCode: "ir", City: "Isfahan", ASN: "AS209279"},
			Outcome: checkhost.OutcomeTimeout, Detail: "Connection timed out"},
		{NodeMeta: checkhost.NodeMeta{Node: "de1.node.check-host.net", CountryCode: "de", City: "Frankfurt", ASN: "AS24940"},
			Outcome: checkhost.OutcomeOpen, RTTms: 100.2, Detail: "5.161.158.200"},
	}); err != nil {
		t.Fatalf("insert node results: %v", err)
	}

	if err := st.InsertTraceroute(blockedResult, checkhost.TracerouteResult{
		Node:    "ir1.node.check-host.net",
		LastHop: "10.233.65.174", LastHopPrivate: true, DeadHops: 9,
		Hops: []checkhost.Hop{
			{Index: 1, Host: "185.105.238.193", Times: []float64{0.16}, Responded: true},
			{Index: 2, Host: "10.233.65.174", Times: []float64{0.52, 210.88}, Responded: true},
			{Index: 3},
		},
	}, now); err != nil {
		t.Fatalf("insert traceroute: %v", err)
	}

	if err := st.FinishScan(scanID, store.ScanCompleted, 1, nil, now); err != nil {
		t.Fatalf("finish scan: %v", err)
	}

	cfg := &config.Config{
		IRNodes:          []string{"ir1.node.check-host.net", "ir2.node.check-host.net"},
		ControlNodes:     []string{"de1.node.check-host.net"},
		DashboardUser:    testUser,
		DashboardPass:    testPass,
		DashboardRefresh: 0,
		Location:         time.UTC,
		MinIRFail:        6,
	}
	return st, cfg
}

func insertEvent(t *testing.T, st *store.Store, targetID int64, from, to string, at time.Time, scanID int64) {
	t.Helper()
	var fromVal any
	if from != "" {
		fromVal = from
	}
	if _, err := st.DB().Exec(
		`INSERT INTO events (target_id, from_verdict, to_verdict, changed_at, scan_id) VALUES (?,?,?,?,?)`,
		targetID, fromVal, to, at.UTC().Format(time.RFC3339Nano), scanID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func newApp(t *testing.T) *fiber.App {
	t.Helper()
	st, cfg := seed(t)
	return appWithStore(t, st, cfg)
}

func appWithStore(t *testing.T, st *store.Store, cfg *config.Config) *fiber.App {
	t.Helper()

	sc := scanner.New(cfg,
		splash.NewClient("http://unused", "", "", time.Second),
		checkhost.NewClient("http://unused", 0, time.Second, time.Second),
		st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	set, err := settings.New(st.DB(), strings.Repeat("ab", 32), filepath.Join(t.TempDir(), "k"), nil)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	csrf := api.NewCSRF([]byte("test-secret"), cfg.DashboardUser)

	app := fiber.New(fiber.Config{Views: dashboard.NewEngine(cfg.Location, func() i18n.Lang { return i18n.Parse(set.Get(settings.UILanguage)) })})
	app.Use(api.BasicAuth(cfg.DashboardUser, cfg.DashboardPass))
	app.Use(csrf.Middleware())
	api.NewHandler(context.Background(), api.Deps{
		Config: cfg, Store: st, Scanner: sc, Settings: set,
		Telegram: notify.NewTelegram(set, time.Second),
		Panel:    xui.New(set, time.Second),
		Hetzner:  hetzner.NewRegistry(set, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Minute),
	}).Register(app)
	dashboard.NewHandler(dashboard.Deps{
		Config: cfg, Store: st, Scanner: sc, Settings: set, CSRF: csrf,
		Telegram: notify.NewTelegram(set, time.Second),
		Panel:    xui.New(set, time.Second),
		Hetzner:  hetzner.NewRegistry(set, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Minute),
	}).Register(app)
	return app
}

func get(t *testing.T, app *fiber.App, path string, auth bool) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth {
		req.SetBasicAuth(testUser, testPass)
	}
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestDashboardPagesRender(t *testing.T) {
	app := newApp(t)

	cases := []struct {
		path string
		want []string
	}{
		{"/", []string{"Current status", "5.161.158.200:443", "BLOCKED_IR", "65.109.216.204:443", "HEALTHY"}},
		{"/timeline", []string{"Last 30 days", "<svg", "HEALTHY", "BLOCKED_IR"}},
		{"/lifetime", []string{"Endpoint lifetime", "<svg", "average days"}},
		{"/asn", []string{"Iranian operators", "AS47430", "AS209279"}},
		{"/target/5.161.158.200/443", []string{"10.233.65.174", "never left the operator", "AS47430"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := get(t, app, tc.path, true)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, truncate(body))
			}
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page is missing %q", want)
				}
			}
		})
	}
}

// The dashboard must not depend on anything it cannot serve itself: a page
// that pulls a script or font from a CDN breaks when opened from a network
// that blocks it — which is the exact situation this tool exists to detect.
func TestDashboardFetchesNothingExternal(t *testing.T) {
	app := newApp(t)

	for _, path := range []string{"/", "/timeline", "/lifetime", "/asn", "/target/5.161.158.200/443"} {
		_, body := get(t, app, path, true)

		for _, marker := range []string{"<script", "cdn.", "googleapis", "unpkg", "jsdelivr", "cdnjs"} {
			if strings.Contains(strings.ToLower(body), marker) {
				t.Errorf("%s contains %q: the dashboard must be fully self-contained", path, marker)
			}
		}
		// The only external link allowed is the check-host report, which the
		// operator clicks deliberately.
		for _, tag := range []string{`<link rel="stylesheet"`, `src="http`} {
			if strings.Contains(body, tag) {
				t.Errorf("%s pulls an external asset (%s)", path, tag)
			}
		}
	}
}

func TestLifetimeShowsMeasuredRun(t *testing.T) {
	app := newApp(t)
	_, body := get(t, app, "/lifetime", true)

	// The seeded endpoint was healthy for eight days before being blocked.
	if !strings.Contains(body, "8.0") {
		t.Errorf("lifetime page does not show the 8-day run:\n%s", truncate(body))
	}
}

func TestAuthIsRequired(t *testing.T) {
	app := newApp(t)

	for _, path := range []string{"/", "/timeline", "/api/v1/targets", "/api/v1/blocked"} {
		resp, _ := get(t, app, path, false)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials = %d, want 401", path, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s did not send a WWW-Authenticate challenge", path)
		}
	}
}

func TestHealthzIsOpen(t *testing.T) {
	app := newApp(t)

	// Container health checks have no credentials.
	resp, body := get(t, app, "/healthz", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("healthz body = %s", body)
	}
}

func TestBlockedEndpointIsTheActionableOutput(t *testing.T) {
	app := newApp(t)
	resp, body := get(t, app, "/api/v1/blocked", true)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`"count":1`, `"5.161.158.200"`, `30378`, `30425`, `"last_hop":"10.233.65.174"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("blocked payload is missing %s:\n%s", want, truncate(body))
		}
	}
	if strings.Contains(body, "65.109.216.204") {
		t.Error("a healthy endpoint leaked into the blocked list")
	}
}

func truncate(s string) string {
	if len(s) > 900 {
		return s[:900] + "…"
	}
	return s
}

// The blocked payload has to carry ownership: it is what tells the operator
// whether anything can be replaced automatically. A silent regression here
// would leave every entry looking un-replaceable.
func TestBlockedPayloadCarriesProvider(t *testing.T) {
	app := newApp(t)
	_, body := get(t, app, "/api/v1/blocked", true)

	for _, want := range []string{`"provider"`, `"replaceable"`, `"notify_only"`} {
		if !strings.Contains(body, want) {
			t.Errorf("blocked payload is missing %s:\n%s", want, truncate(body))
		}
	}
	// No Hetzner token is configured in this fixture, so ownership is unknown
	// — and must not be reported as external.
	if !strings.Contains(body, `"provider":"unknown"`) {
		t.Errorf("provider should be unknown without a token:\n%s", truncate(body))
	}
}

// Switching to Persian must actually change the page, not just its dir
// attribute — a template with a hard-coded English string would slip through
// otherwise.
func TestPagesRenderInPersian(t *testing.T) {
	st, cfg := seed(t)
	app := appWithStore(t, st, cfg)

	setLang(t, app, "fa")

	for _, path := range []string{"/", "/timeline", "/lifetime", "/asn", "/providers", "/provisions", "/bot", "/settings"} {
		resp, body := get(t, app, path, true)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
			continue
		}
		if !strings.Contains(body, `dir="rtl"`) {
			t.Errorf("%s is not laid out right-to-left", path)
		}
		if !strings.Contains(body, `lang="fa"`) {
			t.Errorf("%s does not declare Persian", path)
		}
		// Every page carries the nav, so the translated nav proves the
		// language reached the templates.
		if !strings.Contains(body, "وضعیت") {
			t.Errorf("%s still renders the English navigation", path)
		}
	}
}

func TestEnglishIsTheDefault(t *testing.T) {
	app := newApp(t)
	_, body := get(t, app, "/", true)

	if !strings.Contains(body, `dir="ltr"`) || !strings.Contains(body, `lang="en"`) {
		t.Fatal("the default page is not English left-to-right")
	}
}

// The language switch is a form, so it is subject to CSRF like every other.
func TestLanguageSwitchNeedsTheToken(t *testing.T) {
	st, cfg := seed(t)
	app := appWithStore(t, st, cfg)

	req := httptest.NewRequest(http.MethodPost, "/language", strings.NewReader("lang=fa"))
	req.Header.Set("Content-Type", fiber.MIMEApplicationForm)
	req.SetBasicAuth(testUser, testPass)
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a form post without a token = %d, want 403", resp.StatusCode)
	}
}

// setLang flips the language through the real form, token and all.
func setLang(t *testing.T, app *fiber.App, lang string) {
	t.Helper()
	_, body := get(t, app, "/", true)

	token := ""
	if i := strings.Index(body, `name="csrf" value="`); i >= 0 {
		rest := body[i+len(`name="csrf" value="`):]
		token = rest[:strings.Index(rest, `"`)]
	}
	if token == "" {
		t.Fatal("no csrf token in the page")
	}

	req := httptest.NewRequest(http.MethodPost, "/language",
		strings.NewReader("csrf="+token+"&lang="+lang))
	req.Header.Set("Content-Type", fiber.MIMEApplicationForm)
	req.SetBasicAuth(testUser, testPass)
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("switch language: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("switch language = %d, want a redirect", resp.StatusCode)
	}
}
