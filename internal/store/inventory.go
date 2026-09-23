package store

import (
	"time"
)

// Inventory state: which endpoints the panel currently hands out.
//
// Discovery used to come from the app's splash endpoint, which returns a random
// handful of configs per call. Anything it did not happen to pick was never
// added, and anything deleted from the panel was never removed — so the
// dashboard showed neither the servers that had just been added nor the absence
// of ones that had just been deleted.

// KnownEndpoints lists every endpoint ever recorded, keyed "address:port", with
// whether it is currently missing from the panel. Read before a sync so the sync
// can tell what is new and what has come back.
func (s *Store) KnownEndpoints() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT address, port, missing_since IS NOT NULL FROM targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var address string
		var port, missing int
		if err := rows.Scan(&address, &port, &missing); err != nil {
			return nil, err
		}
		out[address+":"+itoa(port)] = missing == 1
	}
	return out, rows.Err()
}

// MarkPresent clears the missing marker on endpoints the panel lists again.
func (s *Store) MarkPresent(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE targets SET missing_since = NULL
		WHERE address || ':' || port = ? AND missing_since IS NOT NULL`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, k := range keys {
		if _, err := stmt.Exec(k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkMissing records every endpoint not in present as gone from the panel, and
// returns the ones that were not already marked.
//
// Only call this with a complete list. Given the splash sample it would mark
// nearly the whole fleet missing on every sync, since a sample leaves out almost
// everything by design.
//
// Manually watched endpoints are never marked: they are monitored precisely
// because the panel does not list them.
func (s *Store) MarkMissing(present map[string]bool, manual map[string]bool, now time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT address, port FROM targets WHERE missing_since IS NULL`)
	if err != nil {
		return nil, err
	}
	var gone []string
	for rows.Next() {
		var address string
		var port int
		if err := rows.Scan(&address, &port); err != nil {
			rows.Close()
			return nil, err
		}
		key := address + ":" + itoa(port)
		if !present[key] && !manual[key] {
			gone = append(gone, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(gone) == 0 {
		return nil, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE targets SET missing_since = ?, active = 0
		WHERE address || ':' || port = ?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, k := range gone {
		if _, err := stmt.Exec(ts(now), k); err != nil {
			return nil, err
		}
	}
	return gone, tx.Commit()
}
