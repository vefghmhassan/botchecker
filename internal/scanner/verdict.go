package scanner

import (
	"sort"

	"github.com/vefgh/botchecker/internal/checkhost"
)

// Verdict is the conclusion drawn about one endpoint.
type Verdict string

const (
	// VerdictHealthy means every Iranian probe completed the handshake.
	VerdictHealthy Verdict = "HEALTHY"
	// VerdictBlockedIR means the control nodes connect but Iran gets nothing
	// back at all — the signature of a blackholed address.
	VerdictBlockedIR Verdict = "BLOCKED_IR"
	// VerdictPartialBlock means some Iranian networks reach it and others do not.
	VerdictPartialBlock Verdict = "PARTIAL_BLOCK"
	// VerdictPortClosed means Iranian packets arrive and are refused. The
	// address is reachable; the service is not listening. Not filtering.
	VerdictPortClosed Verdict = "PORT_CLOSED"
	// VerdictServerDown means the control nodes could not connect either, so
	// nothing can be concluded about Iran.
	VerdictServerDown Verdict = "SERVER_DOWN"
	// VerdictUnknown means too few probes reported to decide.
	VerdictUnknown Verdict = "UNKNOWN"
)

// Assessment is the classification plus the counts behind it.
type Assessment struct {
	Verdict      Verdict
	IROpen       int
	IRTimeout    int
	IRRefused    int
	IRError      int
	IRPending    int
	IRTotal      int
	ControlOpen  int
	ControlTotal int
	// BlockedASNs lists the Iranian ASNs that saw no answer. Because each
	// Iranian probe sits on a different operator, this distinguishes a
	// nationwide block from one that only a few carriers apply.
	BlockedASNs []string
}

// Reachable reports whether any Iranian probe got a packet back, by either
// completing the handshake or being refused.
func (a Assessment) Reachable() bool { return a.IROpen > 0 || a.IRRefused > 0 }

// Classify turns raw node results into a verdict.
//
// minIRFail is the number of Iranian probes that must fail before a block is
// declared; the Iranian probes are themselves flaky, so a single silent node
// is not evidence. controlEnabled is false when the operator deliberately
// removed the control nodes, in which case "server down" cannot be ruled out
// and the Iranian result is taken at face value.
func Classify(results []checkhost.NodeResult, irNodes map[string]bool, minIRFail int, controlEnabled bool) Assessment {
	var a Assessment
	blockedASN := map[string]bool{}

	for _, r := range results {
		if irNodes[r.Node] {
			a.IRTotal++
			switch r.Outcome {
			case checkhost.OutcomeOpen:
				a.IROpen++
			case checkhost.OutcomeTimeout:
				a.IRTimeout++
				if r.ASN != "" {
					blockedASN[r.ASN] = true
				}
			case checkhost.OutcomeRefused:
				a.IRRefused++
			case checkhost.OutcomePending:
				a.IRPending++
			default:
				a.IRError++
			}
			continue
		}

		a.ControlTotal++
		if r.Outcome == checkhost.OutcomeOpen {
			a.ControlOpen++
		}
	}

	for asn := range blockedASN {
		a.BlockedASNs = append(a.BlockedASNs, asn)
	}
	sort.Strings(a.BlockedASNs)

	a.Verdict = decide(a, minIRFail, controlEnabled)
	return a
}

func decide(a Assessment, minIRFail int, controlEnabled bool) Verdict {
	// Without a working vantage point outside Iran there is no way to tell a
	// filtered address from a server that has stopped listening.
	if controlEnabled && a.ControlTotal > 0 && a.ControlOpen == 0 {
		return VerdictServerDown
	}

	decided := a.IROpen + a.IRTimeout + a.IRRefused + a.IRError
	if decided == 0 {
		return VerdictUnknown
	}

	switch {
	// A refusal proves the packet arrived, so this is not filtering even
	// though the endpoint is unusable.
	case a.IROpen == 0 && a.IRRefused > 0 && a.IRTimeout < minIRFail:
		return VerdictPortClosed

	// Nothing came back anywhere in Iran while the rest of the world connects.
	case a.IROpen == 0 && a.IRRefused == 0 && a.IRTimeout >= minIRFail:
		return VerdictBlockedIR

	// Some Iranian networks reach it and some do not.
	case a.IRTimeout > 0 && a.Reachable():
		return VerdictPartialBlock

	case a.IROpen > 0 && a.IRTimeout == 0:
		return VerdictHealthy

	default:
		return VerdictUnknown
	}
}

// NeedsConfirmation reports whether a verdict is serious enough to be re-checked
// before it is recorded. Only the two failure states that trigger action are
// worth the extra round trip.
func NeedsConfirmation(v Verdict) bool {
	return v == VerdictBlockedIR || v == VerdictPartialBlock
}
