package morning

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms-agent/internal/timeexpr"
)

var errNoBackend = errors.New("no morning agent backend available")

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

func (d *Comp) seedOr() string {
	if d.seed == nil {
		return ""
	}
	return d.seed(personas.Morning)
}

// isEndWord reports whether the owner asked to close the morning session, using
// the same detach words as the bridge.
func isEndWord(lower string) bool {
	switch strings.TrimSpace(lower) {
	case "end", "stop", "exit", "/end":
		return true
	}
	return false
}

// sendGreeting opens the day: one warm good-morning, then the session goes
// dormant. It never lists events yet, that comes on the owner's first reply.
// busy is cleared when it lands so an eager reply is answered next.
func (d *Comp) sendGreeting() {
	defer func() {
		d.mu.Lock()
		d.busy = false
		d.mu.Unlock()
		if r := recover(); r != nil {
			d.log.Printf("greeting panicked: %v", r)
			d.mu.Lock()
			d.active = false
			d.mu.Unlock()
		}
	}()

	transport, addr := d.ownerRoute()
	if addr == "" {
		d.log.Printf("no owner chat resolved; skipping morning briefing")
		d.mu.Lock()
		d.active = false
		d.mu.Unlock()
		return
	}
	now := time.Now()

	// If the backend is unreachable, close the session before sending anything so
	// we never greet without being able to follow through.
	greet, sess, err := d.turn(withSeed(d.seedOr(), greetPrompt(now)), "")
	if err != nil {
		d.log.Printf("greeting: %v", err)
		d.mu.Lock()
		d.active = false
		d.mu.Unlock()
		return
	}
	g := strings.TrimSpace(parseFields(greet)["say"])
	if g == "" {
		g = "Good morning. Whenever you're up, give me a shout and I'll run through your day with you."
	}
	d.mu.Lock()
	d.sessionID = sess
	d.mu.Unlock()
	d.sendTo(transport, addr, g)
}

// handleReply drives one turn of the open session from an owner message. An
// explicit end word closes it; anything else runs a briefing/conversation turn
// (the first reply presents the day, the rest just talk). A message that lands
// while a turn is still running is answered with a nudge rather than dropped
// into a second concurrent turn.
func (d *Comp) handleReply(text string) {
	transport, addr := d.ownerRoute()
	if addr == "" {
		return
	}
	msg := strings.TrimSpace(text)
	if isEndWord(strings.ToLower(msg)) {
		d.mu.Lock()
		d.active, d.first, d.sessionID = false, false, ""
		d.mu.Unlock()
		d.sendTo(transport, addr, "Alright, I'll let you get on with your day. Just say the word whenever you need me.")
		return
	}

	d.mu.Lock()
	if d.busy {
		d.mu.Unlock()
		d.sendTo(transport, addr, "One sec, still finishing that.")
		return
	}
	d.busy = true
	first := d.first
	d.first = false
	sess := d.sessionID
	d.mu.Unlock()

	go d.runTurn(transport, addr, msg, first, sess)
}

