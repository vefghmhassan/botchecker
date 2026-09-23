package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// MaxMessageLength is Telegram's limit for a message body.
const MaxMessageLength = 4096

// MaxCallbackData is Telegram's limit for a button's callback payload.
const MaxCallbackData = 64

// Button is one inline keyboard button.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Keyboard is an inline keyboard laid out in rows.
type Keyboard struct {
	Rows [][]Button
}

// MarshalJSON renders the shape Telegram expects.
func (k Keyboard) MarshalJSON() ([]byte, error) {
	rows := k.Rows
	if rows == nil {
		rows = [][]Button{}
	}
	return json.Marshal(map[string]any{"inline_keyboard": rows})
}

// User is the sender of an update.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// Label renders a user for logs and the audit trail.
func (u *User) Label() string {
	if u == nil {
		return "unknown"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	return strconv.FormatInt(u.ID, 10)
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      *Chat  `json:"chat"`
	Text      string `json:"text"`
}

// CallbackQuery is a button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Update is one item from getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Sender identifies who produced the update, whichever kind it is.
func (u Update) Sender() *User {
	switch {
	case u.CallbackQuery != nil:
		return u.CallbackQuery.From
	case u.Message != nil:
		return u.Message.From
	default:
		return nil
	}
}

// ChatID is where a reply should go.
func (u Update) ChatID() int64 {
	switch {
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil && u.CallbackQuery.Message.Chat != nil:
		return u.CallbackQuery.Message.Chat.ID
	case u.Message != nil && u.Message.Chat != nil:
		return u.Message.Chat.ID
	default:
		return 0
	}
}

// SendMessage posts a message to the configured chat and returns its id so it
// can be edited later.
func (t *Telegram) SendMessage(ctx context.Context, chatID int64, text string, kb *Keyboard) (int64, error) {
	body := map[string]any{
		"chat_id":                  t.chatOr(chatID),
		"text":                     Clamp(text),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb != nil {
		body["reply_markup"] = kb
	}

	var out struct {
		MessageID int64 `json:"message_id"`
	}
	err := t.call(ctx, "sendMessage", body, &out)
	return out.MessageID, err
}

// EditMessageText replaces a message in place, which keeps a menu from filling
// the chat with a new message on every tap.
func (t *Telegram) EditMessageText(ctx context.Context, chatID, messageID int64, text string, kb *Keyboard) error {
	body := map[string]any{
		"chat_id":                  t.chatOr(chatID),
		"message_id":               messageID,
		"text":                     Clamp(text),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb != nil {
		body["reply_markup"] = kb
	}
	return t.call(ctx, "editMessageText", body, nil)
}

// AnswerCallback acknowledges a button press. Telegram shows a spinner on the
// button until this is called.
func (t *Telegram) AnswerCallback(ctx context.Context, callbackID, text string, alert bool) error {
	body := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		// The popup is limited to 200 characters.
		if len(text) > 200 {
			text = text[:197] + "..."
		}
		body["text"] = text
		body["show_alert"] = alert
	}
	return t.call(ctx, "answerCallbackQuery", body, nil)
}

// GetUpdates long-polls for new updates. The HTTP deadline is deliberately
// longer than the poll timeout so the connection is not cut mid-wait.
func (t *Telegram) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	body := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}

	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()

	var out []Update
	err := t.callWithClient(ctx, &http.Client{Timeout: timeout + 15*time.Second}, "getUpdates", body, &out)
	return out, err
}

// chatOr falls back to the configured chat when none is given.
func (t *Telegram) chatOr(chatID int64) any {
	if chatID != 0 {
		return chatID
	}
	return t.cfg.Get(chatIDKey)
}

// Clamp trims a message to Telegram's length limit, marking the cut.
func Clamp(s string) string {
	if len(s) <= MaxMessageLength {
		return s
	}
	const note = "\n…(truncated)"
	return s[:MaxMessageLength-len(note)] + note
}
