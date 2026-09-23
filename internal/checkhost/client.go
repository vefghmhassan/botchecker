package checkhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	BaseURL       string
	HTTP          *http.Client
	PollInterval  time.Duration
	ResultTimeout time.Duration

	lim *limiter
}

func NewClient(baseURL string, rpsDelay, pollInterval, resultTimeout time.Duration) *Client {
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		HTTP:          &http.Client{Timeout: 30 * time.Second},
		PollInterval:  pollInterval,
		ResultTimeout: resultTimeout,
		lim:           newLimiter(rpsDelay),
	}
}

// Rate reports what the limiter has settled on, so "why is this slow" can be
// answered from the dashboard rather than by reading the source.
func (c *Client) Rate() Rate { return c.lim.rate() }

// TCPCheck is a completed check-tcp run.
type TCPCheck struct {
	RequestID     string
	PermanentLink string
	Results       []NodeResult
}

// createResponse is what check-host returns when a check is accepted.
type createResponse struct {
	OK            json.RawMessage            `json:"ok"`
	RequestID     string                     `json:"request_id"`
	PermanentLink string                     `json:"permanent_link"`
	Nodes         map[string][]string        `json:"nodes"`
	Error         string                     `json:"error"`
	Limits        map[string]json.RawMessage `json:"limits"`
}

// CheckTCP runs a TCP handshake check against hostPort from the given nodes
// and blocks until every node has reported or ResultTimeout elapses.
func (c *Client) CheckTCP(ctx context.Context, hostPort string, nodes []string) (*TCPCheck, error) {
	created, err := c.startCheck(ctx, "check-tcp", hostPort, nodes)
	if err != nil {
		return nil, err
	}

	raw, err := c.pollResult(ctx, created.RequestID, nodes, c.ResultTimeout)
	if err != nil {
		return nil, err
	}

	out := &TCPCheck{RequestID: created.RequestID, PermanentLink: created.PermanentLink}
	for _, node := range nodes {
		meta := metaFor(node, created.Nodes[node])
		outcome, rtt, detail := parseTCPEntry(raw[node])
		out.Results = append(out.Results, NodeResult{
			NodeMeta: meta,
			Outcome:  outcome,
			RTTms:    rtt,
			Detail:   detail,
		})
	}
	return out, nil
}

// Traceroute runs check-traceroute against host. It is much slower than a TCP
// check — the live API took well over 90 seconds — so it takes its own timeout.
func (c *Client) Traceroute(ctx context.Context, host string, nodes []string, timeout time.Duration) ([]TracerouteResult, error) {
	created, err := c.startCheck(ctx, "check-traceroute", host, nodes)
	if err != nil {
		return nil, err
	}

	raw, err := c.pollResult(ctx, created.RequestID, nodes, timeout)
	if err != nil {
		return nil, err
	}

	var out []TracerouteResult
	for _, node := range nodes {
		if res, ok := parseTraceroute(node, raw[node]); ok {
			out = append(out, *res)
		}
	}
	return out, nil
}

// Nodes fetches the live probe directory from /nodes/hosts.
func (c *Client) Nodes(ctx context.Context) (map[string]NodeMeta, error) {
	var payload struct {
		Nodes map[string]struct {
			Location []string `json:"location"`
			IP       string   `json:"ip"`
			ASN      string   `json:"asn"`
		} `json:"nodes"`
	}
	if err := c.getJSON(ctx, c.BaseURL+"/nodes/hosts", &payload); err != nil {
		return nil, err
	}

	out := make(map[string]NodeMeta, len(payload.Nodes))
	for name, n := range payload.Nodes {
		m := NodeMeta{Node: name, IP: n.IP, ASN: n.ASN}
		switch len(n.Location) {
		case 0:
		case 1:
			m.CountryCode = n.Location[0]
		case 2:
			m.CountryCode, m.Country = n.Location[0], n.Location[1]
		default:
			m.CountryCode, m.Country, m.City = n.Location[0], n.Location[1], n.Location[2]
		}
		out[name] = m
	}
	return out, nil
}

