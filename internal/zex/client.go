// Package zex talks to the Zex VPN admin panel — the service that hands configs
// to the mobile app. It is what finally lets a replacement address reach users.
package zex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/settings"
)

// ErrNotConfigured means no panel URL or admin credentials are set.
var ErrNotConfigured = errors.New("the Zex panel is not configured")

// sessionSkew expires the cached token a little early so a call never goes out
// with a token that dies in flight.
const sessionSkew = 5 * time.Minute

type Client struct {
	cfg  *settings.Provider
	http *http.Client
	base string // overrides the configured URL, for tests

	mu      sync.Mutex
	token   string
	expires time.Time
}

func New(cfg *settings.Provider, timeout time.Duration) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: timeout,
			// The panel answers a successful login with a redirect and a
			// Set-Cookie; following it would drop the cookie we came for.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// WithBaseURL points the client at a different host, for tests.
func (c *Client) WithBaseURL(u string) *Client {
	c.base = strings.TrimRight(u, "/")
	return c
}

// Configured reports whether the panel can be reached right now.
func (c *Client) Configured() bool {
	return c.baseURL() != "" &&
		c.cfg.Get(settings.ZexAdminEmail) != "" &&
		c.cfg.Get(settings.ZexAdminPassword) != ""
}

func (c *Client) baseURL() string {
	if c.base != "" {
		return c.base
	}
	return strings.TrimRight(c.cfg.Get(settings.ZexBaseURL), "/")
}

// ---- session ----

// Login exchanges the admin credentials for a session token.
//
// The panel is a server-rendered admin UI: it sets an admin_token cookie rather
// than returning a token. Its auth middleware also accepts that same token as
// an Authorization: Bearer header, which is how every later call is made.
func (c *Client) Login(ctx context.Context) error {
	email := c.cfg.Get(settings.ZexAdminEmail)
	password := c.cfg.Get(settings.ZexAdminPassword)
	if c.baseURL() == "" || email == "" || password == "" {
		return ErrNotConfigured
	}

	form := url.Values{"email": {email}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL()+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	for _, ck := range resp.Cookies() {
		if ck.Name != "admin_token" || ck.Value == "" {
			continue
		}
		expires := ck.Expires
		if expires.IsZero() {
			// The panel always sets one, but a missing expiry must not mean
			// "never expires" — re-login hourly instead.
			expires = time.Now().Add(time.Hour)
		}

		c.mu.Lock()
		c.token, c.expires = ck.Value, expires
		c.mu.Unlock()
		return nil
	}

	// A failed login re-renders the form with 401 rather than redirecting.
	return fmt.Errorf("the panel rejected the admin credentials (http %d)", resp.StatusCode)
}

// session returns a valid token, logging in when needed.
func (c *Client) session(ctx context.Context) (string, error) {
	c.mu.Lock()
	token, expires := c.token, c.expires
	c.mu.Unlock()

	if token != "" && time.Now().Before(expires.Add(-sessionSkew)) {
		return token, nil
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

// forget drops the cached token so the next call logs in again.
func (c *Client) forget() {
	c.mu.Lock()
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()
}

// Ping verifies the URL and credentials, for the settings page's test button.
func (c *Client) Ping(ctx context.Context) error {
	c.forget()
	return c.Login(ctx)
}

// ---- operations ----

// SetActive shows or hides one config.
func (c *Client) SetActive(ctx context.Context, nodeID int, active bool) error {
	form := url.Values{"active": {strconv.FormatBool(active)}}
	return c.post(ctx, fmt.Sprintf("/admin/v2ray/%d/active", nodeID), form, nil)
}

// BulkActive shows or hides every config that serves one address, which is the
// unit a server is replaced in: one machine backs many configs.
func (c *Client) BulkActive(ctx context.Context, address string, active bool) (int, error) {
	form := url.Values{
		"address": {address},
		"active":  {strconv.FormatBool(active)},
	}
	var out struct {
		Updated int `json:"updated"`
	}
	if err := c.post(ctx, "/admin/v2ray/bulk-active", form, &out); err != nil {
		return 0, err
	}
	return out.Updated, nil
}

// ReplaceRequest moves every config off one address and onto another.
//
// There is deliberately no port here. The panel matches on address alone, so a
// port sent with the request is applied to every config on that machine — and a
// machine serving 443 and 8443 had its 8443 configs silently rewritten to 443,
// unverified, because the replacement was triggered by the 443 reading. The
// new server is a clone of the old one and already listens on the same ports,
// so the right port for each config is the one it already has. Leaving the
// field out is what makes that impossible to get wrong again.
type ReplaceRequest struct {
	OldAddress string
	NewAddress string
	Country    string
	Activate   bool
}

// ReplaceResult reports what the panel changed.
type ReplaceResult struct {
	Updated int      `json:"updated"`
	Skipped int      `json:"skipped"`
	IDs     []uint   `json:"ids"`
	Errors  []string `json:"errors"`
}

// ReplaceAddress performs the swap. The panel rewrites each config's raw link
// as well as its address column — without that the app would keep dialling the
// old, blocked IP.
func (c *Client) ReplaceAddress(ctx context.Context, req ReplaceRequest) (*ReplaceResult, error) {
	// new_port is never sent: the panel would apply it to every config on the
	// address, including the ones that live on a different port.
	form := url.Values{
		"old_address": {req.OldAddress},
		"new_address": {req.NewAddress},
		"country":     {req.Country},
		"activate":    {strconv.FormatBool(req.Activate)},
	}

	var out ReplaceResult
	if err := c.post(ctx, "/admin/v2ray/replace-address", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- transport ----

// post performs one admin call, logging in again once if the session expired
// between the check and the request.
func (c *Client) post(ctx context.Context, path string, form url.Values, dst any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}

	err := c.postOnce(ctx, path, form, dst)
	if errors.Is(err, errSessionExpired) {
		c.forget()
		return c.postOnce(ctx, path, form, dst)
	}
	return err
}

// errSessionExpired is internal: it only ever triggers one retry.
var errSessionExpired = errors.New("panel session expired")

func (c *Client) postOnce(ctx context.Context, path string, form url.Values, dst any) error {
	token, err := c.session(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	// The admin middleware redirects an unauthenticated caller to /login
	// instead of answering 401, so a redirect means the session is gone.
	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusFound ||
		resp.StatusCode == http.StatusSeeOther ||
		resp.StatusCode == http.StatusTemporaryRedirect {
		return errSessionExpired
	}
	if resp.StatusCode == http.StatusForbidden {
		return errors.New("the admin account lacks permission for this action")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("panel returned http %d: %s", resp.StatusCode, snippet(body))
	}

	if dst == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("could not decode the panel response: %w (body: %s)", err, snippet(body))
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
