// Package checkhost is a client for the public check-host.net API.
//
// Only the endpoints this project needs are implemented: check-tcp,
// check-traceroute and the node directory. Every response shape handled here
// was captured from the live API.
package checkhost

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
)

// Outcome is what one probe node observed for one target.
type Outcome string

const (
	// OutcomeOpen means the TCP handshake completed.
	OutcomeOpen Outcome = "OPEN"
	// OutcomeRefused means the packet reached the host and was rejected with
	// an RST. The address is routable; the port is simply closed.
	OutcomeRefused Outcome = "REFUSED"
	// OutcomeTimeout means nothing came back at all. Combined with a healthy
	// control node this is the signature of a blackholed address.
	OutcomeTimeout Outcome = "TIMEOUT"
	// OutcomeError covers any other error string the API returns.
	OutcomeError Outcome = "ERROR"
	// OutcomePending means the node had not reported by the time we gave up.
	OutcomePending Outcome = "PENDING"
)

// NodeMeta is the per-node descriptor returned when a check is created:
// [countryCode, country, city, ip, asn].
type NodeMeta struct {
	Node        string
	CountryCode string
	Country     string
	City        string
	IP          string
	ASN         string
}

// NodeResult is one node's verdict on one target.
type NodeResult struct {
	NodeMeta
	Outcome Outcome
	RTTms   float64
	Detail  string
}

// ShortName trims "ir1.node.check-host.net" down to "ir1".
func ShortName(node string) string {
	if i := strings.Index(node, "."); i > 0 {
		return node[:i]
	}
	return node
}

// parseTCPEntry decodes one node's slot of a check-result payload.
//
// The slot is null while the check is still running, otherwise a one-element
// array holding either {"address":..,"time":0.0992} on success or
// {"error":"Connection refused"} / {"error":"Connection timed out"} on failure.
// Note that "time" is in seconds.
func parseTCPEntry(raw json.RawMessage) (Outcome, float64, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return OutcomePending, 0, ""
	}

	var slots []json.RawMessage
	if err := json.Unmarshal(raw, &slots); err != nil || len(slots) == 0 {
		return OutcomeError, 0, "unreadable result"
	}

	var entry struct {
		Address string   `json:"address"`
		Time    *float64 `json:"time"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(slots[0], &entry); err != nil {
		return OutcomeError, 0, "unreadable entry"
	}

	if entry.Error != "" {
		return classifyError(entry.Error), 0, entry.Error
	}
	if entry.Time != nil {
		return OutcomeOpen, *entry.Time * 1000, entry.Address
	}
	return OutcomeError, 0, "empty entry"
}

func classifyError(msg string) Outcome {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "refus"):
		return OutcomeRefused
	case strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"):
		return OutcomeTimeout
	default:
		return OutcomeError
	}
}

// Hop is one traceroute hop.
type Hop struct {
	Index     int       `json:"index"`
	Host      string    `json:"host"`
	Times     []float64 `json:"times_ms"`
	Responded bool      `json:"responded"`
}

// TracerouteResult summarises a traceroute from one node.
type TracerouteResult struct {
	Node string `json:"node"`
	Hops []Hop  `json:"hops"`
	// LastHop is the final hop that answered.
	LastHop string `json:"last_hop"`
	// LastHopPrivate reports whether LastHop is an RFC1918 / CGNAT address.
	// When it is, the packet died inside the operator's own network and never
	// reached an international border — domestic filtering rather than a
	// routing problem further along the path.
	LastHopPrivate bool `json:"last_hop_private"`
	// DeadHops counts the silent hops trailing the last answer.
	DeadHops int `json:"dead_hops"`
}

// parseTraceroute decodes one node's slot of a traceroute result.
//
// Shape: [ [ hop, hop, ... ] ] where each hop is an array of
// {"host":"1.2.3.4","query_times":["0.16",...]}. Silent hops arrive as
// {"query_times":[null,null,null]} with no host, and the payload can end with
// a bare {}.
func parseTraceroute(node string, raw json.RawMessage) (*TracerouteResult, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}

	var outer []json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil || len(outer) == 0 {
		return nil, false
	}
	var hopList []json.RawMessage
	if err := json.Unmarshal(outer[0], &hopList); err != nil {
		return nil, false
	}

	res := &TracerouteResult{Node: node}
	for i, rawHop := range hopList {
		var entries []struct {
			Host       string    `json:"host"`
			QueryTimes []*string `json:"query_times"`
		}
		if err := json.Unmarshal(rawHop, &entries); err != nil {
			continue
		}

		hop := Hop{Index: i + 1}
		for _, e := range entries {
			if e.Host != "" && hop.Host == "" {
				hop.Host = e.Host
			}
			for _, t := range e.QueryTimes {
				if t == nil {
					continue
				}
				if v, err := strconv.ParseFloat(*t, 64); err == nil {
					hop.Times = append(hop.Times, v)
				}
			}
		}
		hop.Responded = hop.Host != ""
		res.Hops = append(res.Hops, hop)
	}

	// Walk back from the end to find the last hop that answered.
	for i := len(res.Hops) - 1; i >= 0; i-- {
		if res.Hops[i].Responded {
			res.LastHop = res.Hops[i].Host
			res.LastHopPrivate = isPrivateIP(res.LastHop)
			res.DeadHops = len(res.Hops) - 1 - i
			break
		}
	}
	return res, true
}

var cgnat = mustCIDR("100.64.0.0/10")

// isPrivateIP reports whether addr is non-routable on the public internet.
func isPrivateIP(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}
