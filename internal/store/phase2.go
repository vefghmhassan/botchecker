package store

import (
	"database/sql"
	"time"
)

// ---- provider ownership ----

// SetTargetProvider records which provider an address belongs to. It is
// refreshed on every full scan so that a server moved between projects is
// reclassified.
// SetAddressProvider records ownership for every port of one machine.
//
// Ownership is a property of the address — the Hetzner inventory is keyed by
// address and knows nothing about ports — so writing it per port meant the same
// answer stored twice and, worse, two ports of one machine able to disagree
// about which project owns it.
func (s *Store) SetAddressProvider(address, provider, project, kind string,
	serverID int64, serverName string, abuseBlocked bool, at time.Time) error {
	var sid any
	if serverID != 0 {
		sid = serverID
	}
	_, err := s.db.Exec(`
		UPDATE targets
		SET provider = ?, hz_project = ?, hz_kind = ?, hz_server_id = ?,
		    hz_server_name = ?, hz_abuse_blocked = ?, provider_checked_at = ?
		WHERE address = ?`,
		provider, nullIfEmpty(project), nullIfEmpty(kind), sid, nullIfEmpty(serverName),
		boolToInt(abuseBlocked), ts(at), address)
	return err
}

// AddressProject returns which Hetzner project holds a machine, if it is known.
func (s *Store) AddressProject(address string) (string, error) {
	var project sql.NullString
	err := s.db.QueryRow(
		`SELECT hz_project FROM targets WHERE address = ? AND hz_project IS NOT NULL LIMIT 1`,
		address).Scan(&project)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return project.String, nil
}

func (s *Store) SetTargetProvider(targetID int64, provider, kind string, serverID int64, serverName string, abuseBlocked bool, at time.Time) error {
	var sid any
	if serverID != 0 {
		sid = serverID
	}
	_, err := s.db.Exec(`
		UPDATE targets SET provider = ?, hz_kind = ?, hz_server_id = ?, hz_server_name = ?,
		                   hz_abuse_blocked = ?, provider_checked_at = ?
		WHERE id = ?`,
		provider, nullIfEmpty(kind), sid, nullIfEmpty(serverName), boolToInt(abuseBlocked), ts(at), targetID)
	return err
}

// TargetProvider reads the stored ownership of one endpoint.
func (s *Store) TargetProvider(targetID int64) (provider, kind, serverName string, serverID int64, abuseBlocked bool, err error) {
	var p, k, n sql.NullString
	var id sql.NullInt64
	var abuse int
	err = s.db.QueryRow(`
		SELECT provider, hz_kind, hz_server_name, hz_server_id, hz_abuse_blocked
		FROM targets WHERE id = ?`, targetID).Scan(&p, &k, &n, &id, &abuse)
	return p.String, k.String, n.String, id.Int64, abuse == 1, err
}

// SetNotifyOnly marks an endpoint as hands-off. It keeps being probed and
// keeps alerting, but no replacement server will ever be created for it.
func (s *Store) SetNotifyOnly(targetID int64, on bool) error {
	_, err := s.db.Exec(`UPDATE targets SET notify_only = ? WHERE id = ?`, boolToInt(on), targetID)
	return err
}

// NotifyOnly reports whether an endpoint is marked hands-off.
func (s *Store) NotifyOnly(targetID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COALESCE(notify_only,0) FROM targets WHERE id = ?`, targetID).Scan(&n)
	return n == 1, err
}

// ---- outages ----

// RecordOutage appends one blocked reading from the watch loop.
func (s *Store) RecordOutage(targetID int64, resultID, scanID int64, at time.Time) error {
	var rid, sid any
	if resultID != 0 {
		rid = resultID
	}
	if scanID != 0 {
		sid = scanID
	}
	_, err := s.db.Exec(
		`INSERT INTO outages (target_id, detected_at, result_id, scan_id) VALUES (?, ?, ?, ?)`,
		targetID, ts(at), rid, sid)
	return err
}

// OutageCount counts blocked readings inside the sliding window.
// OutageCountForAddress counts outages across every port of one machine, which
// is the unit the threshold is judged in.
func (s *Store) OutageCountForAddress(address string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM outages o
		JOIN targets t ON t.id = o.target_id
		WHERE t.address = ? AND o.detected_at >= ?`,
		address, ts(since)).Scan(&n)
	return n, err
}

