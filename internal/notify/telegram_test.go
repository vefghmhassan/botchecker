package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/settings"

	_ "modernc.org/sqlite"
)

func testSettings(t *testing.T, token, chat string) *settings.Provider {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value BLOB,
		secret INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	p, err := settings.New(db, "", filepath.Join(dir, "k"), nil)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if token != "" {
		if err := p.Set(settings.TelegramBotToken, token); err != nil {
			t.Fatal(err)
		}
	}
	if chat != "" {
		if err := p.Set(settings.TelegramChatID, chat); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestSendDeliversMessage(t *testing.T) {
	var gotPath string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	tg := NewTelegram(testSettings(t, "123:ABC", "-100777"), 5*time.Second).WithBaseURL(srv.URL)
	if err := tg.Send(context.Background(), "<b>hello</b>"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != "/bot123:ABC/sendMessage" {
		t.Errorf("path = %q, want the bot token in the path", gotPath)
	}
	if body["chat_id"] != "-100777" {
		t.Errorf("chat_id = %v, want -100777", body["chat_id"])
	}
	if body["parse_mode"] != "HTML" {
		t.Errorf("parse_mode = %v, want HTML", body["parse_mode"])
	}
}

func TestSendReportsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"chat not found"}`)
	}))
	defer srv.Close()

	tg := NewTelegram(testSettings(t, "123:ABC", "-1"), 5*time.Second).WithBaseURL(srv.URL)
	err := tg.Send(context.Background(), "hi")
	if err == nil {
		t.Fatal("Send succeeded on a 400")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error = %v, want it to carry the API's reason", err)
	}
	// The token must not end up in an error that gets stored and displayed.
	if strings.Contains(err.Error(), "123:ABC") {
		t.Error("the error message leaks the bot token")
	}
}

func TestSendWithoutCredentials(t *testing.T) {
	tg := NewTelegram(testSettings(t, "", ""), time.Second)

	if tg.Configured() {
		t.Error("Configured() = true with no credentials")
	}
	if err := tg.Send(context.Background(), "hi"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// A token changed in the dashboard must apply to the next call, without a
// restart — credentials are read per request, not captured at construction.
func TestCredentialsAreReadPerCall(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	set := testSettings(t, "first:token", "1")
	tg := NewTelegram(set, 5*time.Second).WithBaseURL(srv.URL)

	if err := tg.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := set.Set(settings.TelegramBotToken, "second:token"); err != nil {
		t.Fatal(err)
	}
	if err := tg.Send(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 || !strings.Contains(seen[1], "second:token") {
		t.Fatalf("paths = %v, want the second call to use the new token", seen)
	}
}

// A menu that repaints itself would otherwise post a duplicate every time the
// same button is tapped twice.
func TestUnchangedEditIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,`+
			`"description":"Bad Request: message is not modified: specified new message content..."}`)
	}))
	defer srv.Close()

	tg := NewTelegram(testSettings(t, "1:a", "5"), 5*time.Second).WithBaseURL(srv.URL)
	err := tg.EditMessageText(context.Background(), 5, 9, "same text", nil)
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("err = %v, want ErrNotModified", err)
	}
}

// Two pollers on one bot is a configuration mistake, not a transient failure.
func TestConflictIsReportedDistinctly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":409,`+
			`"description":"Conflict: terminated by other getUpdates request"}`)
	}))
	defer srv.Close()

	tg := NewTelegram(testSettings(t, "1:a", "5"), 5*time.Second).WithBaseURL(srv.URL)
	_, err := tg.GetUpdates(context.Background(), 0, time.Second)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCallbackDataLimitIsExported(t *testing.T) {
	if MaxCallbackData != 64 || MaxMessageLength != 4096 {
		t.Fatalf("limits drifted: callback=%d message=%d", MaxCallbackData, MaxMessageLength)
	}
}

func TestClampTrimsLongMessages(t *testing.T) {
	long := strings.Repeat("x", MaxMessageLength+500)
	got := Clamp(long)
	if len(got) > MaxMessageLength {
		t.Fatalf("Clamp returned %d characters, over the limit", len(got))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Error("a trimmed message does not say it was cut")
	}
	if short := "hello"; Clamp(short) != short {
		t.Error("Clamp altered a short message")
	}
}
