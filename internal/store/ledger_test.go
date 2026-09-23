package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAnAddressStaysBurnedUntilItIsReleased(t *testing.T) {
	st := openTestStore(t)

	if burned, _, err := st.IsBurned("5.161.158.200"); err != nil || burned {
		t.Fatalf("a fresh address must not be burned: burned=%v err=%v", burned, err)
	}

	err := st.BurnAddress(LedgerEntry{
		Address: "5.161.158.200", Reason: BurnRetired,
		Verdict: "BLOCKED_IR", Project: "proxy", ServerID: 166713715,
	})
	if err != nil {
		t.Fatalf("burn: %v", err)
	}

	burned, entry, err := st.IsBurned("5.161.158.200")
	if err != nil || !burned {
		t.Fatalf("want burned, got burned=%v err=%v", burned, err)
	}
	if entry.Reason != BurnRetired || entry.Verdict != "BLOCKED_IR" || entry.ServerID != 166713715 {
		t.Fatalf("the entry lost its detail: %+v", entry)
	}
	if entry.RecordedAt.IsZero() {
		t.Fatal("an entry with no timestamp cannot be explained to a reader")
	}

	if err := st.ReleaseAddress("5.161.158.200", time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}
	burned, entry, err = st.IsBurned("5.161.158.200")
	if err != nil {
		t.Fatalf("read after release: %v", err)
	}
	if burned {
		t.Fatal("a released address must stop blocking")
	}
	// The row survives on purpose: "this was burned and then let back in" is
	// the history an operator needs when it goes wrong a second time.
	if entry.ReleasedAt == nil || entry.Reason != BurnRetired {
		t.Fatalf("the history was lost on release: %+v", entry)
	}
}

func TestReBurningClearsAnEarlierRelease(t *testing.T) {
	st := openTestStore(t)

	if err := st.BurnAddress(LedgerEntry{Address: "1.2.3.4", Reason: BurnDiscarded}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseAddress("1.2.3.4", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Let back in, tried again, broken again. The newer event is the one that
	// counts, or the address would be handed out forever.
	if err := st.BurnAddress(LedgerEntry{
		Address: "1.2.3.4", Reason: BurnDeletedByOperator, Verdict: "SERVER_DOWN",
	}); err != nil {
		t.Fatal(err)
	}

	burned, entry, err := st.IsBurned("1.2.3.4")
	if err != nil || !burned {
		t.Fatalf("re-burning must bar the address again: burned=%v err=%v", burned, err)
	}
	if entry.ReleasedAt != nil {
		t.Fatal("the stale release was not cleared")
	}
	if entry.Reason != BurnDeletedByOperator || entry.Verdict != "SERVER_DOWN" {
		t.Fatalf("the newer reason did not win: %+v", entry)
	}
}

func TestActiveBurnedAddressesLeavesOutReleasedOnes(t *testing.T) {
	st := openTestStore(t)

	for _, a := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		if err := st.BurnAddress(LedgerEntry{Address: a, Reason: BurnDiscarded}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReleaseAddress("2.2.2.2", time.Now()); err != nil {
		t.Fatal(err)
	}

	active, err := st.ActiveBurnedAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active["2.2.2.2"].Address != "" {
		t.Fatalf("released addresses must not be in the build-time set: %v", active)
	}

	all, err := st.BurnedAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("the full listing must still show the released row, got %d", len(all))
	}
}

func TestReleasingSomethingThatWasNeverBurnedSaysSo(t *testing.T) {
	st := openTestStore(t)
	if err := st.ReleaseAddress("9.9.9.9", time.Now()); !errors.Is(err, ErrNoSuchAddress) {
		t.Fatalf("want ErrNoSuchAddress, got %v", err)
	}
}

func TestAnEmptyAddressIsRefused(t *testing.T) {
	st := openTestStore(t)
	if err := st.BurnAddress(LedgerEntry{Reason: BurnManual}); err == nil {
		t.Fatal("an empty address would bar nothing and hide a bug")
	}
}

// The backfill is what stops the loop on the first boot after the upgrade,
// rather than only from the next failure onwards. It has to read the evidence
// already in the provisions table without inventing any.
func TestTheBackfillReadsOnlyProvenlyDeadAddresses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.db")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(address, newAddress, verdict, status string) {
		t.Helper()
		if _, err := st.DB().Exec(`INSERT INTO targets
			(address, port, active, first_seen, last_seen) VALUES (?, 443, 1, ?, ?)`,
			address, ts(time.Now()), ts(time.Now())); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := st.DB().QueryRow(`SELECT id FROM targets WHERE address = ?`, address).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO provisions
			(target_id, triggered_at, finished_at, status, address, new_address, verify_verdict)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, ts(time.Now()), ts(time.Now()), status, address, newAddress, verdict); err != nil {
			t.Fatal(err)
		}
	}

	// A completed replacement: the old machine was handed back.
	seed("2.29.60.163", "65.109.183.208", "HEALTHY", "swapped")
	// A build that was proved unreachable from Iran and then deleted.
	seed("10.0.0.1", "5.161.158.200", "BLOCKED_IR", "failed")
	// A build whose probe itself failed. This says nothing about the address.
	seed("10.0.0.2", "2.29.50.112", "UNKNOWN", "failed")
	st.Close()

	// Reopening is what runs the migration.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	want := map[string]string{
		"2.29.60.163":   BurnRetired,
		"5.161.158.200": BurnDiscarded,
	}
	got, err := st2.ActiveBurnedAddresses()
	if err != nil {
		t.Fatal(err)
	}
	for addr, reason := range want {
		e, ok := got[addr]
		if !ok {
			t.Errorf("%s was not backfilled, so it can be bought back", addr)
			continue
		}
		if e.Reason != reason {
			t.Errorf("%s: reason = %q, want %q", addr, e.Reason, reason)
		}
	}
	// A probe failure is not evidence against the address. Barring one on that
	// basis would throw away usable addresses for a check-host outage.
	if _, ok := got["2.29.50.112"]; ok {
		t.Error("an address was condemned for a probe that failed, not for being dead")
	}
	// The address that works must obviously stay usable.
	if _, ok := got["65.109.183.208"]; ok {
		t.Error("a healthy replacement address was burned")
	}
}
