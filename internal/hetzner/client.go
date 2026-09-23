package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/settings"
)

// ErrNotConfigured means no API token is set.
var ErrNotConfigured = errors.New("hetzner token is not configured")

const defaultBaseURL = "https://api.hetzner.cloud/v1"

type Client struct {
	cfg  *settings.Provider
	log  *slog.Logger
	http *http.Client
	base string // overridden in tests

	inventoryTTL time.Duration

	// token, when set, overrides the single-project setting. The registry uses
	// it to give each project its own client.
	token func() string

	mu     sync.Mutex
	cached *Inventory
}

func New(cfg *settings.Provider, log *slog.Logger, timeout, inventoryTTL time.Duration) *Client {
	return &Client{
		cfg:          cfg,
		log:          log,
		http:         &http.Client{Timeout: timeout},
		inventoryTTL: inventoryTTL,
	}
}

// WithBaseURL points the client at a different host, for tests.
func (c *Client) WithBaseURL(u string) *Client {
	c.base = strings.TrimRight(u, "/")
	return c
}

// Configured reports whether the API can be called right now.
func (c *Client) Configured() bool { return c.tokenFor() != "" }

// tokenFor reads the credential this client sends. It is a function rather than
// a stored string so a token changed in the panel takes effect without a
// restart, and so the registry can bind a client to one project's token without
// the client knowing projects exist.
func (c *Client) tokenFor() string {
	if c.token != nil {
		return c.token()
	}
	return c.cfg.Get(settings.HetznerToken)
}

// withToken pins this client to one credential. Used by the registry.
func (c *Client) withToken(f func() string) *Client {
	c.token = f
	return c
}

func (c *Client) baseURL() string {
	if c.base != "" {
		return c.base
	}
	return defaultBaseURL
}

// ---- inventory ----

type pageMeta struct {
	Pagination struct {
		NextPage int `json:"next_page"`
	} `json:"pagination"`
}

