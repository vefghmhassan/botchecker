PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- Runtime configuration editable from the dashboard. Secret values are stored
-- encrypted so a copy of this file does not hand over the API tokens.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      BLOB,
    secret     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS scans (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at    TEXT    NOT NULL,
    finished_at   TEXT,
    status        TEXT    NOT NULL,              -- running | completed | failed
    trigger       TEXT    NOT NULL,              -- manual | scheduled
    config_count  INTEGER NOT NULL DEFAULT 0,
    target_count  INTEGER NOT NULL DEFAULT 0,
    blocked_count INTEGER NOT NULL DEFAULT 0,
    error         TEXT
);
CREATE INDEX IF NOT EXISTS idx_scans_started ON scans(started_at DESC);

CREATE TABLE IF NOT EXISTS targets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    address         TEXT    NOT NULL,
    port            INTEGER NOT NULL,
    country_code    TEXT,
    protocol        TEXT,
    config_ids_json TEXT,
    names_json      TEXT,
    ads             INTEGER NOT NULL DEFAULT 0,
    active          INTEGER NOT NULL DEFAULT 1,
    first_seen      TEXT    NOT NULL,
    last_seen       TEXT    NOT NULL,
    UNIQUE(address, port)
);

CREATE TABLE IF NOT EXISTS results (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_id        INTEGER NOT NULL REFERENCES scans(id)   ON DELETE CASCADE,
    target_id      INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    verdict        TEXT    NOT NULL,
    ir_open        INTEGER NOT NULL DEFAULT 0,
    ir_timeout     INTEGER NOT NULL DEFAULT 0,
    ir_refused     INTEGER NOT NULL DEFAULT 0,
    ir_total       INTEGER NOT NULL DEFAULT 0,
    control_open   INTEGER NOT NULL DEFAULT 0,
    control_total  INTEGER NOT NULL DEFAULT 0,
    confirmed      INTEGER NOT NULL DEFAULT 0,
    permanent_link TEXT,
    checked_at     TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_results_target  ON results(target_id, checked_at DESC);
CREATE INDEX IF NOT EXISTS idx_results_scan    ON results(scan_id);
CREATE INDEX IF NOT EXISTS idx_results_checked ON results(checked_at);

CREATE TABLE IF NOT EXISTS node_results (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    result_id    INTEGER NOT NULL REFERENCES results(id) ON DELETE CASCADE,
    node         TEXT    NOT NULL,
    country_code TEXT,
    city         TEXT,
    asn          TEXT,
    outcome      TEXT    NOT NULL,
    rtt_ms       REAL,
    detail       TEXT
);
CREATE INDEX IF NOT EXISTS idx_node_results_result ON node_results(result_id);
CREATE INDEX IF NOT EXISTS idx_node_results_asn    ON node_results(asn, outcome);

CREATE TABLE IF NOT EXISTS traceroutes (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    result_id        INTEGER NOT NULL REFERENCES results(id) ON DELETE CASCADE,
    node             TEXT    NOT NULL,
    last_hop         TEXT,
    last_hop_private INTEGER NOT NULL DEFAULT 0,
    dead_hops        INTEGER NOT NULL DEFAULT 0,
    hops_json        TEXT,
    created_at       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_traceroutes_result ON traceroutes(result_id);

-- Verdict transitions. Kept separate from results (and never pruned) so that
-- "which addresses were blocked this month" and per-address lifetime are a
-- small indexed lookup rather than a scan of every probe ever taken.
CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id    INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    from_verdict TEXT,
    to_verdict   TEXT NOT NULL,
    changed_at   TEXT NOT NULL,
    scan_id      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_events_target  ON events(target_id, changed_at);
CREATE INDEX IF NOT EXISTS idx_events_changed ON events(changed_at);

-- Individual blocked readings from the fast watch loop. Three of these inside
-- the window are what triggers an alert.
CREATE TABLE IF NOT EXISTS outages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id   INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    detected_at TEXT    NOT NULL,
    result_id   INTEGER,
    scan_id     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_outages_target ON outages(target_id, detected_at);

CREATE TABLE IF NOT EXISTS provisions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id         INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    triggered_at      TEXT    NOT NULL,
    finished_at       TEXT,
    status            TEXT    NOT NULL,
    provider          TEXT,
    dry_run           INTEGER NOT NULL DEFAULT 0,
    outage_count      INTEGER NOT NULL DEFAULT 0,
    online_count      INTEGER,
    hetzner_server_id INTEGER,
    hetzner_action_id INTEGER,
    snapshot_id       TEXT,
    new_address       TEXT,
    new_port          INTEGER,
    verify_verdict    TEXT,
    verified_at       TEXT,
    handoff_status    TEXT,
    error             TEXT
);
CREATE INDEX IF NOT EXISTS idx_provisions_target ON provisions(target_id, triggered_at DESC);
CREATE INDEX IF NOT EXISTS idx_provisions_time   ON provisions(triggered_at DESC);

CREATE TABLE IF NOT EXISTS notifications (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL,
    target_id    INTEGER,
    provision_id INTEGER,
    sent_at      TEXT    NOT NULL,
    ok           INTEGER NOT NULL DEFAULT 0,
    error        TEXT,
    message      TEXT
);
CREATE INDEX IF NOT EXISTS idx_notifications_time ON notifications(sent_at DESC);

-- Every Telegram update the bot saw, including ones from unauthorized ids, so
-- that a stranger finding the bot is visible rather than silent.
CREATE TABLE IF NOT EXISTS bot_actions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    telegram_id  INTEGER NOT NULL,
    username     TEXT,
    authorized   INTEGER NOT NULL DEFAULT 0,
    action       TEXT    NOT NULL,
    target_id    INTEGER,
    detail       TEXT,
    requested_at TEXT    NOT NULL,
    executed_at  TEXT,
    ok           INTEGER,
    error        TEXT
);
CREATE INDEX IF NOT EXISTS idx_bot_actions_time ON bot_actions(requested_at DESC);

-- Addresses that are known dead: a machine we handed back, a fresh build whose
-- address came back filtered, or one the operator deleted by hand.
--
-- Hetzner returns the IP of a deleted server to its location's pool, so the
-- next server built there can be handed the same address back. Without a record
-- of what was thrown away, that address is bought again, booted again, and
-- probed from Iran for minutes before being thrown away again — which is
-- exactly what happened three times on 2026-09-21, twice with the same IP.
--
-- A released row is kept rather than deleted so the history of what was burned,
-- and when it was let back in, survives.
CREATE TABLE IF NOT EXISTS address_ledger (
    address      TEXT PRIMARY KEY,
    reason       TEXT    NOT NULL,   -- retired | discarded | deleted_by_operator | verify_failed | manual
    verdict      TEXT,               -- the Iranian reading that condemned it, when there was one
    project      TEXT,
    server_id    INTEGER,
    provision_id INTEGER,
    note         TEXT,
    recorded_at  TEXT    NOT NULL,
    released_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_address_ledger_recorded ON address_ledger(recorded_at DESC);
