package reminders

import (
	"fmt"
	"strings"
	"time"
)

// Recurring specs are stored canonically as one of:
//
//	"daily HH:MM"            — every day at HH:MM
//	"weekdays HH:MM"         — Mon–Fri at HH:MM
//	"weekly <Mon..Sun> HH:MM" — that weekday each week at HH:MM
//
// normalizeRecur canonicalizes a looser spec into one of those; nextRecur returns
// the next occurrence strictly after now. Both the classifier (to set the first
// EventAt) and the engine (to reschedule the next occurrence) go through here.

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

func normalizeRecur(s string) string {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	if len(f) == 0 {
		return ""
	}
	switch f[0] {
	case "daily", "everyday":
		if hm, ok := normHM(lastField(f)); ok {
			return "daily " + hm
		}
	case "weekdays", "weekday":
		if hm, ok := normHM(lastField(f)); ok {
			return "weekdays " + hm
		}
	case "weekly", "every", "each":
		if len(f) >= 3 {
			if wd, ok := weekdayNames[f[1]]; ok {
				if hm, ok := normHM(lastField(f)); ok {
					return "weekly " + wd.String()[:3] + " " + hm
				}
			}
		}
	}
	return strings.TrimSpace(s)
}

// nextRecur returns the next occurrence of a canonical recur spec after now.
func nextRecur(spec string, now time.Time) (time.Time, bool) {
	f := strings.Fields(spec)
	if len(f) == 0 {
		return time.Time{}, false
	}
	switch strings.ToLower(f[0]) {
	case "daily":
		h, m, ok := parseHM(lastField(f))
		if !ok {
			return time.Time{}, false
		}
		t := atTime(now, h, m)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	case "weekdays":
		h, m, ok := parseHM(lastField(f))
		if !ok {
			return time.Time{}, false
		}
		return nextWeekdayAt(h, m, now), true
	case "weekly":
		if len(f) < 3 {
			return time.Time{}, false
		}
		wd, ok := weekdayNames[strings.ToLower(f[1])]
		if !ok {
			return time.Time{}, false
		}
		h, m, ok := parseHM(lastField(f))
		if !ok {
			return time.Time{}, false
		}
		return nextOnWeekday(wd, h, m, now), true
	}
	return time.Time{}, false
}

func lastField(f []string) string { return f[len(f)-1] }

func atTime(now time.Time, h, m int) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
}

func nextOnWeekday(wd time.Weekday, h, m int, now time.Time) time.Time {
	t := atTime(now, h, m)
	for i := 0; i < 8; i++ {
		if t.After(now) && t.Weekday() == wd {
			return t
		}
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// parseHM reads a clock token ("9am", "09:00", "21:30", "9:00pm") into 24h H:M.
func parseHM(s string) (hh, mm int, ok bool) {
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	if m[1] != "" {
		hh, mm = atoiSafe(m[1]), atoiSafe(m[2])
		switch strings.ToLower(m[3]) {
		case "pm":
			if hh < 12 {
				hh += 12
			}
		case "am":
			if hh == 12 {
				hh = 0
			}
		}
	} else {
		hh, mm = atoiSafe(m[4]), atoiSafe(m[5])
	}
	if hh > 23 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

func normHM(s string) (string, bool) {
	h, m, ok := parseHM(s)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", h, m), true
}
