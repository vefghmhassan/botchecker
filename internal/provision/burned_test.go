package provision

import (
	"context"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/store"
)

// The loop these end: on 2026-09-21 one destroyed address accumulated nine
// provision attempts and another seven, none of which could ever succeed,
// because nothing remembered that the machine had been deleted on purpose.

func TestABurnedAddressIsNeverReplaced(t *testing.T) {
	m, st, _ := testManager(t, "http://127.0.0.1:1")
	seedBrokenTarget(t, st, "5.161.158.200", 443, "BLOCKED_IR")
	if err := st.BurnAddress(store.LedgerEntry{
		Address: "5.161.158.200", Reason: store.BurnRetired, Verdict: "BLOCKED_IR",
	}); err != nil {
		t.Fatal(err)
	}

	addr, err := st.AddressFor("5.161.158.200")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Trigger(context.Background(), addr, 3); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	provs, err := st.Provisions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 {
		t.Fatalf("want exactly one record of the refusal, got %d", len(provs))
	}
	if provs[0].Status != store.ProvBurned {
		t.Fatalf("want status %q, got %q", store.ProvBurned, provs[0].Status)
	}
	// The record has to say why, or the next reader repeats the investigation.
	if provs[0].Error == "" {
		t.Fatal("the refusal was recorded with no reason")
	}
}

func TestReleasingAnAddressLetsItBeReplacedAgain(t *testing.T) {
	m, st, _ := testManager(t, "http://127.0.0.1:1")
	seedBrokenTarget(t, st, "5.161.158.200", 443, "BLOCKED_IR")
	if err := st.BurnAddress(store.LedgerEntry{
		Address: "5.161.158.200", Reason: store.BurnRetired}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseAddress("5.161.158.200", time.Now()); err != nil {
		t.Fatal(err)
	}

	addr, _ := st.AddressFor("5.161.158.200")
	if err := m.Trigger(context.Background(), addr, 3); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	provs, _ := st.Provisions(10)
	if len(provs) == 0 {
		t.Fatal("nothing was recorded at all")
	}
	if provs[0].Status == store.ProvBurned {
		t.Fatal("a released address is still being refused by the ledger")
	}
}

func TestARetiredMachineStillInTheAppIsReportedOnceADay(t *testing.T) {
	m, st, _ := testManager(t, "http://127.0.0.1:1")
	seedBrokenTarget(t, st, "5.161.158.200", 443, "SERVER_DOWN")
	if err := st.BurnAddress(store.LedgerEntry{
		Address: "5.161.158.200", Reason: store.BurnRetired}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := m.ReportBurned(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// Silence here would be worse than the loop: the panel is still handing
	// users an address that answers nothing.
	first, err := st.Notifications(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Kind != KindBurned {
		t.Fatalf("want one %s notification, got %+v", KindBurned, first)
	}

	// Repeating it every hour would train the reader to ignore it, and the
	// whole value of this alert is that it gets read.
	if err := m.ReportBurned(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	again, _ := st.Notifications(10)
	if len(again) != 1 {
		t.Fatalf("the alert repeated within the day: %d notifications", len(again))
	}
}

func TestAnAddressNotInTheAppIsNotReported(t *testing.T) {
	// Burned, but no config points at it any more. Nothing is wrong, so there
	// is nothing to say.
	m, st, _ := testManager(t, "http://127.0.0.1:1")
	if err := st.BurnAddress(store.LedgerEntry{
		Address: "5.161.158.200", Reason: store.BurnRetired}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReportBurned(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent, _ := st.Notifications(10)
	if len(sent) != 0 {
		t.Fatalf("a cleanly retired address must be silent, got %+v", sent)
	}
}