type apiServer struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	PublicNet struct {
		IPv4 struct {
			IP      string `json:"ip"`
			Blocked bool   `json:"blocked"`
		} `json:"ipv4"`
		IPv6 struct {
			IP      string `json:"ip"`
			Blocked bool   `json:"blocked"`
		} `json:"ipv6"`
	} `json:"public_net"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
}

type apiIP struct {
	ID           int64  `json:"id"`
	IP           string `json:"ip"`
	Type         string `json:"type"`
	Blocked      bool   `json:"blocked"`
	Name         string `json:"name"`
	AssigneeID   int64  `json:"assignee_id"`
	AssigneeType string `json:"assignee_type"`
	Server       *int64 `json:"server"`
}

// Inventory lists every address in the project, from servers, primary IPs and
// floating IPs, so that an address can be classified without guessing from IP
// ranges or ASN. The result is cached for inventoryTTL.
func (c *Client) Inventory(ctx context.Context) (*Inventory, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	c.mu.Lock()
	if c.cached != nil && time.Since(c.cached.FetchedAt) < c.inventoryTTL {
		cached := c.cached
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	inv := &Inventory{byAddress: map[string]Resource{}, FetchedAt: time.Now()}

	var servers []apiServer
	if err := c.paginate(ctx, "/servers", "servers", func(raw json.RawMessage) error {
		var page []apiServer
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		servers = append(servers, page...)
		return nil
	}); err != nil {
		return nil, err
	}

	byServerID := map[int64]apiServer{}
	for _, s := range servers {
		byServerID[s.ID] = s
		for _, addr := range []struct {
			ip      string
			blocked bool
		}{
			{s.PublicNet.IPv4.IP, s.PublicNet.IPv4.Blocked},
			{s.PublicNet.IPv6.IP, s.PublicNet.IPv6.Blocked},
		} {
			ip := normalise(addr.ip)
			if ip == "" {
				continue
			}
			inv.byAddress[ip] = Resource{
				Address: ip, Kind: "server",
				ServerID: s.ID, ServerName: s.Name,
				ServerType: s.ServerType.Name, Location: s.Location.Name,
				Status: s.Status, AbuseBlocked: addr.blocked,
			}
		}
	}
	inv.Servers = len(servers)

	// Primary and floating IPs can carry an address that is not the server's
	// own public IP, so they are collected separately. They never overwrite a
	// server entry, which already carries richer metadata.
	for _, spec := range []struct{ path, key string }{
		{"/primary_ips", "primary_ips"},
		{"/floating_ips", "floating_ips"},
	} {
		kind := strings.TrimSuffix(strings.TrimPrefix(spec.path, "/"), "s")
		if err := c.paginate(ctx, spec.path, spec.key, func(raw json.RawMessage) error {
			var page []apiIP
			if err := json.Unmarshal(raw, &page); err != nil {
				return err
			}
			for _, ip := range page {
				addr := normalise(ip.IP)
				if addr == "" {
					continue
				}
				if _, exists := inv.byAddress[addr]; exists {
					continue
				}
				r := Resource{Address: addr, Kind: kind, AbuseBlocked: ip.Blocked}
				switch {
				case ip.Server != nil:
					r.ServerID = *ip.Server
				case ip.AssigneeType == "server":
					r.ServerID = ip.AssigneeID
				}
				if s, ok := byServerID[r.ServerID]; ok {
					r.ServerName, r.ServerType, r.Location = s.Name, s.ServerType.Name, s.Location.Name
				}
				inv.byAddress[addr] = r
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	c.cached = inv
	c.mu.Unlock()
	return inv, nil
}

// InvalidateInventory drops the cache, e.g. after creating a server.
func (c *Client) InvalidateInventory() {
	c.mu.Lock()
	c.cached = nil
	c.mu.Unlock()
}

// paginate walks every page of a list endpoint, handing each page's raw array
// to fn.
func (c *Client) paginate(ctx context.Context, path, key string, fn func(json.RawMessage) error) error {
	page := 1
	for {
		var envelope map[string]json.RawMessage
		url := fmt.Sprintf("%s?page=%d&per_page=50", path, page)
		if err := c.do(ctx, http.MethodGet, url, nil, &envelope); err != nil {
			return err
		}
		if raw, ok := envelope[key]; ok {
			if err := fn(raw); err != nil {
				return fmt.Errorf("decode %s: %w", key, err)
			}
		}

		var meta pageMeta
		if raw, ok := envelope["meta"]; ok {
			_ = json.Unmarshal(raw, &meta)
		}
		if meta.Pagination.NextPage <= 0 || meta.Pagination.NextPage == page {
			return nil
		}
		page = meta.Pagination.NextPage
	}
}

// ---- snapshots and servers ----

// Snapshots lists the project's snapshot images, newest first.
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	var out []Snapshot
	err := c.paginate(ctx, "/images?type=snapshot", "images", func(raw json.RawMessage) error {
		var page []Snapshot
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		out = append(out, page...)
		return nil
	})
	return out, err
}

// CreateFromSnapshot creates a server and returns it once the API has accepted
// the request. The server is not yet booted; the action must be awaited.
func (c *Client) CreateFromSnapshot(ctx context.Context, spec CreateSpec) (*CreatedServer, error) {
	body := map[string]any{
		"name":               spec.Name,
		"server_type":        spec.ServerType,
		"image":              spec.Image,
		"start_after_create": true,
	}
	// A reserved address lives in one datacenter, so the server must be created
	// there. Hetzner refuses a request carrying both, and the datacenter is the
	// more specific of the two.
	switch {
	case spec.Datacenter != "":
		body["datacenter"] = spec.Datacenter
	case spec.Location != "":
		body["location"] = spec.Location
	}
	if spec.PrimaryIPv4 != 0 {
		body["public_net"] = map[string]any{
			"enable_ipv4": true,
			"ipv4":        spec.PrimaryIPv4,
		}
	}
	if len(spec.SSHKeys) > 0 {
		body["ssh_keys"] = spec.SSHKeys
	}
	if len(spec.Labels) > 0 {
		body["labels"] = spec.Labels
	}

	var resp struct {
		Server apiServer `json:"server"`
		Action struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", body, &resp); err != nil {
		return nil, err
	}

	c.InvalidateInventory()
	return &CreatedServer{
		ID:       resp.Server.ID,
		Name:     resp.Server.Name,
		IPv4:     resp.Server.PublicNet.IPv4.IP,
		ActionID: resp.Action.ID,
		Status:   resp.Action.Status,
	}, nil
}

// WaitAction polls until the action finishes. The interval backs off because
// the API documentation warns against polling actions too frequently.
func (c *Client) WaitAction(ctx context.Context, id int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := 3 * time.Second

	for {
		var resp struct {
			Action struct {
				Status string `json:"status"`
				Error  *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"action"`
		}
		if err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
			return err
		}

		switch resp.Action.Status {
		case "success":
			return nil
		case "error":
			if resp.Action.Error != nil {
				return fmt.Errorf("hetzner action %d failed: %s (%s)", id, resp.Action.Error.Message, resp.Action.Error.Code)
			}
			return fmt.Errorf("hetzner action %d failed", id)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("hetzner action %d still %q after %s", id, resp.Action.Status, timeout)
		}
		if err := sleep(ctx, interval); err != nil {
			return err
		}
		if interval < 15*time.Second {
			interval += 3 * time.Second
		}
	}
}

