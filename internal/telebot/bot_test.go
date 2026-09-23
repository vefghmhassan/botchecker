package telebot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/splash"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

const ownerID = 100000001

// telegramSpy records everything the bot sends.
type telegramSpy struct {
	mu    sync.Mutex
	calls []call
}

type call struct {
	Method string
	Text   string
	Data   map[string]any
}

func (s *telegramSpy) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		s.mu.Lock()
		text, _ := body["text"].(string)
		s.calls = append(s.calls, call{Method: method, Text: text, Data: body})
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *telegramSpy) sent() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.calls...)
}

// replies counts only the calls that put something in front of a user.
func (s *telegramSpy) replies() []call {
	var out []call
	for _, c := range s.sent() {
		if c.Method == "sendMessage" || c.Method == "editMessageText" {
			out = append(out, c)
		}
	}
	return out
}

func (s *telegramSpy) lastText() string {
	r := s.replies()
	if len(r) == 0 {
		return ""
	}
	return r[len(r)-1].Text
}

func (s *telegramSpy) lastKeyboard(t *testing.T) []notify.Button {
	t.Helper()
	r := s.replies()
	if len(r) == 0 {
		return nil
	}
	raw, err := json.Marshal(r[len(r)-1].Data["reply_markup"])
	if err != nil {
		return nil
	}
	var kb struct {
		Rows [][]notify.Button `json:"inline_keyboard"`
	}
	_ = json.Unmarshal(raw, &kb)

	var flat []notify.Button
	for _, row := range kb.Rows {
		flat = append(flat, row...)
	}
	return flat
}

func (s *telegramSpy) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

type harness struct {
	bot *Bot
	tg  *telegramSpy
	st  *store.Store
	set *settings.Provider
	ids map[string]int64
}

