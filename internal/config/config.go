// Package config loads runtime settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// splash config API
	SplashBaseURL   string
	SplashClientKey string
	SplashDeviceID  string
	SplashTimeout   time.Duration

	// check-host.net
	CheckHostBaseURL  string
	IRNodes           []string
	ControlNodes      []string
	CheckHostRPSDelay time.Duration
	PollInterval      time.Duration
	ResultTimeout     time.Duration

	// extra endpoints probed on top of whatever splash returns
	ExtraTargets string

	// verdict logic
	MinIRFail     int
	ConfirmRounds int
	ConfirmDelay  time.Duration
	// ScanConcurrency is how many endpoints are probed at once. Nothing in a
	// probe is local work — it is all waiting on check-host — so one at a time
	// made a scan take as long as the sum of its parts.
	ScanConcurrency int

	// traceroute
	TracerouteEnabled bool
	TracerouteNodes   []string
	TracerouteTimeout time.Duration

	// scheduling and retention
	ScanInterval     time.Duration
	ScanInitialDelay time.Duration
	RetentionDays    int

	// dashboard
	DashboardUser     string
	DashboardPass     string
	DashboardRefresh  int
	DashboardTimezone string
	Location          *time.Location

	// service
	AppPort  string
	DBPath   string
	LogLevel string

	// secrets at rest
	EncryptionKey string
	SecretKeyPath string

	// watch loop (defaults; the panel can override them at runtime)
	WatchInterval   time.Duration
	OutageThreshold int
	OutageWindow    time.Duration

	// integrations
	TelegramTimeout     time.Duration
	XUITimeout          time.Duration
	HetznerTimeout      time.Duration
	HetznerInventoryTTL time.Duration

	// provisioning (defaults; the panel can override the first three)
	ProvisionCooldown      time.Duration
	ProvisionMaxPerDay     int
	ProvisionBootWait      time.Duration
	ProvisionVerifyRounds  int
	ProvisionVerifyDelay   time.Duration
	ProvisionActionTimeout time.Duration
	ProvisionQuietWindow   time.Duration

	// integrations
	ZexTimeout time.Duration
}

// nodeSuffix is appended to the short node names used in configuration.
// check-host.net addresses its probes as "<name>.node.check-host.net".
const nodeSuffix = ".node.check-host.net"

func Load() (*Config, error) {
	c := &Config{
		SplashBaseURL:   env("SPLASH_BASE_URL", "http://37.27.203.234:8080"),
		SplashClientKey: env("SPLASH_CLIENT_KEY", ""),
		SplashDeviceID:  env("SPLASH_DEVICE_ID", ""),
		ExtraTargets:    env("EXTRA_TARGETS", ""),

		CheckHostBaseURL: strings.TrimRight(env("CHECKHOST_BASE_URL", "https://check-host.net"), "/"),

		DashboardUser:     env("DASHBOARD_USER", "admin"),
		DashboardPass:     env("DASHBOARD_PASS", ""),
		DashboardTimezone: env("DASHBOARD_TIMEZONE", "Asia/Tehran"),

		EncryptionKey: env("ENCRYPTION_KEY", ""),

		AppPort:  env("APP_PORT", "8080"),
		DBPath:   env("DB_PATH", "./data/botchecker.db"),
		LogLevel: env("LOG_LEVEL", "info"),
	}

	var err error
	if c.SplashTimeout, err = envDuration("SPLASH_TIMEOUT", 15*time.Second); err != nil {
		return nil, err
	}
	if c.CheckHostRPSDelay, err = envDuration("CHECKHOST_RPS_DELAY", 400*time.Millisecond); err != nil {
		return nil, err
	}
	if c.PollInterval, err = envDuration("POLL_INTERVAL", 3*time.Second); err != nil {
		return nil, err
	}
	if c.ResultTimeout, err = envDuration("RESULT_TIMEOUT", 45*time.Second); err != nil {
		return nil, err
	}
	if c.ConfirmDelay, err = envDuration("CONFIRM_DELAY", 30*time.Second); err != nil {
		return nil, err
	}
	if c.TracerouteTimeout, err = envDuration("TRACEROUTE_TIMEOUT", 150*time.Second); err != nil {
		return nil, err
	}
	if c.ScanInterval, err = envDuration("SCAN_INTERVAL", 30*time.Minute); err != nil {
		return nil, err
	}
	if c.ScanInitialDelay, err = envDuration("SCAN_INITIAL_DELAY", 10*time.Second); err != nil {
		return nil, err
	}
	if c.WatchInterval, err = envDuration("WATCH_INTERVAL", 2*time.Minute); err != nil {
		return nil, err
	}
	if c.OutageWindow, err = envDuration("OUTAGE_WINDOW", 30*time.Minute); err != nil {
		return nil, err
	}
	if c.TelegramTimeout, err = envDuration("TELEGRAM_TIMEOUT", 10*time.Second); err != nil {
		return nil, err
	}
	if c.XUITimeout, err = envDuration("XUI_TIMEOUT", 10*time.Second); err != nil {
		return nil, err
	}
	if c.HetznerTimeout, err = envDuration("HETZNER_TIMEOUT", 30*time.Second); err != nil {
		return nil, err
	}
	if c.HetznerInventoryTTL, err = envDuration("HETZNER_INVENTORY_TTL", 10*time.Minute); err != nil {
		return nil, err
	}
	if c.ProvisionCooldown, err = envDuration("PROVISION_COOLDOWN", 20*time.Minute); err != nil {
		return nil, err
	}
	if c.ProvisionBootWait, err = envDuration("PROVISION_BOOT_WAIT", 90*time.Second); err != nil {
		return nil, err
	}
	if c.ProvisionVerifyDelay, err = envDuration("PROVISION_VERIFY_DELAY", 10*time.Second); err != nil {
		return nil, err
	}
	if c.ProvisionActionTimeout, err = envDuration("PROVISION_ACTION_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.ProvisionQuietWindow, err = envDuration("PROVISION_QUIET_WINDOW", 2*time.Minute); err != nil {
		return nil, err
	}
	if c.ZexTimeout, err = envDuration("ZEX_TIMEOUT", 20*time.Second); err != nil {
		return nil, err
	}

	if c.MinIRFail, err = envInt("MIN_IR_FAIL", 6); err != nil {
		return nil, err
	}
	if c.ScanConcurrency, err = envInt("SCAN_CONCURRENCY", 6); err != nil {
		return nil, err
	}
	if c.ConfirmRounds, err = envInt("CONFIRM_ROUNDS", 2); err != nil {
		return nil, err
	}
	if c.RetentionDays, err = envInt("RETENTION_DAYS", 90); err != nil {
		return nil, err
	}
	if c.DashboardRefresh, err = envInt("DASHBOARD_REFRESH", 60); err != nil {
		return nil, err
	}
	if c.OutageThreshold, err = envInt("OUTAGE_THRESHOLD", 3); err != nil {
		return nil, err
	}
	if c.ProvisionMaxPerDay, err = envInt("PROVISION_MAX_PER_DAY", 20); err != nil {
		return nil, err
	}
	if c.ProvisionVerifyRounds, err = envInt("PROVISION_VERIFY_ROUNDS", 3); err != nil {
		return nil, err
	}

	// The generated encryption key lives beside the database so that a volume
	// backup of one without the other is obviously incomplete.
	c.SecretKeyPath = env("SECRET_KEY_PATH", filepath.Join(filepath.Dir(c.DBPath), "secret.key"))

	c.TracerouteEnabled = envBool("TRACEROUTE_ENABLED", true)

	c.IRNodes = expandNodes(env("IR_NODES", "ir1,ir2,ir3,ir4,ir5,ir6,ir7,ir8"))
	// An explicitly empty CONTROL_NODES means "disabled", so it must not
	// fall back to the default the way an unset variable does.
	c.ControlNodes = expandNodes(envAllowEmpty("CONTROL_NODES", "de1,nl1"))
	c.TracerouteNodes = expandNodes(env("TRACEROUTE_NODES", "ir1"))

	loc, err := time.LoadLocation(c.DashboardTimezone)
	if err != nil {
		// Alpine images without tzdata fall back to UTC rather than failing to boot.
		loc = time.UTC
	}
	c.Location = loc

	return c, c.validate()
}

