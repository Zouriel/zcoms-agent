package projects

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// HistoryDir is the git-ignored folder that holds the project database.
const HistoryDir = ".zcoms.d"

// Task statuses.
const (
	TaskOpen    = "open"
	TaskDone    = "done"
	TaskDropped = "dropped"
)

// History is one project's <folder>/.zcoms.d/history.db: members, daily
// summaries, tasks and standups. Only agent code writes it; a group-call run
// gets it read-only.
type History struct {
	db *sql.DB
}

// HistoryPath returns the database path for a project folder.
func HistoryPath(folder string) string {
	return filepath.Join(folder, HistoryDir, "history.db")
}

const historySchema = `
CREATE TABLE IF NOT EXISTS members (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  transport TEXT NOT NULL,
  platform_id TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  UNIQUE(transport, platform_id)
);
CREATE TABLE IF NOT EXISTS summaries (
  date TEXT PRIMARY KEY,
  text TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  assignee INTEGER REFERENCES members(id),
  source_msg TEXT,
  status TEXT NOT NULL DEFAULT 'open',
  created_at TEXT NOT NULL,
  closed_at TEXT
);
CREATE TABLE IF NOT EXISTS task_events (
  id INTEGER PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id),
  at TEXT NOT NULL,
  kind TEXT NOT NULL,
  actor TEXT,
  detail TEXT
);
CREATE TABLE IF NOT EXISTS standups (
  date TEXT PRIMARY KEY,
  message_id TEXT,
  sent_at INTEGER NOT NULL
);`

