package telebot

import (
	"strings"
	"testing"

	"github.com/vefgh/botchecker/internal/notify"
)

// Telegram silently rejects a keyboard whose callback_data exceeds 64 bytes,
// so the ceiling is asserted against the worst case rather than a typical one.
func TestEncodedPayloadsFitTelegramsLimit(t *testing.T) {
	actions := []string{
		ActMenu, ActServerList, ActServer, ActBlocked, ActProviders, ActHistory,
		ActScan, ActStatus, ActNotifyOnly, ActRequestNew, ActNodeDisable,
		ActNodeEnable, ActNodeDelete, ActHetznerDel, ActHide, ActConfirm,
		ActCancel, ActNoop,
	}
	// A target id far larger than SQLite will ever hand out, and a token twice
	// the length the confirmation store produces.
	args := []any{nil, 1, int64(9223372036854775807), strings.Repeat("f", 16)}

	for _, a := range actions {
		for _, arg := range args {
			data := Encode(a, arg)
			if len(data) > notify.MaxCallbackData {
				t.Errorf("Encode(%s, %v) = %d bytes, over the %d-byte limit",
					a, arg, len(data), notify.MaxCallbackData)
			}
			if len(data) == 0 {
				t.Errorf("Encode(%s, %v) produced an empty payload", a, arg)
			}
		}
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		action string
		arg    any
		wantID int64
	}{
		{ActServer, 42, 42},
		{ActServerList, 3, 3},
		{ActConfirm, "a1b2c3d4", 0},
		{ActMenu, nil, 0},
	}
	for _, tc := range cases {
		cb, err := Decode(Encode(tc.action, tc.arg))
		if err != nil {
			t.Fatalf("Decode(Encode(%s,%v)): %v", tc.action, tc.arg, err)
		}
		if cb.Action != tc.action {
			t.Errorf("action = %q, want %q", cb.Action, tc.action)
		}
		if cb.ID() != tc.wantID {
			t.Errorf("ID() = %d, want %d", cb.ID(), tc.wantID)
		}
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "garbage", "1|", "1||", "2|sv|1", "sv|1"} {
		if _, err := Decode(in); err == nil {
			t.Errorf("Decode(%q) succeeded, want an error", in)
		}
	}
}

// A button from an older release must not be reinterpreted as a current
// action — the codes could have been reused.
func TestDecodeRejectsOtherVersions(t *testing.T) {
	if _, err := Decode("0|dh|5"); err == nil {
		t.Fatal("a payload from another version was accepted")
	}
}

func TestPageDefaultsToFirst(t *testing.T) {
	for _, in := range []string{"", "0", "-2", "abc"} {
		cb := Callback{Action: ActServerList, Arg: in}
		if got := cb.Page(); got != 1 {
			t.Errorf("Page(%q) = %d, want 1", in, got)
		}
	}
	if got := (Callback{Arg: "4"}).Page(); got != 4 {
		t.Errorf("Page(\"4\") = %d, want 4", got)
	}
}
