package lineup

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms-agent/internal/timeexpr"
)

var errNoBackend = errors.New("no lineup agent backend available")

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// NewAgentTurn builds the real lineup turn: one RunAgent call on the live lineup
// backend in a staging dir, threading the session across messages. The role is
// read-only: the lineup never needs the filesystem, its schedule writes go
// through the store in code, not through the agent.
func NewAgentTurn(backendFn func() runner.Backend) AgentTurn {
	dir := ""
	if d, err := runner.DefaultAppDir(); err == nil {
		dir = filepath.Join(d, "lineup-staging")
	}
	return func(prompt, resumeID string) (string, string, error) {
		backend := backendFn()
		if backend == "" || dir == "" {
			return "", "", errNoBackend
		}
		if err := ensureDir(dir); err != nil {
			return "", "", err
		}
		res, err := runner.RunAgent(backend, dir, prompt, resumeID, runner.RoleRead, false)
		return res.Text, res.SessionID, err
	}
}

// isEndWord reports whether the owner asked to close the lineup.
func isEndWord(lower string) bool {
	switch strings.TrimSpace(lower) {
	case "end", "stop", "exit", "/end":
		return true
	}
	return false
}

// handleMessage drives one turn of the session. An explicit end word closes it
// (unless it is the opening keyword line); anything else opens the session if
// needed and runs a turn. A message that lands mid-turn is nudged, not run
// concurrently.
func (d *Comp) handleMessage(msg string, opened bool) {
	transport, addr := d.ownerRoute()
	if addr == "" {
		return
	}
	if !opened && isEndWord(strings.ToLower(strings.TrimSpace(msg))) {
		d.close(transport, addr, "Lineup closed. Say \"lineup\" whenever you want to pick it back up.")
		return
	}

	d.mu.Lock()
	if d.busy {
		d.mu.Unlock()
		d.sendTo(transport, addr, "One sec, still on that.")
		return
	}
	if !d.active {
		d.active, d.first = true, true
	}
	d.busy = true
	first := d.first
	d.first = false
	sess := d.sessionID
	d.mu.Unlock()

	go d.runTurn(transport, addr, msg, first, sess)
}

// runTurn runs one lineup turn: build the prompt from the owner's agenda and
// message, apply the one structured action it chose, send its message, and
// restart the idle clock so a quiet stretch eventually closes the session.
func (d *Comp) runTurn(transport, addr, userMsg string, first bool, sess string) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Printf("lineup turn panicked: %v", r)
		}
		d.mu.Lock()
		d.busy = false
		d.mu.Unlock()
		d.resetIdle(transport, addr)
	}()

	now := time.Now()
	today := d.todaysEvents(now)
	upcoming := d.upcomingEvents(now)
	out, s, err := d.turn(withSeed(d.seedOr(), lineupPrompt(today, upcoming, userMsg, first, now)), sess)
	if err != nil {
		d.log.Printf("lineup turn: %v", err)
		d.sendTo(transport, addr, "Sorry, I hit a snag pulling that up. Try me again in a moment.")
		return
	}
	if s != "" {
		d.mu.Lock()
		d.sessionID = s
		d.mu.Unlock()
	}

	f := parseFields(out)
	d.applyAction(f, transport, addr, now)
	if say := strings.TrimSpace(f["say"]); say != "" && !strings.EqualFold(say, "none") {
		d.sendTo(transport, addr, say)
	}
}

// close ends the session and stops the idle timer.
func (d *Comp) close(transport, addr, note string) {
	d.mu.Lock()
	d.active, d.first, d.sessionID = false, false, ""
	if d.idle != nil {
		d.idle.Stop()
		d.idle = nil
	}
	d.mu.Unlock()
	if note != "" {
		d.sendTo(transport, addr, note)
	}
}

// resetIdle (re)arms the idle-close timer for an open session. It is a no-op once
// the session is closed, so a straggling turn can't revive a timer.
func (d *Comp) resetIdle(transport, addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return
	}
	if d.idle != nil {
		d.idle.Stop()
	}
	d.idle = time.AfterFunc(d.idleWait, func() { d.idleClose(transport, addr) })
}

// idleClose closes the session after a quiet stretch.
func (d *Comp) idleClose(transport, addr string) {
	d.mu.Lock()
	if !d.active {
		d.mu.Unlock()
		return
	}
	d.active, d.first, d.sessionID, d.idle = false, false, "", nil
	d.mu.Unlock()
	d.sendTo(transport, addr, "I'll close the lineup for now. Say \"lineup\" whenever you want to pick it back up.")
}

