package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
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

// HistoryPage is one page of a rule's history and the cursor that continues it.
type HistoryPage struct {
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Alerts []HistoryRow `json:"alerts"`
	// Next is the cursor for the page below this one, absent when there is none.
	Next string `json:"next,omitempty"`
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
//
// A page is taken from below a cursor rather than at an offset. Deliveries arrive while
// the list is on screen, and each one pushes every row down: an offset counted from what
// is already displayed would then re-serve the last row of it. The cursor is a position
// in the list, not a count into it, so nothing arriving above it can move it.
func (h *History) List(rule, after string, limit int) (HistoryPage, error) {
	page := HistoryPage{Limit: limit, Alerts: []HistoryRow{}}
	if h == nil {
		return page, nil
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE rule = ?`, rule).Scan(&page.Total); err != nil {
		return page, err
	}
	where, args := "rule = ?", []any{rule}
	if at, id, ok := parseCursor(after); ok {
		// A row value compares left to right in exactly the order the index is built in,
		// so "everything below the last row handed out" is one comparison against it.
		where += " AND (sent_at, id) < (?, ?)"
		args = append(args, at, id)
	}
	// One more row than asked for, to learn whether a page follows this one without
	// having to trust a total that is still moving.
	args = append(args, limit+1)
	q, err := h.db.Query(
		`SELECT id, sent_at, title, message, priority, tags, click FROM alerts
		 WHERE `+where+` ORDER BY sent_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer q.Close()
	var lastAt, lastID int64
	for q.Next() {
		var r HistoryRow
		var at, id int64
		var tags string
		if err := q.Scan(&id, &at, &r.Title, &r.Message, &r.Priority, &tags, &r.Click); err != nil {
			return page, err
		}
		if len(page.Alerts) == limit {
			page.Next = fmt.Sprintf("%d.%d", lastAt, lastID)
			break
		}
		r.SentAt = time.UnixMilli(at).UTC()
		if tags != "" {
			r.Tags = strings.Split(tags, ",")
		}
		page.Alerts = append(page.Alerts, r)
		lastAt, lastID = at, id
	}
	return page, q.Err()
}

// parseCursor reads a "<sent_at>.<id>" cursor. Anything else is no cursor at all and so
// the first page: it can only come from our own page, and the top of the list is a better
// answer than an error.
func parseCursor(s string) (at, id int64, ok bool) {
	a, b, found := strings.Cut(s, ".")
	if !found {
		return 0, 0, false
	}
	at, errAt := strconv.ParseInt(a, 10, 64)
	id, errID := strconv.ParseInt(b, 10, 64)
	return at, id, errAt == nil && errID == nil
}
