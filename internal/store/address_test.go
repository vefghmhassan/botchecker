package store

import (
	"testing"
	"time"
)

func port(address string, p, irOpen, irTotal int, verdict string) TargetStatus {
	now := time.Now()
	return TargetStatus{
		TargetID: int64(p), Address: address, Port: p, CountryCode: "US",
		Verdict: verdict, IROpen: irOpen, IRTotal: irTotal,
		ControlOpen: 2, ControlTotal: 2, CheckedAt: &now,
	}
}

func TestAddressesGroupsPortsOfOneMachine(t *testing.T) {
	got := Addresses([]TargetStatus{
		port("5.161.158.200", 8443, 0, 8, "BLOCKED_IR"),
		port("1.1.1.1", 443, 8, 8, "HEALTHY"),
		port("5.161.158.200", 443, 0, 8, "BLOCKED_IR"),
	})

	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2", len(got))
	}
	if got[0].Address != "5.161.158.200" || len(got[0].Ports) != 2 {
		t.Fatalf("first = %+v", got[0])
	}
	// Ports are ordered so an alert reads the same way every time.
	if got[0].Ports[0].Port != 443 || got[0].Ports[1].Port != 8443 {
		t.Errorf("ports not in order: %v", got[0].PortNumbers())
	}
}

// Every port must be unreachable before the machine counts as blocked.
func TestFullyBlockedNeedsEveryPort(t *testing.T) {
	both := Addresses([]TargetStatus{
		port("5.161.158.200", 443, 0, 8, "BLOCKED_IR"),
		port("5.161.158.200", 8443, 0, 8, "BLOCKED_IR"),
	})[0]
	if !both.FullyBlocked() {
		t.Error("two ports at 0/8 should be a fully blocked machine")
	}

	// One port still answering means users are still being served there.
	// Replacing this machine would take it away from them.
	mixed := Addresses([]TargetStatus{
		port("5.161.158.200", 443, 0, 8, "BLOCKED_IR"),
		port("5.161.158.200", 8443, 3, 8, "PARTIAL_BLOCK"),
	})[0]
	if mixed.FullyBlocked() {
		t.Error("a machine still reachable on one port is not fully blocked")
	}
	if open, total := mixed.Reachable(); open != 3 || total != 8 {
		t.Errorf("Reachable = %d/%d, want 3/8 — the best port, not the worst", open, total)
	}
}

// Missing evidence must never read as "blocked". Failing safe here is the
// difference between a quiet no-op and buying a server for nothing.
func TestAPortWithNoResultIsNotBlocked(t *testing.T) {
	a := Addresses([]TargetStatus{
		port("5.161.158.200", 443, 0, 8, "BLOCKED_IR"),
		port("5.161.158.200", 8443, 0, 0, ""),
	})[0]
	if a.FullyBlocked() {
		t.Error("a port that was never probed must not count as blocked")
	}
}

func TestEmptyMachineIsNotBlocked(t *testing.T) {
	if (AddressStatus{Address: "1.2.3.4"}).FullyBlocked() {
		t.Error("a machine with no ports must not be replaceable")
	}
}

// Hands-off is asked of the machine, because the action it suppresses takes the
// whole machine with it.
func TestNotifyOnlyOnOnePortCoversTheMachine(t *testing.T) {
	p1 := port("5.161.158.200", 443, 0, 8, "BLOCKED_IR")
	p2 := port("5.161.158.200", 8443, 0, 8, "BLOCKED_IR")
	p2.NotifyOnly = true

	if !Addresses([]TargetStatus{p1, p2})[0].NotifyOnly() {
		t.Error("one port marked hands-off must protect the whole machine")
	}
}

// The alert should quote the most broken reading, not an arbitrary port.
func TestTriggerPicksTheWorstPort(t *testing.T) {
	a := Addresses([]TargetStatus{
		port("5.161.158.200", 443, 5, 8, "PARTIAL_BLOCK"),
		port("5.161.158.200", 8443, 1, 8, "PARTIAL_BLOCK"),
	})[0]
	if got := a.Trigger().Port; got != 8443 {
		t.Errorf("Trigger = port %d, want 8443 (1/8, the worst)", got)
	}
}
