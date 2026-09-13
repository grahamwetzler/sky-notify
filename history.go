package main

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// historyRetention is how long a sent alert is kept. Alerts are cooldown-gated, so even
// a busy receiver writes a few hundred rows a day; this is a bound on the file, not a
// tuning knob.
const historyRetention = 90 * 24 * time.Hour

// History is the record of what was actually delivered, per rule. It stores the
// notification as sent — the same fields the UI shows before it is sent — plus when it
// went out, so an operator can answer "did this rule ever fire, and with what?" without
// scrolling the service log.
//
// It is keyed by the rule's cooldown key, the same string the log and the ledger use. A
// renamed or re-conditioned unnamed rule therefore starts a fresh history: the old rows
// stay under the old key rather than being claimed by a rule that never sent them.
type History struct {
	db *sql.DB
}

type HistoryRow struct {
	SentAt   time.Time `json:"sent_at"`
	Title    string    `json:"title"`
	Message  string    `json:"message"`
	Priority int       `json:"priority"`
	Tags     []string  `json:"tags"`
	Click    string    `json:"click"`
}

func NewHistory(dir string) (*History, error) {
	// WAL so a reading request cannot block the notifier's write, and a busy timeout so
	// it does not fail outright if it does overlap one.
	dsn := filepath.Join(dir, "history.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS alerts (
			id       INTEGER PRIMARY KEY,
			rule     TEXT    NOT NULL,
			sent_at  INTEGER NOT NULL,
			title    TEXT    NOT NULL DEFAULT '',
			message  TEXT    NOT NULL DEFAULT '',
			priority INTEGER NOT NULL DEFAULT 0,
			tags     TEXT    NOT NULL DEFAULT '',
			click    TEXT    NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS alerts_rule_sent ON alerts(rule, sent_at DESC);
	`); err != nil {
		db.Close()
		return nil, err
	}
	return &History{db: db}, nil
}

func (h *History) Close() {
	if h != nil {
		h.db.Close()
	}
}

// Add records one delivered notification. Nil-safe and never fatal: history is a record
// of alerting, not part of it, so a full or unwritable disk must not cost a notification.
func (h *History) Add(rule string, m ntfyMessage, at time.Time) {
	if h == nil {
		return
	}
	if _, err := h.db.Exec(
		`INSERT INTO alerts (rule, sent_at, title, message, priority, tags, click) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rule, at.UnixMilli(), m.Title, m.Message, m.Priority, strings.Join(m.Tags, ","), m.Click,
	); err != nil {
		slog.Warn("alert history write failed", "rule", rule, "err", err)
		return
	}
	// ponytail: pruned on every insert rather than on a timer — an indexed range delete
	// over a handful of writes a day. Move it to the refresh loop if inserts ever get hot.
	if _, err := h.db.Exec(`DELETE FROM alerts WHERE sent_at < ?`,
		at.Add(-historyRetention).UnixMilli()); err != nil {
		slog.Warn("alert history prune failed", "err", err)
	}
}

// List returns one page of a rule's history, newest first, and the total behind it.
func (h *History) List(rule string, limit, offset int) (int, []HistoryRow, error) {
	rows := []HistoryRow{}
	if h == nil {
		return 0, rows, nil
	}
	var total int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE rule = ?`, rule).Scan(&total); err != nil {
		return 0, rows, err
	}
	q, err := h.db.Query(
		`SELECT sent_at, title, message, priority, tags, click FROM alerts
		 WHERE rule = ? ORDER BY sent_at DESC, id DESC LIMIT ? OFFSET ?`, rule, limit, offset)
	if err != nil {
		return 0, rows, err
	}
	defer q.Close()
	for q.Next() {
		var r HistoryRow
		var ms int64
		var tags string
		if err := q.Scan(&ms, &r.Title, &r.Message, &r.Priority, &tags, &r.Click); err != nil {
			return 0, rows, err
		}
		r.SentAt = time.UnixMilli(ms).UTC()
		if tags != "" {
			r.Tags = strings.Split(tags, ",")
		}
		rows = append(rows, r)
	}
	return total, rows, q.Err()
}