func (c *Config) validate() error {
	if c.DashboardPass == "" {
		return errors.New("DASHBOARD_PASS is required: refusing to start an unauthenticated dashboard")
	}
	if len(c.IRNodes) == 0 {
		return errors.New("IR_NODES must list at least one node")
	}
	if c.MinIRFail < 1 || c.MinIRFail > len(c.IRNodes) {
		return fmt.Errorf("MIN_IR_FAIL must be between 1 and %d (number of IR_NODES), got %d", len(c.IRNodes), c.MinIRFail)
	}
	if c.ScanConcurrency < 1 {
		return fmt.Errorf("SCAN_CONCURRENCY must be at least 1, got %d", c.ScanConcurrency)
	}
	if c.ConfirmRounds < 1 {
		return errors.New("CONFIRM_ROUNDS must be at least 1")
	}
	if c.RetentionDays < 1 {
		return errors.New("RETENTION_DAYS must be at least 1")
	}
	if c.PollInterval <= 0 || c.ResultTimeout <= 0 {
		return errors.New("POLL_INTERVAL and RESULT_TIMEOUT must be positive")
	}
	if c.OutageThreshold < 1 {
		return errors.New("OUTAGE_THRESHOLD must be at least 1")
	}
	if c.WatchInterval <= 0 || c.OutageWindow <= 0 {
		return errors.New("WATCH_INTERVAL and OUTAGE_WINDOW must be positive")
	}
	// Counting N outages inside a window shorter than N probe intervals can
	// never reach the threshold, so the alert would silently never fire.
	if need := time.Duration(c.OutageThreshold-1) * c.WatchInterval; need > c.OutageWindow {
		return fmt.Errorf("OUTAGE_WINDOW (%s) is too short to ever see %d outages %s apart; use at least %s",
			c.OutageWindow, c.OutageThreshold, c.WatchInterval, need)
	}
	if c.ProvisionMaxPerDay < 1 {
		return errors.New("PROVISION_MAX_PER_DAY must be at least 1")
	}
	if c.ProvisionVerifyRounds < 1 {
		return errors.New("PROVISION_VERIFY_ROUNDS must be at least 1")
	}
	return nil
}

// ControlEnabled reports whether any control node is configured. Without one,
// a failure from Iran cannot be told apart from a server that is simply down.
func (c *Config) ControlEnabled() bool { return len(c.ControlNodes) > 0 }

// AllNodes returns the nodes probed for every target.
func (c *Config) AllNodes() []string {
	out := make([]string, 0, len(c.IRNodes)+len(c.ControlNodes))
	out = append(out, c.IRNodes...)
	out = append(out, c.ControlNodes...)
	return out
}

// expandNodes turns "ir1, ir2" into fully qualified check-host node hostnames.
// Names that already contain a dot are passed through untouched.
func expandNodes(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !strings.Contains(name, ".") {
			name += nodeSuffix
		}
		out = append(out, name)
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// envAllowEmpty returns the value of key even when it is blank, so that
// setting a variable to nothing is a deliberate choice rather than a
// request for the default.
func envAllowEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return b
}
