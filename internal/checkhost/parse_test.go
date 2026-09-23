package checkhost

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func loadRaw(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return out
}

// The distinction between a refusal and a timeout is what separates a closed
// port from a filtered address, so it is pinned down here against live data.
func TestParseTCPEntryOutcomes(t *testing.T) {
	raw := loadRaw(t, "tcp_port443.json")

	t.Run("open", func(t *testing.T) {
		outcome, rtt, detail := parseTCPEntry(raw["de1.node.check-host.net"])
		if outcome != OutcomeOpen {
			t.Fatalf("outcome = %s, want OPEN", outcome)
		}
		// The API reports seconds; the client stores milliseconds.
		if math.Abs(rtt-100.207) > 0.001 {
			t.Errorf("rtt = %v ms, want ~100.207", rtt)
		}
		if detail != "5.161.158.200" {
			t.Errorf("detail = %q, want the probed address", detail)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		outcome, _, _ := parseTCPEntry(raw["ir1.node.check-host.net"])
		if outcome != OutcomeTimeout {
			t.Fatalf("outcome = %s, want TIMEOUT", outcome)
		}
	})

	t.Run("pending stays pending", func(t *testing.T) {
		outcome, _, _ := parseTCPEntry(raw["ir3.node.check-host.net"])
		if outcome != OutcomePending {
			t.Fatalf("outcome = %s, want PENDING: a node that has not reported "+
				"must not be counted as a failure", outcome)
		}
	})

	t.Run("missing key stays pending", func(t *testing.T) {
		outcome, _, _ := parseTCPEntry(raw["nope.node.check-host.net"])
		if outcome != OutcomePending {
			t.Fatalf("outcome = %s, want PENDING", outcome)
		}
	})
}

func TestParseTCPEntryRefusedVsTimeout(t *testing.T) {
	raw := loadRaw(t, "tcp_port80.json")

	if outcome, _, _ := parseTCPEntry(raw["de1.node.check-host.net"]); outcome != OutcomeRefused {
		t.Errorf("de1 outcome = %s, want REFUSED (packet arrived, port closed)", outcome)
	}
	if outcome, _, _ := parseTCPEntry(raw["ir1.node.check-host.net"]); outcome != OutcomeTimeout {
		t.Errorf("ir1 outcome = %s, want TIMEOUT (packet vanished)", outcome)
	}
}

func TestParseTracerouteBlockedInsideOperator(t *testing.T) {
	raw := loadRaw(t, "traceroute_ir1_blocked.json")

	res, ok := parseTraceroute("ir1.node.check-host.net", raw["ir1.node.check-host.net"])
	if !ok {
		t.Fatal("parseTraceroute returned not-ok for a valid payload")
	}

	if len(res.Hops) != 16 {
		t.Errorf("hops = %d, want 16", len(res.Hops))
	}
	if res.LastHop != "10.233.65.174" {
		t.Errorf("LastHop = %q, want 10.233.65.174", res.LastHop)
	}
	// The packet died on an RFC1918 address, so it never left the operator's
	// own network — domestic filtering rather than an upstream routing fault.
	if !res.LastHopPrivate {
		t.Error("LastHopPrivate = false, want true for 10.233.65.174")
	}
	if res.DeadHops != 9 {
		t.Errorf("DeadHops = %d, want 9", res.DeadHops)
	}

	// The first hop is public and answered three times.
	if h := res.Hops[0]; !h.Responded || h.Host != "185.105.238.193" || len(h.Times) != 3 {
		t.Errorf("hop 1 = %+v, want three answers from 185.105.238.193", h)
	}
	// Silent hops carry no host and no timings.
	if h := res.Hops[7]; h.Responded || len(h.Times) != 0 {
		t.Errorf("hop 8 = %+v, want an unanswered hop", h)
	}
}

func TestParseTracerouteRejectsEmpty(t *testing.T) {
	for _, in := range []string{"null", "[]", "not json"} {
		if _, ok := parseTraceroute("n", json.RawMessage(in)); ok {
			t.Errorf("parseTraceroute(%q) = ok, want not-ok", in)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"10.233.65.174":   true,
		"192.168.1.1":     true,
		"172.16.0.1":      true,
		"100.64.0.1":      true, // CGNAT, common in Iranian access networks
		"127.0.0.1":       true,
		"5.161.158.200":   false,
		"185.105.238.193": false,
		"":                false,
		"not-an-ip":       false,
	}
	for ip, want := range cases {
		if got := isPrivateIP(ip); got != want {
			t.Errorf("isPrivateIP(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestShortName(t *testing.T) {
	if got := ShortName("ir1.node.check-host.net"); got != "ir1" {
		t.Errorf("ShortName = %q, want ir1", got)
	}
	if got := ShortName("ir1"); got != "ir1" {
		t.Errorf("ShortName = %q, want ir1", got)
	}
}
