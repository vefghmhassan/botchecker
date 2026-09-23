// Package store persists scans, probe results and verdict transitions in SQLite.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/splash"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct{ db *sql.DB }

// Open creates the database file if needed and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite tolerates one writer; keeping a single connection avoids
	// "database is locked" between the scanner and the dashboard.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	st := &Store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

// ---- scans ----

const (
	ScanRunning   = "running"
	ScanCompleted = "completed"
	ScanFailed    = "failed"
)

func (s *Store) CreateScan(trigger string, now time.Time) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO scans (started_at, status, trigger) VALUES (?, ?, ?)`,
		ts(now), ScanRunning, trigger,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetScanCounts(scanID int64, configCount, targetCount int) error {
	_, err := s.db.Exec(
		`UPDATE scans SET config_count = ?, target_count = ? WHERE id = ?`,
		configCount, targetCount, scanID,
	)
	return err
}

func (s *Store) FinishScan(scanID int64, status string, blockedCount int, scanErr error, now time.Time) error {
	var errText any
	if scanErr != nil {
		errText = scanErr.Error()
	}
	_, err := s.db.Exec(
		`UPDATE scans SET finished_at = ?, status = ?, blocked_count = ?, error = ? WHERE id = ?`,
		ts(now), status, blockedCount, errText, scanID,
	)
	return err
}

type Scan struct {
	ID           int64      `json:"id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Status       string     `json:"status"`
	Trigger      string     `json:"trigger"`
	ConfigCount  int        `json:"config_count"`
	TargetCount  int        `json:"target_count"`
	BlockedCount int        `json:"blocked_count"`
	Error        string     `json:"error,omitempty"`
}

const scanCols = `id, started_at, finished_at, status, trigger, config_count, target_count, blocked_count, COALESCE(error, '')`

func scanScan(sc interface{ Scan(...any) error }) (Scan, error) {
	var s Scan
	var started string
	var finished sql.NullString
	if err := sc.Scan(&s.ID, &started, &finished, &s.Status, &s.Trigger,
		&s.ConfigCount, &s.TargetCount, &s.BlockedCount, &s.Error); err != nil {
		return s, err
	}
	s.StartedAt = parseTS(started)
	if finished.Valid {
		t := parseTS(finished.String)
		s.FinishedAt = &t
	}
	return s, nil
}