func newHarness(t *testing.T, allowed string) *harness {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	set, err := settings.New(st.DB(), "", filepath.Join(dir, "k"), nil)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	for k, v := range map[string]string{
		settings.TelegramBotToken:  "1:tok",
		settings.TelegramChatID:    "100000001",
		settings.TelegramBotEnable: "true",
		settings.TelegramAllowedID: allowed,
	} {
		if v == "" {
			continue
		}
		if err := set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	spy := &telegramSpy{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tg := notify.NewTelegram(set, 5*time.Second).WithBaseURL(spy.server(t).URL)

	b := New(Deps{
		Config:   &config.Config{Location: time.UTC, OutageWindow: 30 * time.Minute},
		Settings: set, Store: st, Telegram: tg,
		Zex: zex.New(set, time.Second), Log: log,
	})
	return &harness{bot: b, tg: spy, st: st, set: set, ids: map[string]int64{}}
}

// seed registers an endpoint with a verdict and ownership.
func (h *harness) seed(t *testing.T, address string, port int, verdict, provider string, hzServerID int64) int64 {
	t.Helper()
	id, err := h.st.UpsertTarget(splash.Target{
		Address: address, Port: port, ConfigIDs: []int{1}, Names: []string{"cfg"}, Active: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	scanID, err := h.st.CreateScan("manual", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.InsertResult(store.ResultRecord{
		ScanID: scanID, TargetID: id, Verdict: verdict,
		IRTotal: 8, ControlOpen: 2, ControlTotal: 2, CheckedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.RecordVerdict(id, verdict, scanID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.FinishScan(scanID, store.ScanCompleted, 0, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetAddressProvider(address, provider, "default", "server",
		hzServerID, "edge-1", false, time.Now()); err != nil {
		t.Fatal(err)
	}

	h.ids[address] = id
	return id
}

func message(userID int64, text string) notify.Update {
	return notify.Update{
		UpdateID: 1,
		Message: &notify.Message{
			MessageID: 10, Text: text,
			From: &notify.User{ID: userID, Username: "tester"},
			Chat: &notify.Chat{ID: userID, Type: "private"},
		},
	}
}

func press(userID int64, data string) notify.Update {
	return notify.Update{
		UpdateID: 2,
		CallbackQuery: &notify.CallbackQuery{
			ID: "cb1", Data: data,
			From:    &notify.User{ID: userID, Username: "tester"},
			Message: &notify.Message{MessageID: 10, Chat: &notify.Chat{ID: userID, Type: "private"}},
		},
	}
}

// ---- tests ----

// The bot must not answer a stranger at all: a reply would confirm it exists.
func TestUnauthorizedGetsNoReply(t *testing.T) {
	h := newHarness(t, "100000001")
	h.bot.handle(context.Background(), message(999999, "/start"))

	if got := h.tg.replies(); len(got) != 0 {
		t.Fatalf("bot replied to an unauthorized id: %+v", got)
	}

	actions, err := h.st.BotActions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("recorded %d actions, want 1", len(actions))
	}
	if actions[0].Authorized {
		t.Error("the attempt was recorded as authorized")
	}
	if actions[0].TelegramID != 999999 {
		t.Errorf("recorded id = %d, want the sender's", actions[0].TelegramID)
	}
}

func TestAllowedIDsFallBackToChatID(t *testing.T) {
	h := newHarness(t, "")
	ids := h.bot.AllowedIDs()
	if len(ids) != 1 || ids[0] != ownerID {
		t.Fatalf("AllowedIDs = %v, want the chat id as the fallback", ids)
	}
}

func TestLoopRefusesToStartWithoutAnAllowList(t *testing.T) {
	h := newHarness(t, "")
	// Clear both the allow list and the chat id: nobody is permitted.
	if err := h.set.Clear(settings.TelegramChatID); err != nil {
		t.Fatal(err)
	}
	if len(h.bot.AllowedIDs()) != 0 {
		t.Fatalf("AllowedIDs = %v, want none", h.bot.AllowedIDs())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.bot.Start(ctx)

	time.Sleep(50 * time.Millisecond)
	if h.bot.Running() {
		t.Fatal("the command loop started with no allowed ids")
	}
}

func TestStartShowsTheMenu(t *testing.T) {
	h := newHarness(t, "100000001")
	h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	h.bot.handle(context.Background(), message(ownerID, "/start"))

	text := h.tg.lastText()
	if !strings.Contains(text, "botchecker") || !strings.Contains(text, "1 blocked") {
		t.Fatalf("menu text did not summarise the fleet:\n%s", text)
	}
	if len(h.tg.lastKeyboard(t)) == 0 {
		t.Error("the menu came without buttons")
	}
}

func TestServerDetailAndReplaceButtonDependsOnOwnership(t *testing.T) {
	h := newHarness(t, "100000001")
	mine := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	theirs := h.seed(t, "174.138.13.95", 443, "BLOCKED_IR", "external", 0)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, mine)))
	if !hasAction(h.tg.lastKeyboard(t), ActRequestNew) {
		t.Error("an endpoint in the Hetzner project has no replacement button")
	}

	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, theirs)))
	if hasAction(h.tg.lastKeyboard(t), ActRequestNew) {
		t.Error("an endpoint outside the project offers a replacement it cannot create")
	}
	if hasAction(h.tg.lastKeyboard(t), ActHetznerDel) {
		t.Error("an endpoint outside the project offers to destroy a Hetzner server")
	}
}

func TestDestructiveActionAsksFirst(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHide, id)))

	text := h.tg.lastText()
	if !strings.Contains(text, "Confirm") {
		t.Fatalf("no confirmation screen:\n%s", text)
	}
	if !hasAction(h.tg.lastKeyboard(t), ActConfirm) || !hasAction(h.tg.lastKeyboard(t), ActCancel) {
		t.Error("the confirmation has no confirm/cancel pair")
	}
	if h.bot.pending.len() != 1 {
		t.Errorf("pending confirmations = %d, want 1", h.bot.pending.len())
	}
}

func TestCancelRunsNothing(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHide, id)))
	token := confirmToken(h.tg.lastKeyboard(t))
	if token == "" {
		t.Fatal("no confirmation token in the keyboard")
	}

	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActCancel, token)))

	if h.bot.pending.len() != 0 {
		t.Error("the confirmation survived a cancel")
	}
	if strings.Contains(h.tg.lastText(), "✅") {
		t.Errorf("cancelling reported success:\n%s", h.tg.lastText())
	}
}

// Without a panel the hide control is not offered at all — a button that can
// only fail is worse than no button.
func TestHideButtonHiddenWithoutAPanel(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, id)))
	if hasAction(h.tg.lastKeyboard(t), ActHide) {
		t.Error("the hide button appeared without a configured panel")
	}
}

