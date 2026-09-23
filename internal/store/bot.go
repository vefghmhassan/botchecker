package store

import "time"

// BotAction is one thing the Telegram bot was asked to do.
type BotAction struct {
	ID         int64      `json:"id"`
	TelegramID int64      `json:"telegram_id"`
	Username   string     `json:"username,omitempty"`
	Authorized bool       `json:"authorized"`
	Action     string     `json:"action"`
	TargetID   int64      `json:"target_id,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	Requested  time.Time  `json:"requested_at"`
	Executed   *time.Time `json:"executed_at,omitempty"`
	OK         *bool      `json:"ok,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// RecordBotAction opens an audit row and returns its id.
func (s *Store) RecordBotAction(telegramID int64, username string, authorized bool,
	action string, targetID int64, detail string, at time.Time) (int64, error) {

	var tid any
	if targetID != 0 {
		tid = targetID
	}
	res, err := s.db.Exec(`
		INSERT INTO bot_actions (telegram_id, username, authorized, action, target_id, detail, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		telegramID, nullIfEmpty(username), boolToInt(authorized), action, tid, nullIfEmpty(detail), ts(at))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishBotAction records the outcome of an action.
func (s *Store) FinishBotAction(id int64, ok bool, actionErr error, at time.Time) error {
	var errText any
	if actionErr != nil {
		errText = actionErr.Error()
	}
	_, err := s.db.Exec(
		`UPDATE bot_actions SET executed_at = ?, ok = ?, error = ? WHERE id = ?`,
		ts(at), boolToInt(ok), errText, id)
	return err
}

// BotActions lists recent bot activity, newest first.
func (s *Store) BotActions(limit int) ([]BotAction, error) {
	rows, err := s.db.Query(`
		SELECT id, telegram_id, COALESCE(username,''), authorized, action,
		       COALESCE(target_id,0), COALESCE(detail,''), requested_at, executed_at, ok, COALESCE(error,'')
		FROM bot_actions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BotAction
	for rows.Next() {
		var a BotAction
		var authorized int
		var requested string
		var executed, okVal any
		if err := rows.Scan(&a.ID, &a.TelegramID, &a.Username, &authorized, &a.Action,
			&a.TargetID, &a.Detail, &requested, &executed, &okVal, &a.Error); err != nil {
			return nil, err
		}
		a.Authorized = authorized == 1
		a.Requested = parseTS(requested)
		if s, ok := executed.(string); ok && s != "" {
			t := parseTS(s)
			a.Executed = &t
		}
		if n, ok := okVal.(int64); ok {
			b := n == 1
			a.OK = &b
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UnauthorizedBotAttempts counts rejected updates in a window, for the
// dashboard to show whether the bot has been found by someone else.
func (s *Store) UnauthorizedBotAttempts(since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM bot_actions WHERE authorized = 0 AND requested_at >= ?`, ts(since)).Scan(&n)
	return n, err
}
