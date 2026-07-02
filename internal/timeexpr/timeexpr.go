// Package timeexpr parses a human time expression into an absolute instant. It is
// the single shared parser for every scheduler in the agent (reminders and
// errands), so both accept exactly the same syntax: relative durations, relative
// calendar offsets in days/weeks/months/years, wall-clock times, and full
// timestamps. time.ParseDuration alone tops out at the hour, so long horizons
// ("in two months") are expressed here through calendar offsets or an absolute
// date rather than an unwieldy hour count.
package timeexpr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// calendarRe matches a relative calendar offset whose unit is a day, week, month,
// or year. These units are not expressible via time.ParseDuration (its largest
// unit is the hour), so they are handled with time.AddDate, which shifts the
// calendar correctly across month and year boundaries.
var calendarRe = regexp.MustCompile(`(?i)^\+?\s*(\d+)\s*(d|days?|w|wk|wks|weeks?|mo|mon|months?|y|yr|yrs|years?)$`)

// Parse turns a human time expression into an absolute instant relative to now.
// It accepts, case-insensitively:
//
//   - "now" -> now
//   - a relative calendar offset: +2d, "3 days", +3w, +2mo, 1y
//   - a relative duration: +30m, 90m, 1h30m (hours and below)
//   - a full local timestamp: 2026-09-01T09:00[:05] (or space-separated)
//   - a wall-clock time: 15:30, 3:04PM (today if still ahead, else tomorrow)
//
// Absolute forms are read in now's location and must be in the future: a past
// timestamp, or a negative relative offset, is an error. That guarantee is what
// stops a reminder or errand from being scheduled into the past and firing
// immediately.
func Parse(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("no time given")
	}
	if strings.EqualFold(s, "now") {
		return now, nil
	}

	// Relative calendar offset (days / weeks / months / years).
	if m := calendarRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := strings.ToLower(m[2])
		switch {
		case strings.HasPrefix(unit, "d"):
			return now.AddDate(0, 0, n), nil
		case strings.HasPrefix(unit, "w"):
			return now.AddDate(0, 0, 7*n), nil
		case strings.HasPrefix(unit, "y"):
			return now.AddDate(n, 0, 0), nil
		case strings.HasPrefix(unit, "mo"):
			return now.AddDate(0, n, 0), nil
		}
	}

	// Relative duration: "+30m" or a bare "90m" / "1h30m".
	if d, err := time.ParseDuration(strings.TrimPrefix(s, "+")); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("a scheduled time can't be in the past (%q)", s)
		}
		return now.Add(d), nil
	}

	loc := now.Location()

	// Full local timestamps.
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			if t.Before(now) {
				return time.Time{}, fmt.Errorf("%s is in the past", t.Format("Mon 02 Jan 15:04"))
			}
			return t, nil
		}
	}

	// Wall-clock time only: today if still ahead, else tomorrow.
	for _, layout := range []string{"15:04", "15:04:05", "3:04PM", "3:04 PM"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			res := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
			if !res.After(now) {
				res = res.AddDate(0, 0, 1)
			}
			return res, nil
		}
	}

	return time.Time{}, fmt.Errorf("couldn't understand the time %q — try +30m, +2d, +3w, +2mo, 15:30, or 2026-09-01T09:00", s)
}
