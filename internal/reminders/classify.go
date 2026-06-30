package reminders

import (
	"regexp"
	"strings"
	"time"
)

// Decision is the creation-time classification (§4.1 + §4.2): cadence, whether
// the task is bound to a closing deadline, an inferred/explicit event time, and
// the two gaps the loop waits — the pre-reminder lead and the post gap to "did
// you do it?".
type Decision struct {
	Kind          string        // "oneoff" | "recurring"
	DeadlineBound bool          // window closes (meeting/flight) vs chase-until-done
	EventAt       time.Time     // inferred/explicit event or act time (zero = none)
	PreDelay      time.Duration // lead before the pre-reminder (used when no EventAt)
	PostGap       time.Duration // pre-reminder/event → confirm question; also re-ask spacing
	RecurSpec     string        // "daily HH:MM" | "weekdays HH:MM" (recurring)
	Explicit      bool          // the task itself specified a time (so owner-default gaps don't apply)
}

// ReplyVerdict is the read of a confirm reply (§4.3). A reply is one of three
// things: the task is actually done (Positive), they only acknowledged without
// doing it (Ack — "ok", "will do", "on it"), or it's still not done (neither).
// An Ack must NOT close the reminder — that was the "okay = done" bug.
type ReplyVerdict struct {
	Positive bool          // the task is genuinely done
	Ack      bool          // a bare acknowledgment, not a completion
	NewGap   time.Duration // negative + until-done: gap before the next nudge (0 = default)
}

// Classifier makes the three agent decisions. A model-backed implementation
// (R2) infers from fuzzy phrasing; the heuristic below is the deterministic
// fallback (and what the engine's unit tests run against).
type Classifier interface {
	Classify(task string, now time.Time) Decision
	ClassifyReply(task, reply string) ReplyVerdict
}

// Default gaps when nothing more specific is inferred.
const (
	defaultPreDelay = time.Hour
	defaultPostGap  = 15 * time.Minute
	deadlineLead    = 15 * time.Minute // pre-remind this long before a deadline event
	deadlineAfter   = 20 * time.Minute // ask "how did it go?" this long after the event
)

// heuristic is the model-free classifier: it reads explicit clock times and a few
// keyword classes. Good enough to drive the loop on its own and fully testable.
type heuristic struct{}