func (s *Store) GetScan(id int64) (*Scan, error) {
	row := s.db.QueryRow(`SELECT `+scanCols+` FROM scans WHERE id = ?`, id)
	sc, err := scanScan(row)
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

func (s *Store) ListScans(limit int) ([]Scan, error) {
	rows, err := s.db.Query(`SELECT `+scanCols+` FROM scans ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Scan
	for rows.Next() {
		sc, err := scanScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// LatestCompletedScan returns the most recent finished full scan, or nil if
// none. Watch rounds are excluded: they cover only the already-blocked
// endpoints, so reporting one as "the last scan" would understate coverage.
func (s *Store) LatestCompletedScan() (*Scan, error) {
	row := s.db.QueryRow(
		`SELECT `+scanCols+` FROM scans
		 WHERE status = ? AND trigger IN ('manual','scheduled')
		 ORDER BY id DESC LIMIT 1`, ScanCompleted)
	sc, err := scanScan(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// ---- targets ----

// UpsertTarget records the endpoint and refreshes the metadata carried by the
// current config. first_seen is preserved across runs so lifetime is measurable.
func (s *Store) UpsertTarget(t splash.Target, now time.Time) (int64, error) {
	ids, _ := json.Marshal(t.ConfigIDs)
	names, _ := json.Marshal(t.Names)

	_, err := s.db.Exec(`
		INSERT INTO targets (address, port, country_code, protocol, config_ids_json, names_json, ads, active, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(address, port) DO UPDATE SET
			country_code    = excluded.country_code,
			protocol        = excluded.protocol,
			config_ids_json = excluded.config_ids_json,
			names_json      = excluded.names_json,
			ads             = excluded.ads,
			active          = excluded.active,
			last_seen       = excluded.last_seen`,
		t.Address, t.Port, t.CountryCode, t.Protocol, string(ids), string(names),
		boolToInt(t.Ads), boolToInt(t.Active), ts(now), ts(now))
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.db.QueryRow(`SELECT id FROM targets WHERE address = ? AND port = ?`, t.Address, t.Port).Scan(&id)
	return id, err
}

// ---- results ----

type ResultRecord struct {
	ScanID        int64
	TargetID      int64
	Verdict       string
	IROpen        int
	IRTimeout     int
	IRRefused     int
	IRTotal       int
	ControlOpen   int
	ControlTotal  int
	Confirmed     bool
	PermanentLink string
	CheckedAt     time.Time
}

func (s *Store) InsertResult(r ResultRecord) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO results (scan_id, target_id, verdict, ir_open, ir_timeout, ir_refused, ir_total,
		                     control_open, control_total, confirmed, permanent_link, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ScanID, r.TargetID, r.Verdict, r.IROpen, r.IRTimeout, r.IRRefused, r.IRTotal,
		r.ControlOpen, r.ControlTotal, boolToInt(r.Confirmed), r.PermanentLink, ts(r.CheckedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) InsertNodeResults(resultID int64, results []checkhost.NodeResult) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO node_results (result_id, node, country_code, city, asn, outcome, rtt_ms, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, n := range results {
		var rtt any
		if n.Outcome == checkhost.OutcomeOpen {
			rtt = n.RTTms
		}
		if _, err := stmt.Exec(resultID, n.Node, n.CountryCode, n.City, n.ASN,
			string(n.Outcome), rtt, n.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) InsertTraceroute(resultID int64, tr checkhost.TracerouteResult, now time.Time) error {
	hops, _ := json.Marshal(tr.Hops)
	_, err := s.db.Exec(`
		INSERT INTO traceroutes (result_id, node, last_hop, last_hop_private, dead_hops, hops_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		resultID, tr.Node, tr.LastHop, boolToInt(tr.LastHopPrivate), tr.DeadHops, string(hops), ts(now))
	return err
}

// ---- events ----

// LastVerdict returns the most recent verdict recorded for a target, or "" if
// this is the first time it has been seen.
func (s *Store) LastVerdict(targetID int64) (string, error) {
	var v string
	err := s.db.QueryRow(
		`SELECT to_verdict FROM events WHERE target_id = ? ORDER BY id DESC LIMIT 1`, targetID).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// RecordVerdict appends an event only when the verdict actually changed, so
// the events table stays a list of transitions rather than a second copy of
// every result. It reports whether a transition was written.
func (s *Store) RecordVerdict(targetID int64, verdict string, scanID int64, now time.Time) (bool, error) {
	prev, err := s.LastVerdict(targetID)
	if err != nil {
		return false, err
	}
	if prev == verdict {
		return false, nil
	}

	var from any
	if prev != "" {
		from = prev
	}
	_, err = s.db.Exec(
		`INSERT INTO events (target_id, from_verdict, to_verdict, changed_at, scan_id) VALUES (?, ?, ?, ?, ?)`,
		targetID, from, verdict, ts(now), scanID)
	return err == nil, err
}

// ---- retention ----

// Prune deletes raw probe data older than the retention window. Events and
// targets are left alone: they are tiny and carry the long-term history the
// dashboard's monthly view is built from.
func (s *Store) Prune(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM results WHERE checked_at < ?`, ts(olderThan))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	if _, err := s.db.Exec(
		`DELETE FROM scans WHERE finished_at IS NOT NULL AND finished_at < ?`, ts(olderThan)); err != nil {
		return n, err
	}
	return n, nil
}

// ---- helpers ----

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReapInterruptedScans marks scans that were still running when the process
// stopped as failed. Without this a killed process leaves a row claiming to be
// in progress forever, and the dashboard keeps reporting a scan that no longer
// exists.
func (s *Store) ReapInterruptedScans(now time.Time) (int64, error) {
	res, err := s.db.Exec(`
		UPDATE scans SET status = ?, finished_at = ?, error = ?
		WHERE status = ?`,
		ScanFailed, ts(now), "interrupted: the service stopped mid-scan", ScanRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
