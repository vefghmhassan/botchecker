package telebot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/splash"
)

// withScanner gives the harness an idle scanner. Nothing here starts a scan —
// these tests are about the message that follows one.
func (h *harness) withScanner(t *testing.T) *scanner.Scanner {
	t.Helper()
	cfg := &config.Config{Location: time.UTC}
	sc := scanner.New(cfg,
		splash.NewClient("http://127.0.0.1:1", "", "", time.Second),
		checkhost.NewClient("http://127.0.0.1:1", 0, time.Millisecond, time.Second),
		h.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.bot.d.Scanner = sc
	return sc
}

func TestProgressBar(t *testing.T) {
	cases := []struct {
		done, total int
		want        string
	}{
		{0, 10, "░░░░░░░░░░  0%"},
		{5, 10, "█████░░░░░  50%"},
		{10, 10, "██████████  100%"},
		{3, 12, "██░░░░░░░░  25%"},
		// A scan with nothing to do must not divide by zero.
		{0, 0, "░░░░░░░░░░  0%"},
		// Progress beyond the total cannot overflow the bar.
		{15, 10, "██████████  150%"},
	}
	for _, c := range cases {
		got := progressBar(scanner.Progress{Done: c.done, Total: c.total})
		if got != c.want {
			t.Errorf("progressBar(%d/%d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
}

func TestPhaseLabelFallsBackToTheRawPhase(t *testing.T) {
	if got := phaseLabel(i18n.EN, "probing"); got != "probing" {
		t.Errorf("known phase = %q", got)
	}
	// A phase added to the scanner later must still render something.
	if got := phaseLabel(i18n.EN, "brand-new-phase"); got != "brand-new-phase" {
		t.Errorf("unknown phase = %q, want it passed through", got)
	}
	if got := phaseLabel(i18n.FA, "probing"); got == "" || got == "probing" {
		t.Errorf("Persian phase was not translated: %q", got)
	}
}

// The whole point: the tapped message turns into the progress screen straight
// away, rather than telling the reader to come back later.
func TestFollowingAScanEditsTheMessageInPlace(t *testing.T) {
	h := newHarness(t, "100000001")
	h.withScanner(t)

	if err := h.bot.followScan(context.Background(), 1, 42, 7); err != nil {
		t.Fatalf("followScan: %v", err)
	}

	replies := h.tg.replies()
	if len(replies) == 0 {
		t.Fatal("nothing was sent")
	}
	first := replies[0]
	if first.Method != "editMessageText" {
		t.Fatalf("first reply was %q, want the tapped message to be edited", first.Method)
	}
	if !strings.Contains(first.Text, "7") {
		t.Errorf("progress screen does not name the scan: %q", first.Text)
	}
	if !strings.Contains(first.Text, "░") && !strings.Contains(first.Text, "█") {
		t.Errorf("progress screen has no bar: %q", first.Text)
	}
}

// An idle scanner means the scan is already over, so the follower must hand the
// message back to the menu instead of redrawing progress forever.
func TestFollowerStopsWhenTheScanIsNotRunning(t *testing.T) {
	h := newHarness(t, "100000001")
	h.withScanner(t)
	h.seed(t, "1.1.1.1", 443, "HEALTHY", "hetzner", 1)

	if err := h.bot.followScan(context.Background(), 1, 42, 7); err != nil {
		t.Fatalf("followScan: %v", err)
	}

	// Waited on the screen, not on the bookkeeping. The follower deregisters
	// itself before drawing the final menu — deliberately, so that draw is not
	// cancelled by the very render it performs — so an empty map does not yet
	// mean the reader has seen anything.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if got := h.tg.lastText(); got != "" && !strings.Contains(got, "░") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the screen is still the progress bar after the scan ended: %q",
				h.tg.lastText())
		}
		time.Sleep(50 * time.Millisecond)
	}

	h.bot.mu.Lock()
	n := len(h.bot.live)
	h.bot.mu.Unlock()
	if n != 0 {
		t.Errorf("%d follower(s) still registered after the scan ended", n)
	}
}

// Navigating away hands the message to whatever was tapped; a follower still
// writing to it would overwrite that screen a moment later.
func TestRenderingOverAFollowedMessageStopsIt(t *testing.T) {
	h := newHarness(t, "100000001")

	var mu sync.Mutex
	cancelled := false
	h.bot.mu.Lock()
	h.bot.live = map[liveKey]context.CancelFunc{
		{chat: 1, message: 42}: func() { mu.Lock(); cancelled = true; mu.Unlock() },
	}
	h.bot.mu.Unlock()

	if err := h.bot.render(context.Background(), 1, 42, "somewhere else", nil); err != nil {
		t.Fatalf("render: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !cancelled {
		t.Fatal("the follower was left running over a message the reader navigated away from")
	}
	h.bot.mu.Lock()
	defer h.bot.mu.Unlock()
	if len(h.bot.live) != 0 {
		t.Fatalf("the follower is still registered: %+v", h.bot.live)
	}
}

// A second tap must replace the follower, not stack another goroutine onto the
// same message.
func TestSecondTapReplacesTheFollower(t *testing.T) {
	h := newHarness(t, "100000001")

	var mu sync.Mutex
	stopped := 0
	h.bot.mu.Lock()
	h.bot.live = map[liveKey]context.CancelFunc{
		{chat: 1, message: 42}: func() { mu.Lock(); stopped++; mu.Unlock() },
	}
	h.bot.mu.Unlock()

	h.withScanner(t)
	if err := h.bot.followScan(context.Background(), 1, 42, 9); err != nil {
		t.Fatalf("followScan: %v", err)
	}

	mu.Lock()
	got := stopped
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the previous follower was stopped %d times, want 1", got)
	}
	h.bot.mu.Lock()
	n := len(h.bot.live)
	h.bot.mu.Unlock()
	if n > 1 {
		t.Fatalf("%d followers on one message", n)
	}
}

// A /scan command has no message to edit, so it must fall back to sending one.
func TestFollowWithoutAMessageSendsInstead(t *testing.T) {
	h := newHarness(t, "100000001")
	h.withScanner(t)

	if err := h.bot.followScan(context.Background(), 1, 0, 3); err != nil {
		t.Fatalf("followScan: %v", err)
	}
	replies := h.tg.replies()
	if len(replies) != 1 || replies[0].Method != "sendMessage" {
		t.Fatalf("got %+v, want a single sendMessage", replies)
	}
}

// The reader wants to know which endpoint is being checked right now, not just
// how far along the scan is.
func TestLiveFrameNamesTheEndpointBeingChecked(t *testing.T) {
	h := newHarness(t, "100000001")

	p := scanner.Progress{
		ScanID: 13, Running: true, Total: 13, Done: 4,
		Phase: "probing", Current: "5.161.158.200:443",
		StartedAt: time.Now().Add(-2*time.Minute - 14*time.Second),
	}

	for _, lang := range []string{"en", "fa"} {
		if err := h.set.Set("ui.language", lang); err != nil {
			t.Fatal(err)
		}
		got := h.bot.liveText(13, p)

		if !strings.Contains(got, "5.161.158.200:443") {
			t.Errorf("[%s] frame does not say which endpoint: %q", lang, got)
		}
		if !strings.Contains(got, "███░░░░░░░  30%") {
			t.Errorf("[%s] bar is wrong: %q", lang, got)
		}
		if !strings.Contains(got, "2m14s") && !strings.Contains(got, "۲m۱۴s") {
			t.Errorf("[%s] no elapsed time: %q", lang, got)
		}
	}
}

// Between deciding the scan is still running and drawing the frame, the scan
// must not be re-read: a finished scan would render as an empty screen.
func TestLiveFrameRendersTheSnapshotItWasGiven(t *testing.T) {
	h := newHarness(t, "100000001")
	h.withScanner(t) // idle: a second read of Progress would come back blank

	p := scanner.Progress{
		ScanID: 5, Running: true, Total: 8, Done: 8,
		Phase: "confirming", Current: "2.29.60.163:443",
	}
	got := h.bot.liveText(5, p)

	if !strings.Contains(got, "2.29.60.163:443") {
		t.Fatalf("the frame was re-read from the idle scanner: %q", got)
	}
	if !strings.Contains(got, "100%") {
		t.Fatalf("frame lost its counts: %q", got)
	}
}

// A blank phase or endpoint must not leave a dangling label on screen.
func TestLiveFrameOmitsWhatItDoesNotKnow(t *testing.T) {
	h := newHarness(t, "100000001")
	got := h.bot.liveText(1, scanner.Progress{Running: true, Total: 4})

	for _, unwanted := range []string{"Stage:", "Now checking:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("frame shows an empty %q: %q", unwanted, got)
		}
	}
}
