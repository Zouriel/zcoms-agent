package store

import (
	"fmt"
	"strings"
)

// Reminder is an agent-driven reminder instance. It is NOT a state machine — a
// dedicated "reminder agent" runs at NextAt, composes + sends a message, waits
// for the reply, then rewrites CarryOver (its note to the next run) and NextAt,
// or finishes. CarryOver is the whole memory, so the loop stays coherent across
// any number of runs. Writes are agent-internal; the §6 trust check lives in the
// reminders package at creation.
//
// The struct reuses the existing reminders columns (target_* → recipient,
// requester_* → from, task_text → task) and adds carry_over + runs.
type Reminder struct {
	ID                 int64  `json:"id"`
	FromAddr           string `json:"from_addr"` // who set it: "transport|handle"
	FromName           string `json:"from_name,omitempty"`
	RecipientTransport string `json:"recipient_transport"`
	RecipientAddr      string `json:"recipient_addr"`
	RecipientName      string `json:"recipient_name,omitempty"`
	RecipientContactID int64  `json:"recipient_contact_id,omitempty"`
	Task               string `json:"task"`
	// EventStart and EventEnd optionally record the from/to window of the event
	// this reminder is about (RFC3339). OtherParty is an optional free-text label
	// for another person involved in the event, beyond the setter and recipient.
	// All three are nullable and persist-only: nothing in the reminder run loop
	// reads them, they exist for the console and other modules to store and show.
	EventStart string `json:"event_start,omitempty"`
	EventEnd   string `json:"event_end,omitempty"`
	OtherParty string `json:"other_party,omitempty"`
	CarryOver  string `json:"carry_over,omitempty"`
	NextAt     string `json:"next_at,omitempty"` // RFC3339
	State      string `json:"state"`             // active | done | cancelled
	Runs       int    `json:"runs"`
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

const (
	ReminderActive    = "active"
	ReminderDone      = "done"
	ReminderCancelled = "cancelled"
)

const reminderCols = `id, requester_addr, COALESCE(requester_name,''),
	target_transport, target_addr, COALESCE(target_name,''), target_contact_id,
	task_text, COALESCE(carry_over,''), COALESCE(next_at,''), state, runs,
	COALESCE(created_at,''), COALESCE(updated_at,''),
	COALESCE(event_start,''), COALESCE(event_end,''), COALESCE(other_party,'')`

func scanReminder(sc interface{ Scan(...any) error }) (Reminder, error) {
	var r Reminder
	err := sc.Scan(&r.ID, &r.FromAddr, &r.FromName,
		&r.RecipientTransport, &r.RecipientAddr, &r.RecipientName, &r.RecipientContactID,
		&r.Task, &r.CarryOver, &r.NextAt, &r.State, &r.Runs, &r.CreatedAt, &r.UpdatedAt,
		&r.EventStart, &r.EventEnd, &r.OtherParty)
	return r, err
}

func (s *Store) reminderQuery(q string, args ...any) ([]Reminder, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateReminder inserts an active reminder (agent-internal; trust enforced in the
// reminders package). Returns the row with its new id.
func (s *Store) CreateReminder(r Reminder) (Reminder, error) {
	if strings.TrimSpace(r.Task) == "" {
		return r, fmt.Errorf("a reminder needs a task")
	}
	if strings.TrimSpace(r.RecipientAddr) == "" {
		return r, fmt.Errorf("a reminder needs a recipient")
	}
	if r.State == "" {
		r.State = ReminderActive
	}
	r.CreatedAt, r.UpdatedAt = now(), now()
	res, err := s.db.Exec(
		`INSERT INTO reminders(requester_addr, requester_name, target_transport, target_addr,
			target_name, target_contact_id, task_text, carry_over, next_at, state, runs,
			created_at, updated_at, event_start, event_end, other_party)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.FromAddr, nullable(r.FromName), r.RecipientTransport, r.RecipientAddr,
		nullable(r.RecipientName), r.RecipientContactID, r.Task, nullable(r.CarryOver),
		nullable(r.NextAt), r.State, r.Runs, r.CreatedAt, r.UpdatedAt,
		nullable(r.EventStart), nullable(r.EventEnd), nullable(r.OtherParty),
	)
	if err != nil {
		return r, err
	}
	r.ID, _ = res.LastInsertId()
	return r, nil
}

// UpdateReminder persists what the per-run agent decided: the carry-over note, the
// next run time, the state, and the run count. Agent-internal.
func (s *Store) UpdateReminder(r Reminder) error {
	r.UpdatedAt = now()
	res, err := s.db.Exec(
		`UPDATE reminders SET carry_over=?, next_at=?, state=?, runs=?, updated_at=?,
			event_start=?, event_end=?, other_party=? WHERE id=?`,
		nullable(r.CarryOver), nullable(r.NextAt), r.State, r.Runs, r.UpdatedAt,
		nullable(r.EventStart), nullable(r.EventEnd), nullable(r.OtherParty), r.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no reminder with id %d", r.ID)
	}
	return nil
}

// GetReminder returns one reminder by id.
func (s *Store) GetReminder(id int64) (Reminder, bool, error) {
	r, err := scanReminder(s.db.QueryRow(`SELECT `+reminderCols+` FROM reminders WHERE id=?`, id))
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Reminder{}, false, nil
		}
		return Reminder{}, false, err
	}
	return r, true, nil
}

// ListReminders returns every reminder, newest first (client/console view).
func (s *Store) ListReminders() ([]Reminder, error) {
	return s.reminderQuery(`SELECT ` + reminderCols + ` FROM reminders ORDER BY id DESC`)
}

// ActiveReminders returns the still-running reminders.
func (s *Store) ActiveReminders() ([]Reminder, error) {
	return s.reminderQuery(`SELECT ` + reminderCols + ` FROM reminders WHERE state='active' ORDER BY id`)
}

// DueReminders returns active reminders whose next_at has arrived (<= now).
func (s *Store) DueReminders(nowRFC string) ([]Reminder, error) {
	return s.reminderQuery(`SELECT `+reminderCols+` FROM reminders
		WHERE state='active' AND next_at IS NOT NULL AND next_at <> '' AND next_at <= ?
		ORDER BY next_at`, nowRFC)
}

// CancelReminder stops a reminder (owner CLI or either party opting out).
func (s *Store) CancelReminder(id int64) error {
	res, err := s.db.Exec(`UPDATE reminders SET state='cancelled', next_at='', updated_at=? WHERE id=?`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no reminder with id %d", id)
	}
	return nil
}

// AddReminderEvent appends an audit row (best-effort).
func (s *Store) AddReminderEvent(reminderID int64, kind, detail string) {
	_, _ = s.db.Exec(`INSERT INTO reminder_events(reminder_id, at, kind, detail) VALUES(?,?,?,?)`,
		reminderID, now(), kind, nullable(detail))
}

// ReminderEvent is one row of a reminder's timeline (the "log").
type ReminderEvent struct {
	ID         int64  `json:"id"`
	ReminderID int64  `json:"reminder_id"`
	At         string `json:"at"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail,omitempty"`
}

// ListReminderEvents returns one reminder's audit trail, oldest first.
func (s *Store) ListReminderEvents(reminderID int64) ([]ReminderEvent, error) {
	rows, err := s.db.Query(`SELECT id, reminder_id, COALESCE(at,''), COALESCE(kind,''), COALESCE(detail,'')
		FROM reminder_events WHERE reminder_id=? ORDER BY id`, reminderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReminderEvent
	for rows.Next() {
		var e ReminderEvent
		if err := rows.Scan(&e.ID, &e.ReminderID, &e.At, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
