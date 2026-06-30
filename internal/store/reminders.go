package store

import (
	"fmt"
	"strings"
)

// Reminder is one persisted reminder object the agent advances at every tick.
// Unlike a timer it ends only when the *task* is confirmed done (or its deadline
// window passes), so the row carries the whole loop state — not just a fire time.
// Reminders are NOT a crown jewel: the running agent creates and advances them on
// a requester's behalf, so writes here are agent-internal (no owner gate). The
// trust boundary (who may remind whom) is enforced once in the reminders package
// at creation, before the row is ever written.
type Reminder struct {
	ID              int64  `json:"id"`
	RequesterAddr   string `json:"requester_addr"`             // "transport|handle"
	RequesterName   string `json:"requester_name,omitempty"`   // display label
	TargetContactID int64  `json:"target_contact_id,omitempty"`
	TargetTransport string `json:"target_transport"` // "telegram"|"whatsapp"
	TargetAddr      string `json:"target_addr"`      // native reply address
	TargetName      string `json:"target_name,omitempty"`
	TaskText        string `json:"task_text"`
	Kind            string `json:"kind"` // "oneoff"|"recurring"
	RecurSpec       string `json:"recur_spec,omitempty"`
	DeadlineBound   bool   `json:"deadline_bound"`
	EventAt         string `json:"event_at,omitempty"` // RFC3339
	PreDelaySecs    int    `json:"pre_delay_secs"`
	PostGapSecs     int    `json:"post_gap_secs"`
	State           string `json:"state"`
	NextAt          string `json:"next_at,omitempty"` // RFC3339
	Attempts        int    `json:"attempts"`
	LastReply       string `json:"last_reply,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

// Reminder lifecycle states. The four non-terminal states are advanced by the
// scheduler tick (or, for awaiting_confirm, by an inbound reply); the three
// terminal states stop the loop.
const (
	ReminderScheduled   = "scheduled"
	ReminderPreReminded = "pre_reminded"
	ReminderAwaiting    = "awaiting_confirm"
	ReminderSnoozed     = "snoozed"
	ReminderDone        = "done"
	ReminderMissed      = "missed"
	ReminderCancelled   = "cancelled"
)

// activeStates are the non-terminal states (the tick and the reply matcher only
// ever touch these).
var activeStates = map[string]bool{
	ReminderScheduled: true, ReminderPreReminded: true,
	ReminderAwaiting: true, ReminderSnoozed: true,
}

const reminderCols = `id, requester_addr, COALESCE(requester_name,''), target_contact_id,
	target_transport, target_addr, COALESCE(target_name,''), task_text, kind,
	COALESCE(recur_spec,''), deadline_bound, COALESCE(event_at,''),
	pre_delay_secs, post_gap_secs, state, COALESCE(next_at,''), attempts,
	COALESCE(last_reply,''), COALESCE(created_at,''), COALESCE(updated_at,'')`

func scanReminder(sc interface{ Scan(...any) error }) (Reminder, error) {
	var r Reminder
	var deadline int
	err := sc.Scan(&r.ID, &r.RequesterAddr, &r.RequesterName, &r.TargetContactID,
		&r.TargetTransport, &r.TargetAddr, &r.TargetName, &r.TaskText, &r.Kind,
		&r.RecurSpec, &deadline, &r.EventAt, &r.PreDelaySecs, &r.PostGapSecs,
		&r.State, &r.NextAt, &r.Attempts, &r.LastReply, &r.CreatedAt, &r.UpdatedAt)
	r.DeadlineBound = deadline != 0
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

// CreateReminder inserts a reminder. Agent-internal (the trust check lives in the
// reminders package); returns the row with its new id.
func (s *Store) CreateReminder(r Reminder) (Reminder, error) {
	if strings.TrimSpace(r.TaskText) == "" {
		return r, fmt.Errorf("a reminder needs task text")
	}
	if strings.TrimSpace(r.TargetAddr) == "" {
		return r, fmt.Errorf("a reminder needs a target address")
	}
	if r.Kind == "" {
		r.Kind = "oneoff"
	}
	if r.State == "" {
		r.State = ReminderScheduled
	}
	r.CreatedAt, r.UpdatedAt = now(), now()
	res, err := s.db.Exec(
		`INSERT INTO reminders(requester_addr, requester_name, target_contact_id,
			target_transport, target_addr, target_name, task_text, kind, recur_spec,
			deadline_bound, event_at, pre_delay_secs, post_gap_secs, state, next_at,
			attempts, last_reply, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RequesterAddr, nullable(r.RequesterName), r.TargetContactID,
		r.TargetTransport, r.TargetAddr, nullable(r.TargetName), r.TaskText, r.Kind, nullable(r.RecurSpec),
		boolToInt(r.DeadlineBound), nullable(r.EventAt), r.PreDelaySecs, r.PostGapSecs, r.State, nullable(r.NextAt),
		r.Attempts, nullable(r.LastReply), r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return r, err
	}
	r.ID, _ = res.LastInsertId()
	return r, nil
}

