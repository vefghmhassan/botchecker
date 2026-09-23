package hetzner

import (
	"errors"
	"testing"
	"time"
)

func inventoryOf(addresses ...string) *Inventory {
	byAddress := map[string]Resource{}
	for i, a := range addresses {
		byAddress[a] = Resource{Address: a, Kind: "server", ServerID: int64(100 + i), ServerName: "srv"}
	}
	return &Inventory{byAddress: byAddress, FetchedAt: time.Now(), Servers: len(addresses)}
}

func assignmentFor(t *testing.T, as []Assignment, address string) Assignment {
	t.Helper()
	for _, a := range as {
		if a.Address == address {
			return a
		}
	}
	t.Fatalf("no assignment for %s", address)
	return Assignment{}
}

func TestReconcileNamesTheOwningProject(t *testing.T) {
	invs := map[string]*Inventory{
		"main":  inventoryOf("5.161.158.200"),
		"spare": inventoryOf("91.107.149.121"),
	}
	got, conflicts := Reconcile(
		[]string{"5.161.158.200", "91.107.149.121", "203.0.113.9"}, invs, nil)

	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if a := assignmentFor(t, got, "5.161.158.200"); a.Project != "main" || a.Ownership != OwnedHetzner {
		t.Errorf("got %+v, want main/hetzner", a)
	}
	if a := assignmentFor(t, got, "91.107.149.121"); a.Project != "spare" {
		t.Errorf("got %+v, want spare", a)
	}
	// Every project answered, so "nobody holds it" really does mean external.
	if a := assignmentFor(t, got, "203.0.113.9"); a.Ownership != OwnedExternal {
		t.Errorf("got %s, want external", a.Ownership)
	}
}

// The one that matters. A project that failed to answer must not cause its
// addresses to be relabelled external — external is the label that stops an
// address ever being replaced automatically, so a momentary API failure would
// silently switch off the whole point of the service for that account.
func TestReconcileNeverCallsAnAddressExternalOnAPartialView(t *testing.T) {
	invs := map[string]*Inventory{"main": inventoryOf("5.161.158.200")}
	errs := map[string]error{"spare": errors.New("500 from the API")}

	got, _ := Reconcile([]string{"5.161.158.200", "91.107.149.121"}, invs, errs)

	if a := assignmentFor(t, got, "5.161.158.200"); a.Ownership != OwnedHetzner {
		t.Errorf("a positively matched address was downgraded: %+v", a)
	}
	a := assignmentFor(t, got, "91.107.149.121")
	if a.Ownership == OwnedExternal {
		t.Fatal("an address was called external while a project was unreadable — " +
			"it would never be replaced again")
	}
	if a.Ownership != OwnedUnknown {
		t.Errorf("got %s, want unknown", a.Ownership)
	}
}

// One public address cannot really be in two projects, so this means stale data
// or a move in flight — exactly when guessing gets the wrong machine deleted.
func TestReconcileFlagsAnAddressTwoProjectsClaim(t *testing.T) {
	invs := map[string]*Inventory{
		"main":  inventoryOf("5.161.158.200"),
		"spare": inventoryOf("5.161.158.200"),
	}
	got, conflicts := Reconcile([]string{"5.161.158.200"}, invs, nil)

	if len(conflicts) != 1 || conflicts[0].Address != "5.161.158.200" {
		t.Fatalf("conflicts = %+v, want the disputed address reported", conflicts)
	}
	if len(conflicts[0].Projects) != 2 {
		t.Errorf("conflict names %v, want both projects", conflicts[0].Projects)
	}
	// Recorded as ours, but with no project — so nothing will act on it until a
	// human decides which account really holds it.
	a := assignmentFor(t, got, "5.161.158.200")
	if a.Project != "" {
		t.Errorf("a disputed address was assigned to %q instead of being left unresolved", a.Project)
	}
}
