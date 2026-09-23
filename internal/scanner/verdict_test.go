package scanner

import (
	"reflect"
	"testing"

	"github.com/vefgh/botchecker/internal/checkhost"
)

var irNodeSet = map[string]bool{
	"ir1": true, "ir2": true, "ir3": true, "ir4": true,
	"ir5": true, "ir6": true, "ir7": true, "ir8": true,
}

// node builds one probe reading.
func node(name string, outcome checkhost.Outcome, asn string) checkhost.NodeResult {
	return checkhost.NodeResult{
		NodeMeta: checkhost.NodeMeta{Node: name, ASN: asn},
		Outcome:  outcome,
	}
}

// allIR produces one reading per Iranian probe, each on its own ASN — which is
// how the real node set is laid out.
func allIR(outcome checkhost.Outcome) []checkhost.NodeResult {
	asns := []string{"AS47430", "AS209279", "AS213953", "AS212077",
		"AS214431", "AS206596", "AS213727", "AS214361"}
	var out []checkhost.NodeResult
	for i, asn := range asns {
		out = append(out, node([]string{"ir1", "ir2", "ir3", "ir4", "ir5", "ir6", "ir7", "ir8"}[i], outcome, asn))
	}
	return out
}

func controls(outcome checkhost.Outcome) []checkhost.NodeResult {
	return []checkhost.NodeResult{node("de1", outcome, "AS24940"), node("nl1", outcome, "AS60781")}
}

func TestClassifyBlockedFromIran(t *testing.T) {
	// The live 5.161.158.200:443 reading: control nodes connect, every Iranian
	// probe times out.
	results := append(controls(checkhost.OutcomeOpen), allIR(checkhost.OutcomeTimeout)...)

	a := Classify(results, irNodeSet, 6, true)

	if a.Verdict != VerdictBlockedIR {
		t.Fatalf("verdict = %s, want BLOCKED_IR", a.Verdict)
	}
	if a.IRTimeout != 8 || a.IROpen != 0 || a.ControlOpen != 2 {
		t.Errorf("counts = %+v, want 8 timeouts, 0 open, 2 control open", a)
	}
	if len(a.BlockedASNs) != 8 {
		t.Errorf("BlockedASNs = %v, want all 8 operators", a.BlockedASNs)
	}
}

func TestClassifyServerDownWhenControlAlsoFails(t *testing.T) {
	// The live 5.161.158.224 reading: everything times out, including Germany.
	// Without the control nodes this would be misreported as a block and would
	// send the operator off replacing a server that is merely switched off.
	results := append(controls(checkhost.OutcomeTimeout), allIR(checkhost.OutcomeTimeout)...)

	if a := Classify(results, irNodeSet, 6, true); a.Verdict != VerdictServerDown {
		t.Fatalf("verdict = %s, want SERVER_DOWN", a.Verdict)
	}
}

func TestClassifyWithoutControlNodesFallsBackToIran(t *testing.T) {
	// With controls disabled the Iranian reading is taken at face value.
	results := allIR(checkhost.OutcomeTimeout)

	if a := Classify(results, irNodeSet, 6, false); a.Verdict != VerdictBlockedIR {
		t.Fatalf("verdict = %s, want BLOCKED_IR when controls are disabled", a.Verdict)
	}
}

func TestClassifyHealthy(t *testing.T) {
	// The live 65.109.216.204:443 reading.
	results := append(controls(checkhost.OutcomeOpen), allIR(checkhost.OutcomeOpen)...)

	a := Classify(results, irNodeSet, 6, true)
	if a.Verdict != VerdictHealthy {
		t.Fatalf("verdict = %s, want HEALTHY", a.Verdict)
	}
	if len(a.BlockedASNs) != 0 {
		t.Errorf("BlockedASNs = %v, want none", a.BlockedASNs)
	}
}

func TestClassifyPortClosedIsNotFiltering(t *testing.T) {
	// A refusal proves the packet arrived: the address is reachable from Iran
	// and the service is simply not listening.
	results := append(controls(checkhost.OutcomeOpen), allIR(checkhost.OutcomeRefused)...)

	a := Classify(results, irNodeSet, 6, true)
	if a.Verdict != VerdictPortClosed {
		t.Fatalf("verdict = %s, want PORT_CLOSED", a.Verdict)
	}
	if !a.Reachable() {
		t.Error("Reachable() = false, but a refusal means packets got through")
	}
}

func TestClassifyPartialBlockPerOperator(t *testing.T) {
	ir := allIR(checkhost.OutcomeOpen)
	ir[0].Outcome = checkhost.OutcomeTimeout // AS47430
	ir[3].Outcome = checkhost.OutcomeTimeout // AS212077
	results := append(controls(checkhost.OutcomeOpen), ir...)

	a := Classify(results, irNodeSet, 6, true)
	if a.Verdict != VerdictPartialBlock {
		t.Fatalf("verdict = %s, want PARTIAL_BLOCK", a.Verdict)
	}
	want := []string{"AS212077", "AS47430"}
	if !reflect.DeepEqual(a.BlockedASNs, want) {
		t.Errorf("BlockedASNs = %v, want %v", a.BlockedASNs, want)
	}
}

func TestClassifyRespectsConsensusThreshold(t *testing.T) {
	// Five silent probes is below the default threshold of six. The Iranian
	// nodes are flaky on their own, so this must not be called a block.
	ir := allIR(checkhost.OutcomeTimeout)
	for i := 5; i < 8; i++ {
		ir[i].Outcome = checkhost.OutcomeOpen
	}
	results := append(controls(checkhost.OutcomeOpen), ir...)

	if a := Classify(results, irNodeSet, 6, true); a.Verdict == VerdictBlockedIR {
		t.Fatalf("verdict = BLOCKED_IR with only %d/8 failures, want a softer verdict", a.IRTimeout)
	}
}

func TestClassifyPendingProbesAreNotFailures(t *testing.T) {
	results := append(controls(checkhost.OutcomeOpen), allIR(checkhost.OutcomePending)...)

	a := Classify(results, irNodeSet, 6, true)
	if a.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %s, want UNKNOWN when nothing reported", a.Verdict)
	}
	if a.IRPending != 8 {
		t.Errorf("IRPending = %d, want 8", a.IRPending)
	}
}

func TestNeedsConfirmation(t *testing.T) {
	cases := map[Verdict]bool{
		VerdictBlockedIR:    true,
		VerdictPartialBlock: true,
		VerdictHealthy:      false,
		VerdictServerDown:   false,
		VerdictPortClosed:   false,
		VerdictUnknown:      false,
	}
	for v, want := range cases {
		if got := NeedsConfirmation(v); got != want {
			t.Errorf("NeedsConfirmation(%s) = %v, want %v", v, got, want)
		}
	}
}
