package reminders

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
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
		return "", fmt.Errorf("usage: events <date and time>  (e.g. events today, events this week, events 2026-07-04 15:00)")
	}
	all, err := d.store.ListReminders()
	if err != nil {
		return "", err
	}
	// Spans first: "this week" and its neighbours cover several days and cannot be
	// expressed as a moment to look around, so they never reached a parse that
	// understood them and came back as an error instead of the owner's week.
	if lo, hi, label, ok := parseRange(when, now); ok {
		return spanReport(all, lo, hi, label, true), nil
	}
	anchor, hasTime, err := parseAnchor(when, now)
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
	if hasTime {
		return spanReport(all,
			anchor.Add(-eventsWindow), anchor.Add(eventsWindow),
			"Events within 2 hours of "+anchor.Format("Mon 02 Jan 2006, 3:04 PM")+":", false)
	}
	lo := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, loc)
	return spanReport(all,
		lo, lo.AddDate(0, 0, 1).Add(-time.Nanosecond),
		"Events on "+anchor.Format("Mon 02 Jan 2006")+":", false)
}

// spanReport lists every event overlapping [lo, hi] under the given header. byDay
// groups the lines under a date heading, which is what makes a multi-day answer
// readable; a single-day or few-hours window reads better as a flat list.
func spanReport(all []store.Reminder, lo, hi time.Time, header string, byDay bool) string {
	loc := lo.Location()

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
	indent, day := "  ", ""
	for _, h := range hits {
		if byDay {
			if d := h.start.Format("Mon 02 Jan"); d != day {
				day = d
				b.WriteString("  " + d + "\n")
			}
			indent = "    "
		}
		label := h.start.Format("3:04 PM")
		if !h.end.Equal(h.start) {
			label += "–" + h.end.Format("3:04 PM")
		}
		line := indent + label + "  " + h.r.Task
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

// parseDayWord resolves the everyday words for a day ("today", "tomorrow",
// "yesterday", a weekday name) to that local day. Weekday names look forward:
// "friday" is the coming Friday, or today if today is Friday.
func parseDayWord(s string, now time.Time) (time.Time, bool) {
	day := func(offset int) time.Time {
		t := now.AddDate(0, 0, offset)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location())
	}
	switch strings.ToLower(strings.Join(strings.Fields(s), " ")) {
	case "today", "tonight", "this evening", "this morning", "this afternoon", "rest of today", "the rest of today":
		return day(0), true
	case "tomorrow", "tmrw", "tomorow", "tomorrow morning", "tomorrow evening", "tomorrow night":
		return day(1), true
	case "yesterday":
		return day(-1), true
	case "day after tomorrow", "the day after tomorrow", "overmorrow":
		return day(2), true
	}
	weekdays := map[string]time.Weekday{
		"sunday": time.Sunday, "sun": time.Sunday,
		"monday": time.Monday, "mon": time.Monday,
		"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
		"wednesday": time.Wednesday, "wed": time.Wednesday,
		"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
		"friday": time.Friday, "fri": time.Friday,
		"saturday": time.Saturday, "sat": time.Saturday,
	}
	name := strings.ToLower(strings.TrimSpace(s))
	name = strings.TrimPrefix(name, "next ")
	name = strings.TrimPrefix(name, "this ")
	if wd, ok := weekdays[strings.TrimSpace(name)]; ok {
		offset := (int(wd) - int(now.Weekday()) + 7) % 7
		return day(offset), true
	}
	return time.Time{}, false
}

// nextDaysRe matches the open-ended spans ("next 3 days", "coming 2 weeks").
var nextDaysRe = regexp.MustCompile(`(?i)^(?:the\s+)?(?:next|coming|following)\s+(\d{1,3})\s*(day|days|week|weeks)$`)

// parseRange resolves the words for a span of days to the window they cover, and
// a header naming it. ok is false for anything that is a single moment or day,
// which parseAnchor handles.
//
// Spans that include today start at today rather than at the start of the
// calendar period: someone asking what is on this week on a Thursday wants the
// days they still have, not a recap of Monday.
func parseRange(s string, now time.Time) (lo, hi time.Time, label string, ok bool) {
	loc := now.Location()
	midnight := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}
	endOf := func(t time.Time) time.Time { return midnight(t).AddDate(0, 0, 1).Add(-time.Nanosecond) }
	today := midnight(now)

	// Monday-based offset of the current day within its week.
	intoWeek := (int(now.Weekday()) + 6) % 7
	weekStart := today.AddDate(0, 0, -intoWeek)

	named := func(lo, hi time.Time, name string) (time.Time, time.Time, string, bool) {
		return lo, hi, fmt.Sprintf("Events %s (%s to %s):", name,
			lo.Format("Mon 02 Jan"), hi.Format("Mon 02 Jan")), true
	}

	switch strings.ToLower(strings.Join(strings.Fields(s), " ")) {
	case "this week", "the week", "week", "rest of the week", "rest of this week", "the rest of the week":
		// Asked on the last day of the week, "this week" would otherwise cover that
		// one day. Nobody means that: on a Sunday the week being asked about is the
		// one starting tomorrow, so run through the end of it.
		last := weekStart.AddDate(0, 0, 6)
		if intoWeek == 6 {
			last = last.AddDate(0, 0, 7)
		}
		return named(today, endOf(last), "this week")
	case "next week", "the coming week", "coming week":
		start := weekStart.AddDate(0, 0, 7)
		return named(start, endOf(start.AddDate(0, 0, 6)), "next week")
	case "last week", "the past week", "previous week":
		start := weekStart.AddDate(0, 0, -7)
		return named(start, endOf(start.AddDate(0, 0, 6)), "last week")
	case "this weekend", "the weekend", "weekend", "coming weekend":
		// The Saturday of the current week, so asking on a Sunday still means today.
		start := weekStart.AddDate(0, 0, 5)
		return named(start, endOf(start.AddDate(0, 0, 1)), "this weekend")
	case "this month", "the month", "month", "rest of the month":
		return named(today, endOf(time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, loc).AddDate(0, 0, -1)), "this month")
	case "next month":
		start := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, loc)
		return named(start, endOf(start.AddDate(0, 1, -1)), "next month")
	}

	if m := nextDaysRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 {
			return time.Time{}, time.Time{}, "", false
		}
		days := n
		unit := "day"
		if strings.HasPrefix(strings.ToLower(m[2]), "week") {
			days, unit = n*7, "week"
		}
		return named(today, endOf(today.AddDate(0, 0, days-1)), fmt.Sprintf("in the next %d %s(s)", n, unit))
	}

	return time.Time{}, time.Time{}, "", false
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
	// Day words first: these are what a person (and the chat agent relaying them)
	// actually types. Without them "events today" was a parse error, so the agent
	// reported an empty or unreadable schedule instead of the owner's day.
	if t, ok := parseDayWord(s, now); ok {
		return t, false, nil // a day word means the whole day
	}
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
