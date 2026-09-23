// Package xui reads the 3x-ui panel API. It is an optional dependency: when
// the panel is not configured everything still works, alerts simply omit the
// online user count.
package xui

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/settings"
)

// ErrNotConfigured means no panel URL or token is set.
var ErrNotConfigured = errors.New("3x-ui panel is not configured")

// NodeView mirrors the panel's node read contract. Only the fields this
// service uses are decoded.
type NodeView struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	Remark        string   `json:"remark"`
	Address       string   `json:"address"`
	Port          int      `json:"port"`
	Scheme        string   `json:"scheme"`
	Guid          string   `json:"guid"`
	Enable        bool     `json:"enable"`
	Status        string   `json:"status"`
	XrayState     string   `json:"xrayState"`
	XrayVersion   string   `json:"xrayVersion"`
	LastHeartbeat int64    `json:"lastHeartbeat"`
	LatencyMs     int      `json:"latencyMs"`
	CPUPct        float64  `json:"cpuPct"`
	MemPct        float64  `json:"memPct"`
	UptimeSecs    uint64   `json:"uptimeSecs"`
	InboundCount  int      `json:"inboundCount"`
	InboundTags   []string `json:"inboundTags"`
	ClientCount   int      `json:"clientCount"`
	OnlineCount   int      `json:"onlineCount"`
	ActiveCount   int      `json:"activeCount"`
	LastError     string   `json:"lastError"`
}

// envelope is the panel's uniform response wrapper.
type envelope[T any] struct {
	Success bool   `json:"success"`
	Msg     string `json:"msg"`
	Obj     T      `json:"obj"`
}

type Client struct {
	cfg  *settings.Provider
	base string // overrides the configured URL, for tests
	to   time.Duration
}

func New(cfg *settings.Provider, timeout time.Duration) *Client {
	return &Client{cfg: cfg, to: timeout}
}

// WithBaseURL points the client at a different host, for tests.
func (c *Client) WithBaseURL(u string) *Client {
	c.base = strings.TrimRight(u, "/")
	return c
}

// Configured reports whether the panel can be queried right now.
func (c *Client) Configured() bool {
	return c.baseURL() != "" && c.cfg.Get(settings.XUIAPIToken) != ""
}

func (c *Client) baseURL() string {
	if c.base != "" {
		return c.base
	}
	return strings.TrimRight(c.cfg.Get(settings.XUIBaseURL), "/")
}

// Nodes lists every node the panel manages, with its live online count.
func (c *Client) Nodes(ctx context.Context) ([]NodeView, error) {
	var env envelope[[]NodeView]
	if err := c.do(ctx, http.MethodGet, "/panel/api/nodes/list", &env); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("panel refused the request: %s", env.Msg)
	}
	return env.Obj, nil
}

// OnlineFor returns the online client count for the node serving address.
// The second value reports whether a node matched at all — a caller must not
// treat "no such node" as "nobody online".
func (c *Client) OnlineFor(ctx context.Context, address string) (int, bool, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, n := range nodes {
		if strings.EqualFold(strings.TrimSpace(n.Address), strings.TrimSpace(address)) {
			return n.OnlineCount, true, nil
		}
	}
	return 0, false, nil
}

// Ping verifies the URL and token, for the settings page's test button.
func (c *Client) Ping(ctx context.Context) (int, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return 0, err
	}
	return len(nodes), nil
}

func (c *Client) do(ctx context.Context, method, path string, dst any) error {
	base := c.baseURL()
	token := c.cfg.Get(settings.XUIAPIToken)
	if base == "" || token == "" {
		return ErrNotConfigured
	}

	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("panel rejected the API token (http 401) — check the value and that it is enabled")
	}
	if resp.StatusCode == http.StatusForbidden {
		// The panel gates routes by token scope. Reading the node list is not
		// on the monitor or node-sync allowlists, so a narrower token fails
		// here with a 403 that would otherwise look like a bad token.
		return fmt.Errorf("panel refused the request (http 403) — the node list needs an " +
			"admin-scope token; monitor and node-sync tokens cannot read it")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("panel returned http %d: %s", resp.StatusCode, snippet(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("could not decode the panel response: %w (body: %s)", err, snippet(body))
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	cl := &http.Client{Timeout: c.to}
	if c.cfg.Bool(settings.XUIInsecureTLS) {
		cl.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return cl
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// NodeByAddress finds the node serving an address.
func (c *Client) NodeByAddress(ctx context.Context, address string) (*NodeView, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if strings.EqualFold(strings.TrimSpace(nodes[i].Address), strings.TrimSpace(address)) {
			return &nodes[i], nil
		}
	}
	return nil, nil
}

// SetNodeEnabled pauses or resumes traffic sync with a node, taking the whole
// server out of rotation without deleting anything.
func (c *Client) SetNodeEnabled(ctx context.Context, id int, enabled bool) error {
	path := fmt.Sprintf("/panel/api/nodes/setEnable/%d?enable=%t", id, enabled)
	var env envelope[json.RawMessage]
	if err := c.do(ctx, http.MethodPost, path, &env); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("panel refused to change the node: %s", env.Msg)
	}
	return nil
}

// DeleteNode removes a node from the panel.
//
// The panel's own documentation warns that inbounds bound to a node are not
// migrated when it is deleted, so callers must say so before asking.
func (c *Client) DeleteNode(ctx context.Context, id int) error {
	var env envelope[json.RawMessage]
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/panel/api/nodes/del/%d", id), &env); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("panel refused to delete the node: %s", env.Msg)
	}
	return nil
}