// With the panel wired up, hiding actually stops the configs being served.
func TestHideTakesConfigsOutOfCirculation(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	var lastActive string
	h.withZex(t, func(active string) int { lastActive = active; return 4 })

	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, id)))
	if !hasAction(h.tg.lastKeyboard(t), ActHide) {
		t.Fatal("the hide button is missing with a configured panel")
	}

	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActHide, id)))
	token := confirmToken(h.tg.lastKeyboard(t))
	if token == "" {
		t.Fatal("hiding did not ask for confirmation")
	}

	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActConfirm, token)))

	if lastActive != "false" {
		t.Errorf("panel was asked for active=%q, want false", lastActive)
	}
	if !strings.Contains(h.tg.lastText(), "4 config") {
		t.Errorf("the result does not say how many configs moved:\n%s", h.tg.lastText())
	}
}

func TestNotifyOnlyTogglesWithoutConfirmation(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActNotifyOnly, id)))

	on, err := h.st.NotifyOnly(id)
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Fatal("the flag was not set")
	}
	if h.bot.pending.len() != 0 {
		t.Error("a reversible toggle asked for confirmation")
	}
}

func TestConfirmationFromAnotherUserIsRejected(t *testing.T) {
	h := newHarness(t, "100000001,111222")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHide, id)))
	token := confirmToken(h.tg.lastKeyboard(t))

	h.tg.reset()
	// 111222 is on the allow list but did not create this confirmation.
	h.bot.handle(context.Background(), press(111222, Encode(ActConfirm, token)))

	if !strings.Contains(h.tg.lastText(), "belongs to someone else") {
		t.Fatalf("another user's confirmation was accepted:\n%s", h.tg.lastText())
	}
}

func hasAction(buttons []notify.Button, action string) bool {
	for _, b := range buttons {
		if cb, err := Decode(b.Data); err == nil && cb.Action == action {
			return true
		}
	}
	return false
}

func confirmToken(buttons []notify.Button) string {
	for _, b := range buttons {
		if cb, err := Decode(b.Data); err == nil && cb.Action == ActConfirm {
			return cb.Arg
		}
	}
	return ""
}

// withZex attaches a configured Zex client whose bulk-active call is observed.
func (h *harness) withZex(t *testing.T, onBulk func(active string) int) {
	t.Helper()
	for k, v := range map[string]string{
		settings.ZexAdminEmail:    "admin@example.com",
		settings.ZexAdminPassword: "pw",
	} {
		if err := h.set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: "admin_token", Value: "jwt", Expires: time.Now().Add(time.Hour),
		})
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/admin/v2ray/bulk-active", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeJSON(w, map[string]any{"updated": onBulk(r.FormValue("active"))})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if err := h.set.Set(settings.ZexBaseURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	h.bot.d.Zex = zex.New(h.set, 5*time.Second).WithBaseURL(srv.URL)
}