func (c *Client) startCheck(ctx context.Context, checkType, host string, nodes []string) (*createResponse, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%s: no nodes requested", checkType)
	}

	q := url.Values{}
	q.Set("host", host)
	for _, n := range nodes {
		q.Add("node", n)
	}
	endpoint := fmt.Sprintf("%s/%s?%s", c.BaseURL, checkType, q.Encode())

	var created createResponse
	if err := c.getJSON(ctx, endpoint, &created); err != nil {
		return nil, fmt.Errorf("%s %s: %w", checkType, host, err)
	}
	if created.Error != "" {
		return nil, fmt.Errorf("%s %s: api error: %s", checkType, host, created.Error)
	}
	if created.RequestID == "" {
		return nil, fmt.Errorf("%s %s: no request_id in response", checkType, host)
	}
	return &created, nil
}

// pollResult polls /check-result until every requested node has reported.
// Nodes still missing when the deadline passes stay nil and are reported as
// OutcomePending rather than being mistaken for a failure.
func (c *Client) pollResult(ctx context.Context, requestID string, nodes []string, timeout time.Duration) (map[string]json.RawMessage, error) {
	deadline := time.Now().Add(timeout)
	endpoint := c.BaseURL + "/check-result/" + url.PathEscape(requestID)
	latest := map[string]json.RawMessage{}

	// Results are never ready immediately, but they are usually ready well
	// before one full poll interval: a check typically completes in four to six
	// seconds. Asking early and backing off finds that moment, where a fixed
	// interval overshot it by up to a whole interval on every single check.
	wait := firstPollDelay
	if wait > c.PollInterval {
		wait = c.PollInterval
	}

	for {
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		var current map[string]json.RawMessage
		if err := c.getJSON(ctx, endpoint, &current); err != nil {
			if time.Now().After(deadline) {
				return latest, nil
			}
			continue
		}
		latest = current

		if complete(current, nodes) {
			return current, nil
		}
		if time.Now().After(deadline) {
			return latest, nil
		}

		// Back off toward the configured interval, so a check that is slow to
		// finish does not turn into a tight poll loop against the API.
		if wait = time.Duration(float64(wait) * pollBackoff); wait > c.PollInterval {
			wait = c.PollInterval
		}
	}
}

// firstPollDelay is how long to wait before asking for results the first time,
// and pollBackoff how much each subsequent wait grows.
const (
	firstPollDelay = 900 * time.Millisecond
	pollBackoff    = 1.6
)

func complete(results map[string]json.RawMessage, nodes []string) bool {
	for _, n := range nodes {
		v, ok := results[n]
		if !ok || len(v) == 0 || string(v) == "null" {
			return false
		}
	}
	return true
}

// getJSON performs a rate-limited GET and decodes the JSON body.
//
// The Accept header is required: without it check-host serves the HTML page
// instead of JSON.
func (c *Client) getJSON(ctx context.Context, endpoint string, dst any) error {
	if err := c.lim.wait(ctx); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// The limiter learns from this. Without it the client would keep
		// arriving at exactly the rate that just got refused.
		c.lim.throttled()
		return fmt.Errorf("rate limited by check-host (429): %s", trim(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, trim(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, trim(body))
	}
	return nil
}

// metaFor builds node metadata from the create response's
// [countryCode, country, city, ip, asn] array, tolerating short arrays.
func metaFor(node string, loc []string) NodeMeta {
	m := NodeMeta{Node: node}
	for i, v := range loc {
		switch i {
		case 0:
			m.CountryCode = v
		case 1:
			m.Country = v
		case 2:
			m.City = v
		case 3:
			m.IP = v
		case 4:
			m.ASN = v
		}
	}
	return m
}

// SortNodes returns nodes in a stable display order.
func SortNodes(nodes []string) []string {
	out := append([]string(nil), nodes...)
	sort.Strings(out)
	return out
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