// Server fetches one server, used to read the address once it is running.
func (c *Client) Server(ctx context.Context, id int64) (*Resource, error) {
	var resp struct {
		Server apiServer `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
		return nil, err
	}
	s := resp.Server
	return &Resource{
		Address: normalise(s.PublicNet.IPv4.IP), Kind: "server",
		ServerID: s.ID, ServerName: s.Name, ServerType: s.ServerType.Name,
		Location: s.Location.Name, Status: s.Status,
		AbuseBlocked: s.PublicNet.IPv4.Blocked,
	}, nil
}

// DeleteServer removes a server. Not called anywhere in this phase; kept so
// the handoff has it available.
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, &struct{}{})
}

// Ping verifies the token, for the settings page's test button.
func (c *Client) Ping(ctx context.Context) (int, error) {
	var resp struct {
		Servers []apiServer `json:"servers"`
		Meta    pageMeta    `json:"meta"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers?per_page=1", nil, &resp); err != nil {
		return 0, err
	}
	return len(resp.Servers), nil
}

// ---- transport ----

func (c *Client) do(ctx context.Context, method, path string, body any, dst any) error {
	token := c.tokenFor()
	if token == "" {
		return ErrNotConfigured
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if remaining := resp.Header.Get("RateLimit-Remaining"); remaining != "" {
		if n, err := strconv.Atoi(remaining); err == nil && n < 20 {
			c.log.Warn("hetzner rate limit is nearly exhausted",
				"remaining", n, "resets_at", resp.Header.Get("RateLimit-Reset"))
		}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("hetzner rate limit hit (429), resets at %s", resp.Header.Get("RateLimit-Reset"))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		// The message is read before any status is interpreted. Hetzner answers
		// 403 for things that have nothing to do with the token — a full
		// project reports "server limit reached" — and reporting those as an
		// auth failure sends the reader to the wrong place entirely.
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error.Message != "" {
			return &APIError{
				Status:  resp.StatusCode,
				Code:    apiErr.Error.Code,
				Message: apiErr.Error.Message,
			}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("hetzner rejected the API token (http %d)", resp.StatusCode)
		}
		return fmt.Errorf("hetzner returned http %d: %s", resp.StatusCode, snippet(raw))
	}

	if dst == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("could not decode the hetzner response: %w (body: %s)", err, snippet(raw))
	}
	return nil
}

// normalise strips the IPv6 prefix length Hetzner reports (e.g. "2a01:…::/64").
func normalise(ip string) string {
	ip = strings.TrimSpace(ip)
	if i := strings.IndexByte(ip, '/'); i > 0 {
		ip = ip[:i]
	}
	return ip
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
