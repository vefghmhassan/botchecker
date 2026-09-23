package store

import "fmt"

// addedColumns are applied after the schema so that databases created by an
// earlier version gain the new columns. SQLite has no "ADD COLUMN IF NOT
// EXISTS", so each one is checked against the table first.
var addedColumns = []struct{ table, column, ddl string }{
	{"targets", "provider", "TEXT"},
	{"targets", "hz_kind", "TEXT"},
	{"targets", "hz_server_id", "INTEGER"},
	{"targets", "hz_server_name", "TEXT"},
	{"targets", "hz_abuse_blocked", "INTEGER NOT NULL DEFAULT 0"},
	{"targets", "provider_checked_at", "TEXT"},
	// Marks an endpoint as hands-off: it is still probed and still alerts,
	// but no replacement server is ever created for it.
	{"targets", "notify_only", "INTEGER NOT NULL DEFAULT 0"},
	// Which Hetzner project owns this address. A server id means nothing
	// without it — ids are per project, so the same number can name a
	// different machine in each one, and a delete sent to the wrong project
	// either fails or destroys something else.
	{"targets", "hz_project", "TEXT"},
	{"provisions", "project", "TEXT"},
	// The machine the provision is about. target_id records which endpoint's
	// counter crossed; this records what was actually acted on.
	{"provisions", "address", "TEXT"},
	// When the panel stopped handing this endpoint out, or NULL while it still
	// does. Kept rather than deleting the row: the history of a deleted server
	// is still worth reading, and an endpoint that comes back — a config
	// switched off during a replacement and on again — must not look new.
	{"targets", "missing_since", "TEXT"},
}

// addedIndexes run after the columns exist.
//
// They cannot live in schema.sql: Open runs that file before migrate, so an
// index over a column this migration adds would fail on every database created
// by an earlier version — which is every database that already has data in it.
var addedIndexes = []struct{ name, ddl string }{
	{"idx_provisions_address",
		"CREATE INDEX IF NOT EXISTS idx_provisions_address ON provisions(address, triggered_at DESC)"},
}

// backfills repair rows written before a column existed. Each must be a no-op
// on its second run.
var backfills = []string{
	// Without this the address cooldown sees no history on the first boot after
	// the upgrade, and a machine replaced minutes earlier is free to be
	// replaced again immediately.
	`UPDATE provisions SET address = (
		SELECT address FROM targets WHERE targets.id = provisions.target_id
	) WHERE address IS NULL`,

	// Seed the ledger from replacements that actually completed. A swapped
	// provision means the old machine was handed back, so its address is gone
	// and Hetzner may hand it to the next server built in that location — which
	// is how the same IP was bought twice in one day.
	//
	// Only swapped runs are read. A failed or skipped one proves nothing about
	// the address, and a server that is merely down is not burned.
	`INSERT INTO address_ledger (address, reason, note, recorded_at)
		SELECT p.address, 'retired',
		       'backfilled from a completed replacement',
		       COALESCE(p.finished_at, p.triggered_at)
		FROM provisions p
		WHERE p.status = 'swapped' AND p.address IS NOT NULL AND p.address <> ''
		ON CONFLICT(address) DO NOTHING`,

	// And from builds that were proved unusable. An address this service
	// created a server on, probed from Iran, and found unreachable is dead, and
	// the server was deleted again — so Hetzner can hand that address to the
	// next build. It already did: 5.161.158.200 came back twice in five hours.
	//
	// UNKNOWN is deliberately excluded. It means the probe itself failed, which
	// says nothing about the address, and barring an address on that would
	// throw away usable ones.
	`INSERT INTO address_ledger (address, reason, verdict, provision_id, note, recorded_at)
		SELECT p.new_address, 'discarded', p.verify_verdict, p.id,
		       'backfilled from a build that came back ' || p.verify_verdict,
		       COALESCE(p.verified_at, p.finished_at, p.triggered_at)
		FROM provisions p
		WHERE p.new_address IS NOT NULL AND p.new_address <> ''
		  AND p.verify_verdict IS NOT NULL
		  AND p.verify_verdict NOT IN ('HEALTHY', 'UNKNOWN', '')
		ON CONFLICT(address) DO NOTHING`,
}

func (s *Store) migrate() error {
	for _, c := range addedColumns {
		has, err := s.hasColumn(c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}

	for _, idx := range addedIndexes {
		if _, err := s.db.Exec(idx.ddl); err != nil {
			return fmt.Errorf("%s: %w", idx.name, err)
		}
	}

	for _, stmt := range backfills {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