// UpdateReminder overwrites a reminder's mutable loop fields (state machine
// advance). Agent-internal.
func (s *Store) UpdateReminder(r Reminder) error {
	r.UpdatedAt = now()
	res, err := s.db.Exec(
		`UPDATE reminders SET kind=?, recur_spec=?, deadline_bound=?, event_at=?,
			pre_delay_secs=?, post_gap_secs=?, state=?, next_at=?, attempts=?,
			last_reply=?, updated_at=? WHERE id=?`,
		r.Kind, nullable(r.RecurSpec), boolToInt(r.DeadlineBound), nullable(r.EventAt),
		r.PreDelaySecs, r.PostGapSecs, r.State, nullable(r.NextAt), r.Attempts,
		nullable(r.LastReply), r.UpdatedAt, r.ID)
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

// ListReminders returns every reminder, newest first (for the client/console).
func (s *Store) ListReminders() ([]Reminder, error) {
	return s.reminderQuery(`SELECT ` + reminderCols + ` FROM reminders ORDER BY id DESC`)
}

// ActiveReminders returns the non-terminal reminders (loop still running).
func (s *Store) ActiveReminders() ([]Reminder, error) {
	return s.reminderQuery(`SELECT ` + reminderCols + ` FROM reminders
		WHERE state IN ('scheduled','pre_reminded','awaiting_confirm','snoozed') ORDER BY id`)
}

// DueReminders returns non-terminal reminders whose next_at has arrived (<= now),
// soonest first — the scheduler tick's work list.
func (s *Store) DueReminders(nowRFC string) ([]Reminder, error) {
	return s.reminderQuery(`SELECT `+reminderCols+` FROM reminders
		WHERE state IN ('scheduled','pre_reminded','awaiting_confirm','snoozed')
		  AND next_at IS NOT NULL AND next_at <> '' AND next_at <= ?
		ORDER BY next_at`, nowRFC)
}

// OpenReminderForTarget returns the in-flight reminder a reply on (transport,
// addr) should advance: the most recent non-terminal reminder for that party,
// preferring one already awaiting_confirm. ok=false when none.
func (s *Store) OpenReminderForTarget(transport, addr string) (Reminder, bool, error) {
	rs, err := s.reminderQuery(`SELECT `+reminderCols+` FROM reminders
		WHERE target_transport=? AND target_addr=?
		  AND state IN ('scheduled','pre_reminded','awaiting_confirm','snoozed')
		ORDER BY (state='awaiting_confirm') DESC, id DESC`, transport, addr)
	if err != nil || len(rs) == 0 {
		return Reminder{}, false, err
	}
	return rs[0], true, nil
}

// CancelReminder marks a reminder cancelled and clears its next tick. Used by the
// owner CLI and by either party opting out.
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

// AddReminderEvent appends an audit row (best-effort; never fails a caller).
func (s *Store) AddReminderEvent(reminderID int64, kind, detail string) {
	_, _ = s.db.Exec(`INSERT INTO reminder_events(reminder_id, at, kind, detail) VALUES(?,?,?,?)`,
		reminderID, now(), kind, nullable(detail))
}

// IsTerminalReminder reports whether a state ends the loop.
func IsTerminalReminder(state string) bool { return !activeStates[state] }