// --- events ------------------------------------------------------------------

func (d *Comp) activeReminders() []store.Reminder {
	all, err := d.store.ActiveReminders()
	if err != nil {
		d.log.Printf("events: %v", err)
		return nil
	}
	return all
}

// todaysEvents returns the owner's active reminders whose event falls on today,
// earliest first.
func (d *Comp) todaysEvents(now time.Time) []store.Reminder {
	today := now.Format("2006-01-02")
	var out []store.Reminder
	for _, r := range d.activeReminders() {
		if eventDay(r, now.Location()) == today {
			out = append(out, r)
		}
	}
	sortByStart(out, now.Location())
	return out
}

// upcomingEvents returns active events dated after today and within the next
// week, earliest first, so the owner can ask about the days ahead too.
func (d *Comp) upcomingEvents(now time.Time) []store.Reminder {
	today := now.Format("2006-01-02")
	horizon := now.AddDate(0, 0, 8).Format("2006-01-02")
	var out []store.Reminder
	for _, r := range d.activeReminders() {
		day := eventDay(r, now.Location())
		if day > today && day < horizon {
			out = append(out, r)
		}
	}
	sortByStart(out, now.Location())
	return out
}

func sortByStart(rs []store.Reminder, loc *time.Location) {
	sort.SliceStable(rs, func(i, j int) bool {
		return eventStart(rs[i], loc).Before(eventStart(rs[j], loc))
	})
}

func eventStart(r store.Reminder, loc *time.Location) time.Time {
	if t := parseLocal(r.EventStart, loc); !t.IsZero() {
		return t
	}
	return parseLocal(r.NextAt, loc)
}

// eventDay is the local calendar day a reminder sits on: its event start if set,
// else its next scheduled run.
func eventDay(r store.Reminder, loc *time.Location) string {
	if t := eventStart(r, loc); !t.IsZero() {
		return t.Format("2006-01-02")
	}
	return ""
}

func parseLocal(s string, loc *time.Location) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
		return t.In(loc)
	}
	return time.Time{}
}

