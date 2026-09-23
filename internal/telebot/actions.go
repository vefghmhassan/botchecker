package telebot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// confirmTTL is how long a pending confirmation stays valid. Short on purpose:
// a destructive button left open on a phone should expire, not wait.
const confirmTTL = 60 * time.Second

// These reach the user, so the bot translates them when rendering.
var (
	errNoSuchConfirmation = errors.New("bot.expiredconfirm")
	errNotYours           = errors.New("bot.notyours")
)

// pending is one action waiting for its second tap.
type pending struct {
	Action    string
	TargetID  int64
	UserID    int64
	Summary   string
	AuditID   int64
	CreatedAt time.Time
}

// confirmations holds actions between the first tap and the confirmation.
// It lives in memory only: a restart drops every pending action, which is the
// safe direction to fail.
type confirmations struct {
	mu   sync.Mutex
	byID map[string]pending
}

func newConfirmations() *confirmations {
	return &confirmations{byID: map[string]pending{}}
}

// put stores an action and returns the token its buttons carry.
func (c *confirmations) put(p pending) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()

	token := randomToken()
	p.CreatedAt = time.Now()
	c.byID[token] = p
	return token
}

// take consumes a confirmation. It is single-use: the entry is removed whether
// or not the caller turns out to be allowed, so a token cannot be replayed.
func (c *confirmations) take(token string, userID int64) (pending, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()

	p, ok := c.byID[token]
	if !ok {
		return pending{}, errNoSuchConfirmation
	}
	delete(c.byID, token)

	if p.UserID != userID {
		return pending{}, errNotYours
	}
	if time.Since(p.CreatedAt) > confirmTTL {
		return pending{}, errNoSuchConfirmation
	}
	return p, nil
}

// drop removes a confirmation without running it.
func (c *confirmations) drop(token string, userID int64) (pending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	p, ok := c.byID[token]
	if !ok || p.UserID != userID {
		return pending{}, false
	}
	delete(c.byID, token)
	return p, true
}

func (c *confirmations) sweepLocked() {
	for token, p := range c.byID {
		if time.Since(p.CreatedAt) > confirmTTL {
			delete(c.byID, token)
		}
	}
}

func (c *confirmations) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byID)
}

func randomToken() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// A predictable token is still bound to a user id and a 60s window,
		// and the alternative is failing the whole interaction.
		return hex.EncodeToString([]byte(time.Now().Format("150405.00")))[:8]
	}
	return hex.EncodeToString(b)
}