// OpenHistory opens (creating when needed) a project's history database in
// WAL mode. It never creates the database inside a git-tracked path: callers
// run GuardUntracked first.
func OpenHistory(folder string) (*History, error) {
	dir := filepath.Join(folder, HistoryDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	db, err := sql.Open("sqlite", HistoryPath(folder)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(historySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating history tables: %w", err)
	}
	return &History{db: db}, nil
}

// Close closes the database.
func (h *History) Close() error { return h.db.Close() }

// Member is one person in the project group.
type Member struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Transport  string `json:"transport"`
	PlatformID string `json:"platform_id"`
	Active     bool   `json:"active"`
}

// Task is one tracked task. AssigneeID 0 means unassigned.
type Task struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	AssigneeID   int64  `json:"assignee_id,omitempty"`
	AssigneeName string `json:"assignee,omitempty"`
	AssigneePID  string `json:"-"`
	SourceMsg    string `json:"source_msg,omitempty"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	ClosedAt     string `json:"closed_at,omitempty"`
}

// Summary is one day's compacted chat summary.
type Summary struct {
	Date string `json:"date"`
	Text string `json:"text"`
}

// Members returns every member, active first.
func (h *History) Members() ([]Member, error) {
	rows, err := h.db.Query(`SELECT id, name, transport, platform_id, active FROM members ORDER BY active DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var act int
		if err := rows.Scan(&m.ID, &m.Name, &m.Transport, &m.PlatformID, &act); err != nil {
			return nil, err
		}
		m.Active = act == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// SyncMembers reconciles the stored members with the group's current list:
// newcomers are added, people who left are marked inactive, and a returning
// member becomes active again. It reports the names added and deactivated.
func (h *History) SyncMembers(transport string, current []Member) (added, left []string, err error) {
	have, err := h.Members()
	if err != nil {
		return nil, nil, err
	}
	byPID := make(map[string]Member, len(have))
	for _, m := range have {
		byPID[m.PlatformID] = m
	}
	seen := make(map[string]bool, len(current))
	for _, c := range current {
		seen[c.PlatformID] = true
		old, ok := byPID[c.PlatformID]
		switch {
		case !ok:
			if _, err := h.db.Exec(`INSERT INTO members(name, transport, platform_id, active) VALUES(?,?,?,1)`,
				c.Name, transport, c.PlatformID); err != nil {
				return added, left, err
			}
			added = append(added, c.Name)
		case !old.Active:
			if _, err := h.db.Exec(`UPDATE members SET active=1 WHERE id=?`, old.ID); err != nil {
				return added, left, err
			}
		}
	}
	for _, m := range have {
		if m.Active && !seen[m.PlatformID] {
			if _, err := h.db.Exec(`UPDATE members SET active=0 WHERE id=?`, m.ID); err != nil {
				return added, left, err
			}
			left = append(left, m.Name)
		}
	}
	return added, left, nil
}

// MemberByName finds an active member by exact name (case-insensitive).
func (h *History) MemberByName(name string) (Member, bool, error) {
	ms, err := h.Members()
	if err != nil {
		return Member{}, false, err
	}
	for _, m := range ms {
		if m.Active && strings.EqualFold(m.Name, name) {
			return m, true, nil
		}
	}
	return Member{}, false, nil
}

// AddSummary stores (or replaces) a day's summary.
func (h *History) AddSummary(date, text string) error {
	_, err := h.db.Exec(`INSERT INTO summaries(date, text) VALUES(?,?)
		ON CONFLICT(date) DO UPDATE SET text=excluded.text`, date, text)
	return err
}

// Summaries returns the latest n summaries, newest first.
func (h *History) Summaries(n int) ([]Summary, error) {
	rows, err := h.db.Query(`SELECT date, text FROM summaries ORDER BY date DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.Date, &s.Text); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AddTask records a new open task. assignee 0 leaves it unassigned.
func (h *History) AddTask(title string, assignee int64, sourceMsg string) (int64, error) {
	var a any
	if assignee != 0 {
		a = assignee
	}
	res, err := h.db.Exec(`INSERT INTO tasks(title, assignee, source_msg, status, created_at) VALUES(?,?,?,?,?)`,
		title, a, sourceMsg, TaskOpen, stamp())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, h.event(id, "create", "standup", title)
}

const taskCols = `t.id, t.title, COALESCE(t.assignee,0), COALESCE(m.name,''), COALESCE(m.platform_id,''), COALESCE(t.source_msg,''), t.status, t.created_at, COALESCE(t.closed_at,'')`

func (h *History) queryTasks(where string, args ...any) ([]Task, error) {
	rows, err := h.db.Query(`SELECT `+taskCols+` FROM tasks t LEFT JOIN members m ON m.id=t.assignee WHERE `+where+` ORDER BY t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Title, &t.AssigneeID, &t.AssigneeName, &t.AssigneePID, &t.SourceMsg, &t.Status, &t.CreatedAt, &t.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OpenTasks returns every open task.
func (h *History) OpenTasks() ([]Task, error) { return h.queryTasks(`t.status=?`, TaskOpen) }

// Task returns one task by id.
func (h *History) Task(id int64) (Task, bool, error) {
	ts, err := h.queryTasks(`t.id=?`, id)
	if err != nil || len(ts) == 0 {
		return Task{}, false, err
	}
	return ts[0], true, nil
}

// SetTaskStatus closes (done/dropped) or reopens a task.
func (h *History) SetTaskStatus(id int64, status, actor string) error {
	closed := any(nil)
	if status != TaskOpen {
		closed = stamp()
	}
	if _, err := h.db.Exec(`UPDATE tasks SET status=?, closed_at=? WHERE id=?`, status, closed, id); err != nil {
		return err
	}
	return h.event(id, status, actor, "")
}

// AssignTask sets (or with 0 clears) a task's assignee.
func (h *History) AssignTask(id, member int64, actor string) error {
	var a any
	if member != 0 {
		a = member
	}
	if _, err := h.db.Exec(`UPDATE tasks SET assignee=? WHERE id=?`, a, id); err != nil {
		return err
	}
	return h.event(id, "assign", actor, fmt.Sprint(member))
}

func (h *History) event(task int64, kind, actor, detail string) error {
	_, err := h.db.Exec(`INSERT INTO task_events(task_id, at, kind, actor, detail) VALUES(?,?,?,?,?)`,
		task, stamp(), kind, actor, detail)
	return err
}

// RecordStandup stores the day's standup and the id of the message it sent
// (empty when nothing was sent).
func (h *History) RecordStandup(date, messageID string, at time.Time) error {
	_, err := h.db.Exec(`INSERT INTO standups(date, message_id, sent_at) VALUES(?,?,?)
		ON CONFLICT(date) DO UPDATE SET message_id=excluded.message_id, sent_at=excluded.sent_at`,
		date, messageID, at.Unix())
	return err
}

// HasStandup reports whether a standup already ran on date.
func (h *History) HasStandup(date string) (bool, error) {
	var n int
	err := h.db.QueryRow(`SELECT count(*) FROM standups WHERE date=?`, date).Scan(&n)
	return n > 0, err
}

// LastStandup returns when the last standup ran (zero when never).
func (h *History) LastStandup() (time.Time, error) {
	var at sql.NullInt64
	if err := h.db.QueryRow(`SELECT max(sent_at) FROM standups`).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return time.Unix(at.Int64, 0), nil
}

// IsStandupMessage reports whether msgID is a standup the agent sent.
func (h *History) IsStandupMessage(msgID string) (bool, error) {
	if msgID == "" {
		return false, nil
	}
	var n int
	err := h.db.QueryRow(`SELECT count(*) FROM standups WHERE message_id=?`, msgID).Scan(&n)
	return n > 0, err
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339) }
