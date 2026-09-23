package settings

// Kind decides how a value is rendered, validated and protected.
type Kind string

const (
	KindString   Kind = "string"
	KindSecret   Kind = "secret"
	KindBool     Kind = "bool"
	KindInt      Kind = "int"
	KindDuration Kind = "duration"
	// KindChoice is a value picked from a fixed list; see Choices.
	KindChoice Kind = "choice"
)

// Def describes one setting that can be changed from the dashboard.
type Def struct {
	Key   string
	Env   string
	Kind  Kind
	Group string
	Label string
	Help  string
}

// Secret reports whether the value must never be sent back to a client.
func (d Def) Secret() bool { return d.Kind == KindSecret }

// Groups are rendered as separate forms on the settings page.
const (
	GroupGeneral   = "general"
	GroupTelegram  = "telegram"
	GroupXUI       = "xui"
	GroupHetzner   = "hetzner"
	GroupZex       = "zex"
	GroupDetection = "detection"
	GroupProvision = "provision"
)

// Keys every setting is addressed by.
const (
	UILanguage = "ui.language"

	TelegramBotToken  = "telegram.bot_token"
	TelegramChatID    = "telegram.chat_id"
	TelegramBotEnable = "telegram.bot_enabled"
	TelegramAllowedID = "telegram.allowed_ids"

	XUIBaseURL     = "xui.base_url"
	XUIAPIToken    = "xui.api_token"
	XUIInsecureTLS = "xui.insecure_tls"

	HetznerToken      = "hetzner.token"
	HetznerTokens     = "hetzner.tokens"
	HetznerProjects   = "hetzner.projects"
	HetznerSnapshotID = "hetzner.snapshot_id"
	HetznerServerType = "hetzner.server_type"
	HetznerLocation   = "hetzner.location"
	HetznerSSHKeys    = "hetzner.ssh_keys"
	HetznerNamePrefix = "hetzner.name_prefix"
	HetznerLocations  = "hetzner.location_map"
	// HetznerSSHUser and HetznerSSHPrivateKey let the service configure a
	// floating IP inside a server. Without them the same commands are sent to
	// Telegram for a person to run.
	HetznerSSHUser       = "hetzner.ssh_user"
	HetznerSSHPrivateKey = "hetzner.ssh_private_key"

	ZexBaseURL       = "zex.base_url"
	ZexAdminEmail    = "zex.admin_email"
	ZexAdminPassword = "zex.admin_password"

	WatchInterval = "watch.interval"
	WatchScope    = "watch.scope"
	ExtraTargets  = "scan.extra_targets"
	// InventorySyncInterval is how often the endpoint list is re-read from the
	// panel. Separate from the scan interval because reading the list is one
	// call and probing it is dozens.
	InventorySyncInterval  = "inventory.sync_interval"
	ProvisionLocations     = "provision.locations"
	ProvisionFullBlockOnly = "provision.full_block_only"
	// ProvisionIPAttempts caps how many reserved addresses are looked at before
	// giving up on a location — the pool can hand back a burned one more than
	// once, but not forever.
	ProvisionIPAttempts = "provision.ip_attempts"
	// ProvisionFloatOnFailure allows the rescue path: when no project can build,
	// give the blocked machine a new floating address instead.
	ProvisionFloatOnFailure = "provision.float_on_build_failure"
	OutageThreshold         = "outage.threshold"
	OutageWindow            = "outage.window"

	ProvisionEnabled     = "provision.enabled"
	ProvisionDryRun      = "provision.dry_run"
	ProvisionCooldown    = "provision.cooldown"
	ProvisionMaxPerDay   = "provision.max_per_day"
	ProvisionQuietWindow = "provision.quiet_window"
)