func formatEvents(evs []store.Reminder, loc *time.Location) string {
	if len(evs) == 0 {
		return "(nothing yet)"
	}
	var b strings.Builder
	for _, r := range evs {
		line := "#" + itoa(r.ID)
		if when := eventTimeLabel(r, loc); when != "" {
			line += " " + when
		}
		line += " — " + r.Task
		if o := strings.TrimSpace(r.OtherParty); o != "" {
			line += " (with " + o + ")"
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func eventTimeLabel(r store.Reminder, loc *time.Location) string {
	start := eventStart(r, loc)
	if start.IsZero() {
		return ""
	}
	label := start.Format("Mon 3:04 PM")
	if end := parseLocal(r.EventEnd, loc); !end.IsZero() {
		label += " to " + end.Format("3:04 PM")
	}
	return label
}

// --- actions -----------------------------------------------------------------

// applyAction carries out the one structured action the agent chose this turn.
// Everything targets the owner's own reminders, so the trust check is trivial:
// this is the owner's own schedule. Only add and move are allowed.
func (d *Comp) applyAction(f map[string]string, transport, addr string, now time.Time) {
	switch strings.ToLower(strings.TrimSpace(f["action"])) {
	case "add":
		task := strings.TrimSpace(f["task"])
		if task == "" {
			return
		}
		start := d.parseTimeField(f["start"], now)
		if _, err := d.store.CreateReminder(store.Reminder{
			FromAddr: "telegram|" + d.mainUser, FromName: "you",
			RecipientTransport: transport, RecipientAddr: addr, RecipientName: "you",
			Task: task, State: store.ReminderActive,
			EventStart: start, EventEnd: d.parseTimeField(f["end"], now),
			OtherParty: cleanField(f["other"]),
			NextAt:     start,
		}); err != nil {
			d.log.Printf("add: %v", err)
		}
	case "move":
		id := parseID(f["id"])
		rem, ok, err := d.store.GetReminder(id)
		if err != nil || !ok {
			d.log.Printf("move #%d: not found", id)
			return
		}
		if s := d.parseTimeField(f["start"], now); s != "" {
			rem.EventStart, rem.NextAt = s, s
		}
		if e := d.parseTimeField(f["end"], now); e != "" {
			rem.EventEnd = e
		}
		if err := d.store.UpdateReminder(rem); err != nil {
			d.log.Printf("move #%d: %v", id, err)
		}
	}
}

// parseTimeField turns a model time field into a stored RFC3339 UTC instant, or
// "" when it is blank or unparseable.
func (d *Comp) parseTimeField(s string, now time.Time) string {
	s = cleanField(s)
	if s == "" {
		return ""
	}
	if t, err := timeexpr.Parse(s, now); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func parseID(s string) int64 {
	n, _ := strconv.ParseInt(cleanField(s), 10, 64)
	return n
}

// cleanField normalizes an optional model field: a blank, "-", or "none" all mean
// "not provided".
func cleanField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" || strings.EqualFold(s, "none") {
		return ""
	}
	return s
}

// --- prompts -----------------------------------------------------------------

func lineupPrompt(today, upcoming []store.Reminder, userMsg string, first bool, now time.Time) string {
	loc := now.Location()
	req := strings.TrimSpace(userMsg)
	if req == "" {
		req = "(they opened the lineup without a specific request; show them what's on today)"
	}
	lines := []string{
		"You are the owner's schedule lineup: a focused assistant for THEIR own calendar and nothing else.",
		"Right now it is " + now.Format("Mon 2006-01-02 3:04 PM") + ".",
		"The owner said: " + quote(req),
		"",
		"On their agenda today (each has an id):",
		formatEvents(today, loc),
		"",
		"Coming up over the next week:",
		formatEvents(upcoming, loc),
		"",
	}
	if first {
		lines = append(lines, "This is the start of a lineup session. Answer their request directly and warmly.")
	} else {
		lines = append(lines, "Continue the lineup. Answer what they just said.")
	}
	lines = append(lines,
		"You can ONLY do three things: tell them what is on their schedule (today or the days ahead), add a new event, or reschedule (move) an existing one.",
		"You have no shell, no files, no messaging, and no web. If they ask for anything outside their own schedule, briefly say that is outside what the lineup does and offer a schedule action instead.",
		"Always be schedule-aware, and leave a SAFE MARGIN between events. Never call back-to-back timing fine: real life needs a buffer to wrap up, travel, and get set up, so keep at least about 30 minutes of clear space between one thing ending and the next starting (more when travel is obviously involved).",
		"Whenever they ask whether they can do something at a time, or you add or move an event, find what is already near that time above and judge the REAL usable window AFTER subtracting that buffer, not the raw clock gap. A 6pm date before a 7pm standup is NOT an hour, once you leave time to finish up and get there it is barely anything, so say that plainly and suggest a fix (start earlier, keep it short and casual, or move one of them). If there is genuinely enough room with the margin, say so. It is their call, so inform and suggest, never flatly refuse. When you add or move an event, keep that margin too, do not jam it right up against another.",
		"",
		"Available actions (choose ONE per reply):",
		"- add: put a new event on the schedule. Fill TASK, and START/END/OTHER when known.",
		"- move: reschedule an existing event. Fill ID and the new START (and END if given).",
		"- none: just answer or read out the agenda, change nothing.",
		"Times may be a clock time (3:30 PM), a relative offset (+2h, +1d), or a full date-time (2026-07-05T09:00). Use - for anything not applicable.",
		"The session stays open until the owner says \"end\" or \"stop\", or they go quiet, so never say goodbye or try to close it yourself, just answer this turn.",
		"",
		"Reply EXACTLY in these lines and nothing else:",
		"SAY: your message to send now, warm and human, never using em-dashes",
		"ACTION: none | add | move",
		"TASK: the event, if adding, else -",
		"START: the start time, if add or move, else -",
		"END: the end time, or -",
		"OTHER: another person involved, or -",
		"ID: the event id, if moving, else -",
	)
	return strings.Join(lines, "\n")
}

// --- small helpers -----------------------------------------------------------

func withSeed(seed, body string) string {
	if strings.TrimSpace(seed) == "" {
		return body
	}
	return seed + "\n\n" + body
}

func quote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "'") + "\"" }

var fieldRe = regexp.MustCompile(`(?im)^\s*([A-Za-z_]+)\s*:\s*(.+?)\s*$`)

// parseFields pulls "KEY: value" lines into a lowercased-key map (first per key).
func parseFields(text string) map[string]string {
	out := map[string]string{}
	for _, m := range fieldRe.FindAllStringSubmatch(text, -1) {
		k := strings.ToLower(m[1])
		if _, seen := out[k]; !seen {
			out[k] = strings.TrimSpace(m[2])
		}
	}
	return out
}