var (
	clockRe     = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b|\b([01]?\d|2[0-3]):([0-5]\d)\b`)
	recurRe     = regexp.MustCompile(`(?i)\b(every|each|daily|everyday|weekly|weekday|weekdays|each day|every day)\b`)
	relDelayRe  = regexp.MustCompile(`(?i)\b(in \d+\s*(min|mins|minute|minutes|hour|hours|hr|hrs|day|days|week|weeks)|tomorrow|tonight|this (morning|afternoon|evening|week)|next (week|mon|tue|wed|thu|fri|sat|sun))`)
	weekdayRe   = regexp.MustCompile(`(?i)\b(weekday|weekdays|every weekday)\b`)
	eventWordRe = regexp.MustCompile(`(?i)\b(meeting|meet|sync|call|appointment|appt|flight|interview|deadline|due|class|session|standup|stand-up|doctor|dentist|webinar|demo|presentation|review|catch-?up)\b`)
	posRe       = regexp.MustCompile(`(?i)(\bdone\b|\bdid it\b|\byes\b|\byeah\b|\byep\b|\bbought\b|\bsent\b|\bfinished\b|\bcomplete|\bhandled\b|\balready (did|done|sent|bought)\b|✅)`)
	negRe       = regexp.MustCompile(`(?i)(not yet|\bno\b|\bnope\b|didn'?t|haven'?t|\bnot done\b|\blater\b|forgot|\bstill\b|can'?t|couldn'?t|\bfailed\b|didn'?t make)`)
	// Acknowledgments (whole-message): heard you, not done. Checked before pos/neg.
	ackRe = regexp.MustCompile(`(?i)^\s*(ok(ay)?|kk|k|sure|will do|on it|got it|gotcha|alright|aight|fine|noted|👍|🫡)\s*[.!]*\s*$`)
	// Attending/ongoing: they're AT or IN the thing — for a get-ready/attend task
	// that means they MADE IT (done), even if it's still going. Checked before neg
	// so "not done yet, I finish at 8" isn't read as a failure.
	ongoingRe = regexp.MustCompile(`(?i)(\bin class\b|\bin (the|a) meeting\b|\bi'?m (in|at|here|inside)\b|\bon my way\b|\bfinish(es|ed)? at\b|\bend(s|ed)? at\b|\btill \d|\buntil \d)`)
)

func (heuristic) Classify(task string, now time.Time) Decision {
	d := Decision{Kind: "oneoff", PreDelay: defaultPreDelay, PostGap: defaultPostGap}
	low := strings.ToLower(task)

	at, ok := parseClock(task, now)
	if ok {
		d.EventAt = at
		d.Explicit = true
	}
	if relDelayRe.MatchString(low) {
		d.Explicit = true // "in 20 minutes", "in 2 hours"
	}

	if recurRe.MatchString(low) && ok {
		d.Kind = "recurring"
		d.Explicit = true
		hm := at.Format("15:04")
		if weekdayRe.MatchString(low) {
			d.RecurSpec = "weekdays " + hm
			d.EventAt = nextWeekdayAt(at.Hour(), at.Minute(), now)
		} else {
			d.RecurSpec = "daily " + hm
		}
		d.PreDelay = 0 // fire at the time itself
		return d
	}

	// A clock time + an event word means a deadline-bound event whose window
	// closes (the 3pm-sync case): pre-remind before it, ask after it.
	if ok && eventWordRe.MatchString(low) {
		d.DeadlineBound = true
		d.PreDelay = deadlineLead
		d.PostGap = deadlineAfter
	}
	return d
}

func (heuristic) ClassifyReply(task, reply string) ReplyVerdict {
	// A bare acknowledgment ("ok", "will do") is neither done nor a refusal.
	if ackRe.MatchString(reply) {
		return ReplyVerdict{Ack: true}
	}
	// They're at/in the event (even if it's ongoing) → they made it → done. Checked
	// before the negative match so "not done yet, I finish at 8" isn't a failure.
	if ongoingRe.MatchString(reply) {
		return ReplyVerdict{Positive: true}
	}
	low := strings.ToLower(reply)
	// Negative phrasing is checked before positive: "not done"/"didn't" carry no
	// positive token, but "not yet done" would otherwise trip the "done" match.
	if negRe.MatchString(low) {
		return ReplyVerdict{Positive: false}
	}
	if posRe.MatchString(low) {
		return ReplyVerdict{Positive: true}
	}
	return ReplyVerdict{Positive: false}
}

// parseClock extracts the first wall-clock time from text and returns the next
// occurrence (today if still ahead, else tomorrow). Only matches AM/PM or HH:MM
// forms, so a bare quantity ("buy 3 roses") is not mistaken for a time.
func parseClock(text string, now time.Time) (time.Time, bool) {
	m := clockRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	var hour, min int
	if m[1] != "" { // AM/PM form: m[1]=hour, m[2]=min, m[3]=am|pm
		hour = atoiSafe(m[1])
		min = atoiSafe(m[2])
		switch strings.ToLower(m[3]) {
		case "pm":
			if hour < 12 {
				hour += 12
			}
		case "am":
			if hour == 12 {
				hour = 0
			}
		}
	} else { // 24h form: m[4]=hour, m[5]=min
		hour = atoiSafe(m[4])
		min = atoiSafe(m[5])
	}
	if hour > 23 || min > 59 {
		return time.Time{}, false
	}
	res := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if !res.After(now) {
		res = res.AddDate(0, 0, 1)
	}
	return res, true
}

// nextWeekdayAt returns the next Mon–Fri occurrence of HH:MM at or after now.
func nextWeekdayAt(hour, min int, now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	for i := 0; i < 8; i++ {
		if (t.After(now)) && t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
			return t
		}
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
