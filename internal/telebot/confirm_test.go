package telebot

import (
	"errors"
	"testing"
	"time"
)

func TestConfirmationIsSingleUse(t *testing.T) {
	c := newConfirmations()
	token := c.put(pending{Action: ActHetznerDel, TargetID: 7, UserID: 100})

	if _, err := c.take(token, 100); err != nil {
		t.Fatalf("first take: %v", err)
	}
	// Replaying the same token must not run the action a second time.
	if _, err := c.take(token, 100); !errors.Is(err, errNoSuchConfirmation) {
		t.Fatalf("second take err = %v, want it to be gone", err)
	}
}

func TestConfirmationBelongsToOneUser(t *testing.T) {
	c := newConfirmations()
	token := c.put(pending{Action: ActNodeDelete, TargetID: 3, UserID: 100})

	if _, err := c.take(token, 999); !errors.Is(err, errNotYours) {
		t.Fatalf("err = %v, want errNotYours", err)
	}
	// It is consumed even on rejection, so a wrong tap cannot be probed and
	// then used by the rightful owner's token being left lying around.
	if _, err := c.take(token, 100); !errors.Is(err, errNoSuchConfirmation) {
		t.Fatalf("token survived a rejected take: %v", err)
	}
}

func TestConfirmationExpires(t *testing.T) {
	c := newConfirmations()
	token := c.put(pending{Action: ActHide, TargetID: 1, UserID: 100})

	c.mu.Lock()
	p := c.byID[token]
	p.CreatedAt = time.Now().Add(-2 * confirmTTL)
	c.byID[token] = p
	c.mu.Unlock()

	if _, err := c.take(token, 100); !errors.Is(err, errNoSuchConfirmation) {
		t.Fatalf("err = %v, want an expiry error", err)
	}
}

func TestExpiredConfirmationsAreSweptAway(t *testing.T) {
	c := newConfirmations()
	stale := c.put(pending{Action: ActHide, UserID: 1})

	c.mu.Lock()
	p := c.byID[stale]
	p.CreatedAt = time.Now().Add(-2 * confirmTTL)
	c.byID[stale] = p
	c.mu.Unlock()

	c.put(pending{Action: ActHide, UserID: 1}) // any write sweeps
	if got := c.len(); got != 1 {
		t.Fatalf("held %d confirmations, want only the fresh one", got)
	}
}

func TestCancelDropsWithoutRunning(t *testing.T) {
	c := newConfirmations()
	token := c.put(pending{Action: ActNodeDelete, TargetID: 5, UserID: 100})

	p, ok := c.drop(token, 100)
	if !ok || p.TargetID != 5 {
		t.Fatalf("drop = %+v, %v; want the pending action back", p, ok)
	}
	if _, err := c.take(token, 100); err == nil {
		t.Fatal("a cancelled confirmation could still be run")
	}
}

func TestCancelIgnoresOtherUsers(t *testing.T) {
	c := newConfirmations()
	token := c.put(pending{Action: ActNodeDelete, UserID: 100})

	if _, ok := c.drop(token, 999); ok {
		t.Fatal("another user cancelled someone else's confirmation")
	}
	if _, err := c.take(token, 100); err != nil {
		t.Fatalf("the owner's confirmation was destroyed by a stranger: %v", err)
	}
}

func TestTokensAreDistinct(t *testing.T) {
	c := newConfirmations()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok := c.put(pending{Action: ActHide, UserID: 1})
		if seen[tok] {
			t.Fatalf("token %q was issued twice", tok)
		}
		seen[tok] = true
	}
}