// runTurn runs one briefing/conversation turn: build the prompt from today's
// events and the owner's message, apply the one structured action it chose, and
// send its message. The session stays open afterwards, until the owner ends it.
func (d *Comp) runTurn(transport, addr, userMsg string, first bool, sess string) {
	defer func() {
		d.mu.Lock()
		d.busy = false
		d.mu.Unlock()
		if r := recover(); r != nil {
			d.log.Printf("briefing turn panicked: %v", r)
		}
	}()

	now := time.Now()
	events := d.todaysEvents(now)
	out, s, err := d.turn(withSeed(d.seedOr(), convPrompt(events, userMsg, first, now)), sess)
	if err != nil {
		d.log.Printf("briefing turn: %v", err)
		d.sendTo(transport, addr, "Sorry, I hit a snag on my end. Try me again in a moment.")
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

// --- events ------------------------------------------------------------------

// todaysEvents returns the owner's active reminders whose event falls on today.
func (d *Comp) todaysEvents(now time.Time) []store.Reminder {
	all, err := d.store.ActiveReminders()
	if err != nil {
		d.log.Printf("events: %v", err)
		return nil
	}
	today := now.Format("2006-01-02")
	var out []store.Reminder
	for _, r := range all {
		if eventDay(r, now.Location()) == today {
			out = append(out, r)
		}
	}
	return out
}

// eventDay is the local calendar day a reminder sits on: its event start if set,
// else its next scheduled run.
func eventDay(r store.Reminder, loc *time.Location) string {
	if t := parseLocal(r.EventStart, loc); !t.IsZero() {
		return t.Format("2006-01-02")
	}
	if t := parseLocal(r.NextAt, loc); !t.IsZero() {
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
		return "(nothing on the calendar for today yet)"
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
	start := parseLocal(r.EventStart, loc)
	if start.IsZero() {
		start = parseLocal(r.NextAt, loc)
	}
	if start.IsZero() {
		return ""
	}
	label := start.Format("3:04 PM")
	if end := parseLocal(r.EventEnd, loc); !end.IsZero() {
		label += " to " + end.Format("3:04 PM")
	}
	return label
}

// --- actions -----------------------------------------------------------------

// applyAction carries out the one structured action the agent chose this turn.
// Everything targets the owner's own reminders, so the trust check is trivial:
// this is the owner's own morning session.
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
	case "cancel":
		id := parseID(f["id"])
		if err := d.store.CancelReminder(id); err != nil {
			d.log.Printf("cancel #%d: %v", id, err)
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

func greetPrompt(now time.Time) string {
	return strings.Join([]string{
		"You are the owner's warm morning assistant. It is early morning right now (" + now.Format("Mon 2006-01-02 3:04 PM") + ").",
		"Send a short, genuine good-morning that gently starts their day and invites them to reply whenever they are up. Do NOT list any events yet, that comes after they respond.",
		"Reply EXACTLY in this one line and nothing else:",
		"SAY: your good-morning message, warm and human, never using em-dashes",
	}, "\n")
}

func convPrompt(evs []store.Reminder, userMsg string, first bool, now time.Time) string {
	loc := now.Location()
	lines := []string{
		"You are the owner's warm morning assistant, going through their day with them. Right now it is " + now.Format("Mon 2006-01-02 3:04 PM") + ".",
		"The owner just said: " + quote(userMsg),
		"Their events for today (each has an id):",
		formatEvents(evs, loc),
		"",
	}
	if first {
		lines = append(lines,
			"They just replied to your good-morning. Acknowledge them briefly, then walk them through today's events in a natural, human voice (if there are none, say the day is open). Then ask if they would like to reschedule anything or add something new.")
	} else {
		lines = append(lines,
			"Respond naturally to what they said. If they asked to add, move, or cancel an event, carry it out with the matching action below and confirm it warmly.")
	}
	lines = append(lines,
		"",
		"Available actions (choose ONE per reply):",
		"- add: create a new event. Fill TASK, and START/END/OTHER when known.",
		"- move: reschedule an existing event. Fill ID and the new START (and END if given).",
		"- cancel: remove an existing event. Fill ID.",
		"- none: just talk, change nothing.",
		"Times may be a clock time (3:30 PM), a relative offset (+2h, +1d), or a full date-time (2026-07-05T09:00). Use - for anything not applicable.",
		"The conversation stays open until the owner says \"end\" themselves, so never try to close it or say goodbye first, just answer this turn and stop.",
		"",
		"Reply EXACTLY in these lines and nothing else:",
		"SAY: your message to send now, warm and human, never using em-dashes",
		"ACTION: none | add | move | cancel",
		"TASK: the event, if adding, else -",
		"START: the start time, if add or move, else -",
		"END: the end time, or -",
		"OTHER: another person involved, or -",
		"ID: the event id, if move or cancel, else -",
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
