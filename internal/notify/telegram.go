// Package notify talks to the Telegram Bot API: it delivers alerts, and
// carries the transport the interactive bot is built on.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/settings"
)

// ErrNotConfigured means no bot token or chat id is set. It is not a failure
// of the alert itself and callers treat it as "nothing to do".
var ErrNotConfigured = errors.New("telegram is not configured")

// ErrConflict means another process is already long-polling this bot.
var ErrConflict = errors.New("another instance is already polling this bot")

// ErrNotModified means an edit would not change the message. Telegram treats
// that as an error; for a menu that repaints itself it means "already correct".
var ErrNotModified = errors.New("the message is already showing this")

const (
	tokenKey  = settings.TelegramBotToken
	chatIDKey = settings.TelegramChatID
)

type Telegram struct {
	cfg  *settings.Provider
	http *http.Client
	// baseURL is overridden in tests; empty means the real Telegram API.
	baseURL string
}

func NewTelegram(cfg *settings.Provider, timeout time.Duration) *Telegram {
	return &Telegram{cfg: cfg, http: &http.Client{Timeout: timeout}}
}

// WithBaseURL points the client at a different host, for tests.
func (t *Telegram) WithBaseURL(u string) *Telegram {
	t.baseURL = strings.TrimRight(u, "/")
	return t
}

// Configured reports whether an alert could be delivered right now.
func (t *Telegram) Configured() bool {
	return t.cfg.Get(tokenKey) != "" && t.cfg.Get(chatIDKey) != ""
}

// HasToken reports whether a bot token is set, regardless of the chat id.
// The interactive bot replies to whoever wrote to it, so it needs only this.
func (t *Telegram) HasToken() bool { return t.cfg.Get(tokenKey) != "" }

// Send delivers one HTML alert to the configured chat.
func (t *Telegram) Send(ctx context.Context, html string) error {
	if !t.Configured() {
		return ErrNotConfigured
	}
	_, err := t.SendMessage(ctx, 0, html, nil)
	return err
}

// call performs one Bot API method with the default client.
func (t *Telegram) call(ctx context.Context, method string, body any, dst any) error {
	return t.callWithClient(ctx, t.http, method, body, dst)
}

// callWithClient performs one Bot API method. Credentials are read per call so
// a token changed in the dashboard takes effect without a restart.
func (t *Telegram) callWithClient(ctx context.Context, client *http.Client, method string, body any, dst any) error {
	token := t.cfg.Get(tokenKey)
	if token == "" {
		return ErrNotConfigured
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	base := t.baseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", base, token, method)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Result      json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(payload, &out)

	if resp.StatusCode == http.StatusConflict || out.ErrorCode == http.StatusConflict {
		return ErrConflict
	}
	if strings.Contains(strings.ToLower(out.Description), "message is not modified") {
		return ErrNotModified
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !out.OK {
		desc := out.Description
		if desc == "" {
			desc = strings.TrimSpace(string(payload))
		}
		// The token must never end up in a log line or a stored error.
		return fmt.Errorf("telegram rejected %s (http %d): %s", method, resp.StatusCode, truncate(desc, 200))
	}

	if dst == nil || len(out.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(out.Result, dst); err != nil {
		return fmt.Errorf("could not decode the %s result: %w", method, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