// ClearOutagesForAddress resets the counter for every port of one machine, so a
// replacement does not leave a sibling port primed to fire immediately after.
func (s *Store) ClearOutagesForAddress(address string) error {
	_, err := s.db.Exec(`
		DELETE FROM outages WHERE target_id IN (SELECT id FROM targets WHERE address = ?)`,
		address)
	return err
}

func (s *Store) OutageCount(targetID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM outages WHERE target_id = ? AND detected_at >= ?`,
		targetID, ts(since)).Scan(&n)
	return n, err
}

// ClearOutages drops an endpoint's history once it works again, so a slow
// drip of unrelated failures over days cannot add up to a trigger.
func (s *Store) ClearOutages(targetID int64) error {
	_, err := s.db.Exec(`DELETE FROM outages WHERE target_id = ?`, targetID)
	return err
}

// OutageRow is one recorded outage with its endpoint.
type OutageRow struct {
	ID         int64     `json:"id"`
	TargetID   int64     `json:"target_id"`
	Address    string    `json:"address"`
	Port       int       `json:"port"`
	DetectedAt time.Time `json:"detected_at"`
}

// Outages lists recent outages, newest first.
func (s *Store) Outages(since time.Time, limit int) ([]OutageRow, error) {
	rows, err := s.db.Query(`
		SELECT o.id, o.target_id, t.address, t.port, o.detected_at
		FROM outages o JOIN targets t ON t.id = o.target_id
		WHERE o.detected_at >= ?
		ORDER BY o.detected_at DESC LIMIT ?`, ts(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutageRow
	for rows.Next() {
		var o OutageRow
		var at string
		if err := rows.Scan(&o.ID, &o.TargetID, &o.Address, &o.Port, &at); err != nil {
			return nil, err
		}
		o.DetectedAt = parseTS(at)
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---- provisions ----

// Provision statuses.
const (
	ProvTriggered     = "triggered"
	ProvExternal      = "external_provider"
	ProvSkipped       = "skipped"
	ProvNotifyOnly    = "notify_only"
	ProvDryRun        = "dry_run"
	ProvCreating      = "creating"
	ProvBooting       = "booting"
	ProvVerifying     = "verifying"
	ProvQuieted       = "quieted"
	ProvCountryChosen = "country_chosen"
	ProvSwapped       = "swapped"
	ProvVerified      = "verified"
	ProvVerifyFail    = "verify_failed"
	ProvFailed        = "failed"
	// ProvBurned is a machine whose address is in the ledger: it was retired,
	// discarded or deleted on purpose, so asking for it again is a loop.
	ProvBurned = "burned_address"

	HandoffPending = "awaiting_manual_handoff"
	HandoffDone    = "done"
)

// Provision is one reaction to an endpoint crossing the outage threshold.
type Provision struct {
	ID              int64      `json:"id"`
	TargetID        int64      `json:"target_id"`
	Address         string     `json:"address"`
	Port            int        `json:"port"`
	TriggeredAt     time.Time  `json:"triggered_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Status          string     `json:"status"`
	Provider        string     `json:"provider,omitempty"`
	DryRun          bool       `json:"dry_run"`
	OutageCount     int        `json:"outage_count"`
	OnlineCount     *int       `json:"online_count,omitempty"`
	HetznerServerID int64      `json:"hetzner_server_id,omitempty"`
	HetznerActionID int64      `json:"hetzner_action_id,omitempty"`
	SnapshotID      string     `json:"snapshot_id,omitempty"`
	NewAddress      string     `json:"new_address,omitempty"`
	NewPort         int        `json:"new_port,omitempty"`
	VerifyVerdict   string     `json:"verify_verdict,omitempty"`
	VerifiedAt      *time.Time `json:"verified_at,omitempty"`
	HandoffStatus   string     `json:"handoff_status,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// CreateProvision opens a provisioning record.
func (s *Store) CreateProvision(targetID int64, provider string, dryRun bool, outageCount int, onlineCount *int, at time.Time) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO provisions (target_id, triggered_at, status, provider, dry_run, outage_count, online_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		targetID, ts(at), ProvTriggered, provider, boolToInt(dryRun), outageCount, onlineCount)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetProvisionStatus advances the record, optionally finalising it.
func (s *Store) SetProvisionStatus(id int64, status string, finished bool, provErr error, at time.Time) error {
	var errText, finishedAt any
	if provErr != nil {
		errText = provErr.Error()
	}
	if finished {
		finishedAt = ts(at)
	}
	_, err := s.db.Exec(
		`UPDATE provisions SET status = ?, error = COALESCE(?, error), finished_at = COALESCE(?, finished_at) WHERE id = ?`,
		status, errText, finishedAt, id)
	return err
}

// SetProvisionServer records the created server once Hetzner accepted it.
func (s *Store) SetProvisionServer(id int64, serverID, actionID int64, snapshotID, address string, port int) error {
	_, err := s.db.Exec(`
		UPDATE provisions SET hetzner_server_id = ?, hetzner_action_id = ?, snapshot_id = ?,
		                      new_address = ?, new_port = ? WHERE id = ?`,
		serverID, actionID, snapshotID, address, port, id)
	return err
}

// SetProvisionVerdict records how the new address behaved from Iran.
func (s *Store) SetProvisionVerdict(id int64, verdict string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE provisions SET verify_verdict = ?, verified_at = ? WHERE id = ?`,
		verdict, ts(at), id)
	return err
}