// fakePanel serves a 3x-ui node list with a chosen online count.
func fakePanel(t *testing.T, address string, nodeID, online int, enabled bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"success": true,
			"obj": []map[string]any{{
				"id": nodeID, "name": "edge-1", "address": address, "port": 2053,
				"enable": enabled, "status": "online", "onlineCount": online,
				"clientCount": 25,
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// withPanel attaches a configured 3x-ui client to the harness.
func (h *harness) withPanel(t *testing.T, address string, nodeID, online int, enabled bool) {
	t.Helper()
	if err := h.set.Set(settings.XUIAPIToken, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := h.set.Set(settings.XUIBaseURL, "http://panel.invalid"); err != nil {
		t.Fatal(err)
	}
	h.bot.d.Panel = xui.New(h.set, 5*time.Second).
		WithBaseURL(fakePanel(t, address, nodeID, online, enabled).URL)
}

// withHetzner attaches a configured Hetzner client that records deletions.
func (h *harness) withHetzner(t *testing.T, deleted *int) {
	t.Helper()
	if err := h.set.Set(settings.HetznerToken, "tok"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			*deleted++
			writeJSON(w, map[string]any{"action": map[string]any{"id": 1, "status": "success"}})
			return
		}
		// A single-server read answers the identity check the delete makes
		// before it fires.
		if strings.Contains(r.URL.Path, "/servers/") {
			writeJSON(w, map[string]any{"server": map[string]any{
				"id": 166561100, "name": "edge-1", "status": "running",
				"public_net":  map[string]any{"ipv4": map[string]any{"ip": "5.161.158.200"}},
				"server_type": map[string]any{"name": "cpx31"},
				"datacenter":  map[string]any{"location": map[string]any{"name": "ash"}},
			}})
			return
		}
		writeJSON(w, map[string]any{"servers": []any{}, "meta": map[string]any{}})
	}))
	t.Cleanup(srv.Close)

	h.bot.d.Hetzner = hetzner.NewRegistry(h.set,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, time.Minute).
		WithBaseURL(srv.URL)
}

// Destroying a machine that still has users on it is never what was meant, so
// the action is refused outright rather than merely warned about.
func TestHetznerDeleteRefusedWhileUsersAreOnline(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	h.withPanel(t, "5.161.158.200", 7, 3, true)

	deleted := 0
	h.withHetzner(t, &deleted)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHetznerDel, id)))

	text := h.tg.lastText()
	if !strings.Contains(text, "3 user(s) are still connected") {
		t.Fatalf("the refusal did not say why:\n%s", text)
	}
	if strings.Contains(text, "Confirm") {
		t.Error("it offered a confirmation for an action it will not run")
	}
	if h.bot.pending.len() != 0 {
		t.Error("a confirmation was stored for a refused action")
	}
	if deleted != 0 {
		t.Fatal("a server was destroyed despite connected users")
	}
}

func TestHetznerDeleteProceedsWhenEmpty(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	h.withPanel(t, "5.161.158.200", 7, 0, true)

	deleted := 0
	h.withHetzner(t, &deleted)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHetznerDel, id)))
	if !strings.Contains(h.tg.lastText(), "cannot be undone") {
		t.Fatalf("the confirmation did not spell out the consequence:\n%s", h.tg.lastText())
	}
	token := confirmToken(h.tg.lastKeyboard(t))
	if token == "" {
		t.Fatal("no confirmation token")
	}

	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActConfirm, token)))

	if deleted != 1 {
		t.Fatalf("delete calls = %d, want 1", deleted)
	}
	if !strings.Contains(h.tg.lastText(), "destroyed") {
		t.Errorf("no confirmation of the deletion:\n%s", h.tg.lastText())
	}
}

// The panel supplies the online count, so its buttons only appear with it.
func TestPanelButtonsOnlyWithAPanel(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, id)))
	if hasAction(h.tg.lastKeyboard(t), ActNodeDelete) {
		t.Error("panel controls appeared without a configured panel")
	}

	h.withPanel(t, "5.161.158.200", 7, 0, true)
	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, id)))
	if !hasAction(h.tg.lastKeyboard(t), ActNodeDelete) {
		t.Error("panel controls missing with a configured panel")
	}
	if !strings.Contains(h.tg.lastText(), "0 user(s) online") {
		t.Errorf("the online count is missing from the detail screen:\n%s", h.tg.lastText())
	}
}

// The bot follows the same global language setting as the dashboard.
func TestBotSpeaksPersianWhenSelected(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)

	if err := h.set.Set(settings.UILanguage, "fa"); err != nil {
		t.Fatal(err)
	}

	h.bot.handle(context.Background(), message(ownerID, "/start"))
	if !strings.Contains(h.tg.lastText(), "سالم") {
		t.Fatalf("the menu is still English:\n%s", h.tg.lastText())
	}
	for _, b := range h.tg.lastKeyboard(t) {
		if strings.Contains(b.Text, "Servers") {
			t.Errorf("a button is still English: %q", b.Text)
		}
	}

	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActServer, id)))
	text := h.tg.lastText()
	if !strings.Contains(text, "حکم:") {
		t.Errorf("the detail screen is still English:\n%s", text)
	}
	// The address must stay in ASCII so it can be read and copied.
	if !strings.Contains(text, "5.161.158.200:443") {
		t.Errorf("the address was mangled by localisation:\n%s", text)
	}
}

