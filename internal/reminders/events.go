package reminders

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms-agent/internal/timeexpr"
)

// eventsWindow is how far either side of the given moment EventsAround looks.
const eventsWindow = 2 * time.Hour

// EventsAround answers "what's on around this time": every event within two hours
// either side of the given date-time (or the whole day, if only a date is given).
// Events are the owner's reminders, keyed on their event window (or next run).
func (d *Comp) EventsAround(when string, now time.Time) (string, error) {
	when = strings.TrimSpace(when)
	if when == "" {
		return "", fmt.Errorf("usage: events <date and time>  (e.g. events 2026-07-04 15:00)")
	}
	anchor, hasTime, err := parseAnchor(when, now)
	if err != nil {
		return "", err
	}
	all, err := d.store.ListReminders()
	if err != nil {
		return "", err
	}
	return eventsReport(all, anchor, hasTime), nil
}

// eventsReport is the pure formatting behind EventsAround (separated for testing):
// events overlapping [anchor-2h, anchor+2h] when a time is given, else the whole
// local day of anchor.
func eventsReport(all []store.Reminder, anchor time.Time, hasTime bool) string {
	loc := anchor.Location()
	var lo, hi time.Time
	var header string
	if hasTime {
		lo, hi = anchor.Add(-eventsWindow), anchor.Add(eventsWindow)
		header = "Events within 2 hours of " + anchor.Format("Mon 02 Jan 2006, 3:04 PM") + ":"
	} else {
		lo = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, loc)
		hi = lo.AddDate(0, 0, 1).Add(-time.Nanosecond)
		header = "Events on " + anchor.Format("Mon 02 Jan 2006") + ":"
	}

	type hit struct {
		start, end time.Time
		r          store.Reminder
	}
	var hits []hit
	for _, r := range all {
		if r.State == store.ReminderCancelled {
			continue
		}
		s, e, ok := eventSpan(r, loc)
		if !ok {
			continue
		}
		if !s.After(hi) && !e.Before(lo) { // the event's span overlaps the window
			hits = append(hits, hit{s, e, r})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].start.Before(hits[j].start) })

	if len(hits) == 0 {
		return header + "\n  nothing on the calendar in that window"
	}
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, h := range hits {
		label := h.start.Format("3:04 PM")
		if !h.end.Equal(h.start) {
			label += "–" + h.end.Format("3:04 PM")
		}
		line := "  " + label + "  " + h.r.Task
		if o := strings.TrimSpace(h.r.OtherParty); o != "" {
			line += " (with " + o + ")"
		}
		if h.r.State != store.ReminderActive {
			line += "  [" + h.r.State + "]"
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// eventSpan is a reminder's time span in loc: its event window if set, else its
// next run collapsed to an instant. ok is false when it has no parseable time.
func eventSpan(r store.Reminder, loc *time.Location) (start, end time.Time, ok bool) {
	start = parseInstant(r.EventStart, loc)
	if start.IsZero() {
		start = parseInstant(r.NextAt, loc)
	}
	if start.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	end = parseInstant(r.EventEnd, loc)
	if end.IsZero() || end.Before(start) {
		end = start
	}
	return start, end, true
}

func parseInstant(s string, loc *time.Location) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
		return t.In(loc)
	}
	return time.Time{}
}

// parseAnchor reads the moment to look around. Unlike the scheduler's parser it
// accepts times in the past (this is a lookup, not a schedule) and reports
// whether a clock time was actually given (date-only means "the whole day").
func parseAnchor(s string, now time.Time) (anchor time.Time, hasTime bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, fmt.Errorf("need a date and time")
	}
	if strings.EqualFold(s, "now") {
		return now, true, nil
	}
	loc := now.Location()
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05", "2006-01-02T15:04",
		"2006-01-02 15:04:05", "2006-01-02 15:04",
		"2006-01-02 3:04PM", "2006-01-02 3:04 PM",
	} {
		if t, e := time.ParseInLocation(layout, s, loc); e == nil {
			return t, true, nil
		}
	}
	if t, e := time.ParseInLocation("2006-01-02", s, loc); e == nil {
		return t, false, nil // date only: whole-day window
	}
	// A bare clock time is anchored to today.
	for _, layout := range []string{"15:04", "3:04PM", "3:04 PM"} {
		if t, e := time.ParseInLocation(layout, s, loc); e == nil {
			return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc), true, nil
		}
	}
	// Relative offsets (+2h, +1d) are a convenience; timeexpr handles the future.
	if t, e := timeexpr.Parse(s, now); e == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("couldn't understand %q — try \"2026-07-04 15:00\" or \"2026-07-04\"", s)
}
