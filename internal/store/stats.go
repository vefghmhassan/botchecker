package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// TargetStatus is one endpoint with its most recent result attached.
type TargetStatus struct {
	TargetID     int64    `json:"target_id"`
	Address      string   `json:"address"`
	Port         int      `json:"port"`
	CountryCode  string   `json:"country_code"`
	Protocol     string   `json:"protocol"`
	Names        []string `json:"names"`
	ConfigIDs    []int    `json:"config_ids"`
	Ads          bool     `json:"ads"`
	Active       bool     `json:"active"`
	Verdict      string   `json:"verdict"`
	IROpen       int      `json:"ir_open"`
	IRTimeout    int      `json:"ir_timeout"`
	IRRefused    int      `json:"ir_refused"`
	IRTotal      int      `json:"ir_total"`
	ControlOpen  int      `json:"control_open"`
	ControlTotal int      `json:"control_total"`
	Confirmed    bool     `json:"confirmed"`
	// NotifyOnly means the operator has asked for alerts about this endpoint
	// but does not want it replaced automatically.
	NotifyOnly    bool       `json:"notify_only"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	PermanentLink string     `json:"permanent_link,omitempty"`
	// SinceAt is when the endpoint entered its current verdict.
	SinceAt *time.Time `json:"since,omitempty"`
	// MissingSince is when the panel stopped handing this endpoint out, or nil
	// while it still does.
	MissingSince *time.Time `json:"missing_since,omitempty"`
}

// InPanel reports whether the panel currently lists this endpoint.
func (t TargetStatus) InPanel() bool { return t.MissingSince == nil }

func (t TargetStatus) HostPort() string { return t.Address + ":" + itoa(t.Port) }

// TargetStatuses returns every known endpoint with its latest result.
func (s *Store) TargetStatuses() ([]TargetStatus, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.address, t.port, COALESCE(t.country_code,''), COALESCE(t.protocol,''),
		       COALESCE(t.names_json,'[]'), COALESCE(t.config_ids_json,'[]'), t.ads, t.active,
		       COALESCE(t.notify_only,0),
		       COALESCE(r.verdict,''), COALESCE(r.ir_open,0), COALESCE(r.ir_timeout,0),
		       COALESCE(r.ir_refused,0), COALESCE(r.ir_total,0),
		       COALESCE(r.control_open,0), COALESCE(r.control_total,0),
		       COALESCE(r.confirmed,0), r.checked_at, COALESCE(r.permanent_link,''),
		       t.missing_since
		FROM targets t
		LEFT JOIN results r ON r.id = (
			SELECT id FROM results WHERE target_id = t.id ORDER BY checked_at DESC, id DESC LIMIT 1
		)
		ORDER BY t.address, t.port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TargetStatus
	for rows.Next() {
		var t TargetStatus
		var names, ids string
		var ads, active, confirmed, notifyOnly int
		var checked, missing sql.NullString
		if err := rows.Scan(&t.TargetID, &t.Address, &t.Port, &t.CountryCode, &t.Protocol,
			&names, &ids, &ads, &active, &notifyOnly,
			&t.Verdict, &t.IROpen, &t.IRTimeout, &t.IRRefused, &t.IRTotal,
			&t.ControlOpen, &t.ControlTotal, &confirmed, &checked, &t.PermanentLink,
			&missing); err != nil {
			return nil, err
		}
		if missing.Valid {
			m := parseTS(missing.String)
			t.MissingSince = &m
		}
		_ = json.Unmarshal([]byte(names), &t.Names)
		_ = json.Unmarshal([]byte(ids), &t.ConfigIDs)
		t.Ads, t.Active, t.Confirmed = ads == 1, active == 1, confirmed == 1
		t.NotifyOnly = notifyOnly == 1
		if checked.Valid {
			c := parseTS(checked.String)
			t.CheckedAt = &c
		}
		if t.Verdict == "" {
			t.Verdict = "UNKNOWN"
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach how long each endpoint has held its current verdict.
	since, err := s.currentStateSince()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if at, ok := since[out[i].TargetID]; ok {
			t := at
			out[i].SinceAt = &t
		}
	}
	return out, nil
}

// currentStateSince maps each target to the moment it entered its latest verdict.
func (s *Store) currentStateSince() (map[int64]time.Time, error) {
	rows, err := s.db.Query(`
		SELECT e.target_id, e.changed_at
		FROM events e
		JOIN (SELECT target_id, MAX(id) AS max_id FROM events GROUP BY target_id) last
		  ON last.max_id = e.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = parseTS(at)
	}
	return out, rows.Err()
}