// SetProvisionHandoff records what happened at the (intentionally empty) hook.
func (s *Store) SetProvisionHandoff(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE provisions SET handoff_status = ? WHERE id = ?`, status, id)
	return err
}

const provisionCols = `p.id, p.target_id, t.address, t.port, p.triggered_at, p.finished_at, p.status,
	COALESCE(p.provider,''), p.dry_run, p.outage_count, p.online_count,
	p.hetzner_server_id, p.hetzner_action_id, COALESCE(p.snapshot_id,''),
	COALESCE(p.new_address,''), p.new_port, COALESCE(p.verify_verdict,''), p.verified_at,
	COALESCE(p.handoff_status,''), COALESCE(p.error,'')`

func scanProvision(sc interface{ Scan(...any) error }) (Provision, error) {
	var p Provision
	var triggered string
	var finished, verified sql.NullString
	var online, serverID, actionID, newPort sql.NullInt64
	var dryRun int

	err := sc.Scan(&p.ID, &p.TargetID, &p.Address, &p.Port, &triggered, &finished, &p.Status,
		&p.Provider, &dryRun, &p.OutageCount, &online,
		&serverID, &actionID, &p.SnapshotID,
		&p.NewAddress, &newPort, &p.VerifyVerdict, &verified,
		&p.HandoffStatus, &p.Error)
	if err != nil {
		return p, err
	}

	p.TriggeredAt = parseTS(triggered)
	p.DryRun = dryRun == 1
	if finished.Valid {
		t := parseTS(finished.String)
		p.FinishedAt = &t
	}
	if verified.Valid {
		t := parseTS(verified.String)
		p.VerifiedAt = &t
	}
	if online.Valid {
		n := int(online.Int64)
		p.OnlineCount = &n
	}
	p.HetznerServerID, p.HetznerActionID, p.NewPort = serverID.Int64, actionID.Int64, int(newPort.Int64)
	return p, nil
}

// Provisions lists provisioning history, newest first.
func (s *Store) Provisions(limit int) ([]Provision, error) {
	rows, err := s.db.Query(`SELECT `+provisionCols+`
		FROM provisions p JOIN targets t ON t.id = p.target_id
		ORDER BY p.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Provision
	for rows.Next() {
		p, err := scanProvision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProvisionsForTarget lists the history of one endpoint.
func (s *Store) ProvisionsForTarget(targetID int64, limit int) ([]Provision, error) {
	rows, err := s.db.Query(`SELECT `+provisionCols+`
		FROM provisions p JOIN targets t ON t.id = p.target_id
		WHERE p.target_id = ? ORDER BY p.id DESC LIMIT ?`, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Provision
	for rows.Next() {
		p, err := scanProvision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvision fetches one record.
func (s *Store) GetProvision(id int64) (*Provision, error) {
	row := s.db.QueryRow(`SELECT `+provisionCols+`
		FROM provisions p JOIN targets t ON t.id = p.target_id WHERE p.id = ?`, id)
	p, err := scanProvision(row)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// LastProvisionAt returns when this machine last triggered a real reaction, for
// the cooldown. Skipped records are excluded so a quiet period cannot keep
// extending itself.
//
// The join to targets is what makes the cooldown cover the whole machine. Keyed
// on target id alone it only ever suppressed the one port that fired, so the
// sibling port — which a per-IP block takes down at the same moment — sailed
// past a cooldown it had no history in and started a second replacement against
// an address the first one had already abandoned.
func (s *Store) LastProvisionAt(address string) (*time.Time, error) {
	var at sql.NullString
	err := s.db.QueryRow(`
		SELECT MAX(p.triggered_at) FROM provisions p
		JOIN targets t ON t.id = p.target_id
		WHERE t.address = ? AND p.status != ?`,
		address, ProvSkipped).Scan(&at)
	if err != nil || !at.Valid {
		return nil, nil
	}
	t := parseTS(at.String)
	return &t, nil
}

// ServersCreatedSince counts provisions that actually created a server, for
// the daily cap. Dry runs and external-provider records do not count.
func (s *Store) ServersCreatedSince(since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM provisions
		WHERE triggered_at >= ? AND dry_run = 0 AND hetzner_server_id IS NOT NULL`,
		ts(since)).Scan(&n)
	return n, err
}

// ---- notifications ----

// RecordNotification stores the outcome of one alert delivery.
func (s *Store) RecordNotification(kind string, targetID, provisionID int64, ok bool, notifyErr error, message string, at time.Time) error {
	var tid, pid, errText any
	if targetID != 0 {
		tid = targetID
	}
	if provisionID != 0 {
		pid = provisionID
	}
	if notifyErr != nil {
		errText = notifyErr.Error()
	}
	_, err := s.db.Exec(`
		INSERT INTO notifications (kind, target_id, provision_id, sent_at, ok, error, message)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		kind, tid, pid, ts(at), boolToInt(ok), errText, message)
	return err
}

// Notification is one recorded alert.
type Notification struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	TargetID    int64     `json:"target_id,omitempty"`
	ProvisionID int64     `json:"provision_id,omitempty"`
	SentAt      time.Time `json:"sent_at"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Message     string    `json:"message,omitempty"`
}

// Notifications lists recent alerts, newest first.
func (s *Store) Notifications(limit int) ([]Notification, error) {
	rows, err := s.db.Query(`
		SELECT id, kind, COALESCE(target_id,0), COALESCE(provision_id,0), sent_at, ok,
		       COALESCE(error,''), COALESCE(message,'')
		FROM notifications ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Notification
	for rows.Next() {
		var n Notification
		var at string
		var ok int
		if err := rows.Scan(&n.ID, &n.Kind, &n.TargetID, &n.ProvisionID, &at, &ok, &n.Error, &n.Message); err != nil {
			return nil, err
		}
		n.OK = ok == 1
		n.SentAt = parseTS(at)
		out = append(out, n)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
