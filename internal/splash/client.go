package splash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL   string
	ClientKey string
	DeviceID  string
	HTTP      *http.Client

	mu    sync.Mutex
	token string
}

func NewClient(baseURL, clientKey, deviceID string, timeout time.Duration) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		ClientKey: clientKey,
		DeviceID:  deviceID,
		HTTP:      &http.Client{Timeout: timeout},
	}
}

type confRequest struct {
	ClientKey string `json:"client_key"`
	DeviceID  string `json:"device_id"`
}

// Fetch retrieves the current server list.
func (c *Client) Fetch(ctx context.Context) (*Response, error) {
	body, err := json.Marshal(confRequest{ClientKey: c.ClientKey, DeviceID: c.DeviceID})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	url := c.BaseURL + "/api/v1/splash/conf"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("splash request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read splash body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("splash returned %d: %s", resp.StatusCode, snippet(raw))
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode splash body: %w (body: %s)", err, snippet(raw))
	}
	return &out, nil
}

// Nodes lists every active config in the panel.
//
// This is the list to monitor. Fetch — the splash endpoint the app calls on
// launch — returns a random handful, a few configs chosen round-robin across
// addresses, because its job is to hand one phone something to connect to. It
// was never a directory. Monitoring from it meant watching whatever the dice
// picked: every scan got exactly 14 configs whatever the panel held, and ten
// servers added on 2026-09-23 never appeared at all.
//
// /api/v1/nodes needs a mobile token. It is taken as a guest with the same
// device id this client already presents to splash, so the service reads the
// panel as the same app identity it always has, and no admin credential is ever
// sent to the mobile API.
func (c *Client) Nodes(ctx context.Context) ([]ServerConfig, error) {
	nodes, err := c.nodesOnce(ctx)
	if errors.Is(err, errUnauthorized) {
		// The token lasts thirty days, but the panel can rotate its signing
		// key or drop the guest user. One fresh login, then give up.
		c.forgetToken()
		nodes, err = c.nodesOnce(ctx)
	}
	return nodes, err
}

var errUnauthorized = errors.New("the panel refused the token")

func (c *Client) nodesOnce(ctx context.Context) ([]ServerConfig, error) {
	token, err := c.guestToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/nodes", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nodes request: %w", err)
	}
	defer resp.Body.Close()

	// Larger than the splash limit: this is the whole fleet. A production-sized
	// panel is about 2 MB for 4,600 configs, so 64 MB leaves room for thirty
	// times that before anything is cut off.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read nodes body: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("nodes returned %d: %s", resp.StatusCode, snippet(raw))
	}

	var out struct {
		Nodes []ServerConfig `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode nodes body: %w (body: %s)", err, snippet(raw))
	}
	return out.Nodes, nil
}

// guestToken returns a cached mobile token, logging in when there is none.
func (c *Client) guestToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		return token, nil
	}

	if strings.TrimSpace(c.DeviceID) == "" {
		return "", errors.New("no device id is configured, so the panel cannot be asked for its node list")
	}
	body, _ := json.Marshal(map[string]string{"device_id": c.DeviceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/auth/no-login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("guest login: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("guest login returned %d: %s", resp.StatusCode, snippet(raw))
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("guest login returned no token: %s", snippet(raw))
	}

	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return out.Token, nil
}

func (c *Client) forgetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