// Defs is the registry. Order is the render order on the settings page.
var Defs = []Def{
	{UILanguage, "UI_LANGUAGE", KindChoice, GroupGeneral, "Interface language",
		"Applies to this dashboard and to the Telegram bot."},

	{TelegramBotToken, "TELEGRAM_BOT_TOKEN", KindSecret, GroupTelegram, "Bot token",
		"From @BotFather. Alerts are not sent without it."},
	{TelegramChatID, "TELEGRAM_CHAT_ID", KindString, GroupTelegram, "Chat ID",
		"Numeric chat or channel id the alerts go to."},
	{TelegramBotEnable, "TELEGRAM_BOT_ENABLED", KindBool, GroupTelegram, "Accept commands",
		"Off by default: the bot only sends alerts. Turning this on lets it receive button presses, so the allow list below decides who can control this service."},
	{TelegramAllowedID, "TELEGRAM_ALLOWED_IDS", KindString, GroupTelegram, "Allowed Telegram IDs",
		"Comma-separated numeric user ids that may command the bot. Empty falls back to the chat id above. Anyone else is ignored without a reply."},

	{XUIBaseURL, "XUI_BASE_URL", KindString, GroupXUI, "Panel URL",
		"Base URL of the 3x-ui panel, e.g. https://panel.example.com:2053. Optional — without it, alerts omit the online user count."},
	{XUIAPIToken, "XUI_API_TOKEN", KindSecret, GroupXUI, "API token",
		"Panel → Settings → Security → API Token."},
	{XUIInsecureTLS, "XUI_INSECURE_TLS", KindBool, GroupXUI, "Skip TLS verification",
		"Only for a panel using a self-signed certificate."},

	{HetznerToken, "HETZNER_TOKEN", KindSecret, GroupHetzner, "API token",
		"Read/write token for the project the servers live in. Also used to tell which addresses are yours."},
	{HetznerTokens, "HETZNER_TOKENS", KindSecret, GroupHetzner, "Tokens for every project",
		"One Hetzner project per token, as name=token, comma separated — for example main=AbC…, spare=XyZ…. A token only ever sees its own project, so an address in a project listed here is recognised as yours, and a server is always deleted with the token of the project that holds it. Leave empty to use the single token above."},
	{HetznerProjects, "HETZNER_PROJECTS", KindString, GroupHetzner, "Resources per project",
		"What each project can build with, since snapshots, SSH keys and server types only exist inside one project. Format: name|snapshot=…|type=cpx32|types=ash:cpx31|locations=ash,hel1|max=5, projects separated by ;. A project left out here inherits the single-project settings below."},
	{HetznerSnapshotID, "HETZNER_SNAPSHOT_ID", KindString, GroupHetzner, "Snapshot ID",
		"Image new servers are created from."},
	{HetznerServerType, "HETZNER_SERVER_TYPE", KindString, GroupHetzner, "Server type", "e.g. cpx11"},
	{HetznerLocation, "HETZNER_LOCATION", KindString, GroupHetzner, "Location", "e.g. hel1, ash, nbg1"},
	{HetznerSSHKeys, "HETZNER_SSH_KEYS", KindString, GroupHetzner, "SSH keys",
		"Comma-separated key names or ids to inject."},
	{HetznerNamePrefix, "HETZNER_NAME_PREFIX", KindString, GroupHetzner, "Name prefix",
		"New servers are named <prefix><timestamp>."},
	{HetznerSSHUser, "HETZNER_SSH_USER", KindString, GroupHetzner, "SSH user",
		"Account used to configure a floating IP inside a server. Usually root."},
	{HetznerSSHPrivateKey, "HETZNER_SSH_PRIVATE_KEY", KindSecret, GroupHetzner, "SSH private key",
		"PEM private key for that account. Only used to add a floating IP to a server's interface. Left empty, the commands are sent to Telegram to run by hand instead."},
	{HetznerLocations, "HETZNER_LOCATION_MAP", KindString, GroupHetzner, "Country to location",
		"Which Hetzner locations sit in which country, e.g. FI:hel1,DE:fsn1,US:ash. A replacement is created in a country whose addresses are all healthy from Iran."},

	{ZexBaseURL, "ZEX_BASE_URL", KindString, GroupZex, "Panel URL",
		"The admin panel that hands configs to the app, e.g. http://37.27.203.234:8080."},
	{ZexAdminEmail, "ZEX_ADMIN_EMAIL", KindString, GroupZex, "Admin email",
		"An account with SUPER_ADMIN or ADMIN role."},
	{ZexAdminPassword, "ZEX_ADMIN_PASSWORD", KindSecret, GroupZex, "Admin password",
		"Used to log in and obtain a session token, which is cached until it expires."},

	{WatchInterval, "WATCH_INTERVAL", KindDuration, GroupDetection, "Re-probe interval",
		"How often a suspect address is probed again. Only blocked addresses are watched."},
	{InventorySyncInterval, "INVENTORY_SYNC_INTERVAL", KindDuration, GroupDetection, "Read the panel every",
		"How often the list of servers is re-read from the panel, so new ones appear and deleted ones stop being watched. One request of about 2 MB for a production-sized panel. New servers are probed as soon as they are found."},
	{OutageThreshold, "OUTAGE_THRESHOLD", KindInt, GroupDetection, "Outages before alerting",
		"How many blocked readings inside the window trigger the alert."},
	{OutageWindow, "OUTAGE_WINDOW", KindDuration, GroupDetection, "Window",
		"The sliding window the outages are counted in."},
	{ExtraTargets, "EXTRA_TARGETS", KindString, GroupDetection, "Extra endpoints to watch",
		"Endpoints probed even though splash does not return them, comma separated as address:port or address:port:CC. Splash only hands out active configs, so a server whose configs are switched off is invisible here otherwise."},
	{WatchScope, "WATCH_SCOPE", KindChoice, GroupDetection, "What to re-probe",
		"all: every endpoint, so a healthy address that gets filtered is caught within one interval. blocked: only the already-blocked ones, which is cheaper but leaves new blocks to the next full scan."},

	{ProvisionLocations, "PROVISION_LOCATIONS", KindString, GroupProvision, "Where to build, in order",
		"Hetzner locations tried in order until one produces an address that answers from Iran, e.g. ash,hil,nbg1,hel1,sin. A location whose server comes back filtered is deleted and the next one is tried."},
	{ProvisionFullBlockOnly, "PROVISION_FULL_BLOCK_ONLY", KindBool, GroupProvision, "Only replace fully blocked addresses",
		"On: an address is only replaced when no Iranian probe can reach it at all. Off: a partial block is enough."},
	{ProvisionIPAttempts, "PROVISION_IP_ATTEMPTS", KindInt, GroupProvision, "Address attempts per location",
		"How many addresses to reserve and look at before giving up on a location. Hetzner returns a deleted server's IP to the pool, so a replacement can be handed the blocked address straight back; each one that is already in the ledger is released and another asked for."},
	{ProvisionFloatOnFailure, "PROVISION_FLOAT_ON_BUILD_FAILURE", KindBool, GroupProvision, "Rescue with a floating IP",
		"When no project can build — every one at its server limit — give the blocked machine a new floating address instead of leaving its configs off. Needs the SSH key above, or a person to run one command."},
	{ProvisionEnabled, "PROVISION_ENABLED", KindBool, GroupProvision, "Create servers automatically",
		"Off by default. Turning this on lets the service spend money on your Hetzner account."},
	{ProvisionDryRun, "PROVISION_DRY_RUN", KindBool, GroupProvision, "Dry run",
		"Everything runs except the server creation call itself. Leave on until you trust the alerts."},
	{ProvisionCooldown, "PROVISION_COOLDOWN", KindDuration, GroupProvision, "Cooldown per address",
		"An address cannot trigger provisioning again within this period."},
	{ProvisionQuietWindow, "PROVISION_QUIET_WINDOW", KindDuration, GroupProvision, "Quiet window",
		"How long a broken address's configs stay switched off before the replacement is swapped in. A floor, not an extra delay: building usually takes longer."},
	{ProvisionMaxPerDay, "PROVISION_MAX_PER_DAY", KindInt, GroupProvision, "Daily limit",
		"Hard cap on servers created in any 24 hours."},
}

var defByKey = func() map[string]Def {
	m := make(map[string]Def, len(Defs))
	for _, d := range Defs {
		m[d.Key] = d
	}
	return m
}()

// Lookup returns the definition for a key.
func Lookup(key string) (Def, bool) {
	d, ok := defByKey[key]
	return d, ok
}

// GroupsInOrder lists the groups as they should be rendered.
var GroupsInOrder = []string{GroupGeneral, GroupTelegram, GroupXUI, GroupZex, GroupHetzner, GroupDetection, GroupProvision}

// Choices lists the allowed values for a KindChoice setting.
var Choices = map[string][]string{
	UILanguage: {"en", "fa"},
	WatchScope: {"all", "blocked"},
}
