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

const (
	maxConvTurns    = 12
	maxWakeNudges   = 2
	defaultWakeWait = 20 * time.Minute
	defaultConvWait = 15 * time.Minute
)

var errNoBackend = errors.New("no morning agent backend available")

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

func (d *Comp) wakeWait() time.Duration {
	if d.wakeWaitOverride > 0 {
		return d.wakeWaitOverride
	}
	return defaultWakeWait
}

func (d *Comp) convWait() time.Duration {
	if d.convWaitOverride > 0 {
		return d.convWaitOverride
	}
	return defaultConvWait
}

func (d *Comp) seedOr() string {
	if d.seed == nil {
		return ""
	}
	return d.seed(personas.Morning)
}

// runSession is one morning briefing: greet, wait until the owner is up, then walk
// them through today's events and handle any reschedule/add/cancel they ask for.
func (d *Comp) runSession() {
	defer d.releaseSession()
	defer func() {
		if r := recover(); r != nil {
			d.log.Printf("briefing panicked: %v", r)
		}
	}()

	transport, addr := d.ownerRoute()
	if addr == "" {
		d.log.Printf("no owner chat resolved; skipping morning briefing")
		return
	}
	seed := d.seedOr()
	now := time.Now()

	// Greeting turn. If the backend is unreachable, bail before sending anything so
	// we never greet without being able to follow through.
	greet, sess, err := d.turn(withSeed(seed, greetPrompt(now)), "")
	if err != nil {
		d.log.Printf("greeting: %v", err)
		return
	}
	g := strings.TrimSpace(parseFields(greet)["say"])
	if g == "" {
		g = "Good morning. Whenever you're up, give me a shout and I'll run through your day with you."
	}
	d.sendTo(transport, addr, g)

	// Wait until they are actually up: a couple of gentle re-nudges, then stop.
	wake, got := d.awaitWake(transport, addr)
	if !got {
		d.log.Printf("no morning reply; letting it go for the day")
		return
	}

	// Briefing + conversation loop.
	lastMsg, first := wake, true
	for turn := 0; turn < maxConvTurns; turn++ {
		events := d.todaysEvents(now)
		out, s, err := d.turn(withSeed(seed, convPrompt(events, lastMsg, first, now)), sess)
		if err != nil {
			d.log.Printf("briefing turn: %v", err)
			d.sendTo(transport, addr, "Sorry, I hit a snag on my end. We can pick this up later.")
			return
		}
		sess, first = s, false

		f := parseFields(out)
		d.applyAction(f, transport, addr, now)
		if say := strings.TrimSpace(f["say"]); say != "" && !strings.EqualFold(say, "none") {
			d.sendTo(transport, addr, say)
		}
		if strings.EqualFold(strings.TrimSpace(f["end_session"]), "yes") {
			return
		}

		rep, got := d.waitReply(transport, addr, d.convWait())
		if !got {
			d.sendTo(transport, addr, "No rush, we can pick this up whenever. Have a good one.")
			return
		}
		lastMsg = rep
	}
}

// awaitWake parks on the owner's first reply, re-nudging gently a couple of times
// before giving up for the day.
func (d *Comp) awaitWake(transport, addr string) (string, bool) {
	for n := 0; n <= maxWakeNudges; n++ {
		if rep, got := d.waitReply(transport, addr, d.wakeWait()); got {
			return rep, true
		}
		if n < maxWakeNudges {
			d.sendTo(transport, addr, wakeNudge(n))
		}
	}
	return "", false
}

func wakeNudge(n int) string {
	if n == 0 {
		return "Still here whenever you're ready."
	}
	return "No rush, just say the word when you're up and we'll run through today."
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
		"Set END_SESSION to yes only once they are done and need nothing more; otherwise no.",
		"",
		"Reply EXACTLY in these lines and nothing else:",
		"SAY: your message to send now, warm and human, never using em-dashes",
		"ACTION: none | add | move | cancel",
		"TASK: the event, if adding, else -",
		"START: the start time, if add or move, else -",
		"END: the end time, or -",
		"OTHER: another person involved, or -",
		"ID: the event id, if move or cancel, else -",
		"END_SESSION: yes | no",
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
