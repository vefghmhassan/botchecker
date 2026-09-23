package store

import (
	"database/sql"
	"errors"
	"time"
)

// Reasons an address is in the ledger. They are recorded rather than collapsed
// into one flag because they do not mean the same thing to a reader: a machine
// handed back on purpose is a different story from a brand new address that
// came back filtered, and only the second one says anything about the IP.
const (
	// BurnRetired is the machine that was being replaced, handed back once the
	// replacement was serving.
	BurnRetired = "retired"
	// BurnDiscarded is a freshly built address that Iran could not reach, so
	// the server was deleted again.
	BurnDiscarded = "discarded"
	// BurnDeletedByOperator is a server destroyed from the bot.
	BurnDeletedByOperator = "deleted_by_operator"
	// BurnVerifyFailed is an address that failed verification without the
	// server being destroyed.
	BurnVerifyFailed = "verify_failed"
	// BurnManual is an address the operator put in by hand.
	BurnManual = "manual"
)

// LedgerEntry is one address that must not be handed out again.
type LedgerEntry struct {
	Address     string     `json:"address"`
	Reason      string     `json:"reason"`
	Verdict     string     `json:"verdict,omitempty"`
	Project     string     `json:"project,omitempty"`
	ServerID    int64      `json:"server_id,omitempty"`
	ProvisionID int64      `json:"provision_id,omitempty"`
	Note        string     `json:"note,omitempty"`
	RecordedAt  time.Time  `json:"recorded_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

// Active reports whether this entry still blocks the address.
func (e LedgerEntry) Active() bool { return e.ReleasedAt == nil }

// BurnAddress records an address as dead.
//
// Re-burning an address that is already there refreshes the reason and clears
// any release, because the newer event is the one that matters: an address let
// back in and then found broken again is broken again.
func (s *Store) BurnAddress(e LedgerEntry) error {
	if e.Address == "" {
		return errors.New("cannot burn an empty address")
	}
	if e.RecordedAt.IsZero() {
		e.RecordedAt = time.Now()
	}
	_, err := s.db.Exec(`
		INSERT INTO address_ledger
			(address, reason, verdict, project, server_id, provision_id, note, recorded_at, released_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(address) DO UPDATE SET
			reason       = excluded.reason,
			verdict      = excluded.verdict,
			project      = excluded.project,
			server_id    = excluded.server_id,
			provision_id = excluded.provision_id,
			note         = excluded.note,
			recorded_at  = excluded.recorded_at,
			released_at  = NULL`,
		e.Address, e.Reason, nullString(e.Verdict), nullString(e.Project),
		nullInt(e.ServerID), nullInt(e.ProvisionID), nullString(e.Note), ts(e.RecordedAt))
	return err
}

// IsBurned reports whether an address is currently barred, and why.
//
// A released row answers false while still returning the entry, so a caller
// that wants to explain "this was burned on the 9th and you let it back in"
// can do so.
func (s *Store) IsBurned(address string) (bool, LedgerEntry, error) {
	e, err := s.LedgerEntry(address)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, LedgerEntry{}, nil
		}
		return false, LedgerEntry{}, err
	}
	return e.Active(), e, nil
}

// LedgerEntry reads one row, released or not.
func (s *Store) LedgerEntry(address string) (LedgerEntry, error) {
	row := s.db.QueryRow(`
		SELECT address, reason, verdict, project, server_id, provision_id, note, recorded_at, released_at
		FROM address_ledger WHERE address = ?`, address)
	return scanLedger(row)
}

// BurnedAddresses lists the ledger newest first, including released rows so
// that the history is visible rather than silently gone.
func (s *Store) BurnedAddresses() ([]LedgerEntry, error) {
	rows, err := s.db.Query(`
		SELECT address, reason, verdict, project, server_id, provision_id, note, recorded_at, released_at
		FROM address_ledger ORDER BY recorded_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LedgerEntry
	for rows.Next() {
		e, err := scanLedger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActiveBurnedAddresses is the set the build path checks against, as a map so
// a loop over candidate addresses does not make a query each time.
func (s *Store) ActiveBurnedAddresses() (map[string]LedgerEntry, error) {
	all, err := s.BurnedAddresses()
	if err != nil {
		return nil, err
	}
	out := make(map[string]LedgerEntry, len(all))
	for _, e := range all {
		if e.Active() {
			out[e.Address] = e
		}
	}
	return out, nil
}

// ReleaseAddress lets a burned address be used again. The row stays so that
// "this was burned and then released" remains readable.
func (s *Store) ReleaseAddress(address string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE address_ledger SET released_at = ? WHERE address = ? AND released_at IS NULL`,
		ts(at), address)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchAddress
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanLedger(r rowScanner) (LedgerEntry, error) {
	var (
		e                      LedgerEntry
		verdict, project, note sql.NullString
		serverID, provisionID  sql.NullInt64
		recordedAt             string
		releasedAt             sql.NullString
	)
	if err := r.Scan(&e.Address, &e.Reason, &verdict, &project,
		&serverID, &provisionID, &note, &recordedAt, &releasedAt); err != nil {
		return LedgerEntry{}, err
	}
	e.Verdict, e.Project, e.Note = verdict.String, project.String, note.String
	e.ServerID, e.ProvisionID = serverID.Int64, provisionID.Int64
	if t, err := time.Parse(time.RFC3339Nano, recordedAt); err == nil {
		e.RecordedAt = t
	}
	if releasedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, releasedAt.String); err == nil {
			e.ReleasedAt = &t
		}
	}
	return e, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
