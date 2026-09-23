package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigratingACopyOfTheLiveDatabase runs the real migration against a copy of
// the production file, so what the ledger will contain on the next start is
// known before the service is started rather than after.
//
// Skipped when the copy is not there, so this is not a test that only passes on
// one machine.
func TestMigratingACopyOfTheLiveDatabase(t *testing.T) {
	src := os.Getenv("BOTCHECKER_LIVE_DB")
	if src == "" {
		t.Skip("BOTCHECKER_LIVE_DB is not set")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no copy to migrate: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "live.db")
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dst)
	if err != nil {
		t.Fatalf("migrating the live database failed: %v", err)
	}
	defer st.Close()

	entries, err := st.BurnedAddresses()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger after migration: %d entries", len(entries))
	for _, e := range entries {
		t.Logf("  %-16s %-12s %s  %s", e.Address, e.Reason,
			e.RecordedAt.UTC().Format("2006-01-02 15:04"), e.Note)
	}

	// Running it twice must change nothing: a backfill that is not idempotent
	// would re-burn an address the operator had released.
	before := len(entries)
	st.Close()
	st2, err := Open(dst)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer st2.Close()
	after, err := st2.BurnedAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != before {
		t.Fatalf("the backfill is not idempotent: %d then %d", before, len(after))
	}
}