// Event is one recorded verdict transition.
type Event struct {
	ID          int64     `json:"id"`
	TargetID    int64     `json:"target_id"`
	Address     string    `json:"address"`
	Port        int       `json:"port"`
	FromVerdict string    `json:"from_verdict,omitempty"`
	ToVerdict   string    `json:"to_verdict"`
	ChangedAt   time.Time `json:"changed_at"`
}

// Events returns transitions at or after since, oldest first.
func (s *Store) Events(since time.Time) ([]Event, error) {
	rows, err := s.db.Query(`
		SELECT e.id, e.target_id, t.address, t.port, COALESCE(e.from_verdict,''), e.to_verdict, e.changed_at
		FROM events e JOIN targets t ON t.id = e.target_id
		WHERE e.changed_at >= ?
		ORDER BY e.changed_at, e.id`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var at string
		if err := rows.Scan(&e.ID, &e.TargetID, &e.Address, &e.Port, &e.FromVerdict, &e.ToVerdict, &at); err != nil {
			return nil, err
		}
		e.ChangedAt = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllEvents returns every transition ever recorded, oldest first. The table
// only holds changes, so it stays small enough to reconstruct history in memory.
func (s *Store) AllEvents() ([]Event, error) { return s.Events(time.Time{}) }

// ASNStat summarises how one Iranian operator behaved over a window.
type ASNStat struct {
	ASN     string  `json:"asn"`
	Nodes   string  `json:"nodes"`
	City    string  `json:"city"`
	Total   int     `json:"total"`
	Failed  int     `json:"failed"`
	Open    int     `json:"open"`
	FailPct float64 `json:"fail_pct"`
}

// ASNBreakdown groups probe outcomes by Iranian operator. Each Iranian probe
// sits on a different ASN, so this shows whether a block is nationwide or
// carrier-specific.
func (s *Store) ASNBreakdown(since time.Time, irNodes []string) ([]ASNStat, error) {
	if len(irNodes) == 0 {
		return nil, nil
	}
	args := []any{ts(since)}
	placeholders := make([]string, len(irNodes))
	for i, n := range irNodes {
		placeholders[i] = "?"
		args = append(args, n)
	}

	rows, err := s.db.Query(`
		SELECT COALESCE(NULLIF(nr.asn,''),'unknown'),
		       GROUP_CONCAT(DISTINCT nr.node),
		       COALESCE(MAX(nr.city),''),
		       COUNT(*),
		       SUM(CASE WHEN nr.outcome = 'TIMEOUT' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN nr.outcome = 'OPEN'    THEN 1 ELSE 0 END)
		FROM node_results nr
		JOIN results r ON r.id = nr.result_id
		WHERE r.checked_at >= ? AND nr.node IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY COALESCE(NULLIF(nr.asn,''),'unknown')
		ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ASNStat
	for rows.Next() {
		var a ASNStat
		var nodes sql.NullString
		if err := rows.Scan(&a.ASN, &nodes, &a.City, &a.Total, &a.Failed, &a.Open); err != nil {
			return nil, err
		}
		a.Nodes = shortNodeList(nodes.String)
		if a.Total > 0 {
			a.FailPct = float64(a.Failed) / float64(a.Total) * 100
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- per-target detail ----

// NodeResultRow is one probe reading as stored.
type NodeResultRow struct {
	Node        string  `json:"node"`
	CountryCode string  `json:"country_code"`
	City        string  `json:"city"`
	ASN         string  `json:"asn"`
	Outcome     string  `json:"outcome"`
	RTTms       float64 `json:"rtt_ms"`
	Detail      string  `json:"detail,omitempty"`
}

// TracerouteRow is a stored traceroute with its hops still encoded.
type TracerouteRow struct {
	Node           string    `json:"node"`
	LastHop        string    `json:"last_hop"`
	LastHopPrivate bool      `json:"last_hop_private"`
	DeadHops       int       `json:"dead_hops"`
	HopsJSON       string    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// ResultDetail is one stored result with its probe readings.
type ResultDetail struct {
	ResultID    int64           `json:"result_id"`
	ScanID      int64           `json:"scan_id"`
	Verdict     string          `json:"verdict"`
	Confirmed   bool            `json:"confirmed"`
	CheckedAt   time.Time       `json:"checked_at"`
	Link        string          `json:"permanent_link,omitempty"`
	NodeResults []NodeResultRow `json:"node_results,omitempty"`
	Traceroutes []TracerouteRow `json:"traceroutes,omitempty"`
}

// TargetByHostPort resolves an endpoint to its stored id.
func (s *Store) TargetByHostPort(address string, port int) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM targets WHERE address = ? AND port = ?`, address, port).Scan(&id)
	return id, err
}

// ResultsForTarget returns recent results, newest first, with readings attached.
func (s *Store) ResultsForTarget(targetID int64, limit int) ([]ResultDetail, error) {
	rows, err := s.db.Query(`
		SELECT id, scan_id, verdict, confirmed, checked_at, COALESCE(permanent_link,'')
		FROM results WHERE target_id = ?
		ORDER BY checked_at DESC, id DESC LIMIT ?`, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ResultDetail
	for rows.Next() {
		var d ResultDetail
		var at string
		var confirmed int
		if err := rows.Scan(&d.ResultID, &d.ScanID, &d.Verdict, &confirmed, &at, &d.Link); err != nil {
			return nil, err
		}
		d.Confirmed = confirmed == 1
		d.CheckedAt = parseTS(at)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if out[i].NodeResults, err = s.nodeResults(out[i].ResultID); err != nil {
			return nil, err
		}
		if out[i].Traceroutes, err = s.traceroutes(out[i].ResultID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) nodeResults(resultID int64) ([]NodeResultRow, error) {
	rows, err := s.db.Query(`
		SELECT node, COALESCE(country_code,''), COALESCE(city,''), COALESCE(asn,''),
		       outcome, COALESCE(rtt_ms,0), COALESCE(detail,'')
		FROM node_results WHERE result_id = ? ORDER BY id`, resultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NodeResultRow
	for rows.Next() {
		var n NodeResultRow
		if err := rows.Scan(&n.Node, &n.CountryCode, &n.City, &n.ASN, &n.Outcome, &n.RTTms, &n.Detail); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) traceroutes(resultID int64) ([]TracerouteRow, error) {
	rows, err := s.db.Query(`
		SELECT node, COALESCE(last_hop,''), last_hop_private, dead_hops, COALESCE(hops_json,'[]'), created_at
		FROM traceroutes WHERE result_id = ? ORDER BY id`, resultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TracerouteRow
	for rows.Next() {
		var t TracerouteRow
		var private int
		var at string
		if err := rows.Scan(&t.Node, &t.LastHop, &private, &t.DeadHops, &t.HopsJSON, &at); err != nil {
			return nil, err
		}
		t.LastHopPrivate = private == 1
		t.CreatedAt = parseTS(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestTraceroute returns the newest traceroute recorded for an endpoint.
func (s *Store) LatestTraceroute(targetID int64) (*TracerouteRow, error) {
	row := s.db.QueryRow(`
		SELECT tr.node, COALESCE(tr.last_hop,''), tr.last_hop_private, tr.dead_hops,
		       COALESCE(tr.hops_json,'[]'), tr.created_at
		FROM traceroutes tr
		JOIN results r ON r.id = tr.result_id
		WHERE r.target_id = ?
		ORDER BY tr.id DESC LIMIT 1`, targetID)

	var t TracerouteRow
	var private int
	var at string
	err := row.Scan(&t.Node, &t.LastHop, &private, &t.DeadHops, &t.HopsJSON, &at)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.LastHopPrivate = private == 1
	t.CreatedAt = parseTS(at)
	return &t, nil
}

func shortNodeList(csv string) string {
	if csv == "" {
		return ""
	}
	parts := strings.Split(csv, ",")
	for i, p := range parts {
		if idx := strings.Index(p, "."); idx > 0 {
			parts[i] = p[:idx]
		}
	}
	return strings.Join(parts, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