func TestConfirmationIsTranslated(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	if err := h.set.Set(settings.UILanguage, "fa"); err != nil {
		t.Fatal(err)
	}

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHide, id)))
	text := h.tg.lastText()
	if !strings.Contains(text, "تأیید") {
		t.Fatalf("the confirmation is still English:\n%s", text)
	}
	if strings.Contains(text, "This expires") {
		t.Errorf("English leaked into the confirmation:\n%s", text)
	}
}

// An expired confirmation reports through a translation key, so it must not
// surface the key itself to the user.
func TestExpiredConfirmationIsTranslated(t *testing.T) {
	h := newHarness(t, "100000001")
	if err := h.set.Set(settings.UILanguage, "fa"); err != nil {
		t.Fatal(err)
	}

	h.bot.handle(context.Background(), press(ownerID, Encode(ActConfirm, "deadbeef")))
	text := h.tg.lastText()
	if strings.Contains(text, "bot.expiredconfirm") {
		t.Fatalf("a raw translation key reached the user:\n%s", text)
	}
	if !strings.Contains(text, "منقضی") {
		t.Errorf("the expiry message was not translated:\n%s", text)
	}
}

// A Hetzner server id only means something inside its own project — the same
// number names a different machine in every other account. Without a recorded
// project there is no safe client to send the delete to, and guessing one is
// how the wrong machine gets destroyed.
func TestHetznerDeleteRefusedWithoutAProject(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	h.withPanel(t, "5.161.158.200", 7, 0, true)

	// Ownership recorded the old way, before projects existed.
	if err := h.st.SetAddressProvider("5.161.158.200", "hetzner", "", "server",
		166561100, "edge-1", false, time.Now()); err != nil {
		t.Fatal(err)
	}

	deleted := 0
	h.withHetzner(t, &deleted)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHetznerDel, id)))
	token := confirmToken(h.tg.lastKeyboard(t))
	if token == "" {
		t.Fatal("no confirmation token")
	}
	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActConfirm, token)))

	if deleted != 0 {
		t.Fatalf("%d delete(s) were sent without knowing which project owns the server", deleted)
	}
	if !strings.Contains(h.tg.lastText(), "Refresh ownership") {
		t.Errorf("the refusal does not say how to fix it:\n%s", h.tg.lastText())
	}
}

// A stored server id can go stale — the machine was rebuilt, or replaced
// outside this service. Deleting by that id alone would destroy whatever holds
// it now, so the id is checked against the address before anything happens.
func TestHetznerDeleteRefusedWhenTheIDNowNamesSomethingElse(t *testing.T) {
	h := newHarness(t, "100000001")
	id := h.seed(t, "5.161.158.200", 443, "BLOCKED_IR", "hetzner", 166561100)
	h.withPanel(t, "5.161.158.200", 7, 0, true)

	deleted := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted++
			writeJSON(w, map[string]any{"action": map[string]any{"id": 1, "status": "success"}})
			return
		}
		if strings.Contains(r.URL.Path, "/servers/") {
			// The id now answers on a different address.
			writeJSON(w, map[string]any{"server": map[string]any{
				"id": 166561100, "name": "someone-elses-box", "status": "running",
				"public_net":  map[string]any{"ipv4": map[string]any{"ip": "203.0.113.9"}},
				"server_type": map[string]any{"name": "cpx31"},
				"datacenter":  map[string]any{"location": map[string]any{"name": "ash"}},
			}})
			return
		}
		writeJSON(w, map[string]any{"servers": []any{}, "meta": map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	if err := h.set.Set(settings.HetznerToken, "tok"); err != nil {
		t.Fatal(err)
	}
	h.bot.d.Hetzner = hetzner.NewRegistry(h.set,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, time.Minute).
		WithBaseURL(srv.URL)

	h.bot.handle(context.Background(), press(ownerID, Encode(ActHetznerDel, id)))
	token := confirmToken(h.tg.lastKeyboard(t))
	h.tg.reset()
	h.bot.handle(context.Background(), press(ownerID, Encode(ActConfirm, token)))

	if deleted != 0 {
		t.Fatalf("%d delete(s) were sent for an id that no longer names that address", deleted)
	}
	if !strings.Contains(h.tg.lastText(), "203.0.113.9") {
		t.Errorf("the refusal does not say what the id actually points at:\n%s", h.tg.lastText())
	}
}
