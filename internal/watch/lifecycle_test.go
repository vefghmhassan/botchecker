package watch_test

import (
	"strings"
	"testing"

	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

// The complete journeys, against fakes for Iran, Hetzner and the panel.
//
// Every other test in this package checks one property in isolation. These
// check the whole thing, in order — because the failures that have actually
// cost something here were never a wrong fact, they were a right fact at the
// wrong moment:
//
//   - configs switched off before a replacement existed, which made every
//     individual assertion pass while users sat offline for six and a half
//     minutes
//   - an address recorded as dead only after its server was deleted, leaving a
//     window where the pool could hand it straight back
//   - a replacement built on an address this service had thrown away earlier
//     the same day, twice
//
// So these assert the sequence, and print the timeline when they fail.

// before fails unless the first event happened, the second happened, and the
// first came first.
func before(t *testing.T, j *journal, first, second, why string) {
	t.Helper()
	i, k := j.indexOf(first), j.indexOf(second)
	switch {
	case i < 0:
		t.Fatalf("%q never happened — %s\ntimeline:%s", first, why, j)
	case k < 0:
		t.Fatalf("%q never happened — %s\ntimeline:%s", second, why, j)
	case i > k:
		t.Fatalf("%q happened after %q — %s\ntimeline:%s", first, second, why, j)
	}
}

func never(t *testing.T, j *journal, prefix, why string) {
	t.Helper()
	if n := j.count(prefix); n > 0 {
		t.Fatalf("%q happened %d time(s) — %s\ntimeline:%s", prefix, n, why, j)
	}
}

// TestTheWholeJourneyOfABlockedMachine walks one machine from healthy to
// replaced to protected, and checks the order of every step.
func TestTheWholeJourneyOfABlockedMachine(t *testing.T) {
	const (
		machine  = ownedAddr
		recycled = "2.29.50.112" // an address thrown away earlier
		clean    = freshIP
	)

	ch := newCheckHost() // nothing blocked yet
	h := newHarness(t, ch, machine, clean)
	h.seedBlocked(t, machine, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)
	if err := h.set.Set(settings.ProvisionLocations, "hel1"); err != nil {
		t.Fatal(err)
	}

	// The account will offer back an address this service already destroyed
	// before offering a clean one — which is exactly what Hetzner does, because
	// a deleted server's IP goes back into its location's pool.
	h.hz.pool = []string{recycled, clean}
	if err := h.st.BurnAddress(store.LedgerEntry{
		Address: recycled, Reason: store.BurnDiscarded, Verdict: "BLOCKED_IR",
	}); err != nil {
		t.Fatal(err)
	}

	// ---- 1. while it works, nothing happens ----
	h.rounds(t, 2)
	if n := h.hz.createdCount(); n != 0 {
		t.Fatalf("a healthy machine was replaced: %d server(s) bought\ntimeline:%s", n, h.log)
	}

	// ---- 2. Iran stops answering ----
	ch.setBlocked(machine+":443", true)
	h.rounds(t, 3)

	// ---- 3. the whole chain ran, in this order ----
	before(t, h.log, "iran: probed "+machine, "hetzner: address",
		"a replacement was reserved before the machine was ever found blocked")

	before(t, h.log, "hetzner: address "+recycled+" reserved",
		"hetzner: reserved address given back",
		"an address known to be dead was kept instead of refused")

	before(t, h.log, "hetzner: reserved address given back", "hetzner: server created",
		"the dead address was not refused before a server was bought")

	before(t, h.log, "hetzner: server created", "iran: probed "+clean,
		"the new address was trusted without being probed from Iran")

	before(t, h.log, "iran: probed "+clean, "panel: configs moved",
		"configs were moved before the new address had been proved")

	before(t, h.log, "panel: configs moved", "hetzner: server 1 deleted",
		"the old machine was destroyed before its users had somewhere to go")

	// ---- 4. and the user was never left without anything ----
	never(t, h.log, "panel: configs on "+machine+" switched OFF",
		"a replacement was on the way, so there was nothing to gain by taking "+
			"the configs down and six minutes of outage to lose")

	// ---- 5. the machine was bought exactly once ----
	if n := h.hz.createdCount(); n != 1 {
		t.Fatalf("bought %d servers, want 1\ntimeline:%s", n, h.log)
	}
	if got := h.hz.reservedAddresses(); len(got) != 2 {
		t.Fatalf("reserved %v, want the dead one refused then a clean one\ntimeline:%s", got, h.log)
	}

	// ---- 6. configs landed on the clean address, switched on ----
	_, _, replaced := h.zex.snapshot()
	if len(replaced) != 1 {
		t.Fatalf("the panel was told to move configs %d times, want 1\ntimeline:%s",
			len(replaced), h.log)
	}
	move := replaced[0]
	if move["old_address"] != machine || move["new_address"] != clean {
		t.Errorf("configs moved %s -> %s, want %s -> %s",
			move["old_address"], move["new_address"], machine, clean)
	}
	if move["activate"] != "true" {
		t.Errorf("configs were left switched off after the move")
	}
	// No port is sent: the replacement is a clone and listens on the same
	// ports. Sending one moved 200 configs off 8443 and onto 443 unasked.
	if move["new_port"] != "" && move["new_port"] != "0" {
		t.Errorf("a port was sent with the move: %q", move["new_port"])
	}

	// ---- 7. the old address is now recorded as dead ----
	burned, entry, err := h.st.IsBurned(machine)
	if err != nil {
		t.Fatal(err)
	}
	if !burned {
		t.Fatalf("the replaced address was not recorded, so it can be bought back\ntimeline:%s", h.log)
	}
	if entry.Reason != store.BurnRetired {
		t.Errorf("recorded as %q, want %q", entry.Reason, store.BurnRetired)
	}
	if b, _, _ := h.st.IsBurned(clean); b {
		t.Error("the address that works was recorded as dead")
	}

	// ---- 8. and it cannot be asked for again ----
	countBefore := h.hz.createdCount()
	h.rounds(t, 4)
	if n := h.hz.createdCount(); n != countBefore {
		t.Fatalf("a machine that no longer exists was replaced again: %d -> %d\ntimeline:%s",
			countBefore, n, h.log)
	}

	// ---- 9. the operator was told the story, in order ----
	for _, want := range []string{"Blocked from Iran", "Replaced"} {
		if h.tg.containing(want) == "" {
			t.Errorf("no %q alert: %v", want, h.tg.messages())
		}
	}
	if msg := h.tg.containing("Configs taken out of circulation"); msg != "" {
		t.Errorf("an outage was announced that never happened: %s", msg)
	}

	t.Logf("the journey:%s", h.log)

	// ---- 10. and the record says what happened ----
	provs, err := h.st.Provisions(20)
	if err != nil {
		t.Fatal(err)
	}
	var swapped int
	for _, p := range provs {
		if p.Status == store.ProvSwapped {
			swapped++
		}
	}
	if swapped != 1 {
		t.Fatalf("recorded %d completed replacements, want 1: %+v", swapped, provs)
	}
}

// TestTheWholeJourneyWhenTheAccountIsFull is the same machine on an account
// with no room. Every project here is capped at five servers and all of them
// were full on 2026-09-21, which left a machine's configs switched off with
// nothing to move them to.
//
// What is wrong is the address, not the machine: its disk, its certs and its
// configs all work. A new address needs no slot at all.
func TestTheWholeJourneyWhenTheAccountIsFull(t *testing.T) {
	const (
		machine = ownedAddr
		floated = "203.0.113.50"
	)

	ch := newCheckHost(machine + ":443")
	h := newHarness(t, ch, machine, freshIP)
	h.seedBlocked(t, machine, 443)
	h.seedHealthyIn(t, "1.2.3.4", 443, "DE")
	h.enableSwap(t)

	h.hz.full = true // nothing can be built anywhere
	h.hz.floatPool = []string{floated}
	ch.openFrom(floated+":443", "")
	if err := h.set.Set(settings.ProvisionFloatOnFailure, "true"); err != nil {
		t.Fatal(err)
	}

	h.rounds(t, 3)

	// The build was tried first and refused, and only then was the machine
	// given a new address instead. A machine is never re-addressed while there
	// was still room to build beside it.
	before(t, h.log, "hetzner: create REFUSED", "hetzner: floating address",
		"a floating address was taken before a server was even attempted")

	if n := h.hz.createdCount(); n != 0 {
		t.Fatalf("a server was bought on a full account: %d\ntimeline:%s", n, h.log)
	}
	if got := h.hz.floatingAddresses(); len(got) != 1 || got[0] != floated {
		t.Fatalf("floating addresses = %v, want [%s]\ntimeline:%s", got, floated, h.log)
	}
	if h.hz.floatingAssigns() != 1 {
		t.Fatalf("the address was never attached to the machine\ntimeline:%s", h.log)
	}

	// No SSH key is configured, so the address cannot be put on the machine's
	// interface. Nothing can be concluded about it yet, so it is handed back
	// and the one command is asked for — rather than moving configs onto an
	// address that answers nothing, or blaming the address for a command that
	// never ran.
	before(t, h.log, "hetzner: floating address assigned", "hetzner: floating address deleted",
		"an address that could not be configured was left assigned and billed")

	if h.tg.containing("ip addr add") == "" {
		t.Errorf("the operator was not told what to run: %v", h.tg.messages())
	}
	if burned, _, _ := h.st.IsBurned(floated); burned {
		t.Errorf("a good address was condemned for a command that was never run\ntimeline:%s", h.log)
	}

	// And with no replacement coming, the configs do come down — this is the
	// one path where taking them out of circulation is right, because there is
	// nowhere for them to go.
	deactivated, _, replaced := h.zex.snapshot()
	if len(replaced) != 0 {
		t.Fatalf("configs were moved though nothing was built: %v\ntimeline:%s", replaced, h.log)
	}
	if len(deactivated) != 1 || deactivated[0] != machine {
		t.Fatalf("a dead config was left in circulation: %v\ntimeline:%s", deactivated, h.log)
	}
	before(t, h.log, "hetzner: create REFUSED", "panel: configs on "+machine+" switched OFF",
		"configs came down before the build had even been tried")
}

// TestTheJourneyIsReadableWhenItFails is a guard on the harness itself: a
// lifecycle assertion is only useful if its failure shows what happened.
func TestTheJourneyIsReadableWhenItFails(t *testing.T) {
	ch := newCheckHost(ownedAddr + ":443")
	h := newHarness(t, ch, ownedAddr, freshIP)
	h.seedBlocked(t, ownedAddr, 443)
	h.enableSwap(t)

	h.rounds(t, 3)

	timeline := h.log.String()
	if !strings.Contains(timeline, "iran: probed") {
		t.Fatalf("the timeline records nothing useful:%s", timeline)
	}
	if h.log.indexOf("nothing-like-this") != -1 {
		t.Error("indexOf invented an event that never happened")
	}
	if h.log.count("iran: probed") < 2 {
		t.Errorf("probes were not recorded in order:%s", timeline)
	}
	// An empty journal must say so rather than rendering as nothing at all.
	if got := (&journal{}).String(); got != "(nothing happened)" {
		t.Errorf("an empty timeline renders as %q", got)
	}
}
